package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/execution"
	executionhttp "github.com/optimiware/kodama/internal/execution/http"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Expander freezes the agent steps of an admission plan as their resolved
// container commands before the plan becomes durable. It is the single
// expansion path of both admission routes, so they produce identical frozen
// steps.
type Expander struct{ resolver execution.AgentResolver }

// NewExpander requires the resolver.
func NewExpander(r execution.AgentResolver) (Expander, error) {
	if r == nil {
		return Expander{}, errors.New("agents/adapters: an agent resolver is required")
	}
	return Expander{resolver: r}, nil
}

// Expand returns a copy of the plan with every pending agent step expanded. The
// original plan is never modified, and a failure persists neither a partial run
// nor partial profile pins: nothing is written until the delegate commits.
//
// A container step whose arguments open with the agent payload marker is
// refused: such a step could claim a profile and design baseline it never
// resolved through the profile store, and would make the planner reject its own
// claim at dispatch.
func (e Expander) Expand(ctx context.Context, p *execution.AdmissionPlan) (*execution.AdmissionPlan, error) {
	if p == nil || p.Aggregate == nil {
		return nil, execution.Invalidf("an admission plan is required")
	}
	for _, s := range p.Aggregate.Steps {
		if s.Spec.Kind != pipeline.KindAgent && agents.HasPayload(s.Spec.Args) {
			return nil, execution.Refusedf("step %s uses a reserved argument marker", s.ID)
		}
	}
	return execution.ExpandAgents(ctx, e.resolver, p)
}

// Store decorates the execution store for the ordinary admission path
// (Service.Admit): the plan is expanded, then the wrapped store commits it.
// Every other store operation is the wrapped store's own.
type Store struct {
	execution.Store
	expander Expander
}

// Admit expands the plan and delegates the single admission transaction.
func (s Store) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	expanded, err := s.expander.Expand(ctx, p)
	if err != nil {
		return nil, false, err
	}
	return s.Store.Admit(ctx, expanded)
}

// Creator decorates the keyed (idempotent) creator of the HTTP path: the plan is
// expanded, then the wrapped creator runs the one keyed transaction. A replay
// of an already created run returns the stored response as before; expansion is
// repeated beforehand because the creator takes its plan up front, which is
// deterministic for an unchanged profile and catalogue.
type Creator struct {
	inner    executionhttp.Creator
	expander Expander
}

var _ executionhttp.Creator = Creator{}

// CreateKeyed expands the plan and delegates.
func (c Creator) CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *execution.AdmissionPlan, render func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error) {
	expanded, err := c.expander.Expand(ctx, plan)
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	return c.inner.CreateKeyed(ctx, id, authorizeReplay, expanded, render)
}

// ChainConfig supplies every member of the agent admission chain. All of
// Resolver, Store, Creator are required; Planner defaults to
// execution.DefaultPlanner.
type ChainConfig struct {
	Resolver execution.AgentResolver
	Store    execution.Store
	Creator  executionhttp.Creator
	Planner  execution.Planner
}

// Chain is the one documented wiring bundle for agent steps. Use its members
// together: Store as execution.Dependencies.Store, Planner as
// Dependencies.Planner, Hooks as Dependencies.Hooks, Capabilities(...) as
// Dependencies.Capabilities (and as the pipeline validator's capabilities), and
// Creator as executionhttp.Config.Creator. Wiring only some of them is a
// composition error this type exists to prevent.
type Chain struct {
	Store   execution.Store
	Creator executionhttp.Creator
	Planner execution.Planner
	// Hooks enables container and agent steps for dispatch.
	Hooks execution.Hooks
}

// NewChain validates the configuration and builds the bundle.
func NewChain(cfg ChainConfig) (Chain, error) {
	if cfg.Resolver == nil || cfg.Store == nil || cfg.Creator == nil {
		return Chain{}, fmt.Errorf("agents/adapters: the agent chain needs a resolver, a store and a keyed creator: %w", execution.ErrNotConfigured)
	}
	exp, err := NewExpander(cfg.Resolver)
	if err != nil {
		return Chain{}, err
	}
	planner := cfg.Planner
	if planner == nil {
		planner = execution.DefaultPlanner{}
	}
	return Chain{
		Store:   Store{Store: cfg.Store, expander: exp},
		Creator: Creator{inner: cfg.Creator, expander: exp},
		Planner: Planner{inner: planner},
		Hooks:   execution.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent},
	}, nil
}

// Capabilities returns the step kinds admission and validation accept: container
// and agent, plus any others the integration owner enables (gates).
func (Chain) Capabilities(extra ...pipeline.StepKind) pipeline.Capabilities {
	return pipeline.EnabledKinds(append([]pipeline.StepKind{pipeline.KindContainer, pipeline.KindAgent}, extra...))
}
