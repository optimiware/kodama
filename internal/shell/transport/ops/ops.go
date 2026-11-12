// Package ops is the single operation router shared by the REST and MCP public
// edges.
//
// The router owns the immutable, complete operation registry: one binding for
// every registry operation, keyed by authorize.OperationName. It is a transport
// boundary, not a capability: it never authenticates a credential itself, never
// looks up a resource, never performs an effect and never stores a receipt.
// Those responsibilities belong to the injected authorize.CurrentPrincipal,
// ScopeResolver and ExecutionAuthority ports and to the bound capability
// handler.
//
// One Invoke authenticates first, then resolves the scope/targets, then
// authorizes through the shared grant matrix (or the admitted execution
// permission), then validates the request envelope, the registry receipt policy,
// any REST path/body agreement and the operation input codec, and only then
// calls the handler. A refusal is a typed fault whose public class/code/message
// survive while a private cause is never rendered.
package ops

import (
	"context"
	"encoding/json"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Codec validates one operation payload: the decoded input object before the
// effect, or the handler result body after it. It only refuses values; it never
// decodes into a capability-specific type or performs an effect.
type Codec interface {
	Validate(json.RawMessage) error
}

// Handler is one capability binding. The router calls it only after
// authentication, scope resolution, authorization and input validation have all
// succeeded, so a handler never observes an unauthenticated or unauthorized
// call. The handler owns its own atomic effect+receipt commit.
type Handler interface {
	Handle(context.Context, AuthorizedCall) (api.Result, error)
}

// Binding binds one registry operation to its input codec, result codec and
// handler.
type Binding struct {
	Operation authorize.OperationName
	Input     Codec
	Result    Codec
	Handler   Handler
}

// AuthorizedCall is the verified, authorized call one handler receives. The
// router passes the authenticated principal, the registry operation, the
// resolved ownership scope and targets, and the unchanged request envelope so
// the capability can keep its own idempotency receipt.
type AuthorizedCall struct {
	Principal authorize.Principal
	Operation authorize.Operation
	Scope     tenancy.Scope
	Targets   map[string]string
	Request   api.Request
}

// Call is the scope-resolution input: the registry operation, the unchanged
// request envelope and the concrete REST path parameters (nil for MCP).
type Call struct {
	Operation authorize.OperationName
	Request   api.Request
	Params    map[string]string
}

// Invocation is one decoded edge call: the registry operation, the presented
// credential, the decoded request envelope and the concrete path parameters.
type Invocation struct {
	Operation  authorize.OperationName
	Credential string
	Request    api.Request
	Params     map[string]string
}

// Resolution is the ownership scope and targets a ScopeResolver returns for one
// call. A resolver must refuse rather than return an empty scope when ownership
// cannot be established.
type Resolution struct {
	Scope   tenancy.Scope
	Targets map[string]string
}

// ScopeResolver resolves the relational ownership scope and targets of one
// call. It is the only resource-lookup boundary the router has.
type ScopeResolver interface {
	Resolve(context.Context, authorize.Principal, Call) (Resolution, error)
}

// ExecutionAuthority admits the current execution authority of an execution
// principal: its execution/action identity, closed permission and deadline.
type ExecutionAuthority interface {
	Admit(context.Context, authorize.Principal) (authorize.AdmittedExecution, error)
}

// Config is the complete router construction input. Every dependency and the
// complete binding set are required.
type Config struct {
	Principals authorize.CurrentPrincipal
	Scopes     ScopeResolver
	Executions ExecutionAuthority
	Bindings   []Binding
}
