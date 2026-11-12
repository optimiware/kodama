package app

// Workload bearer authentication in the development/test composition.
//
// The derived execution credential the composition delivers into Prepare is a
// real kodex bearer token. Development and test must authenticate it through the
// token manager alongside the explicitly static development tokens, so a
// workload can call the public API (inspect its own execution, submit declared
// outputs). A credential that looks derived but fails verification must never
// fall through to the static development authenticator, and production must
// never accept a static development token.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TestWorkloadBearer covers derived workload-token authentication at the public
// edge for the composed profiles.
func TestWorkloadBearer(t *testing.T) {
	t.Run("development accepts the derived execution token", func(t *testing.T) {
		fixture := newActivationFixture(t, activationRules(true, activationSecretName))
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "bearer-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}, outputs: []id.OutputKey{"result"}},
		), "bearer-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "bearer-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the workload received no execution credential")
		}

		recorder, result, publicError := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, credential, nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("execution.inspect with the workload token status=%d error=%+v", recorder.Code, publicError)
		}
		view := decodeBody[api.ExecutionInspectResult](t, result)
		if view.ExecutionID != started.ExecutionID {
			t.Fatalf("execution.inspect returned %q; want %q", view.ExecutionID, started.ExecutionID)
		}
		if view.State != "running" {
			t.Fatalf("execution.inspect state = %q; want running", view.State)
		}

		inspected := waitForStableExecutionVersion(t, handler, "dev-project-token", started.ExecutionID)
		if len(inspected.Actions) != 1 {
			t.Fatalf("execution has %d actions; want one", len(inspected.Actions))
		}
		submitExecutionOutputs(t, handler, credential, started.ExecutionID, inspected.Actions[0].ActionID, "workload-outputs")
	})

	t.Run("test profile accepts the derived execution token", func(t *testing.T) {
		config := fullTestProfileConfig(t)
		runtime := newRecordingRuntime()
		config.Adapters.Runtime = runtime
		config.Adapters.Workspace = runtime
		config.SecretRules = nil

		app, err := New(context.Background(), config)
		if err != nil {
			t.Fatalf("New ProfileTest: %v", err)
		}
		if err := app.start(context.Background()); err != nil {
			t.Fatalf("start ProfileTest: %v", err)
		}
		t.Cleanup(func() { closeApp(t, app) })
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "test-profile-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "test-profile-task")

		prepares := waitForActivationPrepares(t, runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the test-profile workload received no execution credential")
		}
		recorder, _, publicError := restCall(t, handler, http.MethodGet,
			"/api/v1/executions/"+started.ExecutionID, credential, nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("test-profile execution.inspect with the workload token status=%d error=%+v", recorder.Code, publicError)
		}
	})

	t.Run("static development tokens still authenticate", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		recorder, _, publicError := restCall(t, handler, http.MethodGet,
			"/api/v1/projects/project-1/events", "dev-project-token", nil, nil, "")
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("static development authentication regressed: status=%d error=%+v", recorder.Code, publicError)
		}
	})

	t.Run("revoked token is refused", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "revoked-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "revoked-task")
		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the workload received no execution credential")
		}

		cancelExecution(t, handler, started.ExecutionID)

		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("a revoked execution credential authenticated")
		}
		if strings.Contains(recorder.Body.String(), credential) {
			t.Fatal("the revocation refusal echoed the presented credential")
		}
	})

	t.Run("token of another execution is refused", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "two-execution-pipeline")
		first := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "two-execution-first")
		second := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "two-execution-second")

		prepares := waitForActivationPrepares(t, fixture.runtime, 2)
		credentials := make(map[id.ExecutionID]string, len(prepares))
		for _, prepare := range prepares {
			credentials[prepare.Identity.ExecutionID] = prepare.Credential
		}
		firstCredential := credentials[id.ExecutionID(first.ExecutionID)]
		secondCredential := credentials[id.ExecutionID(second.ExecutionID)]
		if firstCredential == "" || secondCredential == "" {
			t.Fatal("an execution received no credential")
		}
		if firstCredential == secondCredential {
			t.Fatal("two executions share one credential")
		}

		// Positive control: the second execution token authenticates its own
		// execution.
		own, _, _ := rawExecutionInspect(t, handler, secondCredential, second.ExecutionID)
		if own.Code != api.HTTPStatusSuccess {
			t.Fatalf("the execution token did not authenticate its own execution: status=%d", own.Code)
		}
		foreign, _, _ := rawExecutionInspect(t, handler, firstCredential, second.ExecutionID)
		if foreign.Code == api.HTTPStatusSuccess {
			t.Fatal("a credential of another execution addressed a foreign execution")
		}
	})

	t.Run("expired token is refused", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "expiry-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "expiry-task")
		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the workload received no execution credential")
		}

		// The action deadline is 24h and the credential TTL is capped at 8h, so
		// 9h expires exactly the credential while the action stays current.
		fixture.environment.clock.Advance(9 * time.Hour)

		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("an expired execution credential authenticated")
		}
	})

	t.Run("forged garbled and foreign tokens are refused", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "forged-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "forged-task")
		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the workload received no execution credential")
		}

		foreign, err := token.DeriveExecutionToken(bytes.Repeat([]byte{9}, 32), token.ExecutionClaims{
			TenantID:    "tenant-1",
			ExecutionID: id.ExecutionID(started.ExecutionID),
			Nonce:       bytes.Repeat([]byte{7}, token.MinNonceBytes),
			Expiry:      fixture.environment.clock.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("DeriveExecutionToken: %v", err)
		}
		cases := map[string]string{
			"foreign key":  foreign,
			"garbled mac":  garbleCredential(credential),
			"malformed":    "kodex." + started.ExecutionID,
			"service kind": token.ServicePrefix + started.ExecutionID + ".AAAA",
		}
		for name, presented := range cases {
			recorder, _, _ := rawExecutionInspect(t, handler, presented, started.ExecutionID)
			if recorder.Code == api.HTTPStatusSuccess {
				t.Fatalf("%s credential authenticated", name)
			}
			if strings.Contains(recorder.Body.String(), presented) {
				t.Fatalf("%s refusal echoed the presented credential", name)
			}
		}
	})

	t.Run("a derived-shaped credential never falls through to static authentication", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		forged := "kodex.execution-forged.deadbeef"
		// Configure the exact derived-shaped string as a static development
		// credential. A resolver that tries the static authenticator after the
		// derived verification fails would authenticate it.
		fixture.environment.config.Dev.Tokens = append(fixture.environment.config.Dev.Tokens, DevToken{
			Credential: forged,
			Principal:  projectPrincipal(fixture.environment.clock).Principal,
			ExpiresAt:  fixture.environment.clock.Now().Add(24 * time.Hour),
		})
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")

		// Positive control: the same fixture authenticates an ordinary static
		// development token.
		control, _, publicError := restCall(t, handler, http.MethodGet,
			"/api/v1/projects/project-1/events", "dev-project-token", nil, nil, "")
		if control.Code != api.HTTPStatusSuccess {
			t.Fatalf("static development authentication regressed: status=%d error=%+v", control.Code, publicError)
		}

		recorder, _, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/projects/project-1/events", forged, nil, nil, "")
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("a derived-shaped credential fell through to static development authentication")
		}
	})

	t.Run("production refuses static development authentication", func(t *testing.T) {
		config := validProductionConfig(t)
		config.Production.dialNATS = (&fakeNATSServer{}).dial
		app, err := New(context.Background(), config)
		if err != nil {
			t.Fatalf("New production composition: %v", err)
		}
		t.Cleanup(func() { closeApp(t, app) })

		recorder, _, _ := restCall(t, app.Handler(), http.MethodGet,
			"/api/v1/projects/project-1/events", "dev-admin-token", nil, nil, "")
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the production composition accepted a static development credential")
		}
		if strings.Contains(recorder.Body.String(), "dev-admin-token") {
			t.Fatal("the production refusal echoed the presented credential")
		}
	})
}

// garbleCredential changes one character of the MAC segment so the presented
// token is still framed as a derived credential but cannot verify.
func garbleCredential(credential string) string {
	index := strings.LastIndexByte(credential, '.')
	if index < 0 || index+1 >= len(credential) {
		return credential + "A"
	}
	mac := []byte(credential[index+1:])
	replacement := byte('A')
	if mac[0] == 'A' {
		replacement = 'B'
	}
	mac[0] = replacement
	return credential[:index+1] + string(mac)
}

// submitExecutionOutputs stages one declared output with the workload
// credential, retrying a stale expected version so the test does not race the
// Kernel's first running observation.
func submitExecutionOutputs(t *testing.T, handler http.Handler, credential, executionID, actionID, key string) {
	t.Helper()
	for attempt := range 20 {
		inspected := inspectExecution(t, handler, "dev-project-token", executionID)
		version := inspected.Version
		recorder, _, publicError := restCall(t, handler, http.MethodPut,
			"/api/v1/executions/"+executionID+"/actions/"+actionID+"/outputs", credential,
			api.ExecutionActionOutputsRequest{Outputs: map[string]string{"result": "ok"}},
			&version, fmt.Sprintf("%s-%d", key, attempt))
		if recorder.Code == api.HTTPStatusSuccess {
			return
		}
		if publicError.Code != api.CodeVersionConflict {
			t.Fatalf("execution.action.outputs with the workload token status=%d error=%+v", recorder.Code, publicError)
		}
	}
	t.Fatal("execution.action.outputs did not converge on a current execution version")
}

// cancelExecution cancels one execution with the human project token, retrying
// a stale expected version.
func cancelExecution(t *testing.T, handler http.Handler, executionID string) {
	t.Helper()
	for attempt := range 20 {
		inspected := inspectExecution(t, handler, "dev-project-token", executionID)
		version := inspected.Version
		recorder, _, publicError := restCall(t, handler, http.MethodPost,
			"/api/v1/executions/"+executionID+"/cancel", "dev-project-token",
			api.ExecutionCancelRequest{ExecutionID: executionID},
			&version, fmt.Sprintf("cancel-%d", attempt))
		if recorder.Code == api.HTTPStatusSuccess {
			return
		}
		if publicError.Code != api.CodeVersionConflict {
			t.Fatalf("execution.cancel status=%d error=%+v", recorder.Code, publicError)
		}
	}
	t.Fatal("execution.cancel did not converge on a current execution version")
}
