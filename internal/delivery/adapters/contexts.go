package adapters

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Runs reads persisted runs. execution.Store implements it.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// Execution implements delivery.PinnedContexts and delivery.Producers over the
// execution owner's persisted attempts. The pinned context is the immutable
// document dispatch stored; this adapter never reads environments, secrets or
// settings.
type Execution struct{ runs Runs }

var (
	_ delivery.PinnedContexts = (*Execution)(nil)
	_ delivery.Producers      = (*Execution)(nil)
)

// NewExecution requires the run reader.
func NewExecution(runs Runs) (*Execution, error) {
	if runs == nil {
		return nil, errors.New("delivery/adapters: an execution store is required")
	}
	return &Execution{runs: runs}, nil
}

func (e *Execution) attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (*execution.Attempt, error) {
	run, err := e.runs.RunOf(ctx, scope, id)
	if err != nil {
		return nil, mapExecution(err)
	}
	agg, err := e.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, mapExecution(err)
	}
	at := agg.Attempt(id)
	if at == nil || at.Scope != scope {
		return nil, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return at, nil
}

func mapExecution(err error) error {
	switch {
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrDenied):
		return delivery.Fail(delivery.ErrNotFound, err, "not found")
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "execution could not answer")
}

// Pinned implements delivery.PinnedContexts. A foreign or unknown attempt is
// ErrNotFound.
func (e *Execution) Pinned(ctx context.Context, scope domain.Scope, id domain.AttemptID) (runtimev1.Context, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return runtimev1.Context{}, err
	}
	c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrUnavailable, err, "the pinned context is unreadable")
	}
	if c.Scope() != scope || c.AttemptID != id {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return c, nil
}

// Produced implements delivery.Producers. Accepted is true only for a
// succeeded attempt whose accepted result holds the artifact as the value of a
// declared ociArtifact output; the tool image is the producer's immutable
// command image, never caller text.
func (e *Execution) Produced(ctx context.Context, scope domain.Scope, id domain.AttemptID, artifact domain.ArtifactID) (delivery.ProducedOutput, error) {
	at, err := e.attempt(ctx, scope, id)
	if err != nil {
		return delivery.ProducedOutput{}, err
	}
	out := delivery.ProducedOutput{Scope: scope, Attempt: id, Run: at.RunID, Step: at.StepID}
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	if err != nil {
		return delivery.ProducedOutput{}, delivery.Fail(delivery.ErrUnavailable, err, "the pinned context is unreadable")
	}
	out.ToolImage = pinned.Command.Image
	if at.Status != execution.AttemptSucceeded || at.Result == nil || at.Result.Outcome != runtimev1.OutcomeCompleted {
		return out, nil
	}
	for _, decl := range pinned.Outputs {
		want := delivery.ArtifactOCIImage
		switch decl.Type {
		case runtimev1.OutputOCIArtifact:
		case runtimev1.OutputArtifact:
			want = delivery.ArtifactBundle
		default:
			continue
		}
		if v, ok := at.Result.Outputs[decl.Name]; ok && v.IsString() && v.String() == string(artifact) {
			out.Accepted, out.AcceptedKind = true, want
		}
	}
	return out, nil
}
