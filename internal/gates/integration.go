package gates

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// ErrNoApproval: no approved, still-applicable candidate exists for the gate and
// repository: the gate is not approved (pending, rejected), is expired,
// invalidated or stale, belongs to another run, its step did not pass, the
// repository is not a writable source of its subject, or there is nothing to
// integrate. No integration grant exists for it (DQ-17, GR-12).
var ErrNoApproval = errors.New("gates: no approved candidate")

func noApproval(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNoApproval, fmt.Sprintf(format, args...))
}

// IntegrationApproval is what the issuer of an integration grant needs to know,
// derived from the gate and the repository and never from a request: the default
// branch the approval names (Ref), the tip the approval was given over (Base),
// the approved commit (Candidate) and the default branch's tip now (CurrentTip).
// It is the data of runtime v3's IntegrationApproval, plus the run the gate
// belongs to.
type IntegrationApproval struct {
	GateID       ID
	RunID        domain.RunID
	RepositoryID string
	Ref          string
	Base         string
	Candidate    string
	CurrentTip   string
}

// ErrAlreadyIntegrated: the repository's default branch already is the approved
// candidate: the integration was applied (a replay after a lost push result, or
// a second request after success). Nothing is left to authorize, so no grant is
// issued; the error carries the facts in *AlreadyIntegratedError so the pipeline
// can report success instead of a conflict.
var ErrAlreadyIntegrated = errors.New("gates: already integrated")

// AlreadyIntegratedError carries the approval whose update is already applied.
type AlreadyIntegratedError struct {
	Approval IntegrationApproval
}

func (e *AlreadyIntegratedError) Error() string {
	return "gates: already integrated: the default branch is at the approved candidate"
}
func (e *AlreadyIntegratedError) Unwrap() error { return ErrAlreadyIntegrated }

// IntegrationApproval answers whether the integration of one repository of a
// gate's subject is authorized now, for the run that asks. It is the one query a
// grant issuer (T-150) consumes: no grant is minted here, and none may be minted
// without a nil error from this call. Every check is made against current
// records:
//
//   - the gate exists in the scope and belongs to the asking run;
//   - it is approved by a human who still holds the reviewer authority now (the
//     check step completion makes), not expired, not invalidated, with applicable
//     conformance evidence, and its step passed (succeeded);
//   - its subject names a source set in which the repository is a writable source
//     with an integration base and a candidate that differs from it;
//   - the subject still describes the run (content, candidate commits, evidence,
//     deployment), recomputed from current records;
//   - the repository's default branch is still at the approved base.
//
// The default branch's tip decides the rest. At the base: the approval is
// returned. At the approved candidate: the update was already applied and the
// answer is *AlreadyIntegratedError (ErrAlreadyIntegrated), whatever became of the
// approval since (expiry, a later change). At any other commit or branch: the
// approval no longer applies, the gate is invalidated once (base_moved), and the
// answer is a *StaleError (errors.Is ErrStaleSubject) whose SupersedingCommit is
// the current tip; the pipeline recomputes the candidate and asks again.
//
// Every error comes with the zero IntegrationApproval; nothing partial can be
// mistaken for an approval. Any refusal that is not one of the above is
// ErrNoApproval (ErrNotFound for an absent or foreign gate); an outage is
// ErrUnavailable and never an approval. The candidate's ancestry from the base is
// not checked here: Git's fast-forward rule and the expected-old check at the push
// are the protection.
func (s *Service) IntegrationApproval(ctx context.Context, scope domain.Scope, run domain.RunID, gate ID, repository string) (IntegrationApproval, error) {
	var zero IntegrationApproval
	if err := scope.Validate(); err != nil || gate.Validate() != nil || run.Validate() != nil || !repoIDPattern.MatchString(repository) {
		return zero, ErrNotFound
	}
	g, err := s.d.Store.Get(ctx, scope, gate)
	if err != nil {
		return zero, err
	}
	if g.RunID != run {
		return zero, noApproval("the gate belongs to another run")
	}
	set, isSet := g.Subject.SourceSet()
	if !isSet {
		return zero, noApproval("the gate's subject names no source set")
	}
	src, ok := set.Source(repository)
	if !ok || src.Base == nil {
		return zero, noApproval("the repository is not a writable source of the gate's subject")
	}
	out := IntegrationApproval{GateID: g.ID, RunID: g.RunID, RepositoryID: repository, Ref: src.Base.Ref, Base: src.Base.Commit, Candidate: src.Commit}
	if g.Status != StatusApproved || g.Decision == nil {
		return zero, noApproval("the gate is %s", g.Status)
	}
	if src.Base.Commit == src.Commit {
		return zero, noApproval("the candidate is the base: there is nothing to integrate")
	}
	// The deciding reviewer must be a person who still holds the reviewer
	// authority: the same current-authority check step completion makes.
	if g.Decision.By.Type != ActorUser {
		return zero, noApproval("the gate was not decided by a person")
	}
	switch err := s.d.Authority.Authorize(ctx, g.Decision.By, scope, PermDecide); {
	case err == nil:
	case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound):
		return zero, noApproval("the deciding reviewer no longer holds the reviewer authority")
	default:
		return zero, Unavailablef(err, "the authority is unavailable")
	}
	baseMoved := g.Invalidation != nil && g.Invalidation.Reason == ReasonBaseMoved
	agg, err := s.d.Runs.Get(ctx, scope, run)
	if err != nil {
		return zero, mapRunError(err)
	}
	if st := agg.Step(g.StepID); st == nil || st.GateRef != string(g.ID) || (st.Status != execution.StepSucceeded && !baseMoved) {
		return zero, noApproval("the gate step has not passed")
	}
	tips, err := s.tips(ctx, scope, []string{repository})
	if err != nil {
		return zero, err
	}
	tip := tips[repository]
	out.CurrentTip = tip.Commit
	if tip.Ref == src.Base.Ref && tip.Commit == src.Commit {
		return zero, &AlreadyIntegratedError{Approval: out}
	}
	moved := func() error {
		se := definitive(ReasonBaseMoved)
		se.SupersedingCommit = tip.Commit
		return se
	}
	if baseMoved {
		// An approval whose base moved stays a conflict however often asked.
		return zero, moved()
	}
	if err := g.Applicable(s.now()); err != nil {
		return zero, noApproval("the approval is not applicable (%v)", err)
	}
	if err := s.d.Store.Reevaluate(ctx, scope, gate); err != nil {
		var se *StaleError
		if errors.As(err, &se) && se.Definitive {
			_, _, _ = s.d.Store.Invalidate(context.WithoutCancel(ctx), scope, gate, se, "")
			return zero, noApproval("the approval no longer describes the run (%s)", se.Reason)
		}
		if errors.Is(err, ErrStaleSubject) {
			return zero, noApproval("the approval is not applicable now")
		}
		return zero, err
	}
	if tip.Ref != src.Base.Ref || tip.Commit != src.Base.Commit {
		se := moved()
		if _, _, ierr := s.d.Store.Invalidate(context.WithoutCancel(ctx), scope, gate, se.(*StaleError), ""); ierr != nil {
			s.log(ctx, "invalidate", ierr, "gate", string(gate))
		}
		return zero, se
	}
	return out, nil
}

// checkBeforeDecision refuses a decision on a pending gate whose integration
// bases moved, and records the invalidation once. The gate is read and the tips
// observed outside any transaction; a decided or already invalidated gate is left
// to the store (replays converge there).
func (s *Service) checkBeforeDecision(ctx context.Context, requestID string, scope domain.Scope, id ID) error {
	g, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // the store answers
		}
		return err
	}
	if g.Status != StatusPending || g.Invalidation != nil {
		return nil
	}
	repos := g.BaseRepositories()
	if len(repos) == 0 {
		return nil
	}
	tips, err := s.tips(ctx, scope, repos)
	if err != nil {
		return err
	}
	if err := g.CheckBases(tips); err != nil {
		var se *StaleError
		if errors.As(err, &se) && se.Definitive {
			if _, _, ierr := s.d.Store.Invalidate(context.WithoutCancel(ctx), scope, id, se, requestID); ierr != nil {
				s.log(ctx, "invalidate", ierr, "gate", string(id))
			}
		}
		return err
	}
	return nil
}
