package opsapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// requestContextKey keys one synthetic per-request transport context. The
// ownership boundary records the resolved operation per request context, so
// each concurrent invocation gets its own value exactly as an HTTP request
// context would.
type requestContextKey struct{}

var requestContextSequence atomic.Int64

// executionRequest builds a fresh per-request context.
func executionRequest() context.Context {
	return context.WithValue(context.Background(), requestContextKey{}, requestContextSequence.Add(1))
}

// installExecutionFacts replaces the admitted facts of one execution.
func (reader *executionFacts) install(executionID id.ExecutionID, admitted authorize.AdmittedExecution) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.byID[executionID] = admitted
}

// runningExecution creates one real task-free execution through the router and
// promotes its relational rows to a running execution with a running container
// action and a future deadline. It returns the execution id, action id and the
// current execution version.
func runningExecution(t *testing.T, h *harness) (string, string, uint64) {
	t.Helper()
	pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
	executionID := startPipeline(t, h, pipelineID, "start-one")
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	executionRow, ok := h.store.Snapshot(scope, "executions", executionID)
	if !ok {
		t.Fatalf("execution row %s is missing", executionID)
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "executions",
			ID:            executionID,
			ExpectVersion: rowVersion(t, executionRow),
			Values:        map[string]any{"state": "running"},
		}},
	}); err != nil {
		t.Fatalf("promote execution: %v", err)
	}

	inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": executionID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspected, &view)
	if len(view.Actions) != 1 {
		t.Fatalf("execution actions = %d; want 1", len(view.Actions))
	}
	actionID := view.Actions[0].ActionID
	actionRow, ok := h.store.Snapshot(scope, "actions", actionID)
	if !ok {
		t.Fatalf("action row %s is missing", actionID)
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:         "actions",
			ID:            actionID,
			ExpectVersion: rowVersion(t, actionRow),
			Values: map[string]any{
				"state":        "running",
				"activated_at": fixedNow,
				"deadline":     fixedNow.Add(time.Hour),
			},
		}},
	}); err != nil {
		t.Fatalf("promote action: %v", err)
	}
	return executionID, actionID, view.Version
}

// installExecutionPrincipal registers one execution credential and its admitted
// facts, returning the principal.
func installExecutionPrincipal(
	t *testing.T,
	h *harness,
	executionID string,
	permission authorize.ExecutionPermission,
	scope tenancy.Scope,
) identity.Principal {
	t.Helper()
	row, ok := h.store.Snapshot(tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, "executions", executionID)
	if !ok {
		t.Fatalf("execution row %s is missing", executionID)
	}
	principalID, _ := row.Values["principal_id"].(string)
	if principalID == "" {
		t.Fatalf("execution row %s has no principal id", executionID)
	}
	principal := identity.Principal{
		ID:          id.PrincipalID(principalID),
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: id.ExecutionID(executionID),
	}
	if err := principal.Validate(); err != nil {
		t.Fatalf("execution principal: %v", err)
	}
	h.principals.principals["token-execution"] = principal
	h.facts.install(id.ExecutionID(executionID), authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: id.ExecutionID(executionID),
		Permission:  permission,
	})
	return principal
}

// invokeExecution drives one operation through the real router with the
// execution credential and a fresh per-request context.
func invokeExecution(
	t *testing.T,
	h *harness,
	operation, input, key string,
	version *uint64,
	params map[string]string,
) (api.Result, error) {
	t.Helper()
	return h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: "token-execution",
		Request: api.Request{
			Input:           json.RawMessage(input),
			ExpectedVersion: version,
			IdempotencyKey:  key,
		},
		Params: params,
	})
}

// TestExecutionPrincipalUsesRegistryPermission pins that a running execution is
// admitted for exactly the operations whose registry row grants an execution
// permission, and denied for the rest.
func TestExecutionPrincipalUsesRegistryPermission(t *testing.T) {
	h := newHarness(t)
	executionID, actionID, executionVersion := runningExecution(t, h)
	principal := installExecutionPrincipal(t, h, executionID, authorize.ExecOwnOutputs, tenancy.Scope{
		TenantID:  testTenant,
		ProjectID: testProject,
	})

	published, err := invokeExecution(t, h, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-execution",
	}), "execution-publish", nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("execution event.publish: %v", err)
	}
	var eventResult api.EventPublishResult
	decodeResult(t, published, &eventResult)
	var observationIDs []string
	for _, committed := range h.store.Events(tenancy.Scope{TenantID: testTenant, ProjectID: testProject}) {
		if string(committed.Envelope.Type) != "observation.custom" {
			continue
		}
		if got := committed.Envelope.Source; got.Kind != "execution" || string(got.PrincipalID) != string(principal.ID) {
			t.Fatalf("observation source = %+v; want the execution principal", got)
		}
		observationIDs = append(observationIDs, string(committed.Envelope.ID))
	}
	if len(observationIDs) != 1 || observationIDs[0] != eventResult.EventID {
		t.Fatalf("execution observations = %+v; want the committed publish %s", observationIDs, eventResult.EventID)
	}

	outputs, err := invokeExecution(t, h, "execution.action.outputs", `{"outputs":{}}`, "execution-outputs", &executionVersion, map[string]string{
		"execution": executionID,
		"action":    actionID,
	})
	if err != nil {
		t.Fatalf("execution execution.action.outputs: %v", err)
	}
	var outputResult api.ExecutionActionOutputsResult
	decodeResult(t, outputs, &outputResult)
	if outputResult.ExecutionVersion <= executionVersion {
		t.Fatalf("outputs execution version = %d; want > %d", outputResult.ExecutionVersion, executionVersion)
	}

	_, err = invokeExecution(t, h, "membership.list", `{}`, "execution-membership", nil, map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("execution membership.list = %v; want forbidden", err)
	}

	cancelVersion := uint64(1)
	_, err = invokeExecution(t, h, "execution.cancel", `{}`, "execution-cancel", &cancelVersion, map[string]string{"execution": executionID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("execution execution.cancel = %v; want forbidden", err)
	}
}

// TestExecutionAdmissionFailsClosed pins the reader marker and scope guards:
// ExecNone and a scope mismatch are forbidden, and an execution cannot address
// a foreign project.
func TestExecutionAdmissionFailsClosed(t *testing.T) {
	h := newHarness(t)
	executionID, _, _ := runningExecution(t, h)
	principal := installExecutionPrincipal(t, h, executionID, authorize.ExecOwnEvents, tenancy.Scope{
		TenantID:  testTenant,
		ProjectID: testProject,
	})
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	h.facts.install(id.ExecutionID(executionID), authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: id.ExecutionID(executionID),
		Permission:  authorize.ExecNone,
	})
	_, err := invokeExecution(t, h, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-none",
	}), "execution-none", nil, map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("ExecNone marker = %v; want forbidden", err)
	}

	foreign := principal
	foreign.Scope = tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}
	h.facts.install(id.ExecutionID(executionID), authorize.AdmittedExecution{
		Principal:   foreign,
		ExecutionID: id.ExecutionID(executionID),
		Permission:  authorize.ExecOwnEvents,
	})
	_, err = invokeExecution(t, h, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-mismatch",
	}), "execution-mismatch", nil, map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("scope mismatch = %v; want forbidden", err)
	}

	// Restore the correct facts, then name a foreign project in the route.
	h.facts.install(id.ExecutionID(executionID), authorize.AdmittedExecution{
		Principal:   principal,
		ExecutionID: id.ExecutionID(executionID),
		Permission:  authorize.ExecOwnEvents,
	})
	_, err = invokeExecution(t, h, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-foreign",
	}), "execution-foreign", nil, map[string]string{"project": "project-b"})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("foreign project = %v; want not_found", err)
	}
	_ = scope
}

// TestExecutionAdmissionRequiresResolve pins that Admit fails closed without a
// request-local resolved operation.
func TestExecutionAdmissionRequiresResolve(t *testing.T) {
	h := newHarness(t)
	executionID, _, _ := runningExecution(t, h)
	principal := installExecutionPrincipal(t, h, executionID, authorize.ExecOwnEvents, tenancy.Scope{
		TenantID:  testTenant,
		ProjectID: testProject,
	})

	_, err := h.ownership.Admit(executionRequest(), principal)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("Admit without Resolve = %v; want forbidden", err)
	}
}

// TestExecutionAdmissionConcurrentOperationsDoNotCross pins that concurrent
// invocations keep their own resolved operation: an allowed operation is never
// denied by another call's binding, and a denied operation is never admitted.
func TestExecutionAdmissionConcurrentOperationsDoNotCross(t *testing.T) {
	h := newHarness(t)
	executionID, _, _ := runningExecution(t, h)
	installExecutionPrincipal(t, h, executionID, authorize.ExecOwnEvents, tenancy.Scope{
		TenantID:  testTenant,
		ProjectID: testProject,
	})

	const workers = 16
	allowedInput := jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-concurrent",
	})
	params := map[string]string{"project": testProject}
	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			invocation := ops.Invocation{
				Credential: "token-execution",
				Params:     params,
			}
			if index%2 == 0 {
				invocation.Operation = authorize.OperationName("event.publish")
				invocation.Request = api.Request{
					Input:          json.RawMessage(allowedInput),
					IdempotencyKey: fmt.Sprintf("concurrent-%d", index),
				}
			} else {
				invocation.Operation = authorize.OperationName("membership.list")
				invocation.Request = api.Request{Input: json.RawMessage(`{}`)}
			}
			_, errs[index] = h.router.Invoke(
				context.WithValue(context.Background(), requestContextKey{}, requestContextSequence.Add(1)),
				invocation,
			)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if index%2 == 0 {
			if err != nil {
				t.Fatalf("concurrent event.publish %d = %v; want admitted", index, err)
			}
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("concurrent membership.list %d = %v; want forbidden", index, err)
		}
	}
}

// rowVersion reads one committed row version.
func rowVersion(t *testing.T, row store.Row) uint64 {
	t.Helper()
	value, ok := row.Values["version"].(int64)
	if !ok || value <= 0 {
		t.Fatalf("row %s has no positive version: %+v", row.Table, row.Values["version"])
	}
	return uint64(value)
}
