//go:build integration

package composition

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestCompositionNeverRendersCredentialsOrSecretValues scans the captured logs,
// every refusal body and every committed event across one composed run. No dev
// token, service token, one-time issued credential or written secret value may
// appear in them.
func TestCompositionNeverRendersCredentialsOrSecretValues(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)
	pipelineID := environment.applyPipeline("build")

	var captured [][]byte
	capture := func(status int, body []byte) {
		if status == api.HTTPStatusSuccess {
			return
		}
		captured = append(captured, body)
	}

	// Authentication refusals.
	eventsPath := "/api/v1/projects/" + projectID + "/events"
	for _, token := range []string{"", forgedToken, expiredToken} {
		status, body := environment.restRaw(http.MethodGet, eventsPath, token, api.Request{Input: json.RawMessage(`{}`)})
		capture(status, body)
	}

	// Optional capability refusals.
	status, body := environment.restRaw(http.MethodPost, "/api/v1/projects/"+projectID+"/models/m/invoke", projectToken,
		api.Request{Input: encodeInput(t, api.ModelInvokeRequest{
			ProjectID: projectID,
			Model:     "m",
			Messages:  []api.ChatMessage{{Role: "user", Content: "hi"}},
		})})
	capture(status, body)
	status, body = environment.restRaw(http.MethodPost, "/api/v1/projects/"+projectID+"/tools/search/invoke", projectToken,
		api.Request{Input: encodeInput(t, api.ToolInvokeRequest{
			ProjectID: projectID,
			Tool:      "search",
			Arguments: json.RawMessage(`{}`),
		})})
	capture(status, body)

	// A secret write and metadata read (by id and by scoped uppercase name),
	// then a one-time service credential.
	written := environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  "SCAN_TOKEN",
		Value:     secretValue,
	}, nil, "scan-secret-write")
	captured = append(captured, written.Body)
	var stored api.SecretWriteResult
	environment.decode(written, &stored)
	if stored.Name != "SCAN_TOKEN" || stored.SecretID == "" || stored.Version == 0 {
		t.Fatalf("secret.write result = %+v; want SCAN_TOKEN metadata", stored)
	}
	byIDResult := environment.mustInvoke(projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
		ProjectID: projectID,
		SecretID:  stored.SecretID,
	}, nil, "")
	captured = append(captured, byIDResult.Body)
	var byID api.SecretMetadataReadResult
	environment.decode(byIDResult, &byID)
	if byID.SecretID != stored.SecretID || byID.Name != "SCAN_TOKEN" || byID.Version != stored.Version || byID.UpdatedAt.IsZero() {
		t.Fatalf("secret.metadata.read by id = %+v; want %+v with non-zero updated_at", byID, stored)
	}
	byNameResult := environment.mustInvoke(projectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
		ProjectID: projectID,
		SecretID:  "SCAN_TOKEN",
	}, nil, "")
	captured = append(captured, byNameResult.Body)
	var byName api.SecretMetadataReadResult
	environment.decode(byNameResult, &byName)
	if byName.SecretID != stored.SecretID || byName.Name != "SCAN_TOKEN" || byName.Version != stored.Version || !byName.UpdatedAt.Equal(byID.UpdatedAt) {
		t.Fatalf("secret.metadata.read by name = %+v; want %+v", byName, byID)
	}
	environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: "scan-service",
		Roles:     []string{"operator"},
	}, nil, "scan-service-create")
	issueVersion := uint64(1)
	issuedResult := environment.mustInvoke(projectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  "scan-service",
		TTLSeconds: 3600,
	}, &issueVersion, "scan-credential-issue")
	var issued api.ServiceCredentialIssueResult
	environment.decode(issuedResult, &issued)

	// A normal task flow plus a custom event.
	environment.mustInvoke(projectToken, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  projectID,
		Title:      "scan task",
		PipelineID: pipelineID,
	}, nil, "scan-task-start")
	environment.mustInvoke(projectToken, "event.publish", api.EventPublishRequest{
		ProjectID: projectID,
		Type:      "observation.custom",
		SubjectID: "scan-subject",
		Data:      []byte(`{"note":"scan"}`),
	}, nil, "scan-event-publish")

	// Every committed event envelope, including decoded payload bytes.
	result := environment.mustInvoke(projectToken, "event.read", api.EventReadRequest{
		ProjectID: projectID,
		Page:      &api.Page{Limit: 200},
	}, nil, "")
	var page api.EventReadResult
	environment.decode(result, &page)
	for _, envelope := range page.Events {
		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		captured = append(captured, encoded)
		if len(envelope.Data) != 0 {
			captured = append(captured, envelope.Data)
		}
	}

	forbidden := []string{
		adminToken, projectToken, otherToken, foreignToken, expiredToken, forgedToken,
		serviceToken, operatorToken, viewerToken, tenantOperatorToken, secretValue,
	}
	if issued.Token != nil {
		forbidden = append(forbidden, *issued.Token)
	}

	logs := harness.logs.String()
	for _, secret := range forbidden {
		if secret == "" {
			continue
		}
		if bytes.Contains([]byte(logs), []byte(secret)) {
			t.Fatalf("captured logs contain credential or secret material %q", secret)
		}
		for index, body := range captured {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("captured body %d contains credential or secret material %q: %s", index, secret, string(body))
			}
		}
	}
}
