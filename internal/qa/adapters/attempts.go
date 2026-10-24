package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/qa"
)

// Runs reads persisted runs. execution.Store implements it.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Attempts implements qa.Attempts over the execution owner's persisted
// attempts. The pinned context is the immutable document dispatch stored; it is
// verified against its stored digest and its own scope and identity before it
// is believed, and nothing else (profiles, environments, settings) is read.
type Attempts struct{ runs Runs }

var _ qa.Attempts = (*Attempts)(nil)

// NewAttempts requires the run reader.
func NewAttempts(r Runs) (*Attempts, error) {
	if r == nil {
		return nil, errors.New("qa/adapters: an execution store is required")
	}
	return &Attempts{runs: r}, nil
}

func mapExecution(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	return qa.Unavailablef(err, "execution could not answer")
}

// Facts implements qa.Attempts. A foreign or unknown attempt is qa.ErrNotFound.
func (a *Attempts) Facts(ctx context.Context, scope domain.Scope, id domain.AttemptID) (qa.AttemptFacts, error) {
	run, err := a.runs.RunOf(ctx, scope, id)
	if err != nil {
		return qa.AttemptFacts{}, mapExecution(err)
	}
	agg, err := a.runs.Get(ctx, scope, run)
	if err != nil {
		return qa.AttemptFacts{}, mapExecution(err)
	}
	at := agg.Attempt(id)
	if at == nil || at.Scope != scope || at.RunID != run {
		return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	sum := sha256.Sum256(at.Context)
	if at.ContextDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		return qa.AttemptFacts{}, qa.Unavailablef(nil, "the pinned context does not match its digest")
	}
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return qa.AttemptFacts{}, qa.Unavailablef(err, "the pinned context is unreadable")
	}
	if pinned.Scope() != scope || pinned.AttemptID != id || pinned.RunID != run || pinned.StepID != at.StepID {
		return qa.AttemptFacts{}, qa.Failf(qa.ErrNotFound, "attempt not found")
	}
	f := qa.AttemptFacts{Scope: scope, Run: run, Step: at.StepID, Attempt: id, Context: pinned}
	if at.Status == execution.AttemptSucceeded && at.Result != nil && at.Result.Outcome == runtimev1.OutcomeCompleted {
		f.Completed = true
		f.Outputs = map[string]string{}
		for name, v := range at.Result.Outputs {
			if v.IsString() {
				f.Outputs[name] = v.String()
			}
		}
		f.Diagnostics = append(f.Diagnostics, at.Result.DiagnosticArtifactIDs...)
	}
	return f, nil
}
