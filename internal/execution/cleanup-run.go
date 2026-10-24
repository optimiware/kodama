package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MaxCleanupRounds bounds the cleanup runs one deployment can ever have. A
// retry or an explicit reconciliation of a deployment's cleanup is another
// round; the bound keeps the probe of a deployment's rounds finite and
// a failing cleanup from being retried without limit (PX-07).
const MaxCleanupRounds = 32

// CleanupRunID derives the identity of a deployment's cleanup run for a round.
// It is a pure function of (tenant, project, deployment, round): two schedulers,
// a retry after a lost admission response and a restarted control plane all name
// the same run, so the run's own primary key makes a duplicate claim converge on
// one run instead of two competing deleters. A later round is a new identity, so
// a settled failure never blocks its own retry.
func CleanupRunID(scope domain.Scope, deployment string, round int) domain.RunID {
	h := sha256.New()
	for _, part := range []string{"kodama/cleanup-run/v1", string(scope.TenantID), string(scope.ProjectID), deployment} {
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return domain.RunID("run_cln_" + hex.EncodeToString(h.Sum(nil)[:16]) + "_" + strconv.Itoa(round))
}

// CleanupDisposition classifies what is known about one cleanup run's external
// effect, which decides whether a further round is a safe retry.
type CleanupDisposition string

const (
	// CleanupInFlight: the run is not terminal and nothing is wrong; another claim
	// must not start one.
	CleanupInFlight CleanupDisposition = "in_flight"
	// CleanupCompleted: the run succeeded. The deployment record, not the run,
	// says whether the objects were reported deleted.
	CleanupCompleted CleanupDisposition = "completed"
	// CleanupFailedKnown: the run ended without an ambiguous effect (it failed or
	// was cancelled and every attempt's effect is accounted for). A new round is a
	// safe, bounded retry.
	CleanupFailedKnown CleanupDisposition = "failed"
	// CleanupOutcomeUnknown: an attempt's external effect is ambiguous. While the
	// run is open, execution's effect reconciliation owns it; once it ended no new
	// round is a retry: it is an explicit reconciliation that observes the
	// recorded objects first.
	CleanupOutcomeUnknown CleanupDisposition = "unknown"
)

// CleanupRun is the scheduler-facing summary of one round of a deployment's
// cleanup.
type CleanupRun struct {
	Round        int
	RunID        domain.RunID
	DeploymentID string
	Status       RunStatus
	Reason       Reason
	FinishedAt   *time.Time
	Disposition  CleanupDisposition
}

// unaccountedEffect reports whether an ended attempt may have changed external
// objects without evidence that it did not: its launch is unknown, or it failed
// ambiguously, and no reconciliation proved a replay safe. Only a not-started
// launch or a reconciliation with replay-safe evidence accounts for it; a
// reconciliation that merely settled the attempt does not.
func unaccountedEffect(at *Attempt, c *Control) bool {
	if !at.Status.Terminal() || at.Status == AttemptSucceeded {
		return false
	}
	if c == nil {
		return true
	}
	if c.Launch == LaunchNotStarted || (c.ReconciledAt != nil && c.ReplaySafe) {
		return false
	}
	return at.Reason == ReasonOutcomeUnknown || c.Launch == LaunchUnknown || c.Effect == EffectUnknown
}

// summarizeCleanup classifies a round from its attempts, not from the run's
// retained first failure reason: that reason names one failure, while another
// step's ambiguous attempt may still be an unknown external effect (for example
// a known command failure recorded first, then the ambiguous attempt settled by
// a reconciliation that found no replay-safe evidence).
func summarizeCleanup(round int, a *Aggregate) CleanupRun {
	r := a.Run
	out := CleanupRun{Round: round, RunID: r.ID, DeploymentID: r.DeploymentID, Status: r.Status, Reason: r.Reason, FinishedAt: r.FinishedAt}
	unknown := r.Reason == ReasonOutcomeUnknown || r.FailureReason == ReasonOutcomeUnknown
	for _, at := range a.Attempts {
		if unaccountedEffect(at, a.Controls[at.ID]) {
			unknown = true
		}
	}
	switch {
	case r.Status == RunSucceeded:
		out.Disposition = CleanupCompleted
	case unknown:
		// While the run is open, execution's effect reconciliation owns the
		// attempt; once it ended no new round is a retry. Either way nothing is
		// started implicitly.
		out.Disposition = CleanupOutcomeUnknown
	case !r.Status.Terminal():
		out.Disposition = CleanupInFlight
	default:
		out.Disposition = CleanupFailedKnown
	}
	return out
}

// CleanupRounds returns the rounds of the deployment's cleanup in order, up to
// max (at most MaxCleanupRounds). Rounds are probed by their derived identity, so
// the list is exact whatever other runs the project has, and a run of another
// scope, purpose or deployment is never reported.
func (s *Service) CleanupRounds(ctx context.Context, scope domain.Scope, deployment string, max int) ([]CleanupRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := domain.WorkItemID(deployment).Validate(); err != nil {
		return nil, invalidf("a recorded deployment is required")
	}
	if max < 1 || max > MaxCleanupRounds {
		return nil, invalidf("the cleanup round limit is out of range")
	}
	var out []CleanupRun
	for round := 1; round <= max; round++ {
		a, err := s.d.Store.Get(ctx, scope, CleanupRunID(scope, deployment, round))
		if errors.Is(err, ErrNotFound) {
			return out, nil
		}
		if err != nil {
			return nil, mapLookup(err, "cleanup run")
		}
		if a.Run.Purpose != PurposeCleanup || a.Run.DeploymentID != deployment || a.Run.Scope != scope {
			return nil, fmt.Errorf("%w: a run holds the identity of a cleanup round", ErrConflict)
		}
		out = append(out, summarizeCleanup(round, a))
	}
	return out, nil
}

// AdmitCleanupRound admits the cleanup run of one round of the deployment's
// cleanup. It is AdmitCleanup with the run's identity derived from the
// deployment and the round, so it is idempotent: a repeat or a concurrent claim
// of the same round returns the stored run with created=false and starts
// nothing. Current deployment-bound authorization is rechecked for every new
// run (PrepareCleanup); an existing round is returned as it is.
func (s *Service) AdmitCleanupRound(ctx context.Context, cmd CleanupCommand, round int) (*Run, bool, error) {
	if round < 1 || round > MaxCleanupRounds {
		return nil, false, invalidf("the cleanup round is out of range")
	}
	if err := cmd.Scope.Validate(); err != nil {
		return nil, false, invalidf("%v", err)
	}
	id := CleanupRunID(cmd.Scope, cmd.DeploymentID, round)
	if a, err := s.d.Store.Get(ctx, cmd.Scope, id); err == nil {
		if a.Run.Purpose != PurposeCleanup || a.Run.DeploymentID != cmd.DeploymentID {
			return nil, false, fmt.Errorf("%w: a run holds the identity of a cleanup round", ErrConflict)
		}
		return a.Run, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, mapLookup(err, "cleanup run")
	}
	pinned := *s
	newID := s.d.NewID
	pinned.d.NewID = func(prefix string) string {
		if prefix == "run" {
			return string(id)
		}
		return newID(prefix)
	}
	plan, err := pinned.PrepareCleanup(ctx, cmd)
	if err != nil {
		return nil, false, err
	}
	return s.d.Store.Admit(ctx, plan)
}

// CleanupBindings proves which recorded deployment a cleanup run was admitted
// for, from the run's own durable purpose and deployment. It never infers a
// binding from labels, an environment or an attempt's request: a run that is not
// a cleanup run, or is not in the scope, has none.
type CleanupBindings struct{ store Store }

// NewCleanupBindings requires the execution store.
func NewCleanupBindings(store Store) (*CleanupBindings, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: a store is required", ErrNotConfigured)
	}
	return &CleanupBindings{store: store}, nil
}

// Binding returns the deployment the cleanup run is bound to. found is false for
// a missing, foreign or non-cleanup run; an outage is an error, never a binding.
func (b *CleanupBindings) Binding(ctx context.Context, scope domain.Scope, run domain.RunID) (string, bool, error) {
	if scope.Validate() != nil || run.Validate() != nil {
		return "", false, nil
	}
	a, err := b.store.Get(ctx, scope, run)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrHidden):
		return "", false, nil
	case err != nil:
		return "", false, Unavailablef(err, "the cleanup run could not be read")
	}
	if a.Run.Scope != scope || a.Run.Purpose != PurposeCleanup || a.Run.DeploymentID == "" {
		return "", false, nil
	}
	return a.Run.DeploymentID, true, nil
}
