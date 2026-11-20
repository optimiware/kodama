//go:build integration

package composition

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/app"
	"github.com/optimiware/kodema/internal/shell/secret"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
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

// TestProjectSecretMaterializationAndPolicyInvariants verifies the full
// composition behavior for project secret materialization (matching kodema up's
// project-scoped allow_all rule), per-action secret isolation, specific
// unauthorized secret diagnostics, and negative security invariants.
func TestProjectSecretMaterializationAndPolicyInvariants(t *testing.T) {
	t.Run("project AllowAll materializes declared secret only in declaring action and omits it from subsequent actions", func(t *testing.T) {
		harness := newHarness(t)
		rulesPath := filepath.Join(t.TempDir(), "secret-rules.json")
		if err := os.WriteFile(rulesPath, []byte(`{
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "allow_all": true}
  ]
}`), 0o600); err != nil {
			t.Fatalf("write secret-rules.json: %v", err)
		}
		rules, err := app.LoadSecretRulesFile(rulesPath, app.ProfileTest)
		if err != nil {
			t.Fatalf("LoadSecretRulesFile: %v", err)
		}
		harness.config.SecretRules = rules

		runtime := newFakeRuntime(modeComplete)
		environment := harness.start(runtime)
		environment.grantEngineer("admin-1", projectID)

		const mySecretValue = "super-secret-value-my-secret-99"
		environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
			ProjectID: projectID,
			SecretID:  "MY_SECRET",
			Value:     mySecretValue,
		}, nil, "write-my-secret")

		yamlBody := `actions:
  - name: with-secret
    image: docker.io/library/alpine:3.24
    secrets: [MY_SECRET]
    commands:
      - test -r /kodema/secrets/MY_SECRET
  - name: without-secret
    image: docker.io/library/alpine:3.24
    commands:
      - test ! -e /kodema/secrets/MY_SECRET
`
		applied := environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
			ProjectID: projectID,
			Name:      "secret-isolation",
			YAML:      []byte(yamlBody),
		}, nil, "apply-secret-isolation")
		var pipelineResult api.PipelineApplyResult
		environment.decode(applied, &pipelineResult)

		version := pipelineResult.Version
		started := environment.mustInvoke(projectToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  projectID,
			PipelineID: pipelineResult.PipelineID,
		}, &version, "start-secret-isolation")
		var startResult api.PipelineStartResult
		environment.decode(started, &startResult)

		var inspected api.ExecutionInspectResult
		environment.eventually(5*time.Second, func() bool {
			result, err := environment.invoke(projectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: startResult.ExecutionID,
			}, nil, "")
			if err != nil {
				return false
			}
			environment.decode(result, &inspected)
			return inspected.State == "succeeded"
		})

		prepares := runtime.prepareRequests()
		if len(prepares) != 2 {
			t.Fatalf("Prepare called %d times; want 2 actions", len(prepares))
		}
		if prepares[0].ActionName != "with-secret" {
			t.Fatalf("first action = %q; want with-secret", prepares[0].ActionName)
		}
		if len(prepares[0].Secrets) != 1 || prepares[0].Secrets[0].Name != "MY_SECRET" || prepares[0].Secrets[0].Value != mySecretValue {
			t.Fatalf("first action secrets = %+v; want MY_SECRET with stored value", prepares[0].Secrets)
		}
		secretPath, err := workspace.SecretPath(prepares[0].Secrets[0].Name)
		if err != nil || secretPath != "/kodema/secrets/MY_SECRET" {
			t.Fatalf("SecretPath = %q, %v; want /kodema/secrets/MY_SECRET", secretPath, err)
		}
		if prepares[1].ActionName != "without-secret" {
			t.Fatalf("second action = %q; want without-secret", prepares[1].ActionName)
		}
		if len(prepares[1].Secrets) != 0 {
			t.Fatalf("second action unexpectedly received secrets: %+v", prepares[1].Secrets)
		}
		if strings.Contains(harness.logs.String(), mySecretValue) {
			t.Fatal("composition logs leaked MY_SECRET plaintext value")
		}
	})

	t.Run("nonexistent secret fails closed at preparation and KODEMA_TOKEN is rejected at write and admission", func(t *testing.T) {
		harness := newHarness(t)
		harness.config.SecretRules = []secret.NameRule{{
			Scope:    tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
			AllowAll: true,
		}}
		runtime := newFakeRuntime(modeComplete)
		environment := harness.start(runtime)
		environment.grantEngineer("admin-1", projectID)

		// Shadowing KODEMA_TOKEN is refused at write.
		writeErr := environment.restError(projectToken, "secret.write", api.SecretWriteRequest{
			ProjectID: projectID,
			SecretID:  id.SecretTokenName,
			Value:     "forged-token-value",
		}, nil, "write-reserved-token")
		if writeErr.Code != string(fault.InvalidInput) {
			t.Fatalf("secret.write KODEMA_TOKEN code = %q; want %q (%s)", writeErr.Code, fault.InvalidInput, writeErr.Message)
		}

		// Declaring KODEMA_TOKEN in pipeline YAML is refused at admission.
		applyErr := environment.restError(projectToken, "pipeline.apply", api.PipelineApplyRequest{
			ProjectID: projectID,
			Name:      "shadow-token",
			YAML: []byte(`actions:
  - name: bad
    image: docker.io/library/alpine:3.24
    secrets: [KODEMA_TOKEN]
    commands: ["true"]
`),
		}, nil, "apply-reserved-token")
		if applyErr.Code != string(fault.InvalidInput) {
			t.Fatalf("pipeline.apply KODEMA_TOKEN code = %q; want %q (%s)", applyErr.Code, fault.InvalidInput, applyErr.Message)
		}

		// Declaring a nonexistent secret fails closed at preparation before any start.
		applied := environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
			ProjectID: projectID,
			Name:      "missing-secret",
			YAML: []byte(`actions:
  - name: missing
    image: docker.io/library/alpine:3.24
    secrets: [DOES_NOT_EXIST]
    commands: ["true"]
`),
		}, nil, "apply-missing-secret")
		var pipelineResult api.PipelineApplyResult
		environment.decode(applied, &pipelineResult)

		version := pipelineResult.Version
		started := environment.mustInvoke(projectToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  projectID,
			PipelineID: pipelineResult.PipelineID,
		}, &version, "start-missing-secret")
		var startResult api.PipelineStartResult
		environment.decode(started, &startResult)

		var inspected api.ExecutionInspectResult
		environment.eventually(5*time.Second, func() bool {
			result, err := environment.invoke(projectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: startResult.ExecutionID,
			}, nil, "")
			if err != nil {
				return false
			}
			environment.decode(result, &inspected)
			return inspected.State == "failed"
		})
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("missing secret failure = %+v; want preparation failure", inspected.Failure)
		}
		if runtime.startCount() != 0 || len(runtime.prepareRequests()) != 0 {
			t.Fatalf("nonexistent secret reached runtime: prepares=%d starts=%d", len(runtime.prepareRequests()), runtime.startCount())
		}
	})

	t.Run("unauthorized secret names the specific secret and respects project scope boundaries", func(t *testing.T) {
		harness := newHarness(t)
		harness.config.SecretRules = []secret.NameRule{{
			Scope: tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
			Names: []id.SecretName{"ALLOWED_SECRET"},
		}}
		runtime := newFakeRuntime(modeComplete)
		environment := harness.start(runtime)
		environment.grantEngineer("admin-1", projectID)

		const blockedValue = "blocked-secret-plaintext-value"
		environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
			ProjectID: projectID,
			SecretID:  "BLOCKED_SECRET",
			Value:     blockedValue,
		}, nil, "write-blocked-secret")

		applied := environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
			ProjectID: projectID,
			Name:      "blocked-secret",
			YAML: []byte(`actions:
  - name: blocked
    image: docker.io/library/alpine:3.24
    secrets: [BLOCKED_SECRET]
    commands: ["true"]
`),
		}, nil, "apply-blocked-secret")
		var pipelineResult api.PipelineApplyResult
		environment.decode(applied, &pipelineResult)

		version := pipelineResult.Version
		started := environment.mustInvoke(projectToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  projectID,
			PipelineID: pipelineResult.PipelineID,
		}, &version, "start-blocked-secret")
		var startResult api.PipelineStartResult
		environment.decode(started, &startResult)

		var inspected api.ExecutionInspectResult
		environment.eventually(5*time.Second, func() bool {
			result, err := environment.invoke(projectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: startResult.ExecutionID,
			}, nil, "")
			if err != nil {
				return false
			}
			environment.decode(result, &inspected)
			return inspected.State == "failed"
		})
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("blocked secret failure = %+v; want preparation failure", inspected.Failure)
		}
		if !strings.Contains(inspected.Failure.Detail, `"BLOCKED_SECRET"`) ||
			!strings.Contains(inspected.Failure.Detail, "not authorized by server secret rules") {
			t.Fatalf("failure detail = %q; want specific unauthorized secret named", inspected.Failure.Detail)
		}
		if strings.Contains(inspected.Failure.Detail, "ALLOWED_SECRET") || strings.Contains(inspected.Failure.Detail, blockedValue) {
			t.Fatalf("failure detail leaked allowed rule or secret value: %q", inspected.Failure.Detail)
		}
		if runtime.startCount() != 0 || len(runtime.prepareRequests()) != 0 {
			t.Fatalf("unauthorized secret reached runtime: prepares=%d starts=%d", len(runtime.prepareRequests()), runtime.startCount())
		}
	})

	t.Run("project AllowAll does not widen to a sibling project", func(t *testing.T) {
		harness := newHarness(t)
		harness.config.SecretRules = []secret.NameRule{{
			Scope:    tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
			AllowAll: true,
		}}
		runtime := newFakeRuntime(modeComplete)
		environment := harness.start(runtime)
		environment.mustInvoke(adminToken, "project.create", api.ProjectCreateRequest{
			TenantID:  tenantID,
			ProjectID: otherID,
			Name:      "Project Two",
		}, nil, "create-other-project")
		environment.grantEngineer("admin-2", otherID)
		environment.mustInvoke(otherToken, "secret.write", api.SecretWriteRequest{
			ProjectID: otherID,
			SecretID:  "SIBLING_SECRET",
			Value:     "sibling-secret-value",
		}, nil, "write-sibling-secret")

		applied := environment.mustInvoke(otherToken, "pipeline.apply", api.PipelineApplyRequest{
			ProjectID: otherID,
			Name:      "sibling-secret",
			YAML: []byte(`actions:
  - name: sibling
    image: docker.io/library/alpine:3.24
    secrets: [SIBLING_SECRET]
    commands: ["true"]
`),
		}, nil, "apply-sibling-secret")
		var pipelineResult api.PipelineApplyResult
		environment.decode(applied, &pipelineResult)
		version := pipelineResult.Version
		started := environment.mustInvoke(otherToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  otherID,
			PipelineID: pipelineResult.PipelineID,
		}, &version, "start-sibling-secret")
		var startResult api.PipelineStartResult
		environment.decode(started, &startResult)

		var inspected api.ExecutionInspectResult
		environment.eventually(5*time.Second, func() bool {
			result, err := environment.invoke(otherToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: startResult.ExecutionID,
			}, nil, "")
			if err != nil {
				return false
			}
			environment.decode(result, &inspected)
			return inspected.State == "failed"
		})
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" ||
			!strings.Contains(inspected.Failure.Detail, `"SIBLING_SECRET"`) {
			t.Fatalf("sibling project failure = %+v; want preparation failure naming SIBLING_SECRET", inspected.Failure)
		}
		if runtime.startCount() != 0 || len(runtime.prepareRequests()) != 0 {
			t.Fatalf("sibling project unauthorized secret reached runtime: prepares=%d starts=%d", len(runtime.prepareRequests()), runtime.startCount())
		}
	})

	t.Run("production profile refuses wildcard AllowAll secret rules", func(t *testing.T) {
		rulesPath := filepath.Join(t.TempDir(), "secret-rules.json")
		if err := os.WriteFile(rulesPath, []byte(`{
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "allow_all": true}
  ]
}`), 0o600); err != nil {
			t.Fatalf("write secret-rules.json: %v", err)
		}
		if rules, err := app.LoadSecretRulesFile(rulesPath, app.ProfileProduction); err == nil || rules != nil {
			t.Fatalf("LoadSecretRulesFile(ProfileProduction) accepted allow_all: %+v", rules)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("LoadSecretRulesFile(ProfileProduction) error = %v; want InvalidInput", err)
		}
	})
}
