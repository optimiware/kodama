package opsapp_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// invokeKeyVersion invokes one operation with an explicit idempotency key and
// expected version.
func invokeKeyVersion(
	t *testing.T,
	h *harness,
	credential, operation, input, key string,
	version uint64,
	params map[string]string,
) (api.Result, error) {
	t.Helper()
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request: api.Request{
			Input:           json.RawMessage(input),
			ExpectedVersion: &version,
			IdempotencyKey:  key,
		},
		Params: params,
	})
}

// applyPipelineWithKey applies one immutable pipeline definition under an
// explicit idempotency key and returns its id.
func applyPipelineWithKey(t *testing.T, h *harness, name, key string) string {
	t.Helper()
	yaml := []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo ok\n")
	applied, err := h.invokeKey(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(yaml),
	}), key, map[string]string{"project": testProject, "pipeline": name})
	if err != nil {
		t.Fatalf("pipeline.apply %s: %v", name, err)
	}
	var result api.PipelineApplyResult
	decodeResult(t, applied, &result)
	return result.PipelineID
}

// startPipeline starts one pipeline at its currently observed stored version
// and returns its execution id.
func startPipeline(t *testing.T, h *harness, pipelineID, key string) string {
	t.Helper()
	read, err := h.invoke(t, testToken, "pipeline.read", `{}`, map[string]string{
		"project":  testProject,
		"pipeline": pipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.read %s: %v", pipelineID, err)
	}
	var definition api.PipelineReadResult
	decodeResult(t, read, &definition)
	if definition.Version == 0 {
		t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	started, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, key, definition.Version, map[string]string{
		"project":  testProject,
		"pipeline": pipelineID,
	})
	if err != nil {
		t.Fatalf("pipeline.start %s: %v", pipelineID, err)
	}
	var result api.PipelineStartResult
	decodeResult(t, started, &result)
	return result.ExecutionID
}

// TestIdempotencyContentCoversRouteTargets pins that the receipt fingerprint of
// a receipt-bearing operation covers the route-derived target, not only the
// body. Reusing one key against a different target must refuse as changed
// idempotency content and apply no second effect.
func TestIdempotencyContentCoversRouteTargets(t *testing.T) {
	t.Run("execution cancel", func(t *testing.T) {
		h := newHarness(t)
		pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
		first := startPipeline(t, h, pipelineID, "start-one")
		second := startPipeline(t, h, pipelineID, "start-two")

		if _, err := invokeKeyVersion(t, h, testToken, "execution.cancel", `{}`, "shared-cancel", 1, map[string]string{"execution": first}); err != nil {
			t.Fatalf("first execution.cancel: %v", err)
		}
		_, err := invokeKeyVersion(t, h, testToken, "execution.cancel", `{}`, "shared-cancel", 1, map[string]string{"execution": second})
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("second execution.cancel = %v; want changed_idempotency_content", err)
		}
		inspected, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": second})
		if err != nil {
			t.Fatalf("execution.inspect: %v", err)
		}
		var view api.ExecutionInspectResult
		decodeResult(t, inspected, &view)
		if view.State != "queued" {
			t.Fatalf("second execution state = %q; want queued (no effect)", view.State)
		}
	})

	t.Run("secret write", func(t *testing.T) {
		h := newHarness(t)
		written, err := h.invokeKey(t, testToken, "secret.write", `{"value":"same-value"}`, "shared-secret", map[string]string{
			"project": testProject,
			"secret":  "SECRET_A",
		})
		if err != nil {
			t.Fatalf("first secret.write: %v", err)
		}
		var first api.SecretWriteResult
		decodeResult(t, written, &first)
		_, err = h.invokeKey(t, testToken, "secret.write", `{"value":"same-value"}`, "shared-secret", map[string]string{
			"project": testProject,
			"secret":  "SECRET_B",
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("second secret.write = %v; want changed_idempotency_content", err)
		}
		if rows := secretRows(t, h, "SECRET_B"); len(rows) != 0 {
			t.Fatalf("second secret.write applied an effect: %+v", rows)
		}
	})

	t.Run("task start", func(t *testing.T) {
		h := newHarness(t)
		pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
		firstTask := createTask(t, h, "task-one", pipelineID, "create-one")
		secondTask := createTask(t, h, "task-two", pipelineID, "create-two")

		if _, err := invokeKeyVersion(t, h, testToken, "task.start", `{}`, "shared-start", 1, map[string]string{"task": firstTask}); err != nil {
			t.Fatalf("first task.start: %v", err)
		}
		_, err := invokeKeyVersion(t, h, testToken, "task.start", `{}`, "shared-start", 1, map[string]string{"task": secondTask})
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("second task.start = %v; want changed_idempotency_content", err)
		}
		read, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": secondTask})
		if err != nil {
			t.Fatalf("task.read: %v", err)
		}
		var task api.TaskReadResult
		decodeResult(t, read, &task)
		if task.CurrentExecution != "" {
			t.Fatalf("second task.start applied an execution: %q", task.CurrentExecution)
		}
	})

	t.Run("pipeline start", func(t *testing.T) {
		h := newHarness(t)
		firstPipeline := applyPipelineWithKey(t, h, "build.one", "apply-one")
		secondPipeline := applyPipelineWithKey(t, h, "build.two", "apply-two")
		startPipeline(t, h, firstPipeline, "shared-pipeline-start")
		_, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "shared-pipeline-start", 1, map[string]string{
			"project":  testProject,
			"pipeline": secondPipeline,
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
			t.Fatalf("second pipeline.start = %v; want changed_idempotency_content", err)
		}
		listed, err := h.invoke(t, testToken, "execution.list", `{}`, map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("execution.list: %v", err)
		}
		var executions api.ExecutionListResult
		decodeResult(t, listed, &executions)
		if len(executions.Executions) != 1 {
			t.Fatalf("executions = %d; want 1 (second start applied no effect)", len(executions.Executions))
		}
	})
}

// TestRESTAndMCPReplayCanonicalRequest pins that one logical request delivered
// over REST (route fields) and over MCP (input fields) fingerprints equal, so
// the second edge replays the first result instead of refusing changed content.
func TestRESTAndMCPReplayCanonicalRequest(t *testing.T) {
	h := newHarness(t)

	rest, err := h.invokeKey(t, testToken, "secret.write", `{"value":"parity-value"}`, "parity-secret", map[string]string{
		"project": testProject,
		"secret":  "PARITY",
	})
	if err != nil {
		t.Fatalf("REST secret.write: %v", err)
	}
	var restResult api.SecretWriteResult
	decodeResult(t, rest, &restResult)

	mcp, err := h.invokeKey(t, testToken, "secret.write", jsonBody(t, map[string]any{
		"project_id": testProject,
		"secret_id":  "PARITY",
		"value":      "parity-value",
	}), "parity-secret", nil)
	if err != nil {
		t.Fatalf("MCP secret.write replay: %v", err)
	}
	var mcpResult api.SecretWriteResult
	decodeResult(t, mcp, &mcpResult)
	if mcpResult.SecretID != restResult.SecretID {
		t.Fatalf("MCP replay secret id = %q; want %q", mcpResult.SecretID, restResult.SecretID)
	}
	if rows := secretRows(t, h, "PARITY"); len(rows) != 1 {
		t.Fatalf("secret rows after REST/MCP replay = %d; want 1", len(rows))
	}

	// A route-targeted start must also replay across edges.
	pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
	restStart := startPipeline(t, h, pipelineID, "parity-start")
	mcpStart, err := invokeKeyVersion(t, h, testToken, "pipeline.start", jsonBody(t, map[string]any{
		"project_id":  testProject,
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), "parity-start", 1, nil)
	if err != nil {
		t.Fatalf("MCP pipeline.start replay: %v", err)
	}
	var replayed api.PipelineStartResult
	decodeResult(t, mcpStart, &replayed)
	if replayed.ExecutionID != restStart {
		t.Fatalf("MCP replay execution id = %q; want %q", replayed.ExecutionID, restStart)
	}
}

// createTask creates one open task with a selected pipeline.
func createTask(t *testing.T, h *harness, title, pipelineID, key string) string {
	t.Helper()
	created, err := h.invokeKey(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":       title,
		"pipeline_id": pipelineID,
		"inputs":      map[string]string{},
	}), key, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create %s: %v", title, err)
	}
	var result api.TaskCreateResult
	decodeResult(t, created, &result)
	return result.TaskID
}

// secretRows returns the committed secret rows of one name in the test project.
func secretRows(t *testing.T, h *harness, name string) []store.Row {
	t.Helper()
	page, err := h.store.Query(context.Background(), tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, store.ScanQuery{
		Table:  "secrets",
		Equals: map[string]any{"name": name},
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("query secrets %s: %v", name, err)
	}
	return page.Rows
}
