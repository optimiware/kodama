package runtimeauth

import (
	"context"
	"fmt"
	"slices"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// StepKind is the executor family of the step an attempt runs. The values
// mirror the pipeline format's executable kinds; this package does not import
// the pipeline module, so a kind is compared as text and an unknown one is
// never granted anything beyond DefaultActions.
type StepKind string

const (
	StepContainer StepKind = "container"
	StepAgent     StepKind = "agent"
)

// ceilingGrants are the only actions an operator can add to DefaultActions.
// Each one is conditional on a fact of the attempt itself, so configuring it
// states "may hold", never "holds":
//
//   - model.invoke: the pinned agent profile names a model.
//   - tool.invoke: the pinned agent profile names at least one tool.
//   - deployment.call: the step declares a deployment environment.
//
// The default set is never widened by anything else.
var ceilingGrants = []Action{ActionModelInvoke, ActionToolInvoke, ActionDeploymentCall}

// KindCeiling is the operator's upper bound for the attempts of one step kind.
type KindCeiling struct {
	// Grants lists the actions an attempt of the kind may hold beyond
	// DefaultActions, when its own facts allow them.
	Grants []Action
}

// CeilingConfig is the operator configuration of the action ceiling: for each
// step kind, which conditional actions may be granted. A kind that is absent
// keeps DefaultActions.
type CeilingConfig struct {
	Kinds map[StepKind]KindCeiling
}

// Validate rejects an unknown kind, an action outside the three conditional
// ones, a duplicate, and a grant whose condition the kind can never satisfy
// (a container step has no agent profile, so it cannot hold model.invoke or
// tool.invoke). A rejected configuration grants nothing.
func (c CeilingConfig) Validate() error {
	for kind, k := range c.Kinds {
		if kind != StepContainer && kind != StepAgent {
			return fmt.Errorf("runtimeauth: the action ceiling names unknown step kind %q", truncate(string(kind), 40))
		}
		seen := map[Action]bool{}
		for _, a := range k.Grants {
			switch {
			case !slices.Contains(ceilingGrants, a):
				return fmt.Errorf("runtimeauth: the action ceiling of %s cannot grant %q", kind, truncate(string(a), 40))
			case seen[a]:
				return fmt.Errorf("runtimeauth: the action ceiling of %s grants %q twice", kind, a)
			case kind != StepAgent && (a == ActionModelInvoke || a == ActionToolInvoke):
				return fmt.Errorf("runtimeauth: %s needs an agent profile; the %s kind has none", a, kind)
			}
			seen[a] = true
		}
	}
	return nil
}

// CeilingSource supplies the operator configuration. It is read at every
// decision so a reloadable source applies to the next registration; an error
// means the configuration is unreadable and the decision is DefaultActions.
type CeilingSource interface {
	Ceiling(ctx context.Context) (CeilingConfig, error)
}

// StaticCeiling is a CeilingSource over one fixed configuration.
type StaticCeiling CeilingConfig

// Ceiling implements CeilingSource.
func (s StaticCeiling) Ceiling(context.Context) (CeilingConfig, error) { return CeilingConfig(s), nil }

// ProfileCapabilities says what an agent profile version names. It carries no
// endpoint, tool name or credential: only whether the attempt has a model or
// tools to call.
type ProfileCapabilities struct {
	Model bool
	Tools bool
}

// ProfileResolver resolves a pinned profile version, inside the scope, to its
// capabilities. The agents module implements it (T-101 binds it). An error,
// including "no such version", grants nothing beyond the defaults.
type ProfileResolver interface {
	ProfileCapabilities(ctx context.Context, scope domain.Scope, pin runtimev1.Profile) (ProfileCapabilities, error)
}

// CeilingInput holds the facts of one attempt the ceiling is chosen from. All
// of them come from execution's pinned state, never from a request.
type CeilingInput struct {
	Scope domain.Scope
	Kind  StepKind
	// Writer is true for a workspace writer (adds source.propose by default).
	Writer bool
	// Profile is the pinned agent profile, nil when the step has none.
	Profile *runtimev1.Profile
	// Deployment is true when the step declares a deployment environment.
	Deployment bool
}

// CeilingPolicy chooses the action ceiling of an attempt: DefaultActions plus
// the conditional actions the operator configured for its step kind and its own
// facts satisfy. The result is an upper bound; the attempt's current allowed
// actions (AttemptState.Actions) still intersect with it at every use and can
// take actions away while the attempt runs.
type CeilingPolicy struct {
	source   CeilingSource
	profiles ProfileResolver
}

// NewCeilingPolicy requires a source. A nil resolver means no profile names a
// model or tool, so agent steps keep the defaults.
func NewCeilingPolicy(source CeilingSource, profiles ProfileResolver) (CeilingPolicy, error) {
	if source == nil {
		return CeilingPolicy{}, fmt.Errorf("runtimeauth: an action ceiling source is required")
	}
	if s, ok := source.(StaticCeiling); ok {
		if err := CeilingConfig(s).Validate(); err != nil {
			return CeilingPolicy{}, err
		}
	}
	return CeilingPolicy{source: source, profiles: profiles}, nil
}

// Actions returns the ceiling for the attempt. It never fails and never
// returns less than DefaultActions: an unreadable or invalid source, an
// unknown kind, a missing profile pin, an unresolvable profile and the zero
// CeilingPolicy all yield exactly DefaultActions(in.Writer).
func (p CeilingPolicy) Actions(ctx context.Context, in CeilingInput) ActionSet {
	def := defaultSet(in.Writer)
	if p.source == nil {
		return def
	}
	cfg, err := p.source.Ceiling(ctx)
	if err != nil || cfg.Validate() != nil {
		return def
	}
	kind, ok := cfg.Kinds[in.Kind]
	if !ok || len(kind.Grants) == 0 {
		return def
	}
	// An agent attempt without a pinned profile is not a well-formed agent:
	// it receives nothing beyond the defaults, whatever is configured.
	if in.Kind == StepAgent && in.Profile == nil {
		return def
	}
	var caps ProfileCapabilities
	if in.Kind == StepAgent && (slices.Contains(kind.Grants, ActionModelInvoke) || slices.Contains(kind.Grants, ActionToolInvoke)) {
		if p.profiles == nil {
			return def
		}
		caps, err = p.profiles.ProfileCapabilities(ctx, in.Scope, *in.Profile)
		if err != nil {
			return def
		}
	}
	all := DefaultActions(in.Writer)
	for _, a := range kind.Grants {
		if (a == ActionModelInvoke && caps.Model) || (a == ActionToolInvoke && caps.Tools) || (a == ActionDeploymentCall && in.Deployment) {
			all = append(all, a)
		}
	}
	set, err := NewActionSet(all...)
	if err != nil {
		return def
	}
	return set
}

func defaultSet(writer bool) ActionSet {
	s, err := NewActionSet(DefaultActions(writer)...)
	if err != nil {
		return ActionSet{} // unreachable: the default set is a fixed valid list
	}
	return s
}
