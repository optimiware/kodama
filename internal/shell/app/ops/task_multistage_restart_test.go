package opsapp_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/identity"
	storeexecution "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

func applyNamedPipeline(t *testing.T, h *harness, name, yamlDoc string) string {
	t.Helper()
	res, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "pipeline.apply",
		Credential: testToken,
		Params:     map[string]string{"project": testProject, "pipeline": name},
		Request: api.Request{
			Input:          []byte(jsonBody(t, map[string]any{"name": name, "yaml": []byte(yamlDoc)})),
			IdempotencyKey: "apply-" + name,
		},
	})
	if err != nil {
		t.Fatalf("pipeline.apply %q: %v", name, err)
	}
	var applied api.PipelineApplyResult
	decodeResult(t, res, &applied)
	return applied.PipelineID
}

const stageOneYAML = `inputs: [repository]
actions:
  - name: implement
    image: registry.example.com/tool@sha256:1111111111111111111111111111111111111111111111111111111111111111
    commands:
      - implement
`

const stageTwoReviewYAML = `inputs: [repository, branch]
actions:
  - name: build
    image: registry.example.com/tool@sha256:2222222222222222222222222222222222222222222222222222222222222222
    commands:
      - build
  - name: review
    request: role/reviewer
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: {from: inputs.branch}
    outputs: [approved]
`

func TestTaskStartAtomicStageTransitionAndAuthorization(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	stage1ID := applyNamedPipeline(t, h, "implement-stage", stageOneYAML)
	stage2ID := applyNamedPipeline(t, h, "integrate-stage", stageTwoReviewYAML)

	// Add an operator-only principal to verify pipeline-selection authorization.
	operator := identity.Principal{
		ID:      "operator-only",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	}
	h.principals.principals["token-operator"] = operator
	h.seedMembership(t, scope, "membership-operator-only", operator.ID, 1, tenancy.RoleOperator)

	// Create and start Stage 1.
	csRaw, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Two-stage feature",
		"pipeline_id": stage1ID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start: %v", err)
	}
	var cs api.TaskCreateAndStartResult
	decodeResult(t, csRaw, &cs)

	// Complete Stage 1 -> task becomes done at lifecycle version 3.
	if _, err := h.taskService.Progress(context.Background(), task.ProgressCommand{
		Scope: scope, TaskID: id.TaskID(cs.TaskID), ExecutionID: id.ExecutionID(cs.ExecutionID), Outcome: task.ProgressSucceeded,
	}); err != nil {
		t.Fatalf("Progress to done: %v", err)
	}

	// Operator-only cannot switch pipeline on task.start.
	_, err = h.invokeVersion(t, "token-operator", "task.start", jsonBody(t, map[string]any{
		"pipeline_id": stage2ID,
		"inputs":      map[string]string{"repository": "shop/web", "branch": "work/1"},
	}), 3, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("operator stage switch err = %v; want forbidden", err)
	}

	// Engineer atomically transitions the done task to Stage 2 with new inputs.
	stage2Raw, err := h.invokeVersion(t, testToken, "task.start", jsonBody(t, map[string]any{
		"pipeline_id": stage2ID,
		"inputs":      map[string]string{"repository": "shop/web", "branch": "work/1"},
	}), 3, map[string]string{"task": cs.TaskID})
	if err != nil {
		t.Fatalf("stage 2 task.start: %v", err)
	}
	var stage2 api.TaskStartResult
	decodeResult(t, stage2Raw, &stage2)
	if stage2.ExecutionID == "" || stage2.ExecutionID == cs.ExecutionID || stage2.LifecycleVersion != 4 {
		t.Fatalf("stage 2 start result = %+v", stage2)
	}

	// Verify task reopened to open with updated selected_pipeline and inputs.
	readRaw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": cs.TaskID})
	if err != nil {
		t.Fatalf("task.read: %v", err)
	}
	var read api.TaskReadResult
	decodeResult(t, readRaw, &read)
	if read.State != "open" || read.SelectedPipeline != stage2ID || read.Inputs["branch"] != "work/1" || read.CurrentExecution != stage2.ExecutionID {
		t.Fatalf("task after stage transition = %+v", read)
	}
}

func TestTaskRestartSupersedesPendingReviewRequestAndPreservesIdempotency(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	stage2ID := applyNamedPipeline(t, h, "integrate-stage", stageTwoReviewYAML)

	csRaw, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Integrate feature",
		"pipeline_id": stage2ID,
		"inputs":      map[string]string{"repository": "shop/web", "branch": "work/1"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start: %v", err)
	}
	var cs api.TaskCreateAndStartResult
	decodeResult(t, csRaw, &cs)

	// Advance execution 1 through action 0 (build) into waiting at action 1 (review request).
	engAdapter := storeexecution.NewEngine(h.store.Controller())
	pre, err := h.execs.Load(context.Background(), scope, id.ExecutionID(cs.ExecutionID))
	if err != nil {
		t.Fatalf("Load execution 1: %v", err)
	}
	_, act0, err := flow.Reduce(pre, flow.ActivateFact{ExecutionID: pre.ID, ActionID: pre.Actions[0].ID, At: fixedNow})
	if err != nil {
		t.Fatalf("activate 0: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *act0); err != nil {
		t.Fatalf("commit activate 0: %v", err)
	}
	pre, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(cs.ExecutionID))
	_, done0, err := flow.Reduce(pre, flow.ObserveFact{
		ExecutionID: pre.ID, ActionID: pre.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("done")),
		At:          fixedNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("observe 0: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *done0); err != nil {
		t.Fatalf("commit observe 0: %v", err)
	}
	pre, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(cs.ExecutionID))
	_, act1, err := flow.Reduce(pre, flow.ActivateFact{
		ExecutionID: pre.ID, ActionID: pre.Actions[1].ID,
		At: fixedNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("activate review: %v", err)
	}
	if _, err := engAdapter.Commit(context.Background(), *act1); err != nil {
		t.Fatalf("commit activate review: %v", err)
	}

	// Confirm the review request is pending in the inbox before restart.
	inboxBeforeRaw, err := h.invoke(t, testToken, "task.inbox", `{"kinds":["request"]}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox before restart: %v", err)
	}
	var inboxBefore api.TaskInboxResult
	decodeResult(t, inboxBeforeRaw, &inboxBefore)
	if len(inboxBefore.Items) != 1 || inboxBefore.Items[0].ExecutionID != cs.ExecutionID {
		t.Fatalf("inbox before restart = %+v; want 1 pending review request", inboxBefore.Items)
	}

	// Invoke task.restart via MCP (no REST params, explicit task_id in input) with fixed idempotency key.
	version := cs.LifecycleVersion
	restartInput := jsonBody(t, map[string]any{
		"task_id": cs.TaskID,
		"reason":  "rebased onto latest main",
		"inputs":  map[string]string{"repository": "shop/web", "branch": "work/1-rebased"},
	})
	restartRes, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "task.restart",
		Credential: testToken,
		Request: api.Request{
			Input:           []byte(restartInput),
			ExpectedVersion: &version,
			IdempotencyKey:  "restart-idem-1",
		},
	})
	if err != nil {
		t.Fatalf("task.restart: %v", err)
	}
	var restarted api.TaskRestartResult
	decodeResult(t, restartRes, &restarted)
	if restarted.ExecutionID == "" || restarted.SupersededExecution != cs.ExecutionID || restarted.LifecycleVersion != cs.LifecycleVersion+1 {
		t.Fatalf("task.restart result = %+v", restarted)
	}

	// Inspect superseded execution: cancelled with failure=cancelled/superseded/reason and NO decision=fail.
	inspectedRaw, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": cs.ExecutionID})
	if err != nil {
		t.Fatalf("execution.inspect superseded: %v", err)
	}
	var inspected api.ExecutionInspectResult
	decodeResult(t, inspectedRaw, &inspected)
	if inspected.State != "cancelled" {
		t.Fatalf("superseded execution state = %q; want cancelled", inspected.State)
	}
	if inspected.Failure == nil || inspected.Failure.Kind != "cancelled" || inspected.Failure.Reason != "superseded" || inspected.Failure.Detail != "rebased onto latest main" {
		t.Fatalf("superseded failure = %+v; want cancelled/superseded/reason", inspected.Failure)
	}
	reviewAction := inspected.Actions[1]
	if reviewAction.State != "cancelled" || reviewAction.Decision != "" {
		t.Fatalf("superseded review action = state:%q decision:%q; want cancelled with no decision", reviewAction.State, reviewAction.Decision)
	}

	// Pending review request must no longer appear in inbox.
	inboxAfterRaw, err := h.invoke(t, testToken, "task.inbox", `{"kinds":["request"]}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.inbox after restart: %v", err)
	}
	var inboxAfter api.TaskInboxResult
	decodeResult(t, inboxAfterRaw, &inboxAfter)
	if len(inboxAfter.Items) != 0 {
		t.Fatalf("inbox after restart still has %+v; want empty", inboxAfter.Items)
	}

	// Identical idempotent replay returns the exact same result.
	replayRes, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "task.restart",
		Credential: testToken,
		Request: api.Request{
			Input:           []byte(restartInput),
			ExpectedVersion: &version,
			IdempotencyKey:  "restart-idem-1",
		},
	})
	if err != nil {
		t.Fatalf("task.restart replay: %v", err)
	}
	var replayed api.TaskRestartResult
	decodeResult(t, replayRes, &replayed)
	if replayed != restarted {
		t.Fatalf("replayed = %+v; want %+v", replayed, restarted)
	}

	// Cancel task -> subsequent task.restart must be refused with version_conflict.
	cancelledRaw, err := h.invokeVersion(t, testToken, "task.cancel", `{}`, restarted.LifecycleVersion, map[string]string{"task": cs.TaskID})
	if err != nil {
		t.Fatalf("task.cancel: %v", err)
	}
	var cancelled api.TaskCancelResult
	decodeResult(t, cancelledRaw, &cancelled)

	_, err = h.invokeVersion(t, testToken, "task.restart", jsonBody(t, map[string]any{
		"reason": "cannot restart cancelled task",
	}), cancelled.LifecycleVersion, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("restart on cancelled task err = %v; want version_conflict", err)
	}
}
