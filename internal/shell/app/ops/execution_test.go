package opsapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// TestExecutionFamily drives the real execution service reads and cancellation
// after a task-free start, and proves the mutating handlers reach the real
// service (a queued execution cannot stage outputs or resolve a request).
func TestExecutionFamily(t *testing.T) {
	h := newHarness(t)
	yaml := []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo ok\n")
	applied, err := h.invoke(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(yaml),
	}), map[string]string{"project": testProject, "pipeline": "build.v2"})
	if err != nil {
		t.Fatalf("pipeline.apply: %v", err)
	}
	var pipeline api.PipelineApplyResult
	decodeResult(t, applied, &pipeline)
	started, err := h.invokeVersion(t, testToken, "pipeline.start", `{"inputs":{}}`, 1, map[string]string{
		"project":  testProject,
		"pipeline": pipeline.PipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.start: %v", err)
	}
	var start api.PipelineStartResult
	decodeResult(t, started, &start)

	inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": start.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspected, &view)
	if view.ExecutionID != start.ExecutionID || len(view.Actions) == 0 {
		t.Fatalf("inspect result = %+v", view)
	}

	listed, err := h.invoke(t, testToken, "execution.list", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("execution.list: %v", err)
	}
	var list api.ExecutionListResult
	decodeResult(t, listed, &list)
	if len(list.Executions) != 1 || list.Executions[0].ExecutionID != start.ExecutionID {
		t.Fatalf("list result = %+v", list)
	}

	// A queued execution cannot stage container outputs: the real service
	// refuses through the Kernel reducer rather than the handler inventing one.
	_, err = h.invokeVersion(t, testToken, "execution.action.outputs", jsonBody(t, map[string]any{
		"outputs": map[string]string{"key": "value"},
	}), view.Version, map[string]string{"execution": start.ExecutionID, "action": view.Actions[0].ActionID})
	if err == nil {
		t.Fatal("execution.action.outputs accepted a queued execution")
	}
	if _, ok := fault.ClassOf(err); !ok {
		t.Fatalf("outputs refusal is not a typed fault: %v", err)
	}

	// The first action is a container action, so request resolution is refused
	// by the request service for the action kind, not by the body codec.
	_, err = h.invoke(t, testToken, "execution.request.resolve", jsonBody(t, map[string]any{
		"expected_action_version": view.Actions[0].Version,
		"decision":                "pass",
	}), map[string]string{"execution": start.ExecutionID, "action": view.Actions[0].ActionID})
	if err == nil {
		t.Fatal("execution.request.resolve accepted a container action")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("resolve refusal = %v; want invalid_input", err)
	}
	if !strings.Contains(err.Error(), "is not a request action") {
		t.Fatalf("resolve container refusal = %v; want the request-action guard, not the input codec", err)
	}

	cancelled, err := h.invokeVersion(t, testToken, "execution.cancel", `{}`, view.Version, map[string]string{"execution": start.ExecutionID})
	if err != nil {
		t.Fatalf("execution.cancel: %v", err)
	}
	var cancellation api.ExecutionCancelResult
	decodeResult(t, cancelled, &cancellation)
	if cancellation.ExecutionVersion <= view.Version {
		t.Fatalf("cancel version = %d; want > %d", cancellation.ExecutionVersion, view.Version)
	}

	// The harness has no store-backed retained sink; the injected boundary
	// reports not_found and the handler preserves it.
	_, err = h.invoke(t, testToken, "execution.logs.read", jsonBody(t, map[string]any{
		"action_id": view.Actions[0].ActionID,
		"stream":    "stdout",
	}), map[string]string{"execution": start.ExecutionID})
	if err == nil {
		t.Fatal("execution.logs.read succeeded without a retained sink")
	}
}

// recordingRequestResolution captures the exact command the handler builds and
// returns a valid committed result, so the handler's envelope plumbing can be
// asserted without a second service implementation.
type recordingRequestResolution struct {
	calls   int
	command request.ResolveCommand
}

func (recorder *recordingRequestResolution) Resolve(_ context.Context, command request.ResolveCommand) (request.ResolveResult, error) {
	recorder.calls++
	recorder.command = command
	return request.ResolveResult{
		ExecutionVersion: command.ExpectedExecutionVersion + 1,
		ActionVersion:    command.ExpectedActionVersion + 1,
	}, nil
}

// TestExecutionResolveHandlerPassesEnvelopeExpectedVersion pins the action-only
// handler plumbing: the body no longer carries an execution version, so the
// shared envelope expected_version must reach the request service as its
// execution guard. A handler that drops it would silently weaken the fresh
// decision's execution-version check.
func TestExecutionResolveHandlerPassesEnvelopeExpectedVersion(t *testing.T) {
	h := newHarness(t)
	deps := h.deps
	resolution := &recordingRequestResolution{}
	deps.Requests = resolution
	bindings, err := opsapp.Bindings(deps)
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	var handler ops.Handler
	for _, binding := range bindings {
		if binding.Operation == "execution.request.resolve" {
			handler = binding.Handler
			break
		}
	}
	if handler == nil {
		t.Fatal("execution.request.resolve has no handler binding")
	}

	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	envelope := uint64(9)
	_, err = handler.Handle(context.Background(), ops.AuthorizedCall{
		Principal: authorize.Principal{ID: "prn-1", Kind: "human", Scope: scope},
		Operation: authorize.Operation{Name: "execution.request.resolve"},
		Scope:     scope,
		Targets:   map[string]string{"execution": "exec-1", "action": "act-1"},
		Request: api.Request{
			Input:           json.RawMessage(`{"expected_action_version":2,"decision":"pass"}`),
			ExpectedVersion: &envelope,
			IdempotencyKey:  "resolve-envelope",
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resolution.calls != 1 {
		t.Fatalf("request service calls = %d; want 1", resolution.calls)
	}
	if resolution.command.ExpectedExecutionVersion != envelope {
		t.Fatalf("resolve command envelope version = %d; want the supplied %d",
			resolution.command.ExpectedExecutionVersion, envelope)
	}
	if resolution.command.ExpectedActionVersion != 2 {
		t.Fatalf("resolve command action version = %d; want 2", resolution.command.ExpectedActionVersion)
	}

	// An omitted envelope version reaches the service as the omitted zero so it
	// derives the internal commit versions from observed state.
	resolution.calls = 0
	_, err = handler.Handle(context.Background(), ops.AuthorizedCall{
		Principal: authorize.Principal{ID: "prn-1", Kind: "human", Scope: scope},
		Operation: authorize.Operation{Name: "execution.request.resolve"},
		Scope:     scope,
		Targets:   map[string]string{"execution": "exec-1", "action": "act-1"},
		Request: api.Request{
			Input:          json.RawMessage(`{"expected_action_version":2,"decision":"pass"}`),
			IdempotencyKey: "resolve-envelope-omitted",
		},
	})
	if err != nil {
		t.Fatalf("Handle without envelope: %v", err)
	}
	if resolution.calls != 1 || resolution.command.ExpectedExecutionVersion != 0 {
		t.Fatalf("omitted envelope command version = %d (calls %d); want 0",
			resolution.command.ExpectedExecutionVersion, resolution.calls)
	}
}

type countingLogReader struct {
	calls int
}

func (reader *countingLogReader) Read(_ context.Context, _ log.ReadCommand) (log.ReadPage, error) {
	reader.calls++
	return log.ReadPage{}, nil
}

// TestExecutionLogsReadRefusesForeignAndUnknownExecutionWithNotFound proves
// execution.logs.read checks execution and action visibility inside the
// caller's resolved scope before touching the retained-log sink, so a foreign
// or nonexistent execution/action is uniformly not_found rather than an empty
// 200 page.
func TestExecutionLogsReadRefusesForeignAndUnknownExecutionWithNotFound(t *testing.T) {
	h := newHarness(t)
	logs := &countingLogReader{}
	deps := h.deps
	deps.Logs = logs
	bindings, err := opsapp.Bindings(deps)
	if err != nil {
		t.Fatalf("Bindings: %v", err)
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.ownership,
		Executions: h.ownership,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}

	yaml := []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo ok\n")
	applied, err := h.invoke(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(yaml),
	}), map[string]string{"project": testProject, "pipeline": "logs.scope"})
	if err != nil {
		t.Fatalf("pipeline.apply: %v", err)
	}
	var pipe api.PipelineApplyResult
	decodeResult(t, applied, &pipe)
	started, err := h.invokeVersion(t, testToken, "pipeline.start", `{"inputs":{}}`, 1, map[string]string{
		"project":  testProject,
		"pipeline": pipe.PipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.start: %v", err)
	}
	var start api.PipelineStartResult
	decodeResult(t, started, &start)
	inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": start.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspected, &view)
	actionID := view.Actions[0].ActionID

	for _, testCase := range []struct {
		name       string
		credential string
		execution  string
		action     string
	}{
		{name: "foreign project engineer", credential: "token-engineer-b", execution: start.ExecutionID, action: actionID},
		{name: "unknown execution in own project", credential: testToken, execution: "execution-missing", action: actionID},
		{name: "unknown action in own execution", credential: testToken, execution: start.ExecutionID, action: "act-missing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := router.Invoke(context.Background(), ops.Invocation{
				Operation:  "execution.logs.read",
				Credential: testCase.credential,
				Params:     map[string]string{"execution": testCase.execution},
				Request: api.Request{
					Input: json.RawMessage(jsonBody(t, map[string]any{
						"action_id": testCase.action,
						"stream":    "stdout",
					})),
				},
			})
			if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
				t.Fatalf("%s error = %v; want not_found", testCase.name, err)
			}
			if logs.calls != 0 {
				t.Fatalf("%s reached the log reader %d time(s); want 0", testCase.name, logs.calls)
			}
		})
	}

	// Positive control: the owning project's engineer reaches the log reader.
	if _, err := router.Invoke(context.Background(), ops.Invocation{
		Operation:  "execution.logs.read",
		Credential: testToken,
		Params:     map[string]string{"execution": start.ExecutionID},
		Request: api.Request{
			Input: json.RawMessage(jsonBody(t, map[string]any{
				"action_id": actionID,
				"stream":    "stdout",
			})),
		},
	}); err != nil {
		t.Fatalf("own execution.logs.read failed: %v", err)
	}
	if logs.calls != 1 {
		t.Fatalf("own execution.logs.read log reader calls = %d; want 1", logs.calls)
	}
}
