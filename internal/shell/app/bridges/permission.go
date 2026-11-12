// Composition-backed execution permission source.
//
// The schema has no execution-permission column, so composition builds
// one source for each consumer kind with the single closed permission that
// consumer requires (the model gateway, the MCP proxy and each
// execution-authority operation family). A source grants that permission only
// from trusted construction configuration plus the current durable facts: the
// execution is running in a valid non-empty scope, its current action is a
// running container action with a non-empty identity and its authoritative
// deadline is strictly in the future. Every other fact, including a request
// wait, an unknown action kind, a zero deadline and an expired deadline, is
// ExecNone. The source never widens and never reads a request, URL,
// environment or stored grant.
package bridges

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
)

// Clock is the authoritative time source used to evaluate an action deadline.
type Clock interface {
	Now() time.Time
}

// PermissionSource is one consumer's closed execution-permission source.
type PermissionSource struct {
	permission authorize.ExecutionPermission
	clock      Clock
}

var _ platform.PermissionSource = (*PermissionSource)(nil)

// NewPermissionSource builds the source for exactly one closed permission.
// ExecNone and any value outside the permission set are refused: a
// source that can grant nothing is a misconfiguration, not a fail-closed
// source.
func NewPermissionSource(permission authorize.ExecutionPermission, clock Clock) (*PermissionSource, error) {
	if !permission.Valid() || permission == authorize.ExecNone {
		return nil, fault.New(fault.InvalidInput, "permission source requires one closed non-none execution permission")
	}
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "permission source requires a clock")
	}
	return &PermissionSource{permission: permission, clock: clock}, nil
}

// Permission returns the configured permission only for a scoped running
// execution with a running container action before its deadline. Unknown or
// incomplete facts fail closed with ExecNone and no error.
func (source *PermissionSource) Permission(ctx context.Context, facts platform.ExecutionFacts) (authorize.ExecutionPermission, error) {
	if err := ctx.Err(); err != nil {
		return authorize.ExecNone, fault.Wrap(fault.DeadlineExpired, "permission evaluation was canceled", err)
	}
	if source == nil || source.clock == nil || source.permission == authorize.ExecNone {
		return authorize.ExecNone, nil
	}
	if err := facts.Scope.Validate(); err != nil {
		return authorize.ExecNone, nil
	}
	if _, err := id.ParseExecutionID(string(facts.ExecutionID)); err != nil {
		return authorize.ExecNone, nil
	}
	if _, err := id.ParsePrincipalID(string(facts.PrincipalID)); err != nil {
		return authorize.ExecNone, nil
	}
	if _, err := id.ParseActionID(string(facts.ActionID)); err != nil {
		return authorize.ExecNone, nil
	}
	if facts.ExecutionState != string(kernel.ExecutionRunning) {
		return authorize.ExecNone, nil
	}
	if facts.ActionKind != string(definition.ActionKindContainer) {
		return authorize.ExecNone, nil
	}
	if facts.ActionState != string(kernel.ActionRunning) {
		return authorize.ExecNone, nil
	}
	if facts.Deadline.IsZero() {
		return authorize.ExecNone, nil
	}
	if !source.clock.Now().Before(facts.Deadline) {
		return authorize.ExecNone, nil
	}
	if err := ctx.Err(); err != nil {
		return authorize.ExecNone, fault.Wrap(fault.DeadlineExpired, "permission evaluation was canceled", err)
	}
	return source.permission, nil
}
