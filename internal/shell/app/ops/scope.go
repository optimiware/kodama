package opsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// ExecutionFactsReader reads the current admitted execution facts of one
// execution identity. The real implementation is the platform store adapter.
//
// Permission is the reader's validity marker: ExecNone means the execution is
// not running, its current action is not a running container action, its
// deadline has passed, or its scope does not match. Any other permission means
// the facts are current. Ownership replaces that marker with the single
// permission the registry operation requires, so the reader never has to guess
// which operation is being called.
type ExecutionFactsReader interface {
	ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error)
}

// ExecutionPermissionSource returns the single permission one registry
// operation requires. Composition derives it from the registry; a test
// may supply a closed subset. It never widens and never reads a request field.
type ExecutionPermissionSource interface {
	Permission(authorize.OperationName) (authorize.ExecutionPermission, error)
}

// RegistryPermissions is the registry-derived permission source: each
// operation's declared execution permission, or ExecNone when the operation is
// not executable.
type RegistryPermissions struct{}

// Permission returns the operation's declared execution permission.
func (RegistryPermissions) Permission(name authorize.OperationName) (authorize.ExecutionPermission, error) {
	operation, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.ExecNone, fault.New(fault.InvalidInput, "operation is not registered")
	}
	return operation.Requires.Execution, nil
}

// Ownership is the one ownership boundary the router has: it resolves the
// scope/targets of one call and admits an execution principal's current
// authority. Both methods run on the same router invocation, so the resolver
// records the operation for the request-local context value and the authority
// port consumes it. The record is removed as soon as it is read, so no call can
// observe another call's operation.
//
// The map is keyed by the context value the router passes to both ports. A
// transport context is a per-request value (an HTTP or JSON-RPC request
// context); a non-comparable context cannot be keyed, and admission then fails
// closed instead of guessing.
type Ownership struct {
	executions  ExecutionFactsReader
	permissions ExecutionPermissionSource

	mu      sync.Mutex
	pending map[context.Context]authorize.OperationName
}

// NewOwnership builds the scope resolver and execution authority.
func NewOwnership(executions ExecutionFactsReader, permissions ExecutionPermissionSource) (*Ownership, error) {
	if executions == nil {
		return nil, fault.New(fault.InvalidInput, "ownership requires an execution facts reader")
	}
	if permissions == nil {
		return nil, fault.New(fault.InvalidInput, "ownership requires an execution permission source")
	}
	return &Ownership{
		executions:  executions,
		permissions: permissions,
		pending:     make(map[context.Context]authorize.OperationName),
	}, nil
}

// Resolve implements the ops.ScopeResolver scope rule.
func (ownership *Ownership) Resolve(ctx context.Context, principal authorize.Principal, call ops.Call) (ops.Resolution, error) {
	operation, ok := authorize.LookupOperation(call.Operation)
	if !ok {
		return ops.Resolution{}, fault.New(fault.InvalidInput, "operation is not registered")
	}
	targets := make(map[string]string, len(call.Params))
	maps.Copy(targets, call.Params)

	// Every identity-scope field the route table names is read from the
	// REST route parameter or, for MCP, from the same canonical input field.
	// A foreign tenant or project is uniformly not_found before any handler,
	// authorization or store read can reveal that the scope exists.
	tenant := identityTarget(call, targets, "tenant")
	if tenant != "" && tenant != string(principal.Scope.TenantID) {
		return ops.Resolution{}, fault.New(fault.NotFound, "tenant is not visible")
	}
	project := identityTarget(call, targets, "project")
	if principal.Scope.ProjectScoped() && project != "" && project != string(principal.Scope.ProjectID) {
		return ops.Resolution{}, fault.New(fault.NotFound, "project is not visible")
	}
	var scope tenancy.Scope
	switch operation.Scope {
	case authorize.ScopeTenant:
		// A tenant operation is resolved inside the credential's own tenant. A
		// tenant operation may still address one project (the membership and
		// project administration routes); the handler applies it explicitly.
		scope = tenancy.Scope{TenantID: principal.Scope.TenantID}
	case authorize.ScopeProject:
		switch {
		case principal.Scope.ProjectScoped():
			scope = principal.Scope
		case project != "":
			scope = tenancy.Scope{TenantID: principal.Scope.TenantID, ProjectID: id.ProjectID(project)}
		default:
			// A tenant-wide credential has no project of its own. An id-only
			// route cannot name one, so it is refused instead of being widened
			// to the whole tenant.
			return ops.Resolution{}, fault.New(fault.InvalidInput,
				"a tenant-wide principal must name the project of the operation")
		}
	default:
		return ops.Resolution{}, fault.New(fault.InternalFailure, "operation has an unknown scope kind")
	}

	if principal.Kind == identity.KindExecution {
		ownership.bind(ctx, operation.Name)
	}
	return ops.Resolution{Scope: scope, Targets: targets}, nil
}

// Admit implements ops.ExecutionAuthority. It consumes the request-local
// operation record the resolver bound, reads the current execution facts and
// replaces the reader's validity marker with the registry permission. An
// execution principal is never admitted without a bound operation, so a
// missing record fails closed.
func (ownership *Ownership) Admit(ctx context.Context, principal authorize.Principal) (authorize.AdmittedExecution, error) {
	operation, ok := ownership.take(ctx)
	if !ok {
		return authorize.AdmittedExecution{}, fault.New(fault.Forbidden,
			"execution admission has no request-local operation")
	}
	admitted, err := ownership.executions.ReadExecution(ctx, principal.ExecutionID)
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	if admitted.Permission == authorize.ExecNone {
		// The reader refused the current facts; AuthorizeExecution denies on
		// the marker without the adapter inventing a permission.
		return admitted, nil
	}
	permission, err := ownership.permissions.Permission(operation)
	if err != nil {
		return authorize.AdmittedExecution{}, err
	}
	admitted.Permission = permission
	return admitted, nil
}

// bind records the operation of one execution-principal request.
func (ownership *Ownership) bind(ctx context.Context, name authorize.OperationName) {
	if !keyable(ctx) {
		return
	}
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	ownership.pending[ctx] = name
}

// take removes and returns the operation bound to the request context.
func (ownership *Ownership) take(ctx context.Context) (authorize.OperationName, bool) {
	if !keyable(ctx) {
		return "", false
	}
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	name, ok := ownership.pending[ctx]
	if ok {
		delete(ownership.pending, ctx)
	}
	return name, ok
}

// keyable reports whether a context value can be used as a map key.
func keyable(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return reflect.TypeOf(ctx).Comparable()
}

// identityTarget returns one identity-scope value the operation's route
// table names: the REST route parameter when the edge supplied it, otherwise the
// canonical MCP input field. It never invents a field name; an operation whose
// route table does not name the identity returns "".
func identityTarget(call ops.Call, targets map[string]string, name string) string {
	if value := targets[name]; value != "" {
		return value
	}
	return routeFieldFromInput(call, name)
}

// routeFieldFromInput reads one route-derived field a MCP caller supplied in the
// request input. The field name comes from the operation's RouteParams
// table, so the REST route and the MCP input always name the same identity.
func routeFieldFromInput(call ops.Call, name string) string {
	codec, ok := codecs[call.Operation]
	if !ok {
		return ""
	}
	var field string
	for _, param := range codec.RouteParams {
		if param.Name == name {
			field = param.Field
			break
		}
	}
	if field == "" {
		return ""
	}
	value, ok := jsonField(call.Request.Input, field)
	if !ok {
		return ""
	}
	return value
}

// jsonField reads one dotted JSON field path from a raw JSON object.
func jsonField(raw json.RawMessage, path string) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false
	}
	var current any
	if err := json.Unmarshal(trimmed, &current); err != nil {
		return "", false
	}
	segments := splitPath(path)
	for index, segment := range segments {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[segment]
		if !ok {
			return "", false
		}
		if index == len(segments)-1 {
			value, ok := current.(string)
			return value, ok
		}
	}
	return "", false
}

// splitPath splits a dotted JSON field path.
func splitPath(path string) []string {
	var segments []string
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || path[index] == '.' {
			if index > start {
				segments = append(segments, path[start:index])
			}
			start = index + 1
		}
	}
	return segments
}
