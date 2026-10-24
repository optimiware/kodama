package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Resolver implements execution.AgentResolver: it resolves the immutable profile
// an agent step names and maps it onto the container command, using the pure
// agents.Expand. An unknown, foreign, unsupported or no longer approved profile
// refuses admission with a constant, content-free message; an outage is
// unavailability.
type Resolver struct{ profiles Profiles }

var _ execution.AgentResolver = Resolver{}

// NewResolver requires the profile reader.
func NewResolver(p Profiles) (Resolver, error) {
	if p == nil {
		return Resolver{}, errors.New("agents/adapters: a profile reader is required")
	}
	return Resolver{profiles: p}, nil
}

// ResolveAgent implements execution.AgentResolver.
func (r Resolver) ResolveAgent(ctx context.Context, scope domain.Scope, step pipeline.Step) (execution.AgentExpansion, error) {
	if step.Kind != pipeline.KindAgent || step.Agent == nil {
		return execution.AgentExpansion{}, execution.Refusedf("the step is not an agent step")
	}
	res, err := r.profiles.Resolve(ctx, scope, step.Agent.Profile)
	switch {
	case err == nil:
	case errors.Is(err, agents.ErrNotFound), errors.Is(err, agents.ErrHidden), errors.Is(err, agents.ErrInvalid):
		return execution.AgentExpansion{}, execution.Refusedf("the agent profile does not exist in this project")
	case errors.Is(err, agents.ErrUnsupported):
		return execution.AgentExpansion{}, execution.Refusedf("the agent profile is not supported by this installation")
	default:
		return execution.AgentExpansion{}, execution.Unavailablef(err, "the agent profile could not be resolved")
	}
	exp, err := agents.Expand(res.Profile)
	if err != nil {
		return execution.AgentExpansion{}, execution.Unavailablef(err, "the agent profile could not be expanded")
	}
	return execution.AgentExpansion{
		Image: exp.Image, Command: exp.Command, Args: exp.Args,
		WallSeconds: exp.WallSeconds, MaxRequests: exp.MaxRequests, MaxTokens: exp.MaxTokens,
	}, nil
}
