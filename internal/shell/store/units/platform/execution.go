package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ExecutionFacts are the current durable facts one admitted-execution read
// observed. They are input to the injected permission source, never authority
// on their own.
type ExecutionFacts struct {
	ExecutionID      id.ExecutionID
	ExecutionState   string
	ExecutionVersion uint64
	PrincipalID      id.PrincipalID
	Scope            tenancy.Scope
	ActionID         id.ActionID
	ActionKind       string
	ActionState      string
	Deadline         time.Time
}

// PermissionSource derives the closed execution permission for one read from
// trusted configuration plus the current durable facts. It is a consumer-owned
// input: the schema has no execution-permission column, so the adapter
// never guesses one.
type PermissionSource interface {
	Permission(context.Context, ExecutionFacts) (authorize.ExecutionPermission, error)
}

// ExecutionReader is the store-backed mcpproxy.ExecutionReader.
//
// ReadExecution reads the execution row and its single running container action
// through the cross-scope controller seam, because the caller presents only an
// execution identity and no tenancy scope. The reader returns the execution
// identity, owning scope, version and state, the current container action
// identity and deadline, and the permission derived by the injected source. It
// is current evidence for one authorization boundary, never a durable grant.
type ExecutionReader struct {
	controller  store.Controller
	permissions PermissionSource
}

// NewExecutionReader builds the execution-fact reader.
func NewExecutionReader(controller store.Controller, permissions PermissionSource) (*ExecutionReader, error) {
	if controller == nil {
		return nil, fault.New(fault.InvalidInput, "execution reader requires a controller")
	}
	if permissions == nil {
		return nil, fault.New(fault.InvalidInput, "execution reader requires a permission source")
	}
	return &ExecutionReader{controller: controller, permissions: permissions}, nil
}

var _ mcpproxy.ExecutionReader = (*ExecutionReader)(nil)

// ReadExecution returns the current admitted execution context for one
// execution identity.
func (reader *ExecutionReader) ReadExecution(ctx context.Context, executionID id.ExecutionID) (authorize.AdmittedExecution, error) {
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return authorize.AdmittedExecution{}, fault.Wrap(fault.InvalidInput, "execution id is invalid", err)
	}
	row, err := reader.controller.Read(ctx, executionsTable, string(executionID))
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	facts, err := executionFactsFromRow(row)
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	actionID, actionKind, actionState, deadline, err := reader.runningContainerAction(ctx, executionID)
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	facts.ActionID = actionID
	facts.ActionKind = actionKind
	facts.ActionState = actionState
	facts.Deadline = deadline

	permission, err := reader.permissions.Permission(ctx, facts)
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	admitted := authorize.AdmittedExecution{
		Principal: identity.Principal{
			ID:          facts.PrincipalID,
			Kind:        identity.KindExecution,
			Scope:       facts.Scope,
			Enabled:     facts.ExecutionState == runningExecutionState,
			Version:     facts.ExecutionVersion,
			ExecutionID: facts.ExecutionID,
		},
		ExecutionID: facts.ExecutionID,
		ActionID:    facts.ActionID,
		Permission:  permission,
		Deadline:    facts.Deadline,
	}
	if err := admitted.Validate(); err != nil {
		return authorize.AdmittedExecution{}, fault.Wrap(fault.InternalFailure, "derived admitted execution is invalid", err)
	}
	return admitted, nil
}

// executionFactsFromRow decodes the execution columns that carry identity,
// scope, state and version.
func executionFactsFromRow(row store.Row) (ExecutionFacts, error) {
	identityText, err := textValue(row, idColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	tenant, err := textValue(row, tenantIDColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	project, err := textValue(row, projectIDColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	principal, err := textValue(row, principalColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	state, err := textValue(row, stateColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	version, err := intValue(row, versionColumn)
	if err != nil {
		return ExecutionFacts{}, err
	}
	if version < 0 {
		return ExecutionFacts{}, fault.New(fault.InternalFailure, "execution row has a negative version")
	}
	return ExecutionFacts{
		ExecutionID:      id.ExecutionID(identityText),
		ExecutionState:   state,
		ExecutionVersion: uint64(version),
		PrincipalID:      id.PrincipalID(principal),
		Scope:            tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
	}, nil
}

// runningContainerAction returns the single running container action of one
// execution and its authoritative deadline. The declared partial unique index
// admits at most one running action, so more than one is durable corruption.
func (reader *ExecutionReader) runningContainerAction(
	ctx context.Context,
	executionID id.ExecutionID,
) (id.ActionID, string, string, time.Time, error) {
	page, err := reader.controller.Scan(ctx, store.ControllerScan{
		Table: actionsTable,
		Equals: map[string]any{
			executionIDColumn: string(executionID),
			"kind":            "container",
			stateColumn:       runningExecutionState,
		},
		OrderBy: []store.Order{{Column: idColumn}},
		Limit:   2})
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	if len(page.Rows) == 0 {
		return "", "", "", time.Time{}, nil
	}
	if len(page.Rows) > 1 {
		return "", "", "", time.Time{}, fault.New(fault.InternalFailure,
			fmt.Sprintf("execution %q has more than one running container action", executionID))
	}
	row := page.Rows[0]
	actionID, err := textValue(row, idColumn)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	kind, err := textValue(row, "kind")
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	state, err := textValue(row, stateColumn)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	deadline, err := nullableTimeValue(row, "deadline")
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return id.ActionID(actionID), kind, state, deadline, nil
}
