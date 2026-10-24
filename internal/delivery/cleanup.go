package delivery

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RequestCleanup records the request to clean up a settled (or unknown-outcome)
// deployment's owned objects. It is idempotent; the cleanup itself is an
// ordinary internal run scheduled elsewhere (T-066). This is the scheduler-facing
// form: it accepts the deployment in whatever version it has. Human callers use
// RequestCleanupAt.
func (s *Service) RequestCleanup(ctx context.Context, c Caller, scope domain.Scope, id DeploymentID) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "requestCleanup", err) }()
	return s.requestCleanup(ctx, c, scope, id, 0)
}

// RequestCleanupAt is the public transition: it records the cleanup request only
// if expected is the deployment's current version. The comparison is made inside
// the store's write on the locked row, so a stale request can never record a
// cleanup against state the caller has not seen. A repeated request whose version
// moved on is a stale version, not a silent replay.
func (s *Service) RequestCleanupAt(ctx context.Context, c Caller, scope domain.Scope, id DeploymentID, expected domain.Version) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "requestCleanupAt", err) }()
	if err := expected.Validate(); err != nil {
		return Receipt{}, invalidf("%v", err)
	}
	return s.requestCleanup(ctx, c, scope, id, expected)
}

// requestCleanup applies the transition; expected 0 means no version precondition.
func (s *Service) requestCleanup(ctx context.Context, c Caller, scope domain.Scope, id DeploymentID, expected domain.Version) (Receipt, error) {
	if err := s.authorize(ctx, c, scope, PermDeploymentManage); err != nil {
		return Receipt{}, err
	}
	if err := id.Validate(); err != nil {
		return Receipt{}, Fail(ErrNotFound, err, "not found")
	}
	m, err := s.mutation(c, "cleanup requested")
	if err != nil {
		return Receipt{}, err
	}
	d, _, err := s.cfg.Store.Update(ctx, scope, id, m, func(d *Deployment) (Transition, error) {
		if expected != 0 && d.Version() != expected {
			return Transition{}, Fail(ErrVersionConflict, nil, "the deployment changed")
		}
		changed, err := d.RequestCleanup(m.Now)
		return Transition{Kind: "cleanup_requested", Change: Change{FromStatus: d.Status(), ToStatus: d.Status(), FromReason: d.Reason(), ToReason: d.Reason(), Changed: changed}}, err
	})
	if err != nil {
		return Receipt{}, err
	}
	return s.receipt(d, false), nil
}

// CleanupReport is a cleanup run's report about the deployment's owned objects.
type CleanupReport struct {
	State   CleanupState
	Reason  string
	Deleted []ObjectRef
}

// checkCleanupBinding proves the caller is a cleanup run admitted for exactly
// this deployment, and that its pinned environment is the deployment's retained
// configuration. Environment equality alone is not proof; an absent binding or
// reader denies.
func (s *Service) checkCleanupBinding(call runtimeCall, d Deployment) error {
	deny := func() error { return Fail(ErrDenied, nil, "the attempt is not bound to this deployment") }
	if s.cfg.Cleanups == nil {
		return deny()
	}
	bound, found, err := s.cfg.Cleanups.Binding(call.ctx, call.p.Scope, call.p.Run)
	if err != nil {
		return Fail(ErrUnavailable, err, "the cleanup binding could not be read")
	}
	if !found || bound != d.ID() || d.Scope() != call.p.Scope || d.Pin() != *call.pinned.Environment {
		return deny()
	}
	// The run's retained configuration (already checked against its credential
	// bindings) is the deployment's own: the same pin, scope and namespace.
	if call.snapshot.Pin() != d.Pin() || call.snapshot.Scope != d.Scope() || call.snapshot.Namespace != d.rec.Namespace {
		return deny()
	}
	return nil
}

// ReportCleanup records a cleanup run's result. The run must prove its binding
// to the deployment; a report about objects the deployment does not own, or
// before cleanup was requested, is refused. Failed and unknown results are kept
// as visible exceptions and may be retried; a completed cleanup sets deleted_at
// and keeps the outcome and objects.
func (s *Service) ReportCleanup(ctx context.Context, attempt domain.AttemptID, bearer string, id DeploymentID, rep CleanupReport) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "reportCleanup", err) }()
	call, err := s.beginRuntime(ctx, attempt, bearer)
	if err != nil {
		return Receipt{}, err
	}
	if err := id.Validate(); err != nil {
		return Receipt{}, Fail(ErrNotFound, err, "not found")
	}
	current, err := s.cfg.Store.GetDeployment(call.ctx, call.p.Scope, id)
	if err != nil {
		return Receipt{}, err
	}
	if err := s.checkCleanupBinding(call, current); err != nil {
		return Receipt{}, err
	}
	m := call.mutation(s.now(), "cleanup "+string(rep.State))
	d, _, err := s.cfg.Store.Update(call.ctx, call.p.Scope, id, m, func(d *Deployment) (Transition, error) {
		if err := s.checkCleanupBinding(call, *d); err != nil {
			return Transition{}, err
		}
		changed, err := d.RecordCleanup(CleanupResult{State: rep.State, Reason: rep.Reason, Deleted: rep.Deleted}, m.Now)
		kind := "cleanup_" + string(rep.State)
		return Transition{Kind: kind, Attempt: call.p.Attempt, Detail: map[string]any{"deleted": len(rep.Deleted)},
			Change: Change{FromStatus: d.Status(), ToStatus: d.Status(), FromReason: d.Reason(), ToReason: d.Reason(), Changed: changed}}, err
	})
	if err != nil {
		return Receipt{}, err
	}
	return s.receipt(d, false), nil
}

// AuthorizeCleanup implements the boundary execution consumes
// (execution.DeploymentCleanup): the recorded deployment exists in the scope,
// cleanup was requested for it and it is not already deleted.
func (s *Service) AuthorizeCleanup(ctx context.Context, scope domain.Scope, deployment string) (err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "authorizeCleanup", err) }()
	id := DeploymentID(deployment)
	if err := id.Validate(); err != nil {
		return Fail(ErrNotFound, err, "not found")
	}
	d, err := s.cfg.Store.GetDeployment(ctx, scope, id)
	if err != nil {
		return err
	}
	if d.rec.CleanupRequestedAt == nil || d.Deleted() {
		return Fail(ErrDenied, nil, "cleanup is not requested for this deployment")
	}
	return nil
}

// CleanupView is the richer record T-077 and T-066 consume: the deployment, the
// retained configuration snapshot it pinned, and the exact objects it owns.
type CleanupView struct {
	Deployment Deployment
	Snapshot   ConfigurationSnapshot
	Objects    []ObjectRef
}

// RetainedView returns the retained configuration and owned objects of a
// recorded deployment. The configuration is the immutable snapshot the
// deployment pinned, never the environment's current settings.
func (s *Service) RetainedView(ctx context.Context, scope domain.Scope, id DeploymentID) (v CleanupView, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "retainedView", err) }()
	if err := id.Validate(); err != nil {
		return CleanupView{}, Fail(ErrNotFound, err, "not found")
	}
	d, err := s.cfg.Store.GetDeployment(ctx, scope, id)
	if err != nil {
		return CleanupView{}, err
	}
	snap, err := s.cfg.Store.GetSnapshot(ctx, scope, d.rec.Environment, d.rec.ConfigDigest)
	if err != nil {
		return CleanupView{}, err
	}
	return CleanupView{Deployment: d, Snapshot: snap, Objects: d.Record().Objects}, nil
}

// ExpiredDeployments enumerates the scope's deployments past their effective
// expiry that were not deleted, for the scheduler (T-066) to request cleanup.
func (s *Service) ExpiredDeployments(ctx context.Context, scope domain.Scope, limit int) (out []Deployment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "expiredDeployments", err) }()
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if limit < 1 || limit > 500 {
		return nil, invalidf("the batch size is out of range")
	}
	return s.cfg.Store.Expired(ctx, scope, s.now().Truncate(time.Microsecond), limit)
}
