package opsapp_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// jsonBody marshals one request input.
func jsonBody(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return string(encoded)
}

const pinnedImage = "registry.example.com/run@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestResourceBindingFamily drives the real resource-binding service.
func TestResourceBindingFamily(t *testing.T) {
	h := newHarness(t)
	h.seedResource(t, tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, "resource-1", 1)

	registered, err := h.invoke(t, testToken, "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "binding-1",
		"resource_id":       "resource-1",
		"enforcing_service": "service-1",
		"trusted_origins":   []string{"https://example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
		"metadata":          map[string]string{"team": "platform"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("resource.binding.register: %v", err)
	}
	var binding api.ResourceBindingRegisterResult
	decodeResult(t, registered, &binding)
	if binding.BindingID != "binding-1" || binding.Version != 1 {
		t.Fatalf("register result = %+v", binding)
	}

	read, err := h.invoke(t, testToken, "resource.binding.read", `{}`, map[string]string{"binding": "binding-1"})
	if err != nil {
		t.Fatalf("resource.binding.read: %v", err)
	}
	var observed api.ResourceBindingReadResult
	decodeResult(t, read, &observed)
	if observed.ResourceID != "resource-1" || observed.EnforcingService != "service-1" || observed.Version != 1 {
		t.Fatalf("read result = %+v", observed)
	}
	if observed.Metadata["team"] != "platform" {
		t.Fatalf("read metadata = %+v", observed.Metadata)
	}

	updated, err := h.invokeVersion(t, testToken, "resource.binding.update", jsonBody(t, map[string]any{
		"resource_id":       "resource-1",
		"enforcing_service": "service-1",
		"trusted_origins":   []string{"https://example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
		"metadata":          map[string]string{"team": "infra"},
	}), 1, map[string]string{"binding": "binding-1"})
	if err != nil {
		t.Fatalf("resource.binding.update: %v", err)
	}
	var update api.ResourceBindingUpdateResult
	decodeResult(t, updated, &update)
	if update.Version != 2 {
		t.Fatalf("update version = %d; want 2", update.Version)
	}

	deleted, err := h.invokeVersion(t, testToken, "resource.binding.delete", `{}`, 2, map[string]string{"binding": "binding-1"})
	if err != nil {
		t.Fatalf("resource.binding.delete: %v", err)
	}
	var deletion api.ResourceBindingDeleteResult
	decodeResult(t, deleted, &deletion)
	if deletion.Version != 3 {
		t.Fatalf("delete version = %d; want 3", deletion.Version)
	}
}

// TestPipelineFamily drives the real pipeline management and admission
// services, including the admit-then-start chain.
func TestPipelineFamily(t *testing.T) {
	h := newHarness(t)
	yaml := []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo ok\n")
	projectParams := map[string]string{"project": testProject}

	applied, err := h.invoke(t, testToken, "pipeline.apply", jsonBody(t, map[string]any{
		"yaml": base64.StdEncoding.EncodeToString(yaml),
	}), map[string]string{"project": testProject, "pipeline": "build.v2"})
	if err != nil {
		t.Fatalf("pipeline.apply: %v", err)
	}
	var appliedResult api.PipelineApplyResult
	decodeResult(t, applied, &appliedResult)
	if appliedResult.PipelineID == "" || appliedResult.Version != 1 {
		t.Fatalf("apply result = %+v", appliedResult)
	}
	pipelineParams := map[string]string{"project": testProject, "pipeline": appliedResult.PipelineID}

	read, err := h.invoke(t, testToken, "pipeline.read", `{}`, pipelineParams)
	if err != nil {
		t.Fatalf("pipeline.read: %v", err)
	}
	var readResult api.PipelineReadResult
	decodeResult(t, read, &readResult)
	if readResult.Name != "build.v2" || readResult.Version != 1 {
		t.Fatalf("read result = %+v", readResult)
	}

	updatedYAML := []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo updated\n")
	updated, err := h.invokeVersion(t, testToken, "pipeline.update", jsonBody(t, map[string]any{
		"name": "build.v2",
		"yaml": base64.StdEncoding.EncodeToString(updatedYAML),
	}), 1, pipelineParams)
	if err != nil {
		t.Fatalf("pipeline.update: %v", err)
	}
	var updateResult api.PipelineUpdateResult
	decodeResult(t, updated, &updateResult)
	if updateResult.Version != 2 {
		t.Fatalf("update version = %d; want 2", updateResult.Version)
	}

	started, err := h.invokeVersion(t, testToken, "pipeline.start", `{"inputs":{}}`, 2, pipelineParams)
	if err != nil {
		t.Fatalf("pipeline.start: %v", err)
	}
	var startResult api.PipelineStartResult
	decodeResult(t, started, &startResult)
	if startResult.ExecutionID == "" || startResult.Version != 1 {
		t.Fatalf("start result = %+v", startResult)
	}

	// The admitted execution is readable through the real execution service.
	inspect, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": startResult.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect: %v", err)
	}
	var view api.ExecutionInspectResult
	decodeResult(t, inspect, &view)
	if view.ExecutionID != startResult.ExecutionID || view.PipelineID != appliedResult.PipelineID {
		t.Fatalf("inspect result = %+v", view)
	}

	deleted, err := h.invokeVersion(t, testToken, "pipeline.delete", `{}`, 2, pipelineParams)
	if err != nil {
		t.Fatalf("pipeline.delete: %v", err)
	}
	var deleteResult api.PipelineDeleteResult
	decodeResult(t, deleted, &deleteResult)
	if deleteResult.Version != 3 {
		t.Fatalf("delete version = %d; want 3", deleteResult.Version)
	}
	_ = projectParams
}
