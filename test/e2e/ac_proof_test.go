//go:build e2e

package e2e

// The AC03..AC20 proofs. Every test drives the documented installation through
// a supported interface (HTTP, Git, the public event stream, or an execution's
// mounted credential) and either asserts a concrete invariant or names the
// missing installation input. A skip here is a configuration skip for an
// unconfigured environment, never a pass. Product defects are reported instead
// of being turned into skips.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// requireACInstallation loads the documented installation and refuses to
// continue with a named missing input. It is the common gate for the AC proofs.
func requireACInstallation(t *testing.T) *live {
	t.Helper()
	instance := startLive(t)
	if missing := smoke.MissingInputs(instance.config); !missing.Empty() {
		if instance.config.RequireInstallation() {
			t.Fatalf("required installation input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured installation input(s): %s", missing)
	}
	for _, persona := range harness.Personas {
		instance.smokeToken(t, persona)
	}
	return instance
}

// requireACGit names the Git-scenario configuration gate. The documented stage
// and local installations have Git enabled; a non-Git run sets Q01_SMOKE_GIT=0
// and skips the Git-specific rows explicitly.
func requireACGit(t *testing.T, instance *live, proof string) {
	t.Helper()
	if !instance.config.GitSmokeEnabled() {
		t.Skipf("Git proof %s is disabled: %s=0", proof, harness.InputSmokeGit)
	}
}

// requirePublicRefusal requires one public API refusal with a code from the
// allowed set. A transport failure or a success is never mistaken for a
// refusal.
func requirePublicRefusal(t *testing.T, err error, codes ...string) *harness.APIError {
	t.Helper()
	var refusal *harness.APIError
	if !errors.As(err, &refusal) {
		t.Fatalf("request = %v; want a public refusal with one of %v", err, codes)
	}
	if slices.Contains(codes, refusal.Code) {
		return refusal
	}
	t.Fatalf("request refusal code = %q; want one of %v", refusal.Code, codes)
	return nil
}

// TestE2E_AC03Idempotency proves retry interfaces converge without duplicate
// work: an inline retry returns the original execution, changed bytes under the
// same key are refused, a stored start replays across a pipeline edit while a
// changed version is refused, and an identical custom-event retry keeps one
// event identity.
func TestE2E_AC03Idempotency(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)

	// (a) An inline retry converges on exactly one execution and one action.
	inlineDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac03-once",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac03-once"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC03 inline pipeline: %v", err)
	}
	inlineKey := instance.key("ac03-inline")
	first := instance.smokeStartInline(ctx, t, engineer, inlineDocument, inlineKey)
	replay := instance.smokeStartInline(ctx, t, engineer, inlineDocument, inlineKey)
	if replay.ExecutionID != first.ExecutionID {
		t.Fatalf("identical inline retry = %q; want the original execution %q", replay.ExecutionID, first.ExecutionID)
	}
	final := instance.smokeWaitComplete(ctx, t, engineer, first.ExecutionID, 6*time.Minute)
	requireSucceeded(t, final)
	if len(final.Actions) != 1 {
		t.Fatalf("replayed inline execution has %d actions; want exactly the one admitted action", len(final.Actions))
	}
	action := final.Actions[0]
	log := instance.smokeReadLogs(ctx, t, engineer, first.ExecutionID, action.ActionID, "stdout")
	if err := smoke.CheckMarker(log, "ac03-once", 1); err != nil {
		t.Fatalf("retried inline execution marker: %v", err)
	}
	ids := instance.smokeExecutionIDs(ctx, t, engineer)
	if got := countString(ids, first.ExecutionID); got != 1 {
		t.Fatalf("execution %s appears %d time(s) after the retry; want exactly 1", first.ExecutionID, got)
	}
	reinspected := instance.smokeStableInspect(ctx, t, engineer, first.ExecutionID)
	if len(reinspected.Actions) != 1 || reinspected.Actions[0].ActionID != action.ActionID {
		t.Fatalf("retry changed the admitted action set: before=%+v after=%+v", final.Actions, reinspected.Actions)
	}

	// (b) Changed exact bytes under the same key are refused and admit nothing.
	changedDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac03-changed",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac03-changed"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the changed AC03 pipeline: %v", err)
	}
	_, changedErr := instance.invoke(ctx, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID: instance.projectID,
		YAML:      changedDocument,
	}, inlineKey, nil)
	requirePublicRefusal(t, changedErr, api.CodeChangedIdempotencyContent)
	afterChanged := instance.smokeExecutionIDs(ctx, t, engineer)
	instance.smokeRequireNoExecutionWithActions(ctx, t, engineer, ids, afterChanged, "ac03-once", "ac03-changed")

	// (c) A stored start replays across a pipeline edit; the receipt pins the
	// admitted version and a changed version under the same key is refused.
	name := "ac03-" + instance.runID
	storedDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac03-stored",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac03-version-one"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the stored AC03 pipeline: %v", err)
	}
	applied := instance.smokeApplyPipeline(ctx, t, engineer, name, storedDocument, instance.key("ac03-apply"))
	storedKey := instance.key("ac03-stored")
	storedVersion := applied.Version
	storedResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: applied.PipelineID,
	}, storedKey, &storedVersion)
	storedStart := decodeResult[api.PipelineStartResult](t, storedResult)

	// Edit the stored pipeline after the receipt was committed. The committed
	// receipt still pins the admitted version, so the identical retry replays.
	updatedDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac03-stored",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac03-version-two"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the updated AC03 pipeline: %v", err)
	}
	updated := instance.updatePipeline(t, applied.PipelineID, name, updatedDocument, applied.Version, instance.key("ac03-update"))

	replayedResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: applied.PipelineID,
	}, storedKey, &storedVersion)
	replayedStart := decodeResult[api.PipelineStartResult](t, replayedResult)
	if replayedStart.ExecutionID != storedStart.ExecutionID {
		t.Fatalf("stored retry after the pipeline edit = %q; want the original execution %q", replayedStart.ExecutionID, storedStart.ExecutionID)
	}
	changedVersion := updated.Version
	_, versionErr := instance.invoke(ctx, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: applied.PipelineID,
	}, storedKey, &changedVersion)
	requirePublicRefusal(t, versionErr, api.CodeChangedIdempotencyContent)

	storedFinal := instance.smokeWaitComplete(ctx, t, engineer, storedStart.ExecutionID, 6*time.Minute)
	requireSucceeded(t, storedFinal)
	storedLog := instance.smokeReadLogs(ctx, t, engineer, storedStart.ExecutionID,
		actionByName(t, storedFinal, "ac03-stored").ActionID, "stdout")
	if err := smoke.CheckMarker(storedLog, "ac03-version-one", 1); err != nil {
		t.Fatalf("replayed stored execution did not run its admitted definition: %v", err)
	}
	if err := smoke.CheckMarker(storedLog, "ac03-version-two", 0); err != nil {
		t.Fatalf("replayed stored execution ran the edited definition: %v", err)
	}

	// Positive control: a new start at the updated version really uses the edit.
	controlVersion := updated.Version
	controlResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: applied.PipelineID,
	}, instance.key("ac03-control"), &controlVersion)
	controlStart := decodeResult[api.PipelineStartResult](t, controlResult)
	controlFinal := instance.smokeWaitComplete(ctx, t, engineer, controlStart.ExecutionID, 6*time.Minute)
	requireSucceeded(t, controlFinal)
	controlLog := instance.smokeReadLogs(ctx, t, engineer, controlStart.ExecutionID,
		actionByName(t, controlFinal, "ac03-stored").ActionID, "stdout")
	if err := smoke.CheckMarker(controlLog, "ac03-version-two", 1); err != nil {
		t.Fatalf("post-edit control did not run the updated definition: %v", err)
	}

	// (d) An identical custom-observation retry keeps one event identity and
	// one committed row.
	observationDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac03-observer",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"sleep 8"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC03 observation pipeline: %v", err)
	}
	observationStart := instance.smokeStartInline(ctx, t, engineer, observationDocument, instance.key("ac03-observer"))
	observationView := instance.smokeWaitActionRunning(ctx, t, engineer, observationStart.ExecutionID, "ac03-observer", 3*time.Minute)
	eventKey := instance.key("ac03-event")
	publish := func() api.EventPublishResult {
		result := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
			ProjectID:      instance.projectID,
			Type:           "observation.custom",
			SubjectID:      observationStart.ExecutionID,
			SubjectVersion: observationView.Version,
			CorrelationID:  observationStart.ExecutionID,
			Data:           []byte(`{"ac03":"idempotent"}`),
		}, eventKey, nil)
		return decodeResult[api.EventPublishResult](t, result)
	}
	firstEvent := publish()
	secondEvent := publish()
	if secondEvent.EventID != firstEvent.EventID || secondEvent.ReceiptID != firstEvent.ReceiptID {
		t.Fatalf("identical event retry = %s/%s; want the original %s/%s", secondEvent.EventID, secondEvent.ReceiptID, firstEvent.EventID, firstEvent.ReceiptID)
	}
	if !secondEvent.Replayed {
		t.Fatal("an identical event retry was not reported as a replay")
	}
	events := instance.smokeEventReadAll(ctx, t, engineer, "observation.custom")
	if got := countEventID(events, firstEvent.EventID); got != 1 {
		t.Fatalf("observation event %s appears %d time(s); want exactly 1", firstEvent.EventID, got)
	}
	observationFinal := instance.smokeWaitComplete(ctx, t, engineer, observationStart.ExecutionID, 6*time.Minute)
	requireSucceeded(t, observationFinal)

	finished := time.Now().UTC()
	instance.record(t, "AC03", "3.2", "TestE2E_AC03Idempotency", &final, started, finished, nil)
	instance.record(t, "AC03", "3.3", "TestE2E_AC03Idempotency", &observationFinal, started, finished, map[string]any{
		"event_id": firstEvent.EventID,
	})
	instance.record(t, "AC03", "3.4", "TestE2E_AC03Idempotency", &storedFinal, started, finished, nil)
}

// TestE2E_AC04RequestSettlement proves the planned-wait pass path continues the
// same execution and workspace, and the rejection path retains the answer,
// fails the execution with the typed rejection cause, and never activates the
// later action.
func TestE2E_AC04RequestSettlement(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()

	passFinal := testReleaseSmokeRequestWait(t, instance,
		smoke.NewNames(instance.runID, smoke.RequestWait), "TestE2E_AC04RequestSettlement-Pass", false)
	instance.record(t, "AC04", "4.3", "TestE2E_AC04RequestSettlement", &passFinal, started, time.Now().UTC(), nil)

	t.Run("RejectionRetainsTheAnswerAndDoesNotAdvance", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		operator := instance.smokeToken(t, harness.PersonaOperator)
		engineer := instance.smokeToken(t, harness.PersonaEngineer)
		operatorPrincipal := smokePersonaPrincipal(t, instance, harness.PersonaOperator)
		ca := instance.materializeCA(t)

		document, err := (harness.Document{Actions: []harness.Action{
			{
				Name:     "ac04-prepare",
				Image:    instance.image,
				Timeout:  "5m",
				Commands: ca.commands(`echo ac04-prepared`),
			},
			{
				Name:    "ac04-answer",
				Request: "role/operator",
				Timeout: "10m",
				Subject: map[string]any{"question": "ac04-reject"},
				Outputs: []string{"answer"},
			},
			{
				Name:     "ac04-after",
				Image:    instance.image,
				Timeout:  "5m",
				Commands: ca.commands(`echo ac04-should-not-run`),
			},
		}}).Build()
		if err != nil {
			t.Fatalf("build the AC04 rejection pipeline: %v", err)
		}
		admitted := instance.smokeStartInline(ctx, t, engineer, document, instance.key("ac04-reject"))
		t.Cleanup(func() {
			instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
		})
		waiting := instance.smokeWaitActionRunning(ctx, t, engineer, admitted.ExecutionID, "ac04-answer", 6*time.Minute)
		review := actionByName(t, waiting, "ac04-answer")
		item := instance.smokeWaitRequestInbox(ctx, t, operator, admitted.ExecutionID, review.ActionID, 3*time.Minute)

		// A stale action version is refused without settling the wait.
		_, staleErr := instance.invoke(ctx, operator, api.MethodPost, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           admitted.ExecutionID,
			ActionID:              review.ActionID,
			ExpectedActionVersion: item.Version + 1,
			Decision:              "fail",
		}, instance.key("ac04-stale"), nil)
		requirePublicRefusal(t, staleErr, api.CodeVersionConflict)
		stillWaiting := instance.smokeInspect(ctx, t, engineer, admitted.ExecutionID)
		stillAction := actionByName(t, stillWaiting, "ac04-answer")
		if stillAction.State != "running" || stillAction.Decision != "" || stillAction.Actor != "" {
			t.Fatalf("stale resolve settled the wait: state=%q decision=%q actor=%q", stillAction.State, stillAction.Decision, stillAction.Actor)
		}

		// A failing decision retains the answer and fails the execution without
		// promoting the response to a downstream output.
		instance.smokeResolve(ctx, t, operator, api.ExecutionRequestResolveRequest{
			ExecutionID:           admitted.ExecutionID,
			ActionID:              review.ActionID,
			ExpectedActionVersion: item.Version,
			Decision:              "fail",
			Outputs:               map[string]string{"answer": "ac04-rejected"},
		}, instance.key("ac04-resolve"))
		final := instance.smokeWaitComplete(ctx, t, operator, admitted.ExecutionID, 6*time.Minute)
		if final.State != "failed" {
			t.Fatalf("rejected execution state = %q; want failed", final.State)
		}
		if final.Failure == nil || final.Failure.Kind != "runtime" || final.Failure.Reason != "answer_rejected" {
			t.Fatalf("rejected execution failure = %+v; want runtime/answer_rejected", final.Failure)
		}
		failed := actionByName(t, final, "ac04-answer")
		if failed.State != "failed" || failed.Decision != "fail" {
			t.Fatalf("rejected request = state %q decision %q; want failed/fail", failed.State, failed.Decision)
		}
		if err := smoke.CheckDecisionActor(failed, "fail", operatorPrincipal); err != nil {
			t.Fatalf("retained rejection: %v", err)
		}
		if len(failed.Outputs) != 0 {
			t.Fatalf("rejected response was promoted as a downstream output: %+v", failed.Outputs)
		}
		after := actionByName(t, final, "ac04-after")
		if after.State != "cancelled" {
			t.Fatalf("later action state = %q; want cancelled", after.State)
		}
		if after.ActivatedAt != nil || after.Stop != nil {
			t.Fatalf("later action was activated after rejection: %+v", after)
		}
		prepare := actionByName(t, final, "ac04-prepare")
		prepareLog := instance.smokeReadLogs(ctx, t, operator, admitted.ExecutionID, prepare.ActionID, "stdout")
		if err := smoke.CheckMarker(prepareLog, "ac04-prepared", 1); err != nil {
			t.Fatalf("pre-rejection workload marker: %v", err)
		}
		instance.record(t, "AC04", "4.4", "TestE2E_AC04RequestSettlement-Rejection", &final, time.Now().UTC(), time.Now().UTC(), map[string]any{
			"decision": "fail",
			"actor":    operatorPrincipal,
		})
	})
}

// TestE2E_AC05ProtectedIntegration proves the composed protected path on the
// installation: external harness, exact candidate test, reviewer decision and
// the served Git ref update in one execution and workspace, including the
// direct-human-push and wrong-role refusals.
func TestE2E_AC05ProtectedIntegration(t *testing.T) {
	e2eParallel(t)
	instance, repository := requireProtectedRepository(t, "AC05 protected integration", gitSlotProtectedTask)
	started := time.Now().UTC()
	final := testReleaseSmokeProtectedTask(t, instance, repository,
		smoke.NewNames(instance.runID, smoke.ProtectedTask), "TestE2E_AC05ProtectedIntegration", false)
	instance.record(t, "AC05", "5.5", "TestE2E_AC05ProtectedIntegration", &final, started, time.Now().UTC(), nil)
}

// TestE2E_AC06Recovery proves a terminal runtime failure is never replayed and
// that a fresh execution after the confirmed stop owns a distinct workspace and
// distinct action identities.
func TestE2E_AC06Recovery(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	engineer := instance.smokeToken(t, harness.PersonaEngineer)

	failureDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac06-fail",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac06-failing", "exit 6"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC06 failure pipeline: %v", err)
	}
	failed := instance.smokeWaitComplete(ctx, t, engineer,
		instance.smokeStartInline(ctx, t, engineer, failureDocument, instance.key("ac06-fail")).ExecutionID, 6*time.Minute)
	if failed.State != "failed" {
		t.Fatalf("AC06 deliberate failure state = %q; want failed", failed.State)
	}
	failedAction := actionByName(t, failed, "ac06-fail")
	if failedAction.Stop == nil || failedAction.Stop.State != "confirmed" {
		t.Fatalf("failed action stop evidence = %+v; want confirmed termination", failedAction.Stop)
	}
	if failed.Failure == nil || failed.Failure.Kind != "runtime" || failed.Failure.Reason != "workload_exited" {
		t.Fatalf("failed execution failure = %+v; want runtime/workload_exited", failed.Failure)
	}
	requireOpaqueIdentity(t, "failed cleanup workspace", failed.Cleanup.WorkspaceID)
	if failed.Cleanup.State != "complete" {
		t.Fatalf("failed execution cleanup = %q; want complete", failed.Cleanup.State)
	}

	// No blind replay: a stable re-read keeps the execution, version, action
	// identity and failure unchanged, and the project has exactly one copy.
	before := instance.smokeExecutionIDs(ctx, t, engineer)
	reinspected := instance.smokeStableInspect(ctx, t, engineer, failed.ExecutionID)
	if reinspected.State != failed.State || reinspected.Version != failed.Version {
		t.Fatalf("terminal failure was replayed: before state=%q version=%d after state=%q version=%d",
			failed.State, failed.Version, reinspected.State, reinspected.Version)
	}
	if prior := actionByName(t, reinspected, "ac06-fail"); prior.ActionID != failedAction.ActionID || prior.Version != failedAction.Version {
		t.Fatalf("terminal failure rewrote its action: before=%+v after=%+v", failedAction, prior)
	}
	after := instance.smokeExecutionIDs(ctx, t, engineer)
	instance.smokeRequireNoExecutionWithActions(ctx, t, engineer, before, after, "ac06-fail")

	// A fresh execution after the confirmed stop gets a new workspace and new
	// action identities; it never shares the failed run's resources.
	successDocument, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac06-fresh",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac06-fresh"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC06 fresh pipeline: %v", err)
	}
	fresh := instance.smokeWaitComplete(ctx, t, engineer,
		instance.smokeStartInline(ctx, t, engineer, successDocument, instance.key("ac06-fresh")).ExecutionID, 6*time.Minute)
	requireSucceeded(t, fresh)
	if err := smoke.CheckDistinctWorkspaces(failed.Cleanup.WorkspaceID, fresh.Cleanup.WorkspaceID); err != nil {
		t.Fatalf("fresh execution workspace: %v", err)
	}
	if err := smoke.CheckFreshActionIDs(failed, fresh); err != nil {
		t.Fatalf("fresh execution actions: %v", err)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC06", "6.2", "TestE2E_AC06Recovery", &failed, started, finished, map[string]any{
		"workspace_id": failed.Cleanup.WorkspaceID,
	})
	instance.record(t, "AC06", "6.3", "TestE2E_AC06Recovery", &fresh, started, finished, map[string]any{
		"workspace_id": fresh.Cleanup.WorkspaceID,
	})
}

// TestE2E_AC07Identity proves the four distinct persona principals, their real
// project-role authorities, the uniform project-scope refusal, a tampered
// credential refusal, and that a committed event's scope and a foreign project
// read are enforced by the server rather than the caller.
func TestE2E_AC07Identity(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	seen := map[string]bool{}
	for _, persona := range harness.Personas {
		view := instance.config.Installation.OAuth.Personas[string(persona)]
		if view.PrincipalID == "" {
			t.Fatalf("%s persona has no derived principal id", persona)
		}
		if seen[view.PrincipalID] {
			t.Fatalf("principal %s is shared by two personas", view.PrincipalID)
		}
		seen[view.PrincipalID] = true
	}

	instance.requireAllowed(t, harness.PersonaAdmin, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaEngineer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaReviewer, api.MethodGet, "task.inbox",
		api.TaskInboxRequest{ProjectID: instance.projectID})
	instance.requireAllowed(t, harness.PersonaOperator, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireForbidden(t, harness.PersonaEngineer, api.MethodGet, "resource.binding.read",
		api.ResourceBindingReadRequest{BindingID: "ac07-missing-binding"})
	instance.requireForbidden(t, harness.PersonaReviewer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})

	// A project outside the caller's scope is never readable as if it were the
	// caller's project.
	_, foreignErr := instance.invoke(ctx, instance.smokeToken(t, harness.PersonaEngineer), api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID + "-foreign"}, "", nil)
	requirePublicRefusal(t, foreignErr, api.CodeForbidden, api.CodeNotFound)

	// A tampered credential is unauthenticated, not a weaker authorization.
	tampered := harness.Secret(instance.token.Reveal() + "tampered")
	_, tamperedErr := instance.invoke(ctx, tampered, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID}, "", nil)
	refusal := requirePublicRefusal(t, tamperedErr, api.CodeUnauthenticated)
	if refusal.Status != 401 {
		t.Fatalf("tampered credential status = %d; want HTTP 401", refusal.Status)
	}

	// A committed observation read is scoped by the server, never by the
	// caller's assertion, and another project's event read is refused. The
	// human event.subscribe operation is a documented composition boundary
	// (its credential expiry is transport-boundary supplied, not store-backed),
	// so the scope proof uses the supported event.read interface.
	subjectID := "ac07-" + instance.runID
	instance.smokeCall(ctx, t, instance.smokeToken(t, harness.PersonaEngineer),
		api.MethodPost, "event.publish", api.EventPublishRequest{
			ProjectID:     instance.projectID,
			Type:          "observation.custom",
			SubjectID:     subjectID,
			CorrelationID: instance.projectID,
		}, instance.key("ac07-event"), nil)
	observed := instance.smokeWaitEvent(ctx, t, instance.smokeToken(t, harness.PersonaEngineer),
		subjectID, instance.projectID, time.Minute)
	if observed.Scope.ProjectID != instance.projectID {
		t.Fatalf("event %s scope project = %q; want %q", observed.ID, observed.Scope.ProjectID, instance.projectID)
	}
	if observed.Scope.TenantID != instance.config.Installation.TenantID {
		t.Fatalf("event %s scope tenant = %q; want %q", observed.ID, observed.Scope.TenantID, instance.config.Installation.TenantID)
	}
	_, foreignEventErr := instance.invoke(ctx, instance.smokeToken(t, harness.PersonaEngineer), api.MethodGet, "event.read",
		api.EventReadRequest{ProjectID: instance.projectID + "-foreign"}, "", nil)
	requirePublicRefusal(t, foreignEventErr, api.CodeForbidden, api.CodeNotFound)

	finished := time.Now().UTC()
	instance.record(t, "AC07", "7.2", "TestE2E_AC07Identity", nil, started, finished, nil)
	instance.record(t, "AC07", "7.4", "TestE2E_AC07Identity", nil, started, finished, nil)
}

// TestE2E_AC10OptionalCapabilities proves a plain task-free pipeline runs with
// no Git, gateway, agent or stored pipeline row, and that the documented test
// profile's disabled model and tool surfaces fail closed rather than silently
// succeeding. The Git surface is enabled on this installation, so the Git arm
// of 10.3 is not asserted here; the model/tool arms are, and the other AC
// proofs exercise the served Git path.
func TestE2E_AC10OptionalCapabilities(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	engineer := instance.smokeToken(t, harness.PersonaEngineer)

	document, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac10-plain",
		Image:    instance.image,
		Timeout:  "5m",
		Commands: []string{"echo ac10-plain-ok"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC10 plain pipeline: %v", err)
	}
	admitted := instance.smokeStartInline(ctx, t, engineer, document, instance.key("ac10-plain"))
	final := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 6*time.Minute)
	requireSucceeded(t, final)
	if final.PipelineID != "" || final.TaskID != "" {
		t.Fatalf("plain execution identity = pipeline %q task %q; want both empty", final.PipelineID, final.TaskID)
	}
	if len(final.Actions) != 1 || final.Actions[0].Name != "ac10-plain" {
		t.Fatalf("plain execution actions = %+v; want exactly ac10-plain", final.Actions)
	}
	log := instance.smokeReadLogs(ctx, t, engineer, admitted.ExecutionID, final.Actions[0].ActionID, "stdout")
	if err := smoke.CheckMarker(log, "ac10-plain-ok", 1); err != nil {
		t.Fatalf("plain pipeline marker: %v", err)
	}

	// The test profile installs no model gateway or tool proxy. Both optional
	// surfaces refuse instead of returning fabricated success.
	_, modelErr := instance.invoke(ctx, engineer, api.MethodPost, "model.invoke", api.ModelInvokeRequest{
		ProjectID: instance.projectID,
		Model:     "ac10-unconfigured",
		Messages:  []api.ChatMessage{{Role: "user", Content: "ac10"}},
	}, "", nil)
	requirePublicRefusal(t, modelErr, api.CodeNotFound, api.CodeUnsupportedCapability)
	_, toolErr := instance.invoke(ctx, engineer, api.MethodPost, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: instance.projectID,
		Tool:      "ac10-unconfigured",
		Arguments: json.RawMessage(`{}`),
	}, "", nil)
	requirePublicRefusal(t, toolErr, api.CodeNotFound, api.CodeUnsupportedCapability)

	finished := time.Now().UTC()
	instance.record(t, "AC10", "10.2", "TestE2E_AC10OptionalCapabilities", &final, started, finished, nil)
	instance.record(t, "AC10", "10.3", "TestE2E_AC10OptionalCapabilities", &final, started, finished, nil)
}

// TestE2E_AC14Integrations proves a scoped accepted Git report starts exactly
// one subscribed pipeline for a human push, while an execution-originated push
// on a subscribed ref is reported but starts nothing.
func TestE2E_AC14Integrations(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	requireACGit(t, instance, "AC14 integrations")
	started := time.Now().UTC()
	final := testReleaseSmokeHumanPush(t, instance,
		smoke.NewNames(instance.runID, smoke.HumanPush), "TestE2E_AC14Integrations", false)
	instance.record(t, "AC14", "14.6", "TestE2E_AC14Integrations", &final, started, time.Now().UTC(), nil)
}

// TestE2E_AC15BrokerIndependence proves committed lifecycle evidence keeps its
// event identity and that a forged custom or lifecycle event cannot settle a
// live planned wait.
func TestE2E_AC15BrokerIndependence(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	ca := instance.materializeCA(t)

	document, err := (harness.Document{Actions: []harness.Action{
		{
			Name:     "ac15-prepare",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: ca.commands(`echo ac15-prepared`),
		},
		{
			Name:    "ac15-wait",
			Request: "role/operator",
			Timeout: "10m",
			Subject: map[string]any{"question": "ac15-broker"},
		},
		{
			Name:     "ac15-after",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: ca.commands(`echo ac15-after`),
		},
	}}).Build()
	if err != nil {
		t.Fatalf("build the AC15 pipeline: %v", err)
	}
	admitted := instance.smokeStartInline(ctx, t, engineer, document, instance.key("ac15-start"))
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})
	waiting := instance.smokeWaitActionRunning(ctx, t, engineer, admitted.ExecutionID, "ac15-wait", 6*time.Minute)
	request := actionByName(t, waiting, "ac15-wait")
	if request.Kind != "request" || request.State != "running" {
		t.Fatalf("AC15 wait = kind %q state %q; want request/running", request.Kind, request.State)
	}

	// A custom observation correlated to the execution is committed and replay
	// stable, but it carries no settlement authority.
	eventKey := instance.key("ac15-forged")
	publish := func() api.EventPublishResult {
		result := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
			ProjectID:      instance.projectID,
			Type:           "observation.custom",
			SubjectID:      admitted.ExecutionID,
			SubjectVersion: waiting.Version,
			CorrelationID:  admitted.ExecutionID,
			Data:           []byte(`{"execution_state":"succeeded","decision":"pass"}`),
		}, eventKey, nil)
		return decodeResult[api.EventPublishResult](t, result)
	}
	firstEvent := publish()
	secondEvent := publish()
	if secondEvent.EventID != firstEvent.EventID || !secondEvent.Replayed {
		t.Fatalf("forged observation retry = %+v/%+v; want one replayed event identity", firstEvent, secondEvent)
	}
	afterPublish := instance.smokeStableInspect(ctx, t, engineer, admitted.ExecutionID)
	if afterPublish.State != "running" {
		t.Fatalf("a custom observation settled the execution to %q", afterPublish.State)
	}
	still := actionByName(t, afterPublish, "ac15-wait")
	if still.State != "running" || still.Decision != "" || still.Actor != "" || still.AnsweredAt != nil {
		t.Fatalf("a custom observation settled the request: %+v", still)
	}
	if successor := actionByName(t, afterPublish, "ac15-after"); successor.State != "pending" {
		t.Fatalf("a custom observation activated the successor: %q", successor.State)
	}

	// Lifecycle names cannot be forged through the custom-observation edge.
	for _, forged := range []string{"action.transition", "request.resolved", "execution.succeeded"} {
		_, err := instance.invoke(ctx, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
			ProjectID:     instance.projectID,
			Type:          forged,
			SubjectID:     admitted.ExecutionID,
			CorrelationID: admitted.ExecutionID,
			Data:          []byte(`{"state":"succeeded"}`),
		}, instance.key("ac15-forged-"+strings.ReplaceAll(forged, ".", "-")), nil)
		requirePublicRefusal(t, err, api.CodeInvalidInput)
	}

	// The committed event keeps its identity across a second read, proving the
	// durable control-plane record survives independently of any broker.
	events := instance.smokeEventReadAll(ctx, t, engineer, "observation.custom")
	if got := countEventID(events, firstEvent.EventID); got != 1 {
		t.Fatalf("committed event %s appears %d time(s); want exactly 1", firstEvent.EventID, got)
	}
	again := instance.smokeEventReadAll(ctx, t, engineer, "observation.custom")
	if got := countEventID(again, firstEvent.EventID); got != 1 {
		t.Fatalf("committed event %s appears %d time(s) on re-read; want exactly 1", firstEvent.EventID, got)
	}

	instance.cancelCurrent(t, admitted.ExecutionID, instance.key("ac15-cancel"))
	cancelled := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 6*time.Minute)
	if cancelled.State != "cancelled" {
		t.Fatalf("AC15 execution after cleanup = %q; want cancelled", cancelled.State)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC15", "15.2", "TestE2E_AC15BrokerIndependence", &cancelled, started, finished, map[string]any{
		"event_id": firstEvent.EventID,
	})
	instance.record(t, "AC15", "15.3", "TestE2E_AC15BrokerIndependence", &cancelled, started, finished, map[string]any{
		"event_id": firstEvent.EventID,
	})
}

// TestE2E_AC17ConversationHandoffs proves ask/reply hands work to a distinct
// fresh execution, restores the durable reference, and never reuses the
// cancelled attempt's workspace.
func TestE2E_AC17ConversationHandoffs(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	final := testReleaseSmokeConversationHandoff(t, instance,
		smoke.NewNames(instance.runID, smoke.ConversationHandoff), "TestE2E_AC17ConversationHandoffs", false)
	instance.record(t, "AC17", "17.1", "TestE2E_AC17ConversationHandoffs", &final, started, time.Now().UTC(), nil)
	instance.record(t, "AC17", "17.2", "TestE2E_AC17ConversationHandoffs", &final, started, time.Now().UTC(), nil)
}

// TestE2E_AC18TaskProgress proves an operator-only retry of a failed task keeps
// the failed execution and its action history immutable, starts from action
// zero in a fresh workspace, and completes only on the current success.
func TestE2E_AC18TaskProgress(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	final := testReleaseSmokeOperatorRetry(t, instance,
		smoke.NewNames(instance.runID, smoke.OperatorRetry), "TestE2E_AC18TaskProgress", false)
	instance.record(t, "AC18", "18.1", "TestE2E_AC18TaskProgress", &final, started, time.Now().UTC(), nil)
	instance.record(t, "AC18", "18.2", "TestE2E_AC18TaskProgress", &final, started, time.Now().UTC(), nil)
}

// TestE2E_AC19AuthorizationVerification proves role-appropriate authority, a
// refused cross-project read, and that cancellation revokes the ability to
// settle a planned wait without recording a decision.
func TestE2E_AC19AuthorizationVerification(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	instance.requireAllowed(t, harness.PersonaEngineer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireForbidden(t, harness.PersonaReviewer, api.MethodGet, "execution.list",
		api.ExecutionListRequest{ProjectID: instance.projectID})
	instance.requireForbidden(t, harness.PersonaOperator, api.MethodGet, "pipeline.read",
		api.PipelineReadRequest{ProjectID: instance.projectID, PipelineID: "ac19-missing"})
	instance.requireForbidden(t, harness.PersonaEngineer, api.MethodGet, "resource.binding.read",
		api.ResourceBindingReadRequest{BindingID: "ac19-missing"})

	operator := instance.smokeToken(t, harness.PersonaOperator)
	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	document, err := (harness.Document{Actions: []harness.Action{
		{
			Name:     "ac19-prepare",
			Image:    instance.image,
			Timeout:  "5m",
			Commands: []string{"echo ac19-prepared"},
		},
		{
			Name:    "ac19-wait",
			Request: "role/operator",
			Timeout: "10m",
			Subject: map[string]any{"question": "ac19-revocation"},
		},
	}}).Build()
	if err != nil {
		t.Fatalf("build the AC19 pipeline: %v", err)
	}
	admitted := instance.smokeStartInline(ctx, t, engineer, document, instance.key("ac19-start"))
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})
	waiting := instance.smokeWaitActionRunning(ctx, t, operator, admitted.ExecutionID, "ac19-wait", 6*time.Minute)
	request := actionByName(t, waiting, "ac19-wait")
	item := instance.smokeWaitRequestInbox(ctx, t, operator, admitted.ExecutionID, request.ActionID, 3*time.Minute)

	// An engineer may not settle an operator-assigned request.
	_, roleErr := instance.invoke(ctx, engineer, api.MethodPost, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           admitted.ExecutionID,
		ActionID:              request.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
	}, instance.key("ac19-engineer-resolve"), nil)
	requirePublicRefusal(t, roleErr, api.CodeForbidden)

	instance.cancelCurrent(t, admitted.ExecutionID, instance.key("ac19-cancel"))
	cancelled := instance.smokeWaitComplete(ctx, t, operator, admitted.ExecutionID, 6*time.Minute)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", cancelled.State)
	}
	cancelledRequest := actionByName(t, cancelled, "ac19-wait")
	if cancelledRequest.State != "cancelled" || cancelledRequest.Decision != "" || cancelledRequest.Actor != "" || cancelledRequest.AnsweredAt != nil {
		t.Fatalf("cancelled request retained a decision: %+v", cancelledRequest)
	}

	// Fresh authorization: the cancelled wait cannot be settled afterwards.
	_, lateErr := instance.invoke(ctx, operator, api.MethodPost, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           admitted.ExecutionID,
		ActionID:              request.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
	}, instance.key("ac19-late-resolve"), nil)
	requirePublicRefusal(t, lateErr,
		api.CodeForbidden, api.CodeNotFound, api.CodeVersionConflict, api.CodeDeadlineExpired)
	afterLate := instance.smokeStableInspect(ctx, t, operator, admitted.ExecutionID)
	if afterLate.State != "cancelled" {
		t.Fatalf("late resolve changed the cancelled execution to %q", afterLate.State)
	}
	if lateRequest := actionByName(t, afterLate, "ac19-wait"); lateRequest.Decision != "" || lateRequest.Actor != "" {
		t.Fatalf("late resolve recorded a decision: %+v", lateRequest)
	}

	// The retained prepare stream carries no bearer credential material.
	prepare := actionByName(t, cancelled, "ac19-prepare")
	prepareLog := instance.smokeReadLogs(ctx, t, operator, admitted.ExecutionID, prepare.ActionID, "stdout")
	if err := smoke.CheckMarker(prepareLog, "ac19-prepared", 1); err != nil {
		t.Fatalf("AC19 prepare marker: %v", err)
	}
	requireOpaqueIdentity(t, "AC19 cleanup workspace", cancelled.Cleanup.WorkspaceID)

	finished := time.Now().UTC()
	instance.record(t, "AC19", "19.1", "TestE2E_AC19AuthorizationVerification", &cancelled, started, finished, nil)
	instance.record(t, "AC19", "19.4", "TestE2E_AC19AuthorizationVerification", &cancelled, started, finished, nil)
}

// TestE2E_AC20UserlandGit proves the served Git service enforces the committed
// admin policy: an authenticated host read, an allowed work-ref push and force
// rewind, a refused protected push and deletion while the protected ref stays
// at the reviewed base, and the documented refusal of a ref delete.
func TestE2E_AC20UserlandGit(t *testing.T) {
	e2eParallel(t)
	instance, repository := requireProtectedRepository(t, "AC20 userland Git", gitSlotUserlandGit)
	adminRepository := instance.smokeAdminRepository(t)
	const targetRef = protectedTargetRef
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	reviewer := instance.smokeToken(t, harness.PersonaReviewer)
	ca := instance.materializeCA(t)
	names := smoke.NewNames(instance.runID, smoke.Scenario("UserlandGit"))
	workRef := names.WorkRef("userland")

	// The admin read proves the committed policy is the served one, and the
	// protected base gives the later refusals a real non-empty ref.
	adminGit := instance.newHostGit(t, harness.PersonaAdmin, "ac20-admin")
	adminDirectory := adminGit.dir + "/admin"
	adminGit.clone(ctx, t, smokeRepositoryURL(instance.config.GitURL(), adminRepository.Name), adminDirectory)
	policy, err := readHostFile(adminDirectory + "/conf/git.conf")
	if err != nil {
		t.Fatalf("read the served admin policy: %v", err)
	}
	if err := smoke.CheckRepositoryPolicy(policy, repository.Name); err != nil {
		t.Fatalf("served admin policy: %v", err)
	}
	engineerGit := instance.newHostGit(t, harness.PersonaEngineer, "ac20-engineer")
	base, _, seedFinal, seeded := instance.smokeEnsureProtectedRepository(ctx, t, names, "TestE2E_AC20UserlandGit-Seed",
		repository, engineer, reviewer, engineerGit, ca, targetRef, names.WorkRef("seed"))

	// A host engineer push to the granted work prefix is accepted and readable.
	// The candidate is a child of the current protected base so the later direct
	// push is a genuine fast-forward that only the approval rule can refuse.
	workDirectory := engineerGit.dir + "/work"
	engineerGit.init(ctx, t, workDirectory)
	url := smokeRepositoryURL(instance.config.GitURL(), repository.Name)
	engineerGit.fetch(ctx, t, workDirectory, url, targetRef)
	engineerGit.mustRun(ctx, t, workDirectory, "reset", "--hard", "FETCH_HEAD")
	candidate := engineerGit.commit(ctx, t, workDirectory, "ac20.txt", names.Marker("work")+"\n", "AC20 work commit")
	if _, err := engineerGit.push(ctx, workDirectory, url, candidate+":"+workRef); err != nil {
		t.Fatalf("the granted work-ref push was refused: %v", err)
	}
	if got := engineerGit.lsRemote(ctx, t, url, workRef); got != candidate {
		t.Fatalf("work ref = %q; want the pushed %q", got, candidate)
	}

	// The protected ref refuses the unapproved candidate and the deletion; both
	// are update-hook refusals, not transport or authentication failures.
	protectedOutput, protectedErr := engineerGit.push(ctx, workDirectory, url, candidate+":"+targetRef)
	if protectedErr == nil {
		t.Fatalf("the host engineer's unapproved push to %s was accepted", targetRef)
	}
	if err := smoke.CheckHookRejected(protectedOutput); err != nil {
		t.Fatalf("protected push refusal: %v; output=%s", err, scanSafeHostGitOutput(protectedOutput, engineerGit.secrets))
	}
	if got := engineerGit.lsRemote(ctx, t, url, targetRef); got != base {
		t.Fatalf("protected ref after the refused push = %q; want the reviewed base %q", got, base)
	}
	deleteOutput, deleteErr := engineerGit.push(ctx, workDirectory, url, ":"+targetRef)
	if deleteErr == nil {
		t.Fatalf("the host engineer's deletion of %s was accepted", targetRef)
	}
	if err := smoke.CheckHookRejected(deleteOutput); err != nil {
		t.Fatalf("protected delete refusal: %v; output=%s", err, scanSafeHostGitOutput(deleteOutput, engineerGit.secrets))
	}
	if got := engineerGit.lsRemote(ctx, t, url, targetRef); got != base {
		t.Fatalf("protected ref after the refused deletion = %q; want %q", got, base)
	}

	// RW+ on the work prefix is real: the same engineer may force-rewind its
	// own work ref, so the protected refusals above are the protected rule,
	// not a blanket deny. Ref deletes are a documented boundary: the receive
	// journal cannot represent one, so every delete is refused even where
	// RW+ grants it. Assert that explicit refusal, not a deletion.
	rewindOutput, rewindErr := engineerGit.push(ctx, workDirectory, url, "+"+base+":"+workRef)
	if rewindErr != nil {
		t.Fatalf("the granted work-ref force rewind was refused: %v; output=%s", rewindErr, scanSafeHostGitOutput(rewindOutput, engineerGit.secrets))
	}
	if got := engineerGit.lsRemote(ctx, t, url, workRef); got != base {
		t.Fatalf("work ref after the granted rewind = %q; want %q", got, base)
	}
	deleteWorkOutput, deleteWorkErr := engineerGit.push(ctx, workDirectory, url, ":"+workRef)
	if deleteWorkErr == nil {
		t.Fatalf("the work-ref deletion was accepted; ref deletes must be refused")
	}
	if err := smoke.CheckHookRejected(deleteWorkOutput); err != nil {
		t.Fatalf("work-ref delete refusal: %v; output=%s", err, scanSafeHostGitOutput(deleteWorkOutput, engineerGit.secrets))
	}
	if got := engineerGit.lsRemote(ctx, t, url, workRef); got != base {
		t.Fatalf("work ref after the refused deletion = %q; want %q", got, base)
	}

	finished := time.Now().UTC()
	var evidence *api.ExecutionInspectResult
	if seeded {
		evidence = &seedFinal
	}
	instance.record(t, "AC20", "20.2", "TestE2E_AC20UserlandGit", evidence, started, finished, map[string]any{
		"target_ref": targetRef,
		"candidate":  base,
	})
}

// countString counts exact occurrences of want in values.
func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

// countEventID counts exact occurrences of one event identity.
func countEventID(events []api.Event, want string) int {
	count := 0
	for _, event := range events {
		if event.ID == want {
			count++
		}
	}
	return count
}
