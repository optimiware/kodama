//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// testReleaseSmokeRequestWait is S1. A stored pipeline writes a marker and the
// execution identity, waits on a role/operator request, and continues in the
// same execution and workspace after the operator resolves through its own
// inbox. The stored form is deliberate: the execution is pipeline-attributed,
// so the settlement's no-new-execution proof can read the execution history
// scoped to that pipeline instead of the unsafe project-wide set.
func testReleaseSmokeRequestWait(t *testing.T, instance *live, names smoke.Names, probe string, recordScenario bool) api.ExecutionInspectResult {
	t.Helper()
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	operator := instance.smokeToken(t, harness.PersonaOperator)
	operatorPrincipal := smokePersonaPrincipal(t, instance, harness.PersonaOperator)
	ca := instance.materializeCA(t)

	prepareMarker := names.Marker("prepare")
	preparedMarker := names.Marker("prepared")
	continuationMarker := names.Marker("continuation")
	answer := names.Answer()
	document, err := (harness.Document{
		Actions: []harness.Action{
			{
				Name:    "prepare",
				Image:   instance.image,
				Timeout: "5m",
				Env: smokeMergeEnv(ca.environment(), map[string]string{
					"SMOKE_MARKER":        prepareMarker,
					"SMOKE_PREPARE_READY": preparedMarker,
				}),
				Commands: ca.commands(
					requestWaitPrepareScript,
				),
			},
			{
				Name:    "answer",
				Request: "role/operator",
				Timeout: "10m",
				Subject: map[string]any{"question": names.Marker("question")},
				Outputs: []string{"answer"},
			},
			{
				Name:    "continue",
				Image:   instance.image,
				Timeout: "5m",
				Env: smokeMergeEnv(ca.environment(), map[string]string{
					"SMOKE_MARKER":       prepareMarker,
					"SMOKE_CONTINUATION": continuationMarker,
					"SMOKE_ANSWER":       answer,
				}),
				Commands: ca.commands(
					requestWaitContinueScript,
				),
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build the S1 request-wait pipeline: %v", err)
	}

	// Apply the document and start it by identity. An inline start carries no
	// pipeline attribution, which would make the scoped execution-set proof
	// below vacuous; the stored start is what exercises it live.
	pipelineName := names.PipelineName("request-wait")
	applied := instance.smokeApplyPipeline(ctx, t, engineer, pipelineName, document, names.Key("apply"))
	pipelineVersion := applied.Version
	admittedResult := instance.smokeCall(ctx, t, engineer, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  instance.projectID,
		PipelineID: applied.PipelineID,
	}, names.Key("start"), &pipelineVersion)
	admitted := decodeResult[api.PipelineStartResult](t, admittedResult)
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})

	waiting := instance.smokeWaitActionRunning(ctx, t, engineer, admitted.ExecutionID, "answer", 6*time.Minute)
	if waiting.State != "running" {
		t.Fatalf("the waiting execution state = %q; want running", waiting.State)
	}
	prepare := actionByName(t, waiting, "prepare")
	if prepare.State != "succeeded" {
		t.Fatalf("prepare action state = %q; want succeeded before the request is answerable", prepare.State)
	}
	if prepare.Stop == nil || prepare.Stop.State != "confirmed" {
		t.Fatalf("prepare stop evidence = %+v; want confirmed termination", prepare.Stop)
	}
	requestAction := actionByName(t, waiting, "answer")
	if requestAction.Kind != "request" || requestAction.State != "running" {
		t.Fatalf("request action = kind %q state %q; want request/running", requestAction.Kind, requestAction.State)
	}
	if requestAction.Assignment != "role/operator" {
		t.Fatalf("request assignment = %q; want role/operator", requestAction.Assignment)
	}
	if got, ok := smokeSubjectValue(requestAction.Subject, "question"); !ok || got != names.Marker("question") {
		t.Fatalf("request subject question = %q (present %v); want the run-unique question", got, ok)
	}
	if requestAction.Deadline == nil || requestAction.ActivatedAt == nil {
		t.Fatalf("request deadline/activation = %v/%v; want both persisted", requestAction.Deadline, requestAction.ActivatedAt)
	}
	if successor := actionByName(t, waiting, "continue"); successor.State != "pending" {
		t.Fatalf("request successor state = %q; want pending", successor.State)
	}
	if waiting.Cleanup.State == "complete" {
		t.Fatal("a running request wait reported cleanup=complete")
	}

	operatorInbox := instance.smokeTaskInbox(ctx, t, operator, "request")
	item, ok := smokeFindRequestInbox(operatorInbox, admitted.ExecutionID, requestAction.ActionID)
	if !ok {
		t.Fatalf("operator inbox has no request %s/%s: %+v", admitted.ExecutionID, requestAction.ActionID, operatorInbox.Items)
	}
	if item.Assignment != "role/operator" {
		t.Fatalf("operator inbox assignment = %q; want role/operator", item.Assignment)
	}
	if err := smoke.CheckMutationVersion(item.Version, requestAction.Version); err != nil {
		t.Fatalf("operator inbox action version: %v", err)
	}
	if got, ok := smokeSubjectValue(item.Subject, "question"); !ok || got != names.Marker("question") {
		t.Fatalf("operator inbox question = %q (present %v); want the run-unique question", got, ok)
	}
	if len(item.Outputs) != 1 || item.Outputs[0] != "answer" {
		t.Fatalf("operator inbox declared outputs = %v; want [answer]", item.Outputs)
	}
	engineerInbox := instance.smokeTaskInbox(ctx, t, engineer, "request")
	if _, found := smokeFindRequestInbox(engineerInbox, admitted.ExecutionID, requestAction.ActionID); found {
		t.Fatalf("engineer inbox exposed the operator-assigned request: %+v", engineerInbox.Items)
	}

	// The grant stage admits every human role for request resolution, so the
	// in-commit assignment check is the only authority. The engineer must not be
	// able to settle the operator-assigned request: the refusal is the public
	// forbidden class and commits nothing.
	engineerResolve := api.ExecutionRequestResolveRequest{
		ExecutionID:           admitted.ExecutionID,
		ActionID:              requestAction.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"answer": answer},
	}
	_, refusalErr := instance.invoke(ctx, engineer, api.MethodPost, "execution.request.resolve", engineerResolve, names.Key("engineer-resolve"), nil)
	var refusal *harness.APIError
	if !errors.As(refusalErr, &refusal) {
		t.Fatalf("engineer resolve of the operator request = %v; want the public forbidden refusal", refusalErr)
	}
	if refusal.Status != 403 || refusal.Code != api.CodeForbidden {
		t.Fatalf("engineer resolve refusal = status %d code %q; want HTTP 403 %q", refusal.Status, refusal.Code, api.CodeForbidden)
	}
	afterEngineerRefusal := instance.smokeInspect(ctx, t, engineer, admitted.ExecutionID)
	stillWaiting := actionByName(t, afterEngineerRefusal, "answer")
	if stillWaiting.State != "running" {
		t.Fatalf("engineer refusal left the request action state = %q; want running", stillWaiting.State)
	}
	if stillWaiting.Version != requestAction.Version {
		t.Fatalf("engineer refusal advanced the request action version %d -> %d", requestAction.Version, stillWaiting.Version)
	}
	if len(stillWaiting.Outputs) != 0 {
		t.Fatalf("engineer refusal staged outputs: %+v", stillWaiting.Outputs)
	}
	if stillWaiting.Decision != "" || stillWaiting.Actor != "" || stillWaiting.AnsweredAt != nil {
		t.Fatalf("engineer refusal recorded a decision: %+v", stillWaiting)
	}

	// The project-wide execution set is not a valid isolate on a shared
	// installation: an unrelated background execution (for example a held push
	// rescan left by an earlier test) can legitimately start inside the same
	// window. Scope the no-new-execution proof to the execution under test and,
	// when the installation attributes it to a task or stored pipeline, to that
	// scope's execution history.
	beforeResolve := instance.smokeInspect(ctx, t, engineer, admitted.ExecutionID)
	if beforeResolve.State != "running" {
		t.Fatalf("execution state before resolution = %q; want running", beforeResolve.State)
	}
	if beforeResolve.PipelineID != applied.PipelineID {
		t.Fatalf("execution pipeline attribution = %q; want the stored pipeline %q", beforeResolve.PipelineID, applied.PipelineID)
	}
	scopedBefore, scoped := instance.smokeScopedExecutionIDs(ctx, t, engineer, beforeResolve.TaskID, beforeResolve.PipelineID)
	if !scoped {
		t.Fatalf("stored pipeline execution %s has no task/pipeline attribution; the scoped execution-set check cannot run", beforeResolve.ExecutionID)
	}
	var taskBefore api.TaskReadResult
	if beforeResolve.TaskID != "" {
		taskBefore = instance.smokeTaskRead(ctx, t, engineer, beforeResolve.TaskID)
	} else {
		t.Logf("settlement is pipeline-scoped and task-free: the task-history branch is not applicable")
	}
	resolveRequest := api.ExecutionRequestResolveRequest{
		ExecutionID:           admitted.ExecutionID,
		ActionID:              requestAction.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"answer": answer},
	}
	resolved := instance.smokeResolve(ctx, t, operator, resolveRequest, names.Key("resolve"))
	replayed := instance.smokeResolve(ctx, t, operator, resolveRequest, names.Key("resolve"))
	if err := smoke.CheckReplayConverged(resolved, replayed); err != nil {
		t.Fatalf("identical resolve replay: %v", err)
	}

	final := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 6*time.Minute)
	requireSucceeded(t, final)
	if final.ExecutionID != admitted.ExecutionID {
		t.Fatalf("resolution continued execution %q; want the admitted %q", final.ExecutionID, admitted.ExecutionID)
	}
	finalRequest := actionByName(t, final, "answer")
	if err := smoke.CheckDecisionActor(finalRequest, "pass", operatorPrincipal); err != nil {
		t.Fatalf("resolved request: %v", err)
	}
	if finalRequest.Outputs["answer"] != answer {
		t.Fatalf("promoted answer output = %q; want the operator answer", finalRequest.Outputs["answer"])
	}
	// The immutable facts observed while waiting stay immutable. The action
	// version advances by design when the settlement commits; the version the
	// operator resolved against was the waiting action version.
	if finalRequest.ActionID != requestAction.ActionID {
		t.Fatalf("request action identity changed from %q to %q", requestAction.ActionID, finalRequest.ActionID)
	}
	if finalRequest.Assignment != requestAction.Assignment {
		t.Fatalf("request assignment changed from %q to %q", requestAction.Assignment, finalRequest.Assignment)
	}
	if err := smoke.CheckSubjectEqual(requestAction.Subject, finalRequest.Subject); err != nil {
		t.Fatalf("request subject changed after resolution: %v", err)
	}
	if requestAction.Version != item.Version {
		t.Fatalf("waiting action version %d differs from the resolved expected version %d", requestAction.Version, item.Version)
	}
	if finalRequest.Version <= item.Version {
		t.Fatalf("settled action version = %d; want > the resolved version %d (settlement advances it by design)", finalRequest.Version, item.Version)
	}
	continuation := actionByName(t, final, "continue")
	if continuation.State != "succeeded" {
		t.Fatalf("continuation action state = %q; want succeeded", continuation.State)
	}
	prepareLog := instance.smokeReadLogs(ctx, t, engineer, admitted.ExecutionID, prepare.ActionID, "stdout")
	if err := smoke.CheckMarker(prepareLog, preparedMarker, 1); err != nil {
		t.Fatalf("prepare marker: %v", err)
	}
	continuationLog := instance.smokeReadLogs(ctx, t, engineer, admitted.ExecutionID, continuation.ActionID, "stdout")
	if err := smoke.CheckMarker(continuationLog, continuationMarker, 1); err != nil {
		t.Fatalf("continuation marker: %v", err)
	}
	requireOpaqueIdentity(t, "cleanup workspace", final.Cleanup.WorkspaceID)

	// The settlement must continue the exact execution the decision was issued
	// against: same execution and actions, advanced versions, no execution
	// added for the task/pipeline, and no task history change.
	if err := smoke.CheckExecutionContinued(beforeResolve, final); err != nil {
		t.Fatalf("resolution did not continue the same execution: %v", err)
	}
	if resolved.ExecutionVersion <= beforeResolve.Version {
		t.Fatalf("resolve reported execution version %d; want > the %d observed before the decision", resolved.ExecutionVersion, beforeResolve.Version)
	}
	if final.Version < resolved.ExecutionVersion {
		t.Fatalf("final execution version %d is below the resolve version %d", final.Version, resolved.ExecutionVersion)
	}
	if scoped {
		scopedAfter, afterScoped := instance.smokeScopedExecutionIDs(ctx, t, engineer, beforeResolve.TaskID, beforeResolve.PipelineID)
		if !afterScoped {
			t.Fatalf("scoped execution list lost its task/pipeline attribution after resolution")
		}
		if err := smoke.CheckScopedExecutionSet(scopedBefore, scopedAfter, beforeResolve.ExecutionID); err != nil {
			t.Fatalf("resolution added an execution for the task/pipeline: %v", err)
		}
		t.Logf("settlement scoped to pipeline %q: execution set %v unchanged, continued %s",
			beforeResolve.PipelineID, scopedAfter, beforeResolve.ExecutionID)
	}
	if beforeResolve.TaskID != "" {
		taskAfter := instance.smokeTaskRead(ctx, t, engineer, beforeResolve.TaskID)
		if taskAfter.CurrentExecution != taskBefore.CurrentExecution {
			t.Fatalf("resolution changed the task current execution %q -> %q", taskBefore.CurrentExecution, taskAfter.CurrentExecution)
		}
		if taskAfter.LifecycleVersion != taskBefore.LifecycleVersion {
			t.Fatalf("resolution changed the task lifecycle version %d -> %d", taskBefore.LifecycleVersion, taskAfter.LifecycleVersion)
		}
		if err := smoke.CheckExactHistory(taskAfter.ExecutionHistory, taskBefore.ExecutionHistory...); err != nil {
			t.Fatalf("resolution changed the task history: %v", err)
		}
	}

	finished := time.Now().UTC()
	if recordScenario {
		instance.recordScenario(t, "RequestWait", probe, &final, started, finished, map[string]any{
			"request_id":   finalRequest.ActionID,
			"workspace_id": final.Cleanup.WorkspaceID,
			"decision":     "pass",
			"actor":        operatorPrincipal,
		})
	}
	return final
}
