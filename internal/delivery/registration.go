package delivery

import (
	"context"
	"errors"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// RegistrationRequest is what a caller supplies to register a deployment: the
// candidate source and the image to deploy. Everything else (tenant, project,
// run, step, action key, originating attempt, environment and its
// configuration identity) comes from the attempt's immutable pinned context.
type RegistrationRequest struct {
	Source Source
	Image  Image
}

// Validate checks the syntax.
func (r RegistrationRequest) Validate() error {
	if err := r.Source.Validate(); err != nil {
		return err
	}
	return r.Image.Validate()
}

// RegisterRuntime creates, or observes, the pre-apply deployment record of the
// attempt's logical action before any external change. It is idempotent: a
// retry, a concurrent identical call and an authorized retry attempt of the same
// step all receive the one original deployment and its receipt.
//
// Order: authenticate deployment.call and verify the pinned context; verify the
// retained configuration and image provenance; protect the image from retention;
// commit the record, history, audit and event in one transaction that repeats the
// session check after the execution locks.
func (s *Service) RegisterRuntime(ctx context.Context, attempt domain.AttemptID, bearer string, req RegistrationRequest) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "registerDeployment", err) }()
	call, err := s.beginRuntime(ctx, attempt, bearer)
	if err != nil {
		return Receipt{}, err
	}
	m := call.mutation(s.now(), "")
	return s.register(call.ctx, m, call.pinned, req)
}

// RegisterForAttempt is the human-authorized creation route. It still requires an
// existing originating attempt and uses that attempt's immutable environment and
// source context; it never creates a deployment from the environment's current
// settings.
func (s *Service) RegisterForAttempt(ctx context.Context, c Caller, scope domain.Scope, attempt domain.AttemptID, req RegistrationRequest) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "createDeployment", err) }()
	plan, err := s.PlanRegistrationForAttempt(ctx, c, scope, attempt, req)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.cfg.Store.Register(ctx, plan.Command)
	return s.CompleteRegistration(ctx, plan, res, err)
}

// RegistrationPlan is a verified, retention-protected registration waiting to
// commit. The HTTP adapter commits it inside the idempotency transaction (see
// postgres.Keyed) and then calls CompleteRegistration.
type RegistrationPlan struct {
	Command  RegisterCommand
	scope    domain.Scope
	artifact domain.ArtifactID
}

// PlanRegistrationForAttempt authorizes the human caller, reads the attempt's
// pinned context, verifies the retained configuration and the image's provenance
// and protects the image from retention. Nothing is committed.
func (s *Service) PlanRegistrationForAttempt(ctx context.Context, c Caller, scope domain.Scope, attempt domain.AttemptID, req RegistrationRequest) (plan RegistrationPlan, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "planDeployment", err) }()
	if err := s.authorize(ctx, c, scope, PermDeploymentManage); err != nil {
		return RegistrationPlan{}, err
	}
	if err := attempt.Validate(); err != nil {
		return RegistrationPlan{}, Fail(ErrNotFound, err, "not found")
	}
	if s.cfg.Contexts == nil {
		return RegistrationPlan{}, Fail(ErrPinRequired, nil, "no pinned-context reader is configured")
	}
	pinned, err := s.cfg.Contexts.Pinned(ctx, scope, attempt)
	if err != nil {
		return RegistrationPlan{}, err
	}
	p := Principal{Scope: scope, Run: pinned.RunID, Attempt: attempt}
	if err := CheckPinned(pinned, p); err != nil {
		return RegistrationPlan{}, err
	}
	m, err := s.mutation(c, "")
	if err != nil {
		return RegistrationPlan{}, err
	}
	return s.plan(ctx, m, pinned, req)
}

// CompleteRegistration finishes a plan after its commit attempt: a failure
// reconciles the retention pin (an uncertain outcome keeps it), a success
// returns the receipt.
func (s *Service) CompleteRegistration(ctx context.Context, plan RegistrationPlan, res RegisterResult, commitErr error) (Receipt, error) {
	if commitErr != nil {
		commitErr = Classify(commitErr)
		if !errors.Is(commitErr, ErrUnavailable) {
			// A definite refusal committed nothing for this request.
			s.reconcile(context.WithoutCancel(ctx), plan.scope, plan.Command.Deployment.ID(), plan.artifact)
		}
		return Receipt{}, commitErr
	}
	return s.receipt(res.Deployment, res.Created), nil
}

// CompleteReplay finishes a plan whose request was answered from a stored
// response: nothing was committed for it, and the preparation pin it re-took is
// reconciled against the authoritative record (kept when the committed
// deployment uses the artifact, released otherwise). It is safe to repeat and a
// failure only defers the reconciliation, never the replayed answer.
func (s *Service) CompleteReplay(ctx context.Context, plan RegistrationPlan) {
	s.reconcile(context.WithoutCancel(ctx), plan.scope, plan.Command.Deployment.ID(), plan.artifact)
}

func (s *Service) register(ctx context.Context, m Mutation, pinned runtimev1.Context, req RegistrationRequest) (Receipt, error) {
	plan, err := s.plan(ctx, m, pinned, req)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.cfg.Store.Register(ctx, plan.Command)
	return s.CompleteRegistration(ctx, plan, res, err)
}

func (s *Service) plan(ctx context.Context, m Mutation, pinned runtimev1.Context, req RegistrationRequest) (RegistrationPlan, error) {
	if s.cfg.Producers == nil || s.cfg.Artifacts == nil {
		return RegistrationPlan{}, Fail(ErrNotConfigured, nil, "deployment registration is not configured")
	}
	if err := req.Validate(); err != nil {
		return RegistrationPlan{}, err
	}
	scope := pinned.Scope()
	// The deployment's source is the candidate the attempt works on. A pinned
	// workspace source must be exactly the requested one.
	if src := pinned.Workspace.Source; src != nil && (src.RepositoryURL != req.Source.RepositoryURL || string(src.Commit) != req.Source.Commit) {
		return RegistrationPlan{}, Fail(ErrProvenance, nil, "the deployment source is not the attempt's source")
	}
	// The retained snapshot of the pin: never the environment's current state.
	snap, err := s.retainedPin(ctx, pinned)
	if err != nil {
		return RegistrationPlan{}, err
	}
	if !ReferencesArtifact(pinned, req.Image.ArtifactID) {
		return RegistrationPlan{}, Fail(ErrProvenance, nil, "the image is not an input of this attempt")
	}
	producer, err := s.verifyImage(ctx, scope, req)
	if err != nil {
		return RegistrationPlan{}, err
	}
	var expires *time.Time
	if snap.PreviewTTL > 0 {
		t := m.Now.Add(snap.PreviewTTL).UTC().Truncate(time.Microsecond)
		expires = &t
	}
	d, err := NewDeployment(Registration{
		Scope: scope, Environment: snap.Environment, ConfigDigest: snap.Digest, Namespace: snap.Namespace, Source: req.Source, Image: req.Image,
		Producer: producer, Run: pinned.RunID, Step: pinned.StepID, ActionKey: pinned.ActionKey, Attempt: pinned.AttemptID, ExpiresAt: expires,
	}, m.Now)
	if err != nil {
		return RegistrationPlan{}, err
	}

	// Retention. The artifact owner's pin opens its own transaction, so it cannot
	// join the deployment's commit. The pin is taken first and idempotently under
	// the deterministic deployment identity: an unknown commit outcome therefore
	// leaves the image protected (conservative), and a definite failure releases
	// the pin only after reconciliation shows no committed deployment uses it.
	if err := s.cfg.Artifacts.Protect(ctx, scope, req.Image.ArtifactID, d.ID()); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Retention removed the artifact between verification and the pin.
			return RegistrationPlan{}, Fail(ErrProvenance, err, "the image is not a verified output of this source")
		}
		return RegistrationPlan{}, Fail(ErrUnavailable, err, "the image could not be protected")
	}
	return RegistrationPlan{Command: RegisterCommand{Mutation: m, Deployment: d, Attempt: pinned.AttemptID}, scope: scope, artifact: req.Image.ArtifactID}, nil
}

// verifyImage reads the artifact owner's and the execution owner's evidence and
// applies DQ-01/DQ-07.
func (s *Service) verifyImage(ctx context.Context, scope domain.Scope, req RegistrationRequest) (Producer, error) {
	ev, err := s.cfg.Artifacts.Evidence(ctx, scope, req.Image.ArtifactID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Producer{}, Fail(ErrProvenance, err, "the image is not a verified output of this source")
		}
		return Producer{}, Fail(ErrUnavailable, err, "the artifact owner could not answer")
	}
	out, err := s.cfg.Producers.Produced(ctx, scope, ev.Attempt, req.Image.ArtifactID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Producer{}, Fail(ErrProvenance, err, "the image is not a verified output of this source")
		}
		return Producer{}, Fail(ErrUnavailable, err, "the execution owner could not answer")
	}
	return VerifyImage(scope, req.Source, req.Image, ev, out)
}

// ReconcileRetention settles the retention pin an uncertain or refused
// registration may have left: it keeps the pin if a committed deployment of the
// logical action uses the artifact, and releases it only when the authoritative
// record shows none does. It is safe to repeat.
func (s *Service) ReconcileRetention(ctx context.Context, scope domain.Scope, id DeploymentID, artifact domain.ArtifactID) (err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "reconcileRetention", err) }()
	if s.cfg.Artifacts == nil {
		return Fail(ErrNotConfigured, nil, "artifact retention is not configured")
	}
	if err := id.Validate(); err != nil {
		return err
	}
	d, err := s.cfg.Store.GetDeployment(ctx, scope, id)
	switch {
	case err == nil:
		if d.rec.Image.ArtifactID == artifact {
			return s.cfg.Artifacts.Protect(ctx, scope, artifact, id)
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}
	return s.cfg.Artifacts.Release(ctx, scope, artifact, id)
}

func (s *Service) reconcile(ctx context.Context, scope domain.Scope, id DeploymentID, artifact domain.ArtifactID) {
	if err := s.ReconcileRetention(ctx, scope, id, artifact); err != nil {
		// The pin stays: protection is the safe side. A later reconciliation
		// resolves it.
		s.log.Warn("delivery: retention reconciliation deferred", "deploymentId", string(id))
	}
}

// LookupByAction returns the deployment of the caller's own logical action so a
// retried or restarted script observes the recorded identities, and an unknown
// outcome, before applying again. The key must be the attempt's pinned key.
func (s *Service) LookupByAction(ctx context.Context, attempt domain.AttemptID, bearer, actionKey string) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "lookupDeployment", err) }()
	call, err := s.beginRuntime(ctx, attempt, bearer)
	if err != nil {
		return Receipt{}, err
	}
	if actionKey != call.pinned.ActionKey {
		return Receipt{}, Fail(ErrPinRequired, nil, "the action key is not the attempt's pinned key")
	}
	d, err := s.cfg.Store.FindByAction(call.ctx, call.p.Scope, actionKey)
	if err != nil {
		return Receipt{}, err
	}
	if !d.BoundTo(call.pinned) {
		return Receipt{}, Fail(ErrPinRequired, nil, "the deployment does not match the attempt's pinned context")
	}
	return s.receipt(d, false), nil
}

// GetRuntime returns a deployment to a runtime caller bound to it: the step of
// its own logical action, or a cleanup run proven bound to it.
func (s *Service) GetRuntime(ctx context.Context, attempt domain.AttemptID, bearer string, id DeploymentID) (rc Receipt, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "getRuntimeDeployment", err) }()
	call, err := s.beginRuntime(ctx, attempt, bearer)
	if err != nil {
		return Receipt{}, err
	}
	if err := id.Validate(); err != nil {
		return Receipt{}, Fail(ErrNotFound, err, "not found")
	}
	d, err := s.cfg.Store.GetDeployment(call.ctx, call.p.Scope, id)
	if err != nil {
		return Receipt{}, err
	}
	if !d.BoundTo(call.pinned) {
		if err := s.checkCleanupBinding(call, d); err != nil {
			return Receipt{}, err
		}
	}
	return s.receipt(d, false), nil
}
