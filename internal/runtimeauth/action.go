package runtimeauth

import (
	"slices"
	"strings"
)

// Action is one explicit capability of a workload. Actions are not
// identity-module roles: no action lets a workload approve, review, complete a
// gate, administer a tenant or read another attempt. The set is closed, so an
// unknown or wildcard action fails closed instead of being interpreted.
type Action string

const (
	ActionContextRead      Action = "context.read"
	ActionTokenRenew       Action = "token.renew"
	ActionSecretResolve    Action = "secret.resolve"
	ActionArtifactReserve  Action = "artifact.reserve"
	ActionArtifactComplete Action = "artifact.complete"
	ActionSourcePropose    Action = "source.propose"
	ActionResultSubmit     Action = "result.submit"
	// Model, tool and deployment calls live on their owning interfaces; the
	// attempt identity only says whether the call is granted.
	ActionModelInvoke    Action = "model.invoke"
	ActionToolInvoke     Action = "tool.invoke"
	ActionDeploymentCall Action = "deployment.call"
)

var knownActions = []Action{
	ActionContextRead, ActionTokenRenew, ActionSecretResolve, ActionArtifactReserve, ActionArtifactComplete,
	ActionSourcePropose, ActionResultSubmit, ActionModelInvoke, ActionToolInvoke, ActionDeploymentCall,
}

// ParseAction accepts exactly the known action names.
func ParseAction(s string) (Action, error) {
	a := Action(s)
	if !slices.Contains(knownActions, a) {
		return "", fail(ErrInvalidRequest, "unknown action %q", truncate(s, 40))
	}
	return a, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ActionForOperation maps a runtime operation ID of the frozen contract to the
// action it requires. Admission has none: its authority is the credential.
func ActionForOperation(operationID string) (Action, bool) {
	switch operationID {
	case "renewToken":
		return ActionTokenRenew, true
	case "getContext":
		return ActionContextRead, true
	case "resolveSecret":
		return ActionSecretResolve, true
	case "reserveArtifact":
		return ActionArtifactReserve, true
	case "completeArtifact":
		return ActionArtifactComplete, true
	case "proposeSource":
		return ActionSourcePropose, true
	case "submitResult":
		return ActionResultSubmit, true
	}
	return "", false
}

// ActionSet is a non-empty, sorted, duplicate-free set of known actions.
type ActionSet struct{ actions []Action }

// DefaultActions returns the default action ceiling of a workload: what an
// ordinary command needs through the runtime interface, plus source.propose
// for a workspace writer. No action lets a workload review, approve or
// administer. Every runtime registers attempts with it unless its policy
// configures another ceiling.
func DefaultActions(writer bool) []Action {
	base := []Action{
		ActionContextRead, ActionTokenRenew, ActionSecretResolve,
		ActionArtifactReserve, ActionArtifactComplete, ActionResultSubmit,
	}
	if writer {
		base = append(base, ActionSourcePropose)
	}
	return base
}

// NewActionSet validates and normalizes a set. Duplicates are an error rather
// than silently merged, so a registration states its ceiling exactly once.
func NewActionSet(actions ...Action) (ActionSet, error) {
	if len(actions) == 0 {
		return ActionSet{}, fail(ErrInvalidRequest, "a grant needs at least one action")
	}
	out := slices.Clone(actions)
	slices.Sort(out)
	for i, a := range out {
		if !slices.Contains(knownActions, a) {
			return ActionSet{}, fail(ErrInvalidRequest, "unknown action %q", truncate(string(a), 40))
		}
		if i > 0 && out[i-1] == a {
			return ActionSet{}, fail(ErrInvalidRequest, "duplicate action %q", a)
		}
	}
	return ActionSet{actions: out}, nil
}

// ParseActionSet builds a set from names.
func ParseActionSet(names []string) (ActionSet, error) {
	as := make([]Action, 0, len(names))
	for _, n := range names {
		a, err := ParseAction(n)
		if err != nil {
			return ActionSet{}, err
		}
		as = append(as, a)
	}
	return NewActionSet(as...)
}

func (s ActionSet) Contains(a Action) bool { return slices.Contains(s.actions, a) }
func (s ActionSet) Len() int               { return len(s.actions) }
func (s ActionSet) IsZero() bool           { return len(s.actions) == 0 }

// Strings returns the sorted names.
func (s ActionSet) Strings() []string {
	out := make([]string, len(s.actions))
	for i, a := range s.actions {
		out[i] = string(a)
	}
	return out
}

// Equal reports whether both sets hold the same actions.
func (s ActionSet) Equal(o ActionSet) bool { return slices.Equal(s.actions, o.actions) }

// SubsetOf reports whether every action of s is in o.
func (s ActionSet) SubsetOf(o ActionSet) bool {
	for _, a := range s.actions {
		if !o.Contains(a) {
			return false
		}
	}
	return true
}

func (s ActionSet) String() string { return strings.Join(s.Strings(), ",") }
