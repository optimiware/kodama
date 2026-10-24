package adapters

import (
	"context"
	"errors"
	"strings"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Registrations is the proxy's read of project registrations, as the approval
// contribution needs it: *postgres.Store satisfies it.
type Registrations interface {
	GetRegistration(ctx context.Context, scope domain.Scope, id mcpproxy.RegistrationID) (mcpproxy.Registration, error)
}

// ToolApprovals contributes the MCP half of agents.Approvals.ApproveTools: every
// listed tool must be a qualified "<registrationID>/<toolName>" naming a live
// registration of the project that enables the tool. Bare names and wildcards
// are refused. It does not implement model approval: compose it with the model
// approval adapter (see ComposeApprovals).
type ToolApprovals struct{ regs Registrations }

// NewToolApprovals requires the registration reader.
func NewToolApprovals(r Registrations) (*ToolApprovals, error) {
	if r == nil {
		return nil, errors.New("mcpproxy/adapters: a registration reader is required")
	}
	return &ToolApprovals{regs: r}, nil
}

// ApproveTools returns agents.ErrDenied for an unapproved tool and
// agents.ErrUnavailable for an outage.
func (a *ToolApprovals) ApproveTools(ctx context.Context, scope domain.Scope, tools []string) error {
	cache := map[mcpproxy.RegistrationID]mcpproxy.Registration{}
	for _, t := range tools {
		if strings.ContainsAny(t, "*?") {
			return agents.ErrDenied
		}
		id, tool, err := mcpproxy.ParseQualified(t)
		if err != nil {
			return agents.ErrDenied
		}
		reg, ok := cache[id]
		if !ok {
			reg, err = a.regs.GetRegistration(ctx, scope, id)
			switch {
			case err == nil:
				cache[id] = reg
			case errors.Is(err, mcpproxy.ErrNotFound):
				return agents.ErrDenied
			default:
				return agents.Unavailablef(err, "the registration lookup failed")
			}
		}
		if !reg.Enables(tool) {
			return agents.ErrDenied
		}
	}
	return nil
}

// ComposeApprovals combines a model-approval adapter with the tool approval
// contribution into one agents.Approvals.
type ComposeApprovals struct {
	Models agents.Approvals
	Tools  *ToolApprovals
}

var _ agents.Approvals = ComposeApprovals{}

// ApproveModel delegates to the model approval adapter.
func (c ComposeApprovals) ApproveModel(ctx context.Context, scope domain.Scope, m agents.Model) error {
	if c.Models == nil {
		return agents.Unavailablef(nil, "no model approval policy is configured")
	}
	return c.Models.ApproveModel(ctx, scope, m)
}

// ApproveTools delegates to the MCP tool approval.
func (c ComposeApprovals) ApproveTools(ctx context.Context, scope domain.Scope, tools []string) error {
	if c.Tools == nil {
		return agents.Unavailablef(nil, "no tool approval policy is configured")
	}
	return c.Tools.ApproveTools(ctx, scope, tools)
}
