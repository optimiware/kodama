package delivery

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
)

// OutcomeState is what a script reports about its apply.
type OutcomeState string

const (
	// OutcomeApplying records the intended objects before the first external
	// change (pending to running).
	OutcomeApplying OutcomeState = "applying"
	// OutcomeSucceeded settles a deployment whose every intended object was
	// observed ready (structured readiness, not output text or an exit code).
	OutcomeSucceeded OutcomeState = "succeeded"
	// OutcomeFailed settles a failed or never started deployment.
	OutcomeFailed OutcomeState = "failed"
	// OutcomeUnknown records an apply that may have taken effect but could not
	// be confirmed. The deployment stays running and blocks a new apply.
	OutcomeUnknown OutcomeState = "unknown"
)

// OutcomeReport is one script report. Objects are intended identities for
// "applying" and observed objects (UID, readiness) afterwards.
type OutcomeReport struct {
	State     OutcomeState
	Reason    string
	Objects   []ObjectRef
	Resources []ResourceRef
}

// Validate checks the structure; the entity applies the lifecycle rules.
func (r OutcomeReport) Validate() error {
	switch r.State {
	case OutcomeApplying, OutcomeSucceeded, OutcomeFailed, OutcomeUnknown:
	default:
		return invalidf("an outcome is applying, succeeded, failed or unknown")
	}
	if len(r.Objects) > MaxObjects {
		return invalidf("a report lists at most %d objects", MaxObjects)
	}
	if len(r.Resources) > MaxResources {
		return invalidf("a report lists at most %d resources", MaxResources)
	}
	if r.State == OutcomeFailed && !reasonCode.MatchString(r.Reason) {
		return invalidf("a failure reports a reason code")
	}
	if r.State != OutcomeFailed && r.Reason != "" {
		return invalidf("only a failure carries a reason")
	}
	if r.State == OutcomeUnknown && len(r.Objects) != 0 {
		return invalidf("an unknown outcome observed nothing")
	}
	return nil
}

// ReportOutcome records a structured outcome of the caller's own deployment. The
// deployment must belong to the attempt's pinned logical action and environment
// configuration; any authorized attempt of the step may report, and a replay
// returns the stored receipt. Settled history is never rewritten, and an
// unknown outcome is never turned into permission to apply again.
func (s *Service) ReportOutcome(ctx context.Context, attempt domain.AttemptID, bearer string, id DeploymentID, rep OutcomeReport) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "reportOutcome", err) }()
	call, err := s.beginRuntime(ctx, attempt, bearer)
	if err != nil {
		return Receipt{}, err
	}
	if err := id.Validate(); err != nil {
		return Receipt{}, Fail(ErrNotFound, err, "not found")
	}
	if err := rep.Validate(); err != nil {
		return Receipt{}, err
	}
	current, err := s.cfg.Store.GetDeployment(call.ctx, call.p.Scope, id)
	if err != nil {
		return Receipt{}, err
	}
	if !current.BoundTo(call.pinned) {
		return Receipt{}, Fail(ErrPinRequired, nil, "the deployment does not match the attempt's pinned context")
	}
	m := call.mutation(s.now(), string(rep.State))
	d, _, err := s.cfg.Store.Update(call.ctx, call.p.Scope, id, m, func(d *Deployment) (Transition, error) {
		// Rechecked on the locked row: the binding is part of the write.
		if !d.BoundTo(call.pinned) {
			return Transition{}, Fail(ErrPinRequired, nil, "the deployment does not match the attempt's pinned context")
		}
		var (
			ch   Change
			kind string
			err  error
		)
		switch rep.State {
		case OutcomeApplying:
			kind = "apply_started"
			if len(rep.Resources) != 0 {
				ch, err = d.BeginApplyResources(rep.Resources, m.Now)
			} else {
				ch, err = d.BeginApply(rep.Objects, m.Now)
			}
		case OutcomeUnknown:
			kind = "outcome_unknown"
			ch, err = d.MarkOutcomeUnknown(m.Now)
		case OutcomeSucceeded:
			kind = "succeeded"
			ch, err = d.Settle(Settlement{Succeeded: true, Objects: rep.Objects, Resources: rep.Resources}, m.Now)
		case OutcomeFailed:
			kind = "failed"
			ch, err = d.Settle(Settlement{Reason: rep.Reason, Objects: rep.Objects, Resources: rep.Resources}, m.Now)
		}
		return Transition{Kind: kind, Change: ch, Attempt: call.p.Attempt, Detail: map[string]any{"objects": len(rep.Objects), "resources": len(rep.Resources)}}, err
	})
	if err != nil {
		return Receipt{}, err
	}
	return s.receipt(d, false), nil
}

// GetDeployment returns a deployment of the scope to a human caller.
func (s *Service) GetDeployment(ctx context.Context, c Caller, scope domain.Scope, id DeploymentID) (d Deployment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "getDeployment", err) }()
	if err := s.authorize(ctx, c, scope, PermRunRead); err != nil {
		return Deployment{}, err
	}
	if err := id.Validate(); err != nil {
		return Deployment{}, Fail(ErrNotFound, err, "not found")
	}
	return s.cfg.Store.GetDeployment(ctx, scope, id)
}

// ListDeployments returns up to limit deployments ordered by ID.
func (s *Service) ListDeployments(ctx context.Context, c Caller, scope domain.Scope, f DeploymentFilter, afterID DeploymentID, limit int) (out []Deployment, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "listDeployments", err) }()
	if err := s.authorize(ctx, c, scope, PermRunRead); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 201 {
		return nil, invalidf("the page size is out of range")
	}
	if f.Environment != "" {
		if err := f.Environment.Validate(); err != nil {
			return nil, invalidf("%v", err)
		}
	}
	switch f.Status {
	case "", StatusPending, StatusRunning, StatusSucceeded, StatusFailed:
	default:
		return nil, invalidf("unknown deployment status")
	}
	return s.cfg.Store.ListDeployments(ctx, scope, f, afterID, limit)
}

// Receipt returns the receipt of a deployment: its identity and the annotation
// values scripts and operators reconcile with.
func (s *Service) Receipt(d Deployment) Receipt { return s.receipt(d, false) }

// History returns the append-only history of a deployment to a human caller.
func (s *Service) History(ctx context.Context, c Caller, scope domain.Scope, id DeploymentID) (out []HistoryEntry, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "deploymentHistory", err) }()
	if err := s.authorize(ctx, c, scope, PermRunRead); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, Fail(ErrNotFound, err, "not found")
	}
	return s.cfg.Store.History(ctx, scope, id)
}
