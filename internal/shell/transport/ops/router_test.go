package ops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

type rejectingCodec struct{}

func (rejectingCodec) Validate(json.RawMessage) error {
	return fault.New(fault.InvalidInput, "refused by codec")
}

func TestNewRouterRejectsInvalidBindings(t *testing.T) {
	completePorts := func() (authorize.CurrentPrincipal, ops.ScopeResolver, ops.ExecutionAuthority) {
		return &fakePrincipals{principal: humanPrincipal(projectScope(), tenancy.RoleEngineer)},
			&fakeScopes{resolution: ops.Resolution{Scope: projectScope()}},
			&fakeExecutions{}
	}

	cases := []struct {
		name   string
		mutate func([]ops.Binding) []ops.Binding
	}{
		{
			name: "missing binding",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				return bindings[:len(bindings)-1]
			},
		},
		{
			name: "duplicate binding",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				return append(bindings, bindings[0])
			},
		},
		{
			name: "unknown operation",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				bindings[0].Operation = authorize.OperationName("git.ref.update")
				return bindings
			},
		},
		{
			name: "empty operation",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				bindings[0].Operation = ""
				return bindings
			},
		},
		{
			name: "nil input codec",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				bindings[0].Input = nil
				return bindings
			},
		},
		{
			name: "nil result codec",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				bindings[0].Result = nil
				return bindings
			},
		},
		{
			name: "nil handler",
			mutate: func(bindings []ops.Binding) []ops.Binding {
				bindings[0].Handler = nil
				return bindings
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			principals, scopes, executions := completePorts()
			bindings := testCase.mutate(buildBindings())
			if _, err := ops.NewRouter(ops.Config{
				Principals: principals,
				Scopes:     scopes,
				Executions: executions,
				Bindings:   bindings,
			}); err == nil {
				t.Fatalf("NewRouter accepted %s", testCase.name)
			}
		})
	}
}

func TestNewRouterRejectsMissingDependencies(t *testing.T) {
	complete := func() ops.Config {
		return ops.Config{
			Principals: &fakePrincipals{principal: humanPrincipal(projectScope(), tenancy.RoleEngineer)},
			Scopes:     &fakeScopes{resolution: ops.Resolution{Scope: projectScope()}},
			Executions: &fakeExecutions{},
			Bindings:   buildBindings(),
		}
	}

	cases := []struct {
		name   string
		mutate func(*ops.Config)
	}{
		{"nil principals", func(config *ops.Config) { config.Principals = nil }},
		{"nil scopes", func(config *ops.Config) { config.Scopes = nil }},
		{"nil executions", func(config *ops.Config) { config.Executions = nil }},
		{"nil bindings", func(config *ops.Config) { config.Bindings = nil }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := complete()
			testCase.mutate(&config)
			if _, err := ops.NewRouter(config); err == nil {
				t.Fatalf("NewRouter accepted %s", testCase.name)
			}
		})
	}
}

// TestNewRouterCopiesBindings pins that the registry is immutable after
// construction: mutating the caller's slice must not change routing or the
// handler that executes.
func TestNewRouterCopiesBindings(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)

	h.bindings[0].Handler = fixedHandler{operation: "wrong"}
	h.bindings[0].Input = rejectingCodec{}

	result, err := h.invoke(operation.Name, api.Request{}, nil)
	if err != nil {
		t.Fatalf("Invoke after caller mutation: %v", err)
	}
	if result.Operation != string(operation.Name) {
		t.Fatalf("result operation = %q; want %q", result.Operation, operation.Name)
	}
	if got := h.probeFor(operation.Name).handler.callCount(); got != 1 {
		t.Fatalf("original handler calls = %d; want 1", got)
	}
}

// TestRouterAuthenticatesBeforeLookup pins the first boundary: a failed
// authentication never reaches scope resolution, input validation or a
// handler, so no resource lookup can expose scoped information.
func TestRouterAuthenticatesBeforeLookup(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.principals.set(humanPrincipal(projectScope(), tenancy.RoleEngineer),
		fault.New(fault.Unauthenticated, "access token is not valid"))

	_, err := h.invoke(operation.Name, api.Request{IdempotencyKey: "key-1"}, nil)
	mustCode(t, err, fault.Unauthenticated)

	if got := h.principals.callCount(); got != 1 {
		t.Fatalf("authenticator calls = %d; want 1", got)
	}
	if got := h.principals.lastToken(); got != testToken {
		t.Fatalf("authenticator token = %q; want %q", got, testToken)
	}
	if got := h.scopes.callCount(); got != 0 {
		t.Fatalf("scope resolver calls = %d; want 0 after auth refusal", got)
	}
	if got := h.probeFor(operation.Name).input.callCount(); got != 0 {
		t.Fatalf("input codec calls = %d; want 0 after auth refusal", got)
	}
	if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
		t.Fatalf("handler calls = %d; want 0 after auth refusal", got)
	}
	if got := h.order.snapshot(); !reflect.DeepEqual(got, []string{"authenticate"}) {
		t.Fatalf("call order = %v; want [authenticate]", got)
	}
}

// TestRouterCallOrderAndPropagation pins the successful boundary order and the
// exact values the handler observes: verified principal, registry operation,
// resolved scope/targets and the unchanged request.
func TestRouterCallOrderAndPropagation(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.scopes.set(ops.Resolution{
		Scope:   projectScope(),
		Targets: map[string]string{"task": "task-1"},
	}, nil)

	request := api.Request{
		Input:           json.RawMessage(`{"task":"task-1"}`),
		ExpectedVersion: new(testVersion),
		IdempotencyKey:  "key-1",
	}
	result, err := h.invoke(operation.Name, request, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Operation != string(operation.Name) || string(result.Body) != `{"ok":true}` {
		t.Fatalf("result = %+v; want operation %q and body {\"ok\":true}", result, operation.Name)
	}

	wantOrder := []string{"authenticate", "resolve", "input", "handle", "result"}
	if got := h.order.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("call order = %v; want %v", got, wantOrder)
	}

	resolved := h.scopes.lastCall()
	if resolved.Operation != operation.Name {
		t.Fatalf("resolver operation = %q; want %q", resolved.Operation, operation.Name)
	}
	if !reflect.DeepEqual(resolved.Request, request) {
		t.Fatalf("resolver request = %#v; want %#v", resolved.Request, request)
	}

	call := h.probeFor(operation.Name).handler.lastCall()
	wantPrincipal := humanPrincipal(projectScope(), tenancy.RoleEngineer)
	if !reflect.DeepEqual(call.Principal, wantPrincipal) {
		t.Fatalf("handler principal = %#v; want %#v", call.Principal, wantPrincipal)
	}
	if call.Operation.Name != operation.Name {
		t.Fatalf("handler operation = %q; want %q", call.Operation.Name, operation.Name)
	}
	if call.Scope != projectScope() {
		t.Fatalf("handler scope = %#v; want %#v", call.Scope, projectScope())
	}
	if !reflect.DeepEqual(call.Targets, map[string]string{"task": "task-1"}) {
		t.Fatalf("handler targets = %#v; want the resolved targets", call.Targets)
	}
	if !reflect.DeepEqual(call.Request, request) {
		t.Fatalf("handler request = %#v; want unchanged %#v", call.Request, request)
	}
}

// TestRouterDeniedAuthorizationNeverValidatesOrHandles pins that the shared
// grant matrix runs before input validation and the effect: a viewer cannot
// reach pipeline.apply even with a well-formed request and receipt key.
func TestRouterDeniedAuthorizationNeverValidatesOrHandles(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("pipeline.apply")
	h.prepare(operation)
	h.principals.set(humanPrincipal(projectScope(), tenancy.RoleViewer), nil)

	_, err := h.invoke(operation.Name, api.Request{
		Input:          json.RawMessage(`{"pipeline":"pipeline-1"}`),
		IdempotencyKey: "key-1",
	}, nil)
	mustCode(t, err, fault.Forbidden)

	if got := h.probeFor(operation.Name).input.callCount(); got != 0 {
		t.Fatalf("input codec calls = %d; want 0 after denial", got)
	}
	if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
		t.Fatalf("handler calls = %d; want 0 after denial", got)
	}
	wantOrder := []string{"authenticate", "resolve"}
	if got := h.order.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("call order = %v; want %v", got, wantOrder)
	}
}

// TestRouterRefusesScopeResolverFault pins that a failed ownership lookup is a
// refusal, never a fallback to an empty scope.
func TestRouterRefusesScopeResolverFault(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.scopes.set(ops.Resolution{}, fault.New(fault.NotFound, "task is not visible"))

	_, err := h.invoke(operation.Name, api.Request{}, nil)
	mustCode(t, err, fault.NotFound)
	if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
		t.Fatalf("handler calls = %d; want 0 after scope refusal", got)
	}
}

// TestRouterExecutionAuthorityPath pins the separate execution authorization
// path: an execution principal is admitted by the injected port and checked
// with authorize.AuthorizeExecution, not the human/service matrix.
func TestRouterExecutionAuthorityPath(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("execution.action.outputs")
	scope := projectScope()
	principal := executionPrincipal(scope)
	h.principals.set(principal, nil)
	h.scopes.set(ops.Resolution{Scope: scope, Targets: map[string]string{"execution": "exec-1"}}, nil)
	h.executions.set(authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  authorize.ExecOwnOutputs,
		Deadline:    testDeadline,
	}, nil)

	request := api.Request{
		Input:          json.RawMessage(`{"outputs":[]}`),
		IdempotencyKey: "key-1",
	}
	if _, err := h.invoke(operation.Name, request, map[string]string{"execution": "exec-1", "action": "action-1"}); err != nil {
		t.Fatalf("Invoke execution.action.outputs: %v", err)
	}
	if got := h.executions.callCount(); got != 1 {
		t.Fatalf("execution authority calls = %d; want 1", got)
	}
	if got := h.probeFor(operation.Name).handler.lastCall().Principal.Kind; got != "execution" {
		t.Fatalf("handler principal kind = %q; want execution", got)
	}

	// A mismatched admitted permission is a denial, not a fallback to the
	// human/service grant path.
	h.executions.set(authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  authorize.ExecNone,
		Deadline:    testDeadline,
	}, nil)
	_, err := h.invoke(operation.Name, request, map[string]string{"execution": "exec-1", "action": "action-1"})
	mustCode(t, err, fault.Forbidden)
	if got := h.probeFor(operation.Name).handler.callCount(); got != 1 {
		t.Fatalf("handler calls = %d; want 1 after denied execution permission", got)
	}
}

// TestRouterExecutionPrincipalCannotUseHumanGrant pins that an execution
// principal never falls back to a human membership grant for an operation with
// no execution permission.
func TestRouterExecutionPrincipalCannotUseHumanGrant(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.cancel")
	scope := projectScope()
	principal := executionPrincipal(scope)
	h.principals.set(principal, nil)
	h.scopes.set(ops.Resolution{Scope: scope}, nil)
	h.executions.set(authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  authorize.ExecNone,
		Deadline:    testDeadline,
	}, nil)

	_, err := h.invoke(operation.Name, api.Request{IdempotencyKey: "key-1"}, nil)
	mustCode(t, err, fault.Forbidden)
	if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
		t.Fatalf("handler calls = %d; want 0 for execution principal on task.cancel", got)
	}
}

// TestRouterValidatesInputAfterAuthorize pins that operation input validation
// is a post-authorization, pre-effect step.
func TestRouterValidatesInputAfterAuthorize(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.create")
	h.prepare(operation)
	h.probeFor(operation.Name).input.setError(fault.New(fault.InvalidInput, "unknown field"))

	_, err := h.invoke(operation.Name, api.Request{
		Input:          json.RawMessage(`{"bogus":true}`),
		IdempotencyKey: "key-1",
	}, nil)
	mustCode(t, err, fault.InvalidInput)

	wantOrder := []string{"authenticate", "resolve", "input"}
	if got := h.order.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("call order = %v; want %v", got, wantOrder)
	}
	if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
		t.Fatalf("handler calls = %d; want 0 after input refusal", got)
	}
	if got := h.probeFor(operation.Name).result.callCount(); got != 0 {
		t.Fatalf("result codec calls = %d; want 0 after input refusal", got)
	}
}

// TestRouterRejectsPathBodyDisagreement pins that a REST path parameter and a
// same-named top-level input field must agree; the handler is never reached.
func TestRouterRejectsPathBodyDisagreement(t *testing.T) {
	params := map[string]string{"execution": "exec-1", "action": "action-1"}

	t.Run("disagreeing value", func(t *testing.T) {
		h := newHarness(t)
		operation, _ := authorize.LookupOperation("execution.action.outputs")
		prepareExecutionCall(h, operation)

		_, err := h.invoke(operation.Name, api.Request{
			Input:          json.RawMessage(`{"execution":"exec-2","action":"action-1"}`),
			IdempotencyKey: "key-1",
		}, params)
		mustCode(t, err, fault.InvalidInput)
		if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
			t.Fatalf("handler calls = %d; want 0 on path/body disagreement", got)
		}
	})

	t.Run("non-string value", func(t *testing.T) {
		h := newHarness(t)
		operation, _ := authorize.LookupOperation("execution.action.outputs")
		prepareExecutionCall(h, operation)

		_, err := h.invoke(operation.Name, api.Request{
			Input:          json.RawMessage(`{"execution":123}`),
			IdempotencyKey: "key-1",
		}, params)
		mustCode(t, err, fault.InvalidInput)
		if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
			t.Fatalf("handler calls = %d; want 0 on non-string path field", got)
		}
	})

	t.Run("agreeing values", func(t *testing.T) {
		h := newHarness(t)
		operation, _ := authorize.LookupOperation("execution.action.outputs")
		prepareExecutionCall(h, operation)

		if _, err := h.invoke(operation.Name, api.Request{
			Input:          json.RawMessage(`{"execution":"exec-1","action":"action-1"}`),
			IdempotencyKey: "key-1",
		}, params); err != nil {
			t.Fatalf("Invoke with agreeing path/body: %v", err)
		}
		if got := h.probeFor(operation.Name).handler.callCount(); got != 1 {
			t.Fatalf("handler calls = %d; want 1", got)
		}
	})
}

func prepareExecutionCall(h *harness, operation authorize.Operation) {
	h.t.Helper()
	scope := scopeFor(operation)
	principal := executionPrincipal(scope)
	h.principals.set(principal, nil)
	h.scopes.set(ops.Resolution{Scope: scope}, nil)
	h.executions.set(authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  operation.Requires.Execution,
		Deadline:    testDeadline,
	}, nil)
}

// TestRouterRequiresIdempotencyKeyPerPolicy enumerates the declared registry
// and pins the receipt-policy boundary: a required-key operation refuses a
// missing key before the handler, while none/request-scoped operations do not
// require a caller key.
func TestRouterRequiresIdempotencyKeyPerPolicy(t *testing.T) {
	for _, operation := range authorize.Operations() {
		t.Run(string(operation.Name), func(t *testing.T) {
			h := newHarness(t)
			h.prepare(operation)

			switch operation.Receipt {
			case authorize.ReceiptRequired:
				_, err := h.invoke(operation.Name, api.Request{}, nil)
				mustCode(t, err, fault.InvalidInput)
				if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
					t.Fatalf("handler calls = %d; want 0 for a required key that is missing", got)
				}
				if _, err := h.invoke(operation.Name, api.Request{IdempotencyKey: "key-1"}, nil); err != nil {
					t.Fatalf("Invoke with required key: %v", err)
				}
				if got := h.probeFor(operation.Name).handler.callCount(); got != 1 {
					t.Fatalf("handler calls = %d; want 1 with a required key", got)
				}
			case authorize.ReceiptNone, authorize.ReceiptRequestScoped:
				if _, err := h.invoke(operation.Name, api.Request{}, nil); err != nil {
					t.Fatalf("Invoke without a caller key: %v", err)
				}
				if got := h.probeFor(operation.Name).handler.callCount(); got != 1 {
					t.Fatalf("handler calls = %d; want 1 without a caller key", got)
				}
			default:
				t.Fatalf("unexpected receipt policy %q", operation.Receipt)
			}
		})
	}
}

// TestRouterRejectsInvalidRequestEnvelope pins the shared envelope validation
// performed by the router for both edges.
func TestRouterRejectsInvalidRequestEnvelope(t *testing.T) {
	zero := uint64(0)
	cases := []struct {
		name    string
		request api.Request
	}{
		{"zero expected version", api.Request{ExpectedVersion: &zero}},
		{"invalid idempotency key", api.Request{IdempotencyKey: "bad key"}},
		{"oversized idempotency key", api.Request{IdempotencyKey: strings.Repeat("a", api.MaxIdempotencyKeyBytes+1)}},
		{"non-object input", api.Request{Input: json.RawMessage(`[]`)}},
		{"malformed input", api.Request{Input: json.RawMessage(`{"a":1`)}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			operation, _ := authorize.LookupOperation("task.read")
			h.prepare(operation)

			_, err := h.invoke(operation.Name, testCase.request, nil)
			mustCode(t, err, fault.InvalidInput)
			if got := h.probeFor(operation.Name).handler.callCount(); got != 0 {
				t.Fatalf("handler calls = %d; want 0 for an invalid envelope", got)
			}
		})
	}
}

// TestRouterRejectsUnknownOperation pins that an operation outside the declared
// registry is refused without a resource lookup or handler effect.
func TestRouterRejectsUnknownOperation(t *testing.T) {
	h := newHarness(t)

	_, err := h.invoke(authorize.OperationName("task.unknown"), api.Request{}, nil)
	mustCode(t, err, fault.InvalidInput)
	if got := h.scopes.callCount(); got != 0 {
		t.Fatalf("scope resolver calls = %d; want 0 for an unknown operation", got)
	}
}

// TestRouterRejectsInvalidHandlerResults pins that a handler result is checked
// against its binding and sanitized as an internal failure when it is not.
func TestRouterRejectsInvalidHandlerResults(t *testing.T) {
	t.Run("operation mismatch", func(t *testing.T) {
		h := newHarness(t)
		operation, _ := authorize.LookupOperation("task.read")
		h.prepare(operation)
		h.probeFor(operation.Name).handler.setFunc(func(context.Context, ops.AuthorizedCall) (api.Result, error) {
			return api.Result{Operation: "pipeline.read", Body: json.RawMessage(`{"ok":true}`)}, nil
		})

		_, err := h.invoke(operation.Name, api.Request{}, nil)
		mustCode(t, err, fault.InternalFailure)
	})

	t.Run("result codec refusal", func(t *testing.T) {
		h := newHarness(t)
		operation, _ := authorize.LookupOperation("task.read")
		h.prepare(operation)
		h.probeFor(operation.Name).result.setError(errors.New("SECRET-RESULT-CAUSE"))

		_, err := h.invoke(operation.Name, api.Request{}, nil)
		mustCode(t, err, fault.InternalFailure)
		if strings.Contains(err.Error(), "SECRET-RESULT-CAUSE") {
			t.Fatalf("internal failure rendered the result codec cause: %v", err)
		}
	})
}

// TestRouterMapsEveryHandlerFault pins that each of the 11 public fault classes
// keeps its stable code and public message while a private cause is never
// rendered.
func TestRouterMapsEveryHandlerFault(t *testing.T) {
	for _, class := range fault.Classes() {
		t.Run(string(class), func(t *testing.T) {
			h := newHarness(t)
			operation, _ := authorize.LookupOperation("task.read")
			h.prepare(operation)
			h.probeFor(operation.Name).handler.setFunc(func(context.Context, ops.AuthorizedCall) (api.Result, error) {
				return api.Result{}, fault.Wrap(class, "public message", errors.New("SECRET-PRIVATE-CAUSE"))
			})

			_, err := h.invoke(operation.Name, api.Request{}, nil)
			mustCode(t, err, class)
			wantCode, ok := fault.CodeFor(class)
			if !ok {
				t.Fatalf("no public code for class %q", class)
			}
			if got, ok := fault.CodeOf(err); !ok || got != wantCode {
				t.Fatalf("fault code = %q (ok %v); want %q", got, ok, wantCode)
			}
			if got, ok := fault.MessageOf(err); !ok || got != "public message" {
				t.Fatalf("fault message = %q (ok %v); want %q", got, ok, "public message")
			}
			if strings.Contains(err.Error(), "SECRET-PRIVATE-CAUSE") {
				t.Fatalf("fault rendered its private cause: %v", err)
			}
		})
	}
}

// TestRouterSanitizesNonFaultHandlerError pins that an unexpected error becomes
// a generic internal failure rather than an echoed implementation error.
func TestRouterSanitizesNonFaultHandlerError(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.probeFor(operation.Name).handler.setFunc(func(context.Context, ops.AuthorizedCall) (api.Result, error) {
		return api.Result{}, errors.New("SECRET-HANDLER-CAUSE")
	})

	_, err := h.invoke(operation.Name, api.Request{}, nil)
	mustCode(t, err, fault.InternalFailure)
	if strings.Contains(err.Error(), "SECRET-HANDLER-CAUSE") {
		t.Fatalf("internal failure rendered the handler cause: %v", err)
	}
}

// TestRouterSanitizeLogsTheCauseWithoutCredentials pins the operator half of
// sanitize: the untyped adapter error is logged for diagnosis, but a DSN
// password, an Authorization value or a password literal in it is redacted.
func TestRouterSanitizeLogsTheCauseWithoutCredentials(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const (
		dsnSecret   = "SENTINEL-ROUTER-DSN-PASSWORD"
		bearerToken = "SENTINEL-ROUTER-BEARER"
	)
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.probeFor(operation.Name).handler.setFunc(func(context.Context, ops.AuthorizedCall) (api.Result, error) {
		return api.Result{}, fmt.Errorf(`connect postgres://kodama_runtime:%s@db/kodama failed: header "Authorization: Bearer %s": constraint violation`,
			dsnSecret, bearerToken)
	})

	_, err := h.invoke(operation.Name, api.Request{}, nil)
	mustCode(t, err, fault.InternalFailure)
	if strings.Contains(err.Error(), dsnSecret) || strings.Contains(err.Error(), "constraint violation") {
		t.Fatalf("client error rendered the cause: %v", err)
	}
	text := logged.String()
	if text == "" {
		t.Fatal("the sanitized cause was not logged")
	}
	if !strings.Contains(text, "constraint violation") {
		t.Fatalf("log = %q; want the adapter diagnostic for the operator", text)
	}
	for _, secret := range []string{dsnSecret, bearerToken} {
		if strings.Contains(text, secret) {
			t.Fatalf("log rendered %q: %s", secret, text)
		}
	}
}

type receiptRecord struct {
	input  []byte
	result api.Result
}

// receiptCapability is a fake capability that owns its atomic receipt map. It
// proves the router preserves the request identity the capability needs; the
// router itself has no receipt store.
type receiptCapability struct {
	mu      sync.Mutex
	records map[string]receiptRecord
	calls   int
	last    ops.AuthorizedCall
}

func newReceiptCapability() *receiptCapability {
	return &receiptCapability{records: make(map[string]receiptRecord)}
}

func (capability *receiptCapability) Handle(_ context.Context, call ops.AuthorizedCall) (api.Result, error) {
	capability.mu.Lock()
	defer capability.mu.Unlock()
	capability.calls++
	capability.last = call

	record, ok := capability.records[call.Request.IdempotencyKey]
	if ok {
		if !bytes.Equal(record.input, call.Request.Input) {
			return api.Result{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
		}
		result := record.result
		result.Replayed = true
		return result, nil
	}

	result := api.Result{
		Operation: string(call.Operation.Name),
		Body:      json.RawMessage(`{"created":true}`),
	}
	capability.records[call.Request.IdempotencyKey] = receiptRecord{
		input:  append([]byte(nil), call.Request.Input...),
		result: result,
	}
	return result, nil
}

func (capability *receiptCapability) callCount() int {
	capability.mu.Lock()
	defer capability.mu.Unlock()
	return capability.calls
}

// TestRouterReceiptRetrySemantics pins that the router preserves the request
// content, expected version and idempotency key across retries, so an identical
// retry returns the capability's original result with Replayed and changed
// content is refused by the capability with changed_idempotency_content.
func TestRouterReceiptRetrySemantics(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.create")
	h.prepare(operation)
	capability := newReceiptCapability()
	h.probeFor(operation.Name).handler.setFunc(capability.Handle)

	first := api.Request{
		Input:           json.RawMessage(`{"title":"first"}`),
		ExpectedVersion: new(testVersion),
		IdempotencyKey:  "key-1",
	}
	firstResult, err := h.invoke(operation.Name, first, nil)
	if err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	if firstResult.Replayed {
		t.Fatal("first result reported Replayed")
	}
	if string(firstResult.Body) != `{"created":true}` {
		t.Fatalf("first result body = %s; want {\"created\":true}", firstResult.Body)
	}

	secondResult, err := h.invoke(operation.Name, first, nil)
	if err != nil {
		t.Fatalf("identical retry Invoke: %v", err)
	}
	if !secondResult.Replayed {
		t.Fatal("identical retry did not preserve the capability's Replayed fact")
	}
	if secondResult.Operation != firstResult.Operation || !bytes.Equal(secondResult.Body, firstResult.Body) {
		t.Fatalf("identical retry result = %+v; want original %+v", secondResult, firstResult)
	}

	changed := first
	changed.Input = json.RawMessage(`{"title":"changed"}`)
	_, err = h.invoke(operation.Name, changed, nil)
	mustCode(t, err, fault.ChangedIdempotencyContent)

	if got := capability.callCount(); got != 3 {
		t.Fatalf("capability calls = %d; want 3", got)
	}
	if capability.last.Request.ExpectedVersion == nil || *capability.last.Request.ExpectedVersion != testVersion {
		t.Fatalf("capability expected version = %v; want %d", capability.last.Request.ExpectedVersion, testVersion)
	}
	if string(capability.last.Request.Input) != `{"title":"changed"}` {
		t.Fatalf("capability input = %s; want the changed content", capability.last.Request.Input)
	}
}

// TestRouterRetryReauthenticatesAfterRevocation pins that no cached receipt
// result can bypass current authentication: a revoked actor's identical retry
// fails before the capability is reached.
func TestRouterRetryReauthenticatesAfterRevocation(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.create")
	h.prepare(operation)
	capability := newReceiptCapability()
	h.probeFor(operation.Name).handler.setFunc(capability.Handle)

	request := api.Request{
		Input:          json.RawMessage(`{"title":"first"}`),
		IdempotencyKey: "key-1",
	}
	if _, err := h.invoke(operation.Name, request, nil); err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	if got := capability.callCount(); got != 1 {
		t.Fatalf("capability calls = %d; want 1", got)
	}

	h.principals.set(humanPrincipal(projectScope(), tenancy.RoleEngineer),
		fault.New(fault.Unauthenticated, "principal was revoked"))
	_, err := h.invoke(operation.Name, request, nil)
	mustCode(t, err, fault.Unauthenticated)

	if got := capability.callCount(); got != 1 {
		t.Fatalf("capability calls = %d; want 1 after revocation", got)
	}
	if got := h.scopes.callCount(); got != 1 {
		t.Fatalf("scope resolver calls = %d; want 1 after revocation", got)
	}
}

// TestRouterConcurrentInvocations pins that one router shares no per-call state:
// concurrent callers keep their own operation and request identity.
func TestRouterConcurrentInvocations(t *testing.T) {
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.probeFor(operation.Name).handler.setFunc(func(_ context.Context, call ops.AuthorizedCall) (api.Result, error) {
		body, err := json.Marshal(map[string]string{"key": call.Request.IdempotencyKey})
		if err != nil {
			return api.Result{}, err
		}
		return api.Result{Operation: string(call.Operation.Name), Body: body}, nil
	})

	const workers = 16
	var wait sync.WaitGroup
	failures := make(chan error, workers)
	for index := range workers {
		wait.Go(func() {
			key := fmt.Sprintf("key-%d", index)
			result, err := h.router.Invoke(context.Background(), ops.Invocation{
				Operation:  operation.Name,
				Credential: testToken,
				Request:    api.Request{IdempotencyKey: key},
			})
			if err != nil {
				failures <- fmt.Errorf("worker %d: %w", index, err)
				return
			}
			var body map[string]string
			if err := json.Unmarshal(result.Body, &body); err != nil {
				failures <- fmt.Errorf("worker %d body: %w", index, err)
				return
			}
			if body["key"] != key {
				failures <- fmt.Errorf("worker %d observed key %q; want %q", index, body["key"], key)
			}
		})
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

// TestRedactForLogCoversEveryCredentialShape pins the operator log redactor
// against the shapes a private adapter diagnostic may carry. Each case names
// the secret substring that must not survive; the label stays readable so the
// operator can still diagnose the failure.
func TestRedactForLogCoversEveryCredentialShape(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0.eyJpc3MiOiJodHRwczovL2lkcCIsInN1YiI6ImFkbWluIn0.c2lnbmF0dXJlLWJ5dGVz"
	authorization := "ABC123TOKEN"
	pemSecret := "MIIBOgIBAAJBAKj34GkxFh"
	for _, tc := range []struct {
		name    string
		text    string
		secrets []string
	}{
		{
			name:    "pem private key block",
			text:    "decode failed:\n-----BEGIN RSA PRIVATE KEY-----\n" + pemSecret + "\n-----END RSA PRIVATE KEY-----\ntrailing",
			secrets: []string{pemSecret},
		},
		{
			name:    "PASSWORD literal",
			text:    `syntax error at or near "ALTER ROLE kodama_runtime PASSWORD 'hunter2'"`,
			secrets: []string{"hunter2"},
		},
		{
			name:    "PASSWORD E literal with doubled quote and backslash",
			text:    `ALTER ROLE "kodama_runtime" PASSWORD E'it''s-\\-secret'`,
			secrets: []string{"it''s", `\`, "secret"},
		},
		{
			name:    "password assignment",
			text:    "connect failed password=hunter2 host=db",
			secrets: []string{"hunter2"},
		},
		{
			name:    "product secret key label",
			text:    "SCW_SECRET_KEY=scw-secret-sentinel was unusable",
			secrets: []string{"scw-secret-sentinel"},
		},
		{
			name:    "snake case secret key label",
			text:    "secret_key: scw-secret-sentinel",
			secrets: []string{"scw-secret-sentinel"},
		},
		{
			name:    "Authorization Token scheme",
			text:    "header 'Authorization: Token " + authorization + "' was refused",
			secrets: []string{authorization},
		},
		{
			name:    "ApiKey scheme",
			text:    "credential ApiKey " + authorization + " was refused",
			secrets: []string{authorization},
		},
		{
			name:    "cookie header",
			text:    "Cookie: session=" + authorization + "; other=tail-sentinel",
			secrets: []string{authorization, "tail-sentinel"},
		},
		{
			name:    "unlabelled bearer",
			text:    "presented Bearer " + authorization + " and failed",
			secrets: []string{authorization},
		},
		{
			name:    "unlabelled jwt",
			text:    "token " + jwt + " was malformed",
			secrets: []string{jwt, "eyJpc3MiOiJodHRwczovL2lkcCJ9"},
		},
		{
			name:    "https userinfo",
			text:    "GET https://user:https-sentinel@idp.example/jwks failed",
			secrets: []string{"https-sentinel"},
		},
		{
			name:    "nats userinfo",
			text:    "dial nats://user:nats-sentinel@nats:4222 failed",
			secrets: []string{"nats-sentinel"},
		},
		{
			name:    "postgres password with at sign",
			text:    "dial postgres://kodama_runtime:p@ss-tail-sentinel@db:5432/kodama failed",
			secrets: []string{"p@ss-tail-sentinel", "ss-tail-sentinel"},
		},
		{
			name:    "x auth token header",
			text:    "header 'X-Auth-Token: " + authorization + "' was refused",
			secrets: []string{authorization},
		},
		{
			name:    "tls userinfo with an empty user",
			text:    "dial tls://:empty-user-sentinel@nats:4222 failed",
			secrets: []string{"empty-user-sentinel"},
		},
		{
			name:    "https user-only userinfo",
			text:    "GET https://user-only-sentinel@idp.example/jwks failed",
			secrets: []string{"user-only-sentinel"},
		},
		{
			name:    "passphrase assignment",
			text:    "decrypt failed passphrase=passphrase-sentinel key=ok",
			secrets: []string{"passphrase-sentinel"},
		},
		{
			name:    "pwd assignment",
			text:    "connect failed pwd=pwd-sentinel host=db",
			secrets: []string{"pwd-sentinel"},
		},
		{
			name:    "pass assignment",
			text:    "connect failed pass=pass-sentinel host=db",
			secrets: []string{"pass-sentinel"},
		},
		{
			name:    "PASSWORD literal across a newline",
			text:    "syntax error PASSWORD\n'plain-newline-sentinel'",
			secrets: []string{"plain-newline-sentinel"},
		},
		{
			name:    "quoted PASSWORD label across a newline",
			text:    "'PASSWORD'\n'newline-password-sentinel'",
			secrets: []string{"newline-password-sentinel"},
		},
		{
			name:    "Authorization value across a newline",
			text:    "header 'Authorization:'\n '" + authorization + "' was refused",
			secrets: []string{authorization},
		},
		{
			name:    "Authorization scheme across a newline",
			text:    "header 'Authorization: Digest\n digest-scheme-sentinel' was refused",
			secrets: []string{"digest-scheme-sentinel"},
		},
		{
			name:    "quoted pass label across a newline",
			text:    "'pass'\n'pass-label-sentinel'",
			secrets: []string{"pass-label-sentinel"},
		},
		{
			name:    "https userinfo with an empty password",
			text:    "GET https://empty-password-sentinel:@idp.example/jwks failed",
			secrets: []string{"empty-password-sentinel"},
		},
		{
			name:    "PASSWORD dollar-quoted literal",
			text:    "ALTER ROLE kodama_runtime PASSWORD $$dollar-quoted-sentinel$$",
			secrets: []string{"dollar-quoted-sentinel"},
		},
		{
			name:    "pw assignment",
			text:    "connect failed pw=pw-assignment-sentinel host=db",
			secrets: []string{"pw-assignment-sentinel"},
		},
		{
			name:    "password label across a newline",
			text:    "connect failed password:\nnewline-assignment-sentinel",
			secrets: []string{"newline-assignment-sentinel"},
		},
		{
			name:    "unlabelled bearer across a newline",
			text:    "presented Bearer\nbearer-newline-sentinel and failed",
			secrets: []string{"bearer-newline-sentinel"},
		},
		{
			name:    "unlabelled token scheme",
			text:    "presented Token token-scheme-sentinel and failed",
			secrets: []string{"token-scheme-sentinel"},
		},
		{
			name:    "unlabelled basic scheme",
			text:    "presented Basic basic-scheme-sentinel and failed",
			secrets: []string{"basic-scheme-sentinel"},
		},
		{
			name:    "PASSWORD dollar-quoted literal across a newline",
			text:    "ALTER ROLE kodama_runtime PASSWORD $$first-line\ndollar-newline-sentinel$$",
			secrets: []string{"dollar-newline-sentinel"},
		},
		{
			name:    "pw colon assignment",
			text:    "connect failed pw: pw-colon-sentinel host=db",
			secrets: []string{"pw-colon-sentinel"},
		},
		{
			name:    "pass label across a newline",
			text:    "connect failed pass:\npass-newline-sentinel",
			secrets: []string{"pass-newline-sentinel"},
		},
		{
			name:    "passwd label across a newline",
			text:    "connect failed passwd:\npasswd-newline-sentinel",
			secrets: []string{"passwd-newline-sentinel"},
		},
		{
			name:    "pwd label across a newline",
			text:    "connect failed pwd:\npwd-newline-sentinel",
			secrets: []string{"pwd-newline-sentinel"},
		},
		{
			name:    "passphrase label across a newline",
			text:    "decrypt failed passphrase:\npassphrase-newline-sentinel",
			secrets: []string{"passphrase-newline-sentinel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redacted := ops.RedactForLog(tc.text)
			for _, secret := range tc.secrets {
				if strings.Contains(redacted, secret) {
					t.Fatalf("redacted text still contains %q: %s", secret, redacted)
				}
			}
			if !strings.Contains(redacted, "redacted") {
				t.Fatalf("redacted text = %q; want a visible redaction marker", redacted)
			}
		})
	}
}

// TestRedactForLogKeepsOrdinaryDiagnostics proves redaction is not a blanket
// dump: a database diagnostic that names no credential is preserved.
func TestRedactForLogKeepsOrdinaryDiagnostics(t *testing.T) {
	const text = "constraint actions_running_unique violated on actions (execution_id, state)"
	if redacted := ops.RedactForLog(text); redacted != text {
		t.Fatalf("redacted = %q; want %q", redacted, text)
	}
	// The short "pw" label is exact: a longer word that merely CONTAINS those
	// letters is not a credential assignment and stays readable. The vector
	// must be one the unbounded rule would match: "tmppw=x" has the letters
	// immediately before the assignment, so dropping the word boundary
	// redacts it. ("upward=x" cannot prove the boundary: its "pw" is followed
	// by "ard", which no variant of the rule matches.)
	const ordinary = "the worker moved upward=x and tmppw=x-sentinel and continued"
	if redacted := ops.RedactForLog(ordinary); redacted != ordinary {
		t.Fatalf("redacted = %q; want %q (the pw label rule over-matched)", redacted, ordinary)
	}
}

// TestRouterSanitizeLogWriterRedactsNamedCredentialForms proves the router's
// real sanitize log writer removes every named credential shape, not only a
// DSN: a PEM private key, a SQL PASSWORD literal with an E prefix and doubled
// quote, a password assignment and a bare Authorization scheme value.
func TestRouterSanitizeLogWriterRedactsNamedCredentialForms(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const (
		pemSecret = "MIIBOgIBAAJBAKj34GkxFhRouterSentinel"
		sqlSecret = "router-sql-sentinel"
		scheme    = "router-scheme-sentinel"
	)
	h := newHarness(t)
	operation, _ := authorize.LookupOperation("task.read")
	h.prepare(operation)
	h.probeFor(operation.Name).handler.setFunc(func(context.Context, ops.AuthorizedCall) (api.Result, error) {
		return api.Result{}, fmt.Errorf("wrap failed:\n-----BEGIN EC PRIVATE KEY-----\n%s\n-----END EC PRIVATE KEY-----\n"+
			"ALTER ROLE kodama_runtime PASSWORD E'it''s-%s': Authorization: Token %s", pemSecret, sqlSecret, scheme)
	})
	_, err := h.invoke(operation.Name, api.Request{}, nil)
	mustCode(t, err, fault.InternalFailure)
	text := logged.String()
	for _, secret := range []string{pemSecret, sqlSecret, "it''s", scheme} {
		if strings.Contains(text, secret) {
			t.Fatalf("router log rendered %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "wrap failed") {
		t.Fatalf("router log lost the operator diagnostic: %s", text)
	}
}
