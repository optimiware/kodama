//go:build integration

package composition

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestTransportParityAcrossOperationFamilies proves the REST and MCP edges are
// the same operation boundary: one representative call per operation family
// yields the same logical result, a shared receipt replays the original result
// over the other transport, and a refusal carries the same public code.
func TestTransportParityAcrossOperationFamilies(t *testing.T) {
	harness := newHarness(t)
	runtime := newFakeRuntime(modeComplete)
	environment := harness.start(runtime)

	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")
	started := environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "parity task",
		PipelineID: pipelineID,
	}, nil, "parity-task-start")
	var task api.TaskCreateAndStartResult
	environment.decode(started, &task)
	environment.eventually(10*time.Second, func() bool {
		result, publicError := environment.call(projectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: task.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var inspected api.ExecutionInspectResult
		environment.decode(result, &inspected)
		return inspected.State == "succeeded"
	})

	serviceCreated := environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: "service-1",
		Roles:     []string{"operator"},
	}, nil, "service-create-parity")
	var service api.ServiceCreateResult
	environment.decode(serviceCreated, &service)
	environment.mustInvoke(projectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        projectID,
		BindingID:        "binding-parity",
		ResourceID:       "repo-parity",
		EnforcingService: "service-1",
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []string{"git.ref.update"},
		ReportableEvents: []string{"git.ref.updated"},
	}, nil, "binding-register-parity")

	secretWritten := environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  "API_TOKEN",
		Value:     secretValue,
	}, nil, "secret-write-parity")
	var secret api.SecretWriteResult
	environment.decode(secretWritten, &secret)

	t.Run("tenancy", func(t *testing.T) {
		assertReadParity(t, environment, adminToken, "membership.list", api.MembershipListRequest{ProjectID: projectID})
	})

	t.Run("resource binding", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "resource.binding.read", api.ResourceBindingReadRequest{BindingID: "binding-parity"})
	})

	t.Run("pipeline", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "pipeline.read", api.PipelineReadRequest{ProjectID: projectID, PipelineID: pipelineID})
	})

	t.Run("secret metadata", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
			ProjectID: projectID,
			SecretID:  secret.SecretID,
		})
		assertReadParity(t, environment, projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
			ProjectID: projectID,
			SecretID:  "API_TOKEN",
		})
		byIDResult := environment.mustInvoke(projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
			ProjectID: projectID,
			SecretID:  secret.SecretID,
		}, nil, "")
		var byID api.SecretMetadataReadResult
		environment.decode(byIDResult, &byID)
		byNameResult := environment.mustInvoke(projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
			ProjectID: projectID,
			SecretID:  "API_TOKEN",
		}, nil, "")
		var byName api.SecretMetadataReadResult
		environment.decode(byNameResult, &byName)
		if byName.SecretID != secret.SecretID || byName.Name != "API_TOKEN" || byName.Version != secret.Version || !byName.UpdatedAt.Equal(byID.UpdatedAt) {
			t.Fatalf("secret.metadata.read by name = %+v; want %+v", byName, byID)
		}
	})

	t.Run("service credential shared receipt", func(t *testing.T) {
		version := service.Version
		rest := environment.mustInvoke(projectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
			ServiceID:  "service-1",
			TTLSeconds: 3600,
		}, &version, "credential-issue-parity")
		var issued api.ServiceCredentialIssueResult
		environment.decode(rest, &issued)
		if issued.Token == nil {
			t.Fatal("a fresh credential issue returned no one-time token")
		}

		replay := environment.mcpMustCall(projectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
			ServiceID:  "service-1",
			TTLSeconds: 3600,
		}, &version, "credential-issue-parity")
		var replayed api.ServiceCredentialIssueResult
		environment.decode(replay, &replayed)
		if replayed.State != issued.State || replayed.Version != issued.Version {
			t.Fatalf("MCP replay = %+v; want state/version %q/%d", replayed, issued.State, issued.Version)
		}
		if replayed.Token != nil {
			t.Fatal("a replayed credential issue returned a plaintext token")
		}
	})

	t.Run("task", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID})
	})

	t.Run("inbox", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "task.inbox", api.TaskInboxRequest{ProjectID: projectID})
	})

	t.Run("execution inspect and list", func(t *testing.T) {
		assertReadParity(t, environment, projectToken, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: task.ExecutionID})
		assertReadParity(t, environment, projectToken, "execution.list", api.ExecutionListRequest{ProjectID: projectID})
	})

	t.Run("verify", func(t *testing.T) {
		request := api.VerifyRequest{
			ActorCredential: projectToken,
			ResourceID:      "repo-parity",
			Operation:       "git.ref.update",
		}
		rest := environment.mustInvoke(serviceToken, "actor.verify", request, nil, "")
		var restVerify api.VerifyResponse
		environment.decode(rest, &restVerify)
		mcp := environment.mcpMustCall(serviceToken, "actor.verify", request, nil, "")
		var mcpVerify api.VerifyResponse
		environment.decode(mcp, &mcpVerify)
		if restVerify.Principal != mcpVerify.Principal ||
			!reflect.DeepEqual(restVerify.Roles, mcpVerify.Roles) ||
			restVerify.VerificationID == "" || mcpVerify.VerificationID == "" {
			t.Fatalf("verify parity REST=%+v MCP=%+v", restVerify, mcpVerify)
		}
	})

	t.Run("event shared receipt", func(t *testing.T) {
		request := api.EventPublishRequest{
			ProjectID: projectID,
			Type:      "observation.custom",
			SubjectID: "subject-1",
			Data:      []byte(`{"note":"parity"}`),
		}
		rest := environment.mustInvoke(projectToken, "event.publish", request, nil, "event-publish-parity")
		var published api.EventPublishResult
		environment.decode(rest, &published)
		if published.Replayed {
			t.Fatal("a fresh event.publish reported Replayed")
		}
		replay := environment.mcpMustCall(projectToken, "event.publish", request, nil, "event-publish-parity")
		var replayed api.EventPublishResult
		environment.decode(replay, &replayed)
		if !replayed.Replayed || replayed.EventID != published.EventID || replayed.ReceiptID != published.ReceiptID {
			t.Fatalf("event.publish replay = %+v; want event %q receipt %q replayed", replayed, published.EventID, published.ReceiptID)
		}
	})

	t.Run("request resolve error class", func(t *testing.T) {
		request := api.ExecutionRequestResolveRequest{
			ExecutionID:           "execution-missing",
			ActionID:              "action-missing",
			ExpectedActionVersion: 1,
			Decision:              "pass",
		}
		assertErrorParity(t, environment, projectToken, "execution.request.resolve", request)
	})

	t.Run("report error class", func(t *testing.T) {
		request := api.ReportRequest{
			ResourceID:     "repo-1",
			SourceEventID:  "event-1",
			EventType:      "git.ref.updated",
			Operation:      "git.ref.update",
			VerificationID: "verification-missing",
		}
		assertErrorParity(t, environment, serviceToken, "resource.report", request)
	})

	t.Run("pipeline apply shared receipt", func(t *testing.T) {
		request := api.PipelineApplyRequest{
			ProjectID: projectID,
			Name:      "parity-build",
			YAML:      []byte(smokePipeline),
		}
		rest := environment.mustInvoke(projectToken, "pipeline.apply", request, nil, "pipeline-apply-parity")
		mcp := environment.mcpMustCall(projectToken, "pipeline.apply", request, nil, "pipeline-apply-parity")
		assertSameBody(t, rest, mcp)
	})
}

// TestMCPToolsListEqualsTheRegistry proves tools/list advertises exactly the 58
// declared routes in registry order with their declared schema titles.
func TestMCPToolsListEqualsTheRegistry(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))

	response, err := environment.mcpRaw(api.MCPMethodToolsList, nil, "")
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("tools/list refused: %+v", response.Error)
	}
	var catalog api.MCPToolsListResult
	if err := json.Unmarshal(response.Result, &catalog); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	routes := api.Routes()
	if len(catalog.Tools) != 58 || len(catalog.Tools) != len(routes) {
		t.Fatalf("tools/list returned %d tools; want 58 declared routes", len(catalog.Tools))
	}
	for index, route := range routes {
		tool := catalog.Tools[index]
		if tool.Name != route.MCPTool {
			t.Fatalf("tools/list[%d] = %q; want %q", index, tool.Name, route.MCPTool)
		}
		var schema struct {
			Type  string `json:"type"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s inputSchema: %v", tool.Name, err)
		}
		if schema.Type != "object" || schema.Title != route.InputSchema {
			t.Fatalf("%s inputSchema = %s; want object title %q", tool.Name, string(tool.InputSchema), route.InputSchema)
		}
	}
}

// assertReadParity proves one read returns the same result over both transports.
func assertReadParity(t *testing.T, environment *runningEnv, token, operation string, input any) {
	t.Helper()
	rest := environment.mustInvoke(token, operation, input, nil, "")
	mcp := environment.mcpMustCall(token, operation, input, nil, "")
	assertSameBody(t, rest, mcp)
}

// assertErrorParity proves one refusal carries the same public code over both
// transports.
func assertErrorParity(t *testing.T, environment *runningEnv, token, operation string, input any) {
	t.Helper()
	_, restError := environment.call(token, operation, input, nil, "")
	_, mcpError, err := environment.mcpCall(token, operation, input, nil, "")
	if err != nil {
		t.Fatalf("%s over MCP: %v", operation, err)
	}
	if restError.Code == "" || restError.Code != mcpError.Code {
		t.Fatalf("%s codes REST=%q MCP=%q; want one equal public code", operation, restError.Code, mcpError.Code)
	}
}

// assertSameBody proves two results name the same operation and carry an equal
// JSON body.
func assertSameBody(t *testing.T, rest, mcp api.Result) {
	t.Helper()
	if rest.Operation != mcp.Operation {
		t.Fatalf("operations REST=%q MCP=%q", rest.Operation, mcp.Operation)
	}
	var restBody, mcpBody any
	if err := json.Unmarshal(rest.Body, &restBody); err != nil {
		t.Fatalf("decode REST body: %v", err)
	}
	if err := json.Unmarshal(mcp.Body, &mcpBody); err != nil {
		t.Fatalf("decode MCP body: %v", err)
	}
	if !reflect.DeepEqual(restBody, mcpBody) {
		t.Fatalf("%s bodies differ:\nREST=%s\nMCP =%s", rest.Operation, string(rest.Body), string(mcp.Body))
	}
}
