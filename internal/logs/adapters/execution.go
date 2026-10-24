package adapters

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/logs"
)

// RunReader is the part of the execution store the log module reads. The
// execution PostgreSQL store satisfies it; both lookups are scoped, and an
// attempt of another scope is ErrNotFound.
type RunReader interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Attempts implements logs.Attempts over execution's scoped lookups.
type Attempts struct{ runs RunReader }

var _ logs.Attempts = (*Attempts)(nil)

// NewAttempts requires the run reader.
func NewAttempts(r RunReader) (*Attempts, error) {
	if r == nil {
		return nil, errors.New("logs/adapters: a run reader is required")
	}
	return &Attempts{runs: r}, nil
}

func lookupErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return logs.ErrNotFound
	}
	return logs.Unavailablef(err, "execution lookup")
}

func info(agg *execution.Aggregate, at *execution.Attempt) logs.AttemptInfo {
	return logs.AttemptInfo{
		Scope: at.Scope, Run: at.RunID, Step: at.StepID, ID: at.ID, Ordinal: at.Ordinal,
		JobName: at.WorkloadName, JobUID: at.WorkloadUID, Terminal: at.Status.Terminal(),
		WorkItem: agg.Run.WorkItemID, Workspace: at.WorkspaceID,
	}
}

// Attempt implements logs.Attempts: the run must be the scope's and must own
// the attempt, so a copied attempt identifier of another run is not found.
func (a *Attempts) Attempt(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID) (logs.AttemptInfo, error) {
	if scope.Validate() != nil || run.Validate() != nil || attempt.Validate() != nil {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	agg, err := a.runs.Get(ctx, scope, run)
	if err != nil {
		return logs.AttemptInfo{}, lookupErr(err)
	}
	if agg == nil || agg.Run == nil || agg.Run.ID != run || agg.Run.Scope != scope {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	at := agg.Attempt(attempt)
	if at == nil || at.RunID != run || at.Scope != scope {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	return info(agg, at), nil
}

// ByID implements logs.Attempts.
func (a *Attempts) ByID(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (logs.AttemptInfo, error) {
	if scope.Validate() != nil || attempt.Validate() != nil {
		return logs.AttemptInfo{}, logs.ErrNotFound
	}
	run, err := a.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return logs.AttemptInfo{}, lookupErr(err)
	}
	return a.Attempt(ctx, scope, run, attempt)
}

// Attempts implements logs.Attempts, oldest first.
func (a *Attempts) Attempts(ctx context.Context, scope domain.Scope, run domain.RunID) ([]logs.AttemptInfo, error) {
	if scope.Validate() != nil || run.Validate() != nil {
		return nil, logs.ErrNotFound
	}
	agg, err := a.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, lookupErr(err)
	}
	if agg == nil || agg.Run == nil || agg.Run.ID != run || agg.Run.Scope != scope {
		return nil, logs.ErrNotFound
	}
	out := make([]logs.AttemptInfo, 0, len(agg.Attempts))
	for _, at := range agg.Attempts {
		if at.RunID == run && at.Scope == scope {
			out = append(out, info(agg, at))
		}
	}
	return out, nil
}

// Placement supplies where an attempt's workload runs. Execution persists the
// Job name and observed UID but not the namespace or container, so composition
// provides them (T-021 creates the Job; T-071 binds the two). The result is
// trusted configuration: it is never derived from a caller.
type Placement interface {
	Placement(ctx context.Context, attempt logs.AttemptInfo) (namespace, container string, err error)
}

// StaticPlacement places every attempt in one namespace and container, the M1
// shape: one trusted run namespace and the step container.
type StaticPlacement struct {
	Namespace string
	Container string
}

var placementRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Placement implements Placement.
func (p StaticPlacement) Placement(context.Context, logs.AttemptInfo) (string, string, error) {
	if !placementRe.MatchString(p.Namespace) || !placementRe.MatchString(p.Container) {
		return "", "", fmt.Errorf("%w: placement is not configured", logs.ErrNotConfigured)
	}
	return p.Namespace, p.Container, nil
}

// Workloads implements logs.Workloads from persisted Job identity plus injected
// placement.
type Workloads struct{ placement Placement }

var _ logs.Workloads = (*Workloads)(nil)

// NewWorkloads requires the placement resolver.
func NewWorkloads(p Placement) (*Workloads, error) {
	if p == nil {
		return nil, errors.New("logs/adapters: a placement resolver is required")
	}
	return &Workloads{placement: p}, nil
}

// Target implements logs.Workloads. An attempt whose Job has not been observed
// has no workload yet: a similarly named object is never selected.
func (w *Workloads) Target(ctx context.Context, a logs.AttemptInfo) (logs.Target, error) {
	if a.JobUID == "" {
		return logs.Target{}, logs.ErrWorkloadNotObserved
	}
	if a.JobName == "" {
		return logs.Target{}, logs.ErrWorkloadNotObserved
	}
	ns, container, err := w.placement.Placement(ctx, a)
	if err != nil {
		return logs.Target{}, err
	}
	return logs.Target{Namespace: ns, JobName: a.JobName, JobUID: a.JobUID, Container: container}, nil
}
