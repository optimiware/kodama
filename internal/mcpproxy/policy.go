package mcpproxy

import (
	"strings"
)

// Tools are addressed in an agent profile by the exact qualified name
// "<registrationID>/<toolName>". A bare tool name or a wildcard names no
// registration and therefore grants nothing: authorization is never inferred
// from spelling.

// Qualify returns the qualified name of a remote tool.
func Qualify(reg RegistrationID, tool string) string { return string(reg) + "/" + tool }

// ParseQualified splits a qualified tool name. A name that is not exactly
// "<registrationID>/<toolName>" is invalid.
func ParseQualified(s string) (RegistrationID, string, error) {
	reg, tool, ok := strings.Cut(s, "/")
	if !ok {
		return "", "", invalidf("a tool must be qualified as registration/tool")
	}
	id := RegistrationID(reg)
	if err := id.Validate(); err != nil {
		return "", "", err
	}
	if err := ValidateToolName(tool); err != nil {
		return "", "", err
	}
	return id, tool, nil
}

// ToolPolicy is the pinned, immutable policy of one attempt. It narrows what
// the attempt may ask for; it establishes no authority.
type ToolPolicy struct {
	allowed map[string]struct{}
	// MaxToolCalls is the claimed run-wide ceiling, nil when unclaimed.
	MaxToolCalls *int64
	// RecordTranscript is the pinned transcript policy.
	RecordTranscript bool
}

// NewToolPolicy builds a policy from the profile's tool list. Entries that are
// not valid qualified names are ignored: they grant nothing.
func NewToolPolicy(tools []string, maxCalls *int64, record bool) ToolPolicy {
	p := ToolPolicy{allowed: map[string]struct{}{}, RecordTranscript: record}
	if maxCalls != nil {
		v := *maxCalls
		p.MaxToolCalls = &v
	}
	for _, t := range tools {
		if _, _, err := ParseQualified(t); err == nil {
			p.allowed[t] = struct{}{}
		}
	}
	return p
}

// Names returns the number of granted qualified tools.
func (p ToolPolicy) Names() int { return len(p.allowed) }

// Permits reports whether the profile names the registration's tool.
func (p ToolPolicy) Permits(reg RegistrationID, tool string) bool {
	_, ok := p.allowed[Qualify(reg, tool)]
	return ok
}

// Visible is the intersection that governs both discovery and invocation: the
// registration must currently enable the tool and the pinned profile must name
// it. Anything else is hidden, and a hidden tool also fails direct invocation.
func Visible(r Registration, p ToolPolicy, tool string) bool {
	return r.Enables(tool) && p.Permits(r.ID, tool)
}

// AnyVisible reports whether the policy exposes any tool of the registration.
func AnyVisible(r Registration, p ToolPolicy) bool {
	for _, t := range r.Tools {
		if Visible(r, p, t) {
			return true
		}
	}
	return false
}
