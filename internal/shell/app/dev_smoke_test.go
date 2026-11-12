package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

const smokeImage = "registry.example.com/kodama/base@sha256:cae662172fd450bb0cd710a769079c05bfc5d8e35efa6576edc7d0377afdd4a2"

const smokePipeline = `actions:
  - name: inspect
    image: ` + smokeImage + `
    commands:
      - echo "plain task-free work completed"
`

func TestDevelopmentCompositionBootstrapToFirstTask(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	handler := app.Handler()
	tenantToken := "dev-admin-token"
	projectToken := "dev-project-token"

	// Project creation through the REST edge.
	recorder, result, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/tenants/tenant-1/projects", tenantToken,
		api.ProjectCreateRequest{ProjectID: "project-2", Name: "Project Two"}, nil, "project-create-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("project.create status=%d error=%+v", recorder.Code, publicError)
	}
	created := decodeBody[api.ProjectCreateResult](t, result)
	if created.ProjectID != "project-2" || created.Version == 0 {
		t.Fatalf("project.create result = %+v", created)
	}

	// Membership grant through the REST edge adds the Engineer role the
	// pipeline and task operations require.
	recorder, _, publicError = restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/memberships/admin-1", tenantToken,
		api.MembershipGrantRequest{Roles: []string{"admin", "engineer"}}, nil, "membership-grant-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("membership.grant status=%d error=%+v", recorder.Code, publicError)
	}

	// Pipeline apply through the REST edge.
	recorder, result, publicError = restCall(t, handler, http.MethodPut,
		"/api/v1/projects/project-1/pipelines/build", projectToken,
		api.PipelineApplyRequest{Name: "build", YAML: []byte(smokePipeline)}, nil, "pipeline-apply-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("pipeline.apply status=%d error=%+v", recorder.Code, publicError)
	}
	applied := decodeBody[api.PipelineApplyResult](t, result)
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply result = %+v", applied)
	}

	// Task creation and start through the REST edge.
	recorder, result, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tasks/create-and-start", projectToken,
		api.TaskCreateAndStartRequest{Title: "smoke", PipelineID: applied.PipelineID}, nil, "task-start-1")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("task.create_and_start status=%d error=%+v", recorder.Code, publicError)
	}
	started := decodeBody[api.TaskCreateAndStartResult](t, result)
	if started.TaskID == "" || started.ExecutionID == "" {
		t.Fatalf("task.create_and_start result = %+v", started)
	}

	// The Kernel reconcile loop drives the queued execution with the fake
	// process executor to success without any test-owned goroutine.
	eventually(t, 5*time.Second, func() bool {
		_, result, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, projectToken, nil, nil, "")
		var inspected api.ExecutionInspectResult
		if err := json.Unmarshal(result.Body, &inspected); err != nil {
			return false
		}
		return inspected.State == "succeeded"
	})
	if environment.runtime.startCount() == 0 {
		t.Fatal("the fake process executor never started the admitted action")
	}

	// Events are committed and readable through the authorized event read.
	recorder, result, publicError = restCall(t, handler, http.MethodGet,
		"/api/v1/projects/project-1/events", projectToken, nil, nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("event.read status=%d error=%+v", recorder.Code, publicError)
	}
	events := decodeBody[api.EventReadResult](t, result)
	if len(events.Events) == 0 {
		t.Fatal("the composition committed no events")
	}
	if events.Next == "" {
		t.Fatal("event.read returned no continuation cursor")
	}
}

func TestMCPToolsListMatchesRegistry(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("tools/list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	routes := api.Routes()
	if len(response.Result.Tools) != len(routes) {
		t.Fatalf("tools/list returned %d tools; want %d", len(response.Result.Tools), len(routes))
	}
	for index, route := range routes {
		if response.Result.Tools[index].Name != route.Operation {
			t.Fatalf("tool %d = %q; want %q", index, response.Result.Tools[index].Name, route.Operation)
		}
	}
	_ = mcp.ToolCatalog()
}

func TestDisabledOptionalCapabilitiesFailClosed(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	handler := app.Handler()
	token := "dev-project-token"

	recorder, _, publicError := restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/models/m/invoke", token,
		api.ModelInvokeRequest{Model: "m", Messages: []api.ChatMessage{{Role: "user", Content: "hi"}}}, nil, "")
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the disabled model gateway succeeded")
	}
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("model.invoke refusal = %q; want not_found", publicError.Code)
	}

	recorder, _, publicError = restCall(t, handler, http.MethodPost,
		"/api/v1/projects/project-1/tools/search/invoke", token,
		api.ToolInvokeRequest{Tool: "search", Arguments: json.RawMessage(`{}`)}, nil, "")
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the disabled MCP tool proxy succeeded")
	}
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("tool.invoke refusal = %q; want not_found", publicError.Code)
	}
}

func TestCompositionNeverLogsCredentials(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.Dev.Tokens[0].Credential = "top-secret-development-token"
	app := environment.startApp(t)
	handler := app.Handler()

	// A refused authentication must not echo the presented credential.
	recorder, _, _ := restCall(t, handler, http.MethodGet,
		"/api/v1/projects/project-1/events", "leaked-credential-value", nil, nil, "")
	if strings.Contains(recorder.Body.String(), "leaked-credential-value") {
		t.Fatalf("refusal rendered the presented credential: %s", recorder.Body.String())
	}
	if strings.Contains(environment.logs.String(), "top-secret-development-token") ||
		strings.Contains(environment.logs.String(), "leaked-credential-value") {
		t.Fatalf("composition logs contain credential material: %s", environment.logs.String())
	}
}
