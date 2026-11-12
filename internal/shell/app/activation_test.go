package app

// Activation secrets and workload credential materialization. These tests
// describe the behavior only through the public composition surface.
//
// Composition API under test:
//
//	Config.SecretRules []secret.NameRule
//	    Trusted workload-consumer secret-name allow rules. The composition
//	    builds bridges.NewWorkloadNamePolicy(Config.SecretRules) and a
//	    secret.SecretResolver over the store-backed name lookup, the
//	    composition cipher and Config.InstallationID, then resolves
//	    PrepareRequest.Container.Secrets at the execution's project (with
//	    tenant fallback) scope.
//
// Behavior pinned here:
//   - Prepare receives every declared secret value in declaration order once
//     per activation and a non-empty execution credential scoped to the
//     execution. The initial credential is delivered directly into Prepare, so
//     no RefreshCredential call precedes the preparation.
//   - A denied or missing declared name refuses the preparation before the
//     runtime adapter is called and before any start.
//   - Consecutive container activations and a controller restart reuse the
//     committed credential (identical token, no second mint, no refresh).
//   - Secret values and token strings never appear in logs, events, task
//     metadata or public results/errors.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
)

// TestActivationSecretsAndCredential covers the activation materialization the
// composed runtime must add to the Kernel preparation request.
func TestActivationSecretsAndCredential(t *testing.T) {
	t.Run("declared value and scoped credential reach Prepare once", func(t *testing.T) {
		fixture := newActivationFixture(t, activationRules(true, activationSecretName))
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "activation-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}, outputs: []id.OutputKey{"result"}},
		), "activation-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "activation-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		if len(prepares) != 1 {
			t.Fatalf("runtime adapter saw %d preparations; want exactly one", len(prepares))
		}
		prepare := prepares[0]
		if prepare.Identity.ExecutionID != id.ExecutionID(started.ExecutionID) {
			t.Fatalf("preparation execution = %q; want %q", prepare.Identity.ExecutionID, started.ExecutionID)
		}
		if prepare.ActionName != "inspect" {
			t.Fatalf("preparation action = %q; want inspect", prepare.ActionName)
		}
		if len(prepare.Secrets) != 1 {
			t.Fatalf("preparation delivered %d secret values; want exactly the one declared name", len(prepare.Secrets))
		}
		if prepare.Secrets[0].Name != activationSecretName {
			t.Fatalf("preparation secret name = %q; want %q", prepare.Secrets[0].Name, activationSecretName)
		}
		if prepare.Secrets[0].Value != activationSecretValue {
			t.Fatal("preparation did not deliver the stored project secret value")
		}
		if !strings.HasPrefix(prepare.Credential, "kodex."+started.ExecutionID+".") {
			t.Fatal("preparation credential is not a kodex token scoped to the execution")
		}
		if fixture.runtime.refreshCount() != 0 {
			t.Fatalf("initial delivery called RefreshCredential %d times before the preparation existed", fixture.runtime.refreshCount())
		}
		if fixture.runtime.delegatedCount() != 1 {
			t.Fatalf("runtime adapter prepared %d times; want exactly once", fixture.runtime.delegatedCount())
		}

		// The delivered credential is the committed execution credential: it
		// authenticates as this execution and reaches inspect.
		derivedRecorder, _, _ := rawExecutionInspect(t, handler, prepare.Credential, started.ExecutionID)
		if derivedRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("delivered credential did not authenticate as the execution: status=%d", derivedRecorder.Code)
		}

		// Secret values and token strings must not appear in logs or in any
		// public result: inspect, event read, task read, task history.
		if containsMaterial(fixture.environment.logs.String(), activationSecretValue, prepare.Credential) {
			t.Fatal("composition logs contain activation secret or credential material")
		}
		inspectRecorder, inspectResult, _ := rawExecutionInspect(t, handler, "dev-project-token", started.ExecutionID)
		if inspectRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("execution.inspect status=%d", inspectRecorder.Code)
		}
		eventRecorder, eventResult, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/projects/project-1/events", "dev-project-token", nil, nil, "")
		if eventRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("event.read status=%d", eventRecorder.Code)
		}
		taskRecorder, taskResult, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/tasks/"+started.TaskID, "dev-project-token", nil, nil, "")
		if taskRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.read status=%d", taskRecorder.Code)
		}
		historyRecorder, historyResult, _ := restCall(t, handler, http.MethodGet,
			"/api/v1/tasks/"+started.TaskID+"/history", "dev-project-token", nil, nil, "")
		if historyRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("task.history status=%d", historyRecorder.Code)
		}
		for name, body := range map[string]string{
			"execution.inspect": string(inspectResult.Body),
			"event.read":        string(eventResult.Body),
			"task.read":         string(taskResult.Body),
			"task.history":      string(historyResult.Body),
		} {
			if containsMaterial(body, activationSecretValue, prepare.Credential) {
				t.Fatalf("%s rendered activation secret or credential material", name)
			}
		}
	})

	t.Run("denied declared name refuses before any start", func(t *testing.T) {
		// The policy allows only a different name. The pipeline declares the
		// stored name, so resolution must be refused before any lookup or
		// runtime effect.
		fixture := newActivationFixture(t, activationRules(true, activationOtherName))
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "denied-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}},
		), "denied-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "denied-task")

		waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if fixture.runtime.delegatedCount() != 0 {
			t.Fatalf("runtime adapter prepared %d denied activations; want none", fixture.runtime.delegatedCount())
		}
		if fixture.runtime.startCount() != 0 {
			t.Fatalf("runtime adapter started %d denied workloads; want none", fixture.runtime.startCount())
		}
		recorder, _, _ := rawExecutionInspect(t, handler, "dev-project-token", started.ExecutionID)
		if recorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("execution.inspect status=%d", recorder.Code)
		}
		if containsMaterial(recorder.Body.String(), activationSecretValue) {
			t.Fatal("the preparation refusal rendered the stored secret value")
		}
		if containsMaterial(fixture.environment.logs.String(), activationSecretValue) {
			t.Fatal("composition logs contain a denied secret value")
		}
	})

	t.Run("missing stored name refuses before any start", func(t *testing.T) {
		// The policy allows the name but no row is stored, so resolution fails
		// closed and the runtime adapter is never asked to prepare.
		fixture := newActivationFixture(t, activationRules(true, activationSecretName))
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}},
		), "missing-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "missing-task")

		waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if fixture.runtime.delegatedCount() != 0 {
			t.Fatalf("runtime adapter prepared %d missing activations; want none", fixture.runtime.delegatedCount())
		}
		if fixture.runtime.startCount() != 0 {
			t.Fatalf("runtime adapter started %d missing workloads; want none", fixture.runtime.startCount())
		}
	})

	t.Run("consecutive containers reuse the committed credential", func(t *testing.T) {
		fixture := newActivationFixture(t, activationRules(true, activationSecretName), "first")
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "reuse-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "first", secrets: []id.SecretName{activationSecretName}},
			activationAction{name: "second", secrets: []id.SecretName{activationSecretName}},
		), "reuse-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "reuse-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 2)
		if len(prepares) != 2 {
			t.Fatalf("runtime adapter saw %d preparations; want two activations", len(prepares))
		}
		first, second := prepares[0], prepares[1]
		if first.ActionName != "first" || second.ActionName != "second" {
			t.Fatalf("activation order = %q,%q; want first,second", first.ActionName, second.ActionName)
		}
		if first.Credential == "" || second.Credential == "" {
			t.Fatal("an activation received an empty execution credential")
		}
		if first.Credential != second.Credential {
			t.Fatal("consecutive containers minted a new credential instead of reusing the current one")
		}
		for _, prepare := range []kernelengine.PrepareRequest{first, second} {
			if prepare.Identity.ExecutionID != id.ExecutionID(started.ExecutionID) {
				t.Fatalf("preparation execution = %q; want %q", prepare.Identity.ExecutionID, started.ExecutionID)
			}
			if len(prepare.Secrets) != 1 || prepare.Secrets[0].Name != activationSecretName ||
				prepare.Secrets[0].Value != activationSecretValue {
				t.Fatalf("activation %q did not deliver exactly the declared value", prepare.ActionName)
			}
		}
		if fixture.runtime.refreshCount() != 0 {
			t.Fatalf("reuse called RefreshCredential %d times; reuse must deliver in Prepare", fixture.runtime.refreshCount())
		}
		derivedRecorder, _, _ := rawExecutionInspect(t, handler, second.Credential, started.ExecutionID)
		if derivedRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the reused credential did not authenticate: status=%d", derivedRecorder.Code)
		}
	})

	t.Run("controller restart reconstructs the same credential", func(t *testing.T) {
		shared := newActivationShared(t, "first")
		// Hold the first action open so the first composition can be closed
		// before it settles; the second composition then drives the successor.
		shared.runtime.pauseExits()

		appA := startComposedApp(t, shared.config(t, activationRules(true, activationSecretName)))
		handlerA := appA.Handler()
		grantProjectEngineer(t, handlerA, "dev-admin-token")
		writeActivationSecret(t, handlerA, "dev-project-token", activationSecretName, activationSecretValue, "restart-secret-write")
		applied := applyActivationPipeline(t, handlerA, "dev-project-token", activationPipeline(
			activationAction{name: "first", secrets: []id.SecretName{activationSecretName}},
			activationAction{name: "second", secrets: []id.SecretName{activationSecretName}},
		), "restart-pipeline")
		started := startActivationTask(t, handlerA, "dev-project-token", applied.PipelineID, "restart-task")

		firstPrepares := waitForActivationPrepares(t, shared.runtime, 1)
		if firstPrepares[0].ActionName != "first" {
			t.Fatalf("first preparation action = %q; want first", firstPrepares[0].ActionName)
		}
		committed := firstPrepares[0].Credential
		if committed == "" {
			t.Fatal("the first activation received an empty execution credential")
		}
		if shared.runtime.refreshCount() != 0 {
			t.Fatalf("initial delivery called RefreshCredential %d times", shared.runtime.refreshCount())
		}

		closeApp(t, appA)
		shared.runtime.releaseExits()

		appB := startComposedApp(t, shared.config(t, activationRules(true, activationSecretName)))
		t.Cleanup(func() { closeApp(t, appB) })

		prepares := waitForActivationPrepares(t, shared.runtime, 2)
		if len(prepares) < 2 {
			t.Fatalf("runtime adapter saw %d preparations after restart; want the successor activation", len(prepares))
		}
		second := prepares[1]
		if second.ActionName != "second" {
			t.Fatalf("post-restart preparation action = %q; want second", second.ActionName)
		}
		if second.Identity.ExecutionID != id.ExecutionID(started.ExecutionID) {
			t.Fatalf("post-restart preparation execution = %q; want %q", second.Identity.ExecutionID, started.ExecutionID)
		}
		if second.Credential != committed {
			t.Fatal("the restarted composition minted a different credential instead of reconstructing the committed one")
		}
		if shared.runtime.refreshCount() != 0 {
			t.Fatalf("restart recovery called RefreshCredential %d times", shared.runtime.refreshCount())
		}
		// The reconstructed token is the committed credential: the restarted
		// composition authenticates it as the running execution.
		derivedRecorder, _, _ := rawExecutionInspect(t, appB.Handler(), second.Credential, started.ExecutionID)
		if derivedRecorder.Code != api.HTTPStatusSuccess {
			t.Fatalf("the reconstructed credential did not authenticate after restart: status=%d", derivedRecorder.Code)
		}
	})

	t.Run("tenant-wide rule authorizes a project execution", func(t *testing.T) {
		// A tenant-wide allow rule covers a project-scoped activation; the
		// resolver may legitimately fall back to the tenant-wide row.
		fixture := newActivationFixture(t, activationRules(false, activationSecretName))
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "tenant-rule-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}},
		), "tenant-rule-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "tenant-rule-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		if prepares[0].Identity.ExecutionID != id.ExecutionID(started.ExecutionID) {
			t.Fatalf("preparation execution = %q; want %q", prepares[0].Identity.ExecutionID, started.ExecutionID)
		}
		if len(prepares[0].Secrets) != 1 || prepares[0].Secrets[0].Value != activationSecretValue {
			t.Fatal("a tenant-wide allow rule did not deliver the project secret value")
		}
	})
}
