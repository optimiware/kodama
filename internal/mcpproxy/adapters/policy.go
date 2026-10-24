package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/agents"
	agentsadapters "github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// AttemptSteps resolves the authoritative step of an attempt. The step comes
// from the execution record, never from a request.
type AttemptSteps interface {
	StepOf(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID) (string, error)
}

// ExecutionSteps reads the step through the execution store.
type ExecutionSteps struct{ store execution.Store }

// NewExecutionSteps requires the execution store.
func NewExecutionSteps(s execution.Store) (*ExecutionSteps, error) {
	if s == nil {
		return nil, errors.New("mcpproxy/adapters: an execution store is required")
	}
	return &ExecutionSteps{store: s}, nil
}

// StepOf implements AttemptSteps. A foreign, unknown or mismatched attempt is
// not found.
func (e *ExecutionSteps) StepOf(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID) (string, error) {
	owner, err := e.store.RunOf(ctx, scope, attempt)
	if err != nil {
		return "", err
	}
	if owner != run {
		return "", execution.ErrNotFound
	}
	agg, err := e.store.Get(ctx, scope, run)
	if err != nil {
		return "", err
	}
	at := agg.Attempt(attempt)
	if at == nil || at.Scope != scope {
		return "", execution.ErrNotFound
	}
	return at.StepID, nil
}

// PolicyReader serves the pinned policy of the attempt's profile.
type PolicyReader interface {
	PolicyOf(ctx context.Context, scope domain.Scope, run domain.RunID, step string, attempt domain.AttemptID) (agents.Policy, error)
}

var _ PolicyReader = agentsadapters.PolicyReader{}

// Policy implements mcpproxy.AttemptPolicy over T-030's PolicyReader: it
// resolves the authoritative step and reads the frozen policy of the attempt's
// context. It never reloads a moving profile and accepts no caller-supplied
// allow-list. An attempt without an agent policy has no tools.
type Policy struct {
	steps  AttemptSteps
	reader PolicyReader
}

var _ mcpproxy.AttemptPolicy = (*Policy)(nil)

// NewPolicy requires both collaborators.
func NewPolicy(steps AttemptSteps, reader PolicyReader) (*Policy, error) {
	if steps == nil || reader == nil {
		return nil, errors.New("mcpproxy/adapters: a step resolver and a policy reader are required")
	}
	return &Policy{steps: steps, reader: reader}, nil
}

// PolicyOf implements mcpproxy.AttemptPolicy.
func (p *Policy) PolicyOf(ctx context.Context, c mcpproxy.Caller) (mcpproxy.ToolPolicy, error) {
	step, err := p.steps.StepOf(ctx, c.Scope, c.RunID, c.AttemptID)
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return mcpproxy.ToolPolicy{}, mcpproxy.ErrNotFound
	default:
		return mcpproxy.ToolPolicy{}, mcpproxy.Unavailablef(err, "the attempt could not be read")
	}
	pol, err := p.reader.PolicyOf(ctx, c.Scope, c.RunID, step, c.AttemptID)
	switch {
	case err == nil:
	case errors.Is(err, agents.ErrNotFound):
		return mcpproxy.ToolPolicy{}, mcpproxy.ErrNotFound
	default:
		return mcpproxy.ToolPolicy{}, mcpproxy.Unavailablef(err, "the pinned policy could not be read")
	}
	return mcpproxy.NewToolPolicy(pol.Tools, pol.Limits.MaxToolCalls, pol.Transcript.Record), nil
}
