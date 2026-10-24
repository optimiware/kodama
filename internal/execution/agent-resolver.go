package execution

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// AgentExpansion is what the agent-profile owner contributes for one agent
// step: the container command the step runs as, and the limits the profile
// claims. It is consumer-owned: the execution module defines it and never
// imports the profile implementation.
type AgentExpansion struct {
	// Image is the digest-pinned harness image.
	Image string
	// Command and Args are the wrapper entrypoint and its arguments (the pinned
	// wrapper payload). Neither may contain an expression marker: arguments are
	// resolved by Resolve, and profile text must never be interpreted.
	Command []string
	Args    []string
	// WallSeconds is the profile's wall-time limit (zero claims none).
	WallSeconds int64
	// MaxRequests and MaxTokens are the profile's run-wide allowance claims
	// (zero claims none).
	MaxRequests int64
	MaxTokens   int64
}

// AgentResolver resolves the agent profile named by an agent step, inside the
// scope, to its container expansion. It runs outside any SQL transaction and
// returns classified execution errors: ErrAdmissionRefused for an unknown,
// unsupported or no longer approved profile, ErrUnavailable for an outage.
type AgentResolver interface {
	ResolveAgent(ctx context.Context, scope domain.Scope, step pipeline.Step) (AgentExpansion, error)
}

var agentImagePattern = regexp.MustCompile(runtimev1.Patterns["ImageRef"])

// ExpandAgents returns a copy of the plan in which every pending agent step is
// frozen as the container command its profile resolves to, before the plan
// becomes durable. The step keeps its identity, dependencies, condition, retry
// rule, outputs, workspace access and action key; only its image, command,
// arguments and effective timeout are filled, and its agent reference stays as
// attribution. The pipeline definition the run pins is untouched: the run keeps
// the original definition ID and digest.
//
// The effective timeout is the smaller of the step's own timeout and the
// profile's wall time: an explicit step restriction narrows the profile limit
// and never widens it (the normal execution limit still applies at dispatch).
//
// Request and token allowances are run-wide, so every expanded agent step of a
// run must declare the same allowances (including none). Declarations that
// differ are refused instead of silently selecting the more permissive one.
//
// The input plan is never modified, and an error leaves nothing behind.
func ExpandAgents(ctx context.Context, r AgentResolver, p *AdmissionPlan) (*AdmissionPlan, error) {
	if p == nil || p.Aggregate == nil || p.Aggregate.Run == nil {
		return nil, invalidf("an admission plan is required")
	}
	run := p.Aggregate.Run
	agg := *p.Aggregate
	agg.Steps = make([]*StepRun, len(p.Aggregate.Steps))
	type allowance struct{ requests, tokens int64 }
	var first *allowance
	for i, s := range p.Aggregate.Steps {
		c := *s
		agg.Steps[i] = &c
		if s.Spec.Kind != pipeline.KindAgent || s.Status != StepPending {
			continue
		}
		if r == nil {
			return nil, fmt.Errorf("%w: an agent resolver is required for agent steps", ErrNotConfigured)
		}
		if s.Spec.Agent == nil || s.Spec.Agent.Profile == "" {
			return nil, Refusedf("agent step %s names no profile", s.ID)
		}
		exp, err := r.ResolveAgent(ctx, run.Scope, s.Spec)
		if err != nil {
			return nil, classifyResolverError(err)
		}
		if err := validateExpansion(exp); err != nil {
			return nil, err
		}
		got := allowance{exp.MaxRequests, exp.MaxTokens}
		if first == nil {
			first = &got
		} else if *first != got {
			return nil, Refusedf("the agent profiles of one run declare different run-wide request or token allowances")
		}
		spec := s.Spec
		spec.Image = exp.Image
		spec.Command = append([]string{}, exp.Command...)
		spec.Args = append([]string{}, exp.Args...)
		if exp.WallSeconds > 0 && (spec.TimeoutSeconds <= 0 || exp.WallSeconds < spec.TimeoutSeconds) {
			spec.TimeoutSeconds = exp.WallSeconds
		}
		c.Spec = spec
	}
	out := *p
	out.Aggregate = &agg
	return &out, nil
}

func classifyResolverError(err error) error {
	switch {
	case IsBusinessError(err):
		return err
	}
	return Unavailablef(err, "the agent profile could not be resolved")
}

func validateExpansion(e AgentExpansion) error {
	switch {
	case !agentImagePattern.MatchString(e.Image):
		return Refusedf("the agent profile has no digest-pinned image")
	case len(e.Command) == 0:
		return Refusedf("the agent profile has no wrapper command")
	case e.WallSeconds < 0 || e.MaxRequests < 0 || e.MaxTokens < 0:
		return Refusedf("the agent profile declares a negative limit")
	}
	for _, a := range append(append([]string{}, e.Command...), e.Args...) {
		if strings.Contains(a, "${{") {
			return Refusedf("the agent profile command contains an expression marker")
		}
	}
	return nil
}
