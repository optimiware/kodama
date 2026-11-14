package opsapp_test

import (
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestModelAndToolFamily pins the optional surfaces: the consumer-owned ports
// receive the authenticated principal and the typed request, and the typed
// result is returned. The real gateway/proxy Invoke paths are covered by their
// own capability tests; here the port is the composition seam.
func TestModelAndToolFamily(t *testing.T) {
	h := newHarness(t)

	invoked, err := h.invoke(t, testToken, "model.invoke", jsonBody(t, map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}), map[string]string{"project": testProject, "model": "gpt-4"})
	if err != nil {
		t.Fatalf("model.invoke: %v", err)
	}
	var completion api.ChatCompletionResponse
	decodeResult(t, invoked, &completion)
	if completion.ID == "" || len(completion.Choices) != 1 {
		t.Fatalf("model.invoke result = %+v", completion)
	}
	if h.models.calls != 1 {
		t.Fatalf("model port calls = %d; want 1", h.models.calls)
	}

	called, err := h.invoke(t, testToken, "tool.invoke", jsonBody(t, map[string]any{
		"arguments": map[string]string{"path": "README.md"},
	}), map[string]string{"project": testProject, "tool": "tool-1"})
	if err != nil {
		t.Fatalf("tool.invoke: %v", err)
	}
	var tool api.ToolInvokeResult
	decodeResult(t, called, &tool)
	if tool.Content != `"ok"` || tool.IsError {
		t.Fatalf("tool.invoke result = %+v", tool)
	}
	if h.tools.calls != 1 {
		t.Fatalf("tool port calls = %d; want 1", h.tools.calls)
	}
}

// TestEventFamily drives the real ingress, reader and subscriber: a published
// observation becomes a committed event row that the read handler returns with
// a cursor round trip, an equal retry replays the original identities, the
// subscription grant is scope-bound, and identity/time/source/lifecycle facts
// are not client-settable.
func TestEventFamily(t *testing.T) {
	h := newHarness(t)
	projectParams := map[string]string{"project": testProject}

	first, err := h.invokeKey(t, testToken, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-1",
		"data":       []byte("first"),
	}), "publish-1", projectParams)
	if err != nil {
		t.Fatalf("event.publish: %v", err)
	}
	var published api.EventPublishResult
	decodeResult(t, first, &published)
	if published.EventID == "" || published.ReceiptID == "" || published.Replayed {
		t.Fatalf("publish result = %+v", published)
	}

	replayed, err := h.invokeKey(t, testToken, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-1",
		"data":       []byte("first"),
	}), "publish-1", projectParams)
	if err != nil {
		t.Fatalf("replayed event.publish: %v", err)
	}
	var replay api.EventPublishResult
	decodeResult(t, replayed, &replay)
	if !replay.Replayed || replay.EventID != published.EventID || replay.ReceiptID != published.ReceiptID {
		t.Fatalf("replay result = %+v; want replayed original ids", replay)
	}

	second, err := h.invokeKey(t, testToken, "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-2",
	}), "publish-2", projectParams)
	if err != nil {
		t.Fatalf("second event.publish: %v", err)
	}
	var secondPublished api.EventPublishResult
	decodeResult(t, second, &secondPublished)

	// First page: limit 1, then follow the cursor to the second event.
	pageOne, err := h.invoke(t, testToken, "event.read", jsonBody(t, map[string]any{
		"page": map[string]any{"limit": 1},
	}), projectParams)
	if err != nil {
		t.Fatalf("event.read: %v", err)
	}
	var firstPage api.EventReadResult
	decodeResult(t, pageOne, &firstPage)
	if len(firstPage.Events) != 1 || !firstPage.HasMore || firstPage.Next == "" {
		t.Fatalf("first page = %+v", firstPage)
	}
	observed := firstPage.Events[0]
	if observed.ID != published.EventID || observed.Type != "observation.custom" {
		t.Fatalf("observed event = %+v", observed)
	}
	if observed.Source.Kind != "shell" || observed.Source.PrincipalID != testEngineer {
		t.Fatalf("observed source = %+v; want server-derived shell source", observed.Source)
	}
	if observed.Scope.TenantID != testTenant || observed.Scope.ProjectID != testProject {
		t.Fatalf("observed scope = %+v", observed.Scope)
	}
	if observed.At.IsZero() || !observed.At.Equal(fixedNow) {
		t.Fatalf("observed time = %v; want the server clock", observed.At)
	}

	pageTwo, err := h.invoke(t, testToken, "event.read", jsonBody(t, map[string]any{
		"page": map[string]any{"limit": 1, "cursor": firstPage.Next},
	}), projectParams)
	if err != nil {
		t.Fatalf("event.read second page: %v", err)
	}
	var secondPage api.EventReadResult
	decodeResult(t, pageTwo, &secondPage)
	if len(secondPage.Events) != 1 || secondPage.Events[0].ID != secondPublished.EventID {
		t.Fatalf("second page = %+v", secondPage)
	}
	if secondPage.HasMore {
		t.Fatalf("second page unexpectedly has more")
	}

	// A cursor beyond the signed sequence range is refused as invalid input.
	overflow, err := api.EncodeCursor("9223372036854775808")
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	_, err = h.invoke(t, testToken, "event.read", jsonBody(t, map[string]any{
		"page": map[string]any{"cursor": overflow},
	}), projectParams)
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("event.read overflowing cursor err = %v; want invalid_input", err)
	}

	// The subscription grant names the scope-qualified subject and resume point.
	subscribed, err := h.invoke(t, testToken, "event.subscribe", `{}`, projectParams)
	if err != nil {
		t.Fatalf("event.subscribe: %v", err)
	}
	var grant api.EventSubscribeResult
	decodeResult(t, subscribed, &grant)
	if len(grant.Subjects) != 1 || grant.Subjects[0] != "kodama.v1."+testTenant+"."+testProject+".events" {
		t.Fatalf("subscribe subjects = %+v", grant.Subjects)
	}
	if grant.Cursor == "" || grant.ExpiresAt.IsZero() {
		t.Fatalf("subscribe result = %+v", grant)
	}

	// Identity, time, source and lifecycle types are not client-settable.
	for name, input := range map[string]string{
		"lifecycle type":  `{"type":"execution.succeeded","subject_id":"subject-1"}`,
		"client identity": `{"id":"event-x","type":"observation.custom","subject_id":"subject-1"}`,
		"client time":     `{"at":"2026-01-01T00:00:00Z","type":"observation.custom","subject_id":"subject-1"}`,
		"client source":   `{"source":{"kind":"shell","principal_id":"x"},"type":"observation.custom","subject_id":"subject-1"}`,
	} {
		if _, err := h.invokeKey(t, testToken, "event.publish", input, "bad-"+name, projectParams); err == nil {
			t.Fatalf("event.publish accepted a client-settable %s", name)
		}
	}
}

// TestVerifyAndReportFamily drives the real verifier and reporter through one
// binding and one committed verification.
func TestVerifyAndReportFamily(t *testing.T) {
	h := newHarness(t)
	h.seedResource(t, tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, "resource-1", 1)

	// The calling service account and its credential mapping.
	if _, err := h.invoke(t, testToken, "service.create", `{"service_id":"service-1","roles":["engineer"]}`, map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create: %v", err)
	}
	h.principals.principals["token-service"] = identity.Principal{
		ID:      "service-1",
		Kind:    identity.KindService,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}

	if _, err := h.invoke(t, testToken, "resource.binding.register", jsonBody(t, map[string]any{
		"binding_id":        "binding-1",
		"resource_id":       "resource-1",
		"enforcing_service": "service-1",
		"trusted_origins":   []string{"https://example.test"},
		"operations":        []string{"git.ref.update"},
		"reportable_events": []string{"git.ref.updated"},
	}), map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register: %v", err)
	}

	verified, err := h.invoke(t, "token-service", "actor.verify", jsonBody(t, map[string]any{
		"actor_credential": "actor-engineer",
		"resource_id":      "resource-1",
		"operation":        "git.ref.update",
		"subject": map[string]any{"fields": []map[string]string{
			{"name": "operation", "kind": "string", "value": "git.ref.update"},
			{"name": "resource", "kind": "string", "value": "resource-1"},
		}},
	}), nil)
	if err != nil {
		t.Fatalf("actor.verify: %v", err)
	}
	var verification api.VerifyResponse
	decodeResult(t, verified, &verification)
	if verification.Principal.ID != testEngineer || verification.VerificationID == "" {
		t.Fatalf("actor.verify result = %+v", verification)
	}

	reported, err := h.invoke(t, "token-service", "resource.report", jsonBody(t, map[string]any{
		"resource_id":     "resource-1",
		"source_event_id": "source-event-1",
		"event_type":      "git.ref.updated",
		"operation":       "git.ref.update",
		"verification_id": verification.VerificationID,
		"subject": map[string]any{"fields": []map[string]string{
			{"name": "operation", "kind": "string", "value": "git.ref.update"},
			{"name": "resource", "kind": "string", "value": "resource-1"},
		}},
	}), nil)
	if err != nil {
		t.Fatalf("resource.report: %v", err)
	}
	var report api.ReportResponse
	decodeResult(t, reported, &report)
	if report.EventID == "" || report.VerificationID != verification.VerificationID {
		t.Fatalf("resource.report result = %+v", report)
	}
	_ = json.RawMessage(nil)
}
