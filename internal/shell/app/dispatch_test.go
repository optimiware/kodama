package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

func TestDispatchGateBlocksNewDispatchUntilEnabled(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.dispatchDisabled = true
	app := environment.startApp(t)
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")

	_, result, publicError := restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/build", "dev-project-token",
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "gate-apply")
	if publicError.Code != "" {
		t.Fatalf("pipeline.apply error=%+v", publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	_, result, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", "dev-project-token",
		api.TaskCreateAndStartRequest{Title: "gated", PipelineID: applied.PipelineID}, nil, "gate-start")
	if publicError.Code != "" {
		t.Fatalf("task.create_and_start error=%+v", publicError)
	}
	started := decodeBody[api.TaskCreateAndStartResult](t, result)

	// The gate is closed: the execution stays queued and no workload starts.
	time.Sleep(50 * time.Millisecond)
	_, result, _ = restCall(t, handler, http.MethodGet,
		"/api/v1/executions/"+started.ExecutionID, "dev-project-token", api.ExecutionInspectRequest{}, nil, "")
	var inspected api.ExecutionInspectResult
	if err := json.Unmarshal(result.Body, &inspected); err != nil {
		t.Fatalf("decode inspect: %v", err)
	}
	if inspected.State != "queued" {
		t.Fatalf("gated execution state = %q; want queued", inspected.State)
	}
	if environment.runtime.startCount() != 0 {
		t.Fatal("gated execution started a workload")
	}

	if err := app.DispatchGate().EnableDispatch(context.Background()); err != nil {
		t.Fatalf("EnableDispatch: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		_, result, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, "dev-project-token", api.ExecutionInspectRequest{}, nil, "")
		var view api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &view); err != nil {
			return false
		}
		return view.State == "succeeded"
	})
}
