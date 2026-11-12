//go:build integration

package composition

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestIdempotencyReplayChangedContentAndNoDuplicateExecution proves the shared
// receipt boundary: an equal retry replays the committed result, a changed
// retry refuses, a lost response never duplicates an execution, and a
// route/body conflict is refused before any effect.
func TestIdempotencyReplayChangedContentAndNoDuplicateExecution(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeComplete)
	environment := harness.start(runtime)
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	// Equal replay over the other transport returns the original result.
	create := api.TaskCreateRequest{ProjectID: projectID, Title: "idempotent task"}
	rest := environment.mustInvoke(projectToken, "task.create", create, nil, "idem-task-1")
	var created api.TaskCreateResult
	environment.decode(rest, &created)
	replay := environment.mcpMustCall(projectToken, "task.create", create, nil, "idem-task-1")
	var replayed api.TaskCreateResult
	environment.decode(replay, &replayed)
	if replayed.TaskID != created.TaskID {
		t.Fatalf("equal replay task = %q; want %q", replayed.TaskID, created.TaskID)
	}

	// Changed content under the same key refuses with the declared code on both
	// transports.
	changed := api.TaskCreateRequest{ProjectID: projectID, Title: "changed task"}
	_, changedRest := environment.call(projectToken, "task.create", changed, nil, "idem-task-1")
	if changedRest.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed REST replay code = %q; want %q", changedRest.Code, api.CodeChangedIdempotencyContent)
	}
	_, changedMCP, err := environment.mcpCall(projectToken, "task.create", changed, nil, "idem-task-1")
	if err != nil {
		t.Fatalf("changed MCP replay: %v", err)
	}
	if changedMCP.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed MCP replay code = %q; want %q", changedMCP.Code, api.CodeChangedIdempotencyContent)
	}

	// A lost response is retried with the same key: the committed execution is
	// replayed instead of launching a second one.
	startRequest := api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "lost response task",
		PipelineID: pipelineID,
	}
	first := environment.mustInvoke(projectToken, "task.create_and_start", startRequest, nil, "idem-start-1")
	var started api.TaskCreateAndStartResult
	environment.decode(first, &started)
	retried := environment.mcpMustCall(projectToken, "task.create_and_start", startRequest, nil, "idem-start-1")
	var retriedStart api.TaskCreateAndStartResult
	environment.decode(retried, &retriedStart)
	if retriedStart.TaskID != started.TaskID || retriedStart.ExecutionID != started.ExecutionID {
		t.Fatalf("lost-response retry = %+v; want the original %+v", retriedStart, started)
	}

	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.list", api.ExecutionListRequest{
			ProjectID: projectID,
			TaskID:    started.TaskID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var listed api.ExecutionListResult
		environment.decode(result, &listed)
		return len(listed.Executions) == 1 && listed.Executions[0].ExecutionID == started.ExecutionID
	})
	environment.eventually(10*time.Second, func() bool { return runtime.startCount() >= 1 })
	if runtime.startCount() != 1 {
		t.Fatalf("fake runtime starts = %d; want one execution launch", runtime.startCount())
	}

	// A body value that conflicts with the REST route parameter refuses before
	// any effect; the path names pipeline "build" while the body names "other".
	conflicting := api.PipelineApplyRequest{
		ProjectID: projectID,
		Name:      "other",
		YAML:      []byte(smokePipeline),
	}
	status, body := environment.restRaw(http.MethodPut, "/api/v1/projects/"+projectID+"/pipelines/build", projectToken,
		api.Request{IdempotencyKey: "route-conflict-1", Input: encodeInput(t, conflicting)})
	if status != http.StatusBadRequest {
		t.Fatalf("route/body conflict status = %d; want 400 (body=%s)", status, string(body))
	}
	var conflict api.Error
	if err := json.Unmarshal(body, &conflict); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if conflict.Code != api.CodeInvalidInput {
		t.Fatalf("route/body conflict code = %q; want %q", conflict.Code, api.CodeInvalidInput)
	}
}

// TestForeignTenantAndProjectRefusedUniformlyOverMCP proves a cross-scope MCP
// call never discloses whether the target exists: both a foreign tenant and a
// foreign project inside the same tenant refuse with not_found.
func TestForeignTenantAndProjectRefusedUniformlyOverMCP(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	_, foreignTenant, err := environment.mcpCall(foreignToken, "event.read", api.EventReadRequest{
		ProjectID: projectID,
	}, nil, "")
	if err != nil {
		t.Fatalf("foreign-tenant MCP read: %v", err)
	}
	if foreignTenant.Code != api.CodeNotFound {
		t.Fatalf("foreign-tenant MCP code = %q; want %q", foreignTenant.Code, api.CodeNotFound)
	}

	_, foreignProject, err := environment.mcpCall(projectToken, "event.read", api.EventReadRequest{
		ProjectID: otherID,
	}, nil, "")
	if err != nil {
		t.Fatalf("foreign-project MCP read: %v", err)
	}
	if foreignProject.Code != api.CodeNotFound {
		t.Fatalf("foreign-project MCP code = %q; want %q", foreignProject.Code, api.CodeNotFound)
	}
}
