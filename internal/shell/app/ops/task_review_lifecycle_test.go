package opsapp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/store"
	storeexecution "github.com/optimiware/kodema/internal/shell/store/units/execution"
	"github.com/optimiware/kodema/internal/shell/task"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

const stageNoCompleteYAML = `completes_task: false
inputs: [repository]
actions:
  - name: implement
    image: registry.example.com/tool@sha256:1111111111111111111111111111111111111111111111111111111111111111
    commands:
      - implement
`

const reviewFeedbackYAML = `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      operation: git.ref.update
      resource: {from: inputs.repository}
      ref: refs/heads/main
      candidate: sha-candidate-1
    outputs: [feedback]
  - name: integrate
    image: registry.example.com/tool@sha256:3333333333333333333333333333333333333333333333333333333333333333
    commands:
      - integrate
`

func invokeKey(t *testing.T, h *harness, token string, op authorize.OperationName, key, body string, version *uint64, params map[string]string) (api.Result, error) {
	t.Helper()
	return h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  op,
		Credential: token,
		Params:     params,
		Request: api.Request{
			Input:           []byte(body),
			ExpectedVersion: version,
			IdempotencyKey:  key,
		},
	})
}

func completeSingleContainerExecution(t *testing.T, h *harness, scope tenancy.Scope, execID id.ExecutionID, exitCode int) {
	t.Helper()
	eng := storeexecution.NewEngine(h.store.Controller())
	pre, err := h.execs.Load(context.Background(), scope, execID)
	if err != nil {
		t.Fatalf("Load execution %s: %v", execID, err)
	}
	_, act, err := flow.Reduce(pre, flow.ActivateFact{ExecutionID: pre.ID, ActionID: pre.Actions[0].ID, At: fixedNow})
	if err != nil {
		t.Fatalf("activate %s: %v", execID, err)
	}
	if _, err := eng.Commit(context.Background(), *act); err != nil {
		t.Fatalf("commit activate %s: %v", execID, err)
	}
	pre, err = h.execs.Load(context.Background(), scope, execID)
	if err != nil {
		t.Fatalf("reload %s: %v", execID, err)
	}
	_, done, err := flow.Reduce(pre, flow.ObserveFact{
		ExecutionID: pre.ID,
		ActionID:    pre.Actions[0].ID,
		Observation: kernel.ExitedObservation(exitCode, kernel.ConfirmedStop("exited")),
		At:          fixedNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("observe %s: %v", execID, err)
	}
	if _, err := eng.Commit(context.Background(), *done); err != nil {
		t.Fatalf("commit observe %s: %v", execID, err)
	}
}

func TestCompletesTaskFalseAndExplicitTaskCompleteLifecycle(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	// Seed additional role-specific principals to verify authorization rules on task.complete.
	addPrincipal := func(token, principalID string, role tenancy.Role) {
		p := identity.Principal{
			ID:      id.PrincipalID(principalID),
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{role},
			Enabled: true,
			Version: 1,
		}
		h.principals.principals[token] = p
		h.seedMembership(t, scope, "mem-"+principalID, p.ID, 1, role)
	}
	addPrincipal("token-operator", "operator-1", tenancy.RoleOperator)
	addPrincipal("token-viewer", "viewer-1", tenancy.RoleViewer)
	addPrincipal("token-reviewer", "reviewer-1", tenancy.RoleReviewer)
	addPrincipal("token-admin", "admin-only-1", tenancy.RoleAdmin)

	noCompletePipeID := applyNamedPipeline(t, h, "stage-no-complete", stageNoCompleteYAML)
	defaultPipeID := applyNamedPipeline(t, h, "stage-default", stageOneYAML)

	// 1. Pipeline with completes_task: false keeps task open after execution succeeds.
	csRaw, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Multi-stage work",
		"pipeline_id": noCompletePipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start: %v", err)
	}
	var cs api.TaskCreateAndStartResult
	decodeResult(t, csRaw, &cs)

	// Even if the stored pipeline is edited to completes_task: true after admission,
	// the in-flight execution retains its snapshotted completes_task: false!
	_, err = h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "pipeline.apply",
		Credential: testToken,
		Params:     map[string]string{"project": testProject, "pipeline": "stage-no-complete"},
		Request: api.Request{
			Input:          []byte(jsonBody(t, map[string]any{"name": "stage-no-complete", "yaml": []byte(stageOneYAML)})),
			IdempotencyKey: "edit-stage-no-complete",
		},
	})
	if err != nil {
		t.Fatalf("pipeline.apply edit: %v", err)
	}

	// While execution is queued (nonterminal), task.complete must refuse!
	_, err = h.invokeVersion(t, testToken, "task.complete", jsonBody(t, map[string]any{
		"reason": "too early queued",
	}), cs.LifecycleVersion, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("task.complete while queued err = %v; want version_conflict", err)
	}

	// Activate action 0 so the execution is running (nonterminal); task.complete must still refuse!
	eng := storeexecution.NewEngine(h.store.Controller())
	preCS, err := h.execs.Load(context.Background(), scope, id.ExecutionID(cs.ExecutionID))
	if err != nil {
		t.Fatalf("Load execution %s: %v", cs.ExecutionID, err)
	}
	_, actCS, err := flow.Reduce(preCS, flow.ActivateFact{ExecutionID: preCS.ID, ActionID: preCS.Actions[0].ID, At: fixedNow})
	if err != nil {
		t.Fatalf("activate %s: %v", cs.ExecutionID, err)
	}
	if _, err := eng.Commit(context.Background(), *actCS); err != nil {
		t.Fatalf("commit activate %s: %v", cs.ExecutionID, err)
	}
	_, err = h.invokeVersion(t, testToken, "task.complete", jsonBody(t, map[string]any{
		"reason": "too early running",
	}), cs.LifecycleVersion, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("task.complete while running err = %v; want version_conflict", err)
	}

	// Succeed the running execution and run Progress: task MUST remain open!
	preCS, err = h.execs.Load(context.Background(), scope, id.ExecutionID(cs.ExecutionID))
	if err != nil {
		t.Fatalf("reload %s: %v", cs.ExecutionID, err)
	}
	_, doneCS, err := flow.Reduce(preCS, flow.ObserveFact{
		ExecutionID: preCS.ID,
		ActionID:    preCS.Actions[0].ID,
		Observation: kernel.ExitedObservation(0, kernel.ConfirmedStop("exited")),
		At:          fixedNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("observe %s: %v", cs.ExecutionID, err)
	}
	if _, err := eng.Commit(context.Background(), *doneCS); err != nil {
		t.Fatalf("commit observe %s: %v", cs.ExecutionID, err)
	}
	progress, err := h.taskService.Progress(context.Background(), task.ProgressCommand{
		Scope:       scope,
		TaskID:      id.TaskID(cs.TaskID),
		ExecutionID: id.ExecutionID(cs.ExecutionID),
		Outcome:     task.ProgressSucceeded,
	})
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if progress.State != task.StateOpen || progress.Applied {
		t.Fatalf("Progress with completes_task=false = %+v; want open/not-applied", progress)
	}

	// Confirm task is still open and never transitioned through done.
	readRaw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": cs.TaskID})
	if err != nil {
		t.Fatalf("task.read: %v", err)
	}
	var read api.TaskReadResult
	decodeResult(t, readRaw, &read)
	if read.State != "open" || read.LifecycleVersion != cs.LifecycleVersion {
		t.Fatalf("task after completes_task=false success = state:%q ver:%d; want open/%d", read.State, read.LifecycleVersion, cs.LifecycleVersion)
	}

	// 2. Authorization checks for task.complete: Viewer, Reviewer, Admin, non-member,
	// and execution principals denied; Operator allowed.
	h.principals.principals["token-nonmember"] = identity.Principal{
		ID:      "nonmember-1",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	}
	execPrincipal := identity.Principal{
		ID:          "exec-principal-1",
		Kind:        identity.KindExecution,
		Scope:       scope,
		ExecutionID: id.ExecutionID(cs.ExecutionID),
		Enabled:     true,
		Version:     1,
	}
	h.principals.principals["token-execution"] = execPrincipal
	h.facts.install(id.ExecutionID(cs.ExecutionID), authorize.AdmittedExecution{
		Principal:   execPrincipal,
		ExecutionID: id.ExecutionID(cs.ExecutionID),
	})
	for _, tok := range []string{"token-viewer", "token-reviewer", "token-admin", "token-execution"} {
		_, err := h.invokeVersion(t, tok, "task.complete", `{}`, read.LifecycleVersion, map[string]string{"task": cs.TaskID})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("task.complete with %s err = %v; want forbidden", tok, err)
		}
	}
	// Non-members of the task's project (no membership row in scope, or member of another project) are refused.
	for _, tok := range []string{"token-nonmember", "token-engineer-b"} {
		_, err := h.invokeVersion(t, tok, "task.complete", `{}`, read.LifecycleVersion, map[string]string{"task": cs.TaskID})
		if class, ok := fault.ClassOf(err); !ok || (class != fault.Forbidden && class != fault.NotFound) {
			t.Fatalf("task.complete with %s err = %v; want forbidden or not_found", tok, err)
		}
	}

	// Stale expected_version is refused with version_conflict.
	_, err = h.invokeVersion(t, "token-operator", "task.complete", `{}`, read.LifecycleVersion+10, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("task.complete stale version err = %v; want version_conflict", err)
	}

	// Operator completes the open task explicitly via MCP (no route params, task_id in body) with idempotency key.
	ver := read.LifecycleVersion
	completeInput := jsonBody(t, map[string]any{"task_id": cs.TaskID, "reason": "all stages verified"})
	compRes, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "task.complete",
		Credential: "token-operator",
		Request: api.Request{
			Input:           []byte(completeInput),
			ExpectedVersion: &ver,
			IdempotencyKey:  "complete-idem-1",
		},
	})
	if err != nil {
		t.Fatalf("task.complete by operator: %v", err)
	}
	var completed api.TaskCompleteResult
	decodeResult(t, compRes, &completed)
	if completed.LifecycleVersion != ver+1 {
		t.Fatalf("completed lifecycle_version = %d; want %d", completed.LifecycleVersion, ver+1)
	}

	// Identical idempotent retry converges cleanly!
	replayRes, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "task.complete",
		Credential: "token-operator",
		Request: api.Request{
			Input:           []byte(completeInput),
			ExpectedVersion: &ver,
			IdempotencyKey:  "complete-idem-1",
		},
	})
	if err != nil {
		t.Fatalf("task.complete idempotent replay: %v", err)
	}
	var replayed api.TaskCompleteResult
	decodeResult(t, replayRes, &replayed)
	if replayed != completed {
		t.Fatalf("replayed = %+v; want %+v", replayed, completed)
	}

	// Changed content with same key is refused.
	_, err = h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "task.complete",
		Credential: "token-operator",
		Request: api.Request{
			Input:           []byte(jsonBody(t, map[string]any{"task_id": cs.TaskID, "reason": "different reason"})),
			ExpectedVersion: &ver,
			IdempotencyKey:  "complete-idem-1",
		},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("changed content replay err = %v; want changed_idempotency_content", err)
	}

	// Fresh complete on already-done task is refused.
	_, err = h.invokeVersion(t, testToken, "task.complete", `{}`, completed.LifecycleVersion, map[string]string{"task": cs.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("fresh task.complete on done task err = %v; want version_conflict", err)
	}

	// Verify task.completed event was recorded in project scope.
	eventsPage, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:   "events",
		Equals:  map[string]any{"type": string(event.TypeTaskCompleted)},
		OrderBy: []store.Order{{Column: "publish_sequence"}},
		Limit:   10,
	})
	if err != nil || len(eventsPage.Rows) != 1 {
		t.Fatalf("task.completed events = %d (err=%v); want 1", len(eventsPage.Rows), err)
	}
	if eventsPage.Rows[0].Values["subject_id"] != cs.TaskID {
		t.Fatalf("event subject_id = %v; want %q", eventsPage.Rows[0].Values["subject_id"], cs.TaskID)
	}

	// 3. Default pipeline (completes_task omitted -> true) overridden via task.create_and_start completes_task: false.
	falseVal := false
	csOverrideRaw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-override-false", jsonBody(t, map[string]any{
		"title":          "Override false",
		"pipeline_id":    defaultPipeID,
		"inputs":         map[string]string{"repository": "shop/web"},
		"completes_task": &falseVal,
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start override false: %v", err)
	}
	var csOverride api.TaskCreateAndStartResult
	decodeResult(t, csOverrideRaw, &csOverride)
	completeSingleContainerExecution(t, h, scope, id.ExecutionID(csOverride.ExecutionID), 0)
	progOverride, err := h.taskService.Progress(context.Background(), task.ProgressCommand{
		Scope: scope, TaskID: id.TaskID(csOverride.TaskID), ExecutionID: id.ExecutionID(csOverride.ExecutionID), Outcome: task.ProgressSucceeded,
	})
	if err != nil || progOverride.State != task.StateOpen || progOverride.Applied {
		t.Fatalf("Progress override false = %+v (err=%v); want open/not-applied", progOverride, err)
	}

	// 4. Waiting and cancelled tasks refuse task.complete.
	_, err = h.invokeVersion(t, testToken, "task.ask", jsonBody(t, map[string]any{
		"question":   "Need info",
		"assignment": "role/engineer",
	}), csOverride.LifecycleVersion, map[string]string{"task": csOverride.TaskID})
	if err != nil {
		t.Fatalf("task.ask: %v", err)
	}
	_, err = h.invokeVersion(t, testToken, "task.complete", `{}`, csOverride.LifecycleVersion+1, map[string]string{"task": csOverride.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("task.complete on waiting task err = %v; want version_conflict", err)
	}
	_, err = h.invokeVersion(t, testToken, "task.cancel", `{}`, csOverride.LifecycleVersion+1, map[string]string{"task": csOverride.TaskID})
	if err != nil {
		t.Fatalf("task.cancel: %v", err)
	}
	_, err = h.invokeVersion(t, testToken, "task.complete", `{}`, csOverride.LifecycleVersion+2, map[string]string{"task": csOverride.TaskID})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("task.complete on cancelled task err = %v; want version_conflict", err)
	}
	// 5. Per-start completes_task: false override MUST survive task.ask -> task.reply
	// on a pipeline whose YAML omits completes_task (default true).
	csReplyRaw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-ask-reply-no-complete", jsonBody(t, map[string]any{
		"title":          "Ask-reply preserves completes_task=false",
		"pipeline_id":    defaultPipeID,
		"inputs":         map[string]string{"repository": "shop/web"},
		"completes_task": &falseVal,
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start for ask/reply: %v", err)
	}
	var csReply api.TaskCreateAndStartResult
	decodeResult(t, csReplyRaw, &csReply)

	askRaw, err := invokeKey(t, h, testToken, "task.ask", "ask-preserve-no-complete", jsonBody(t, map[string]any{
		"question":   "Which deployment target?",
		"assignment": "role/engineer",
	}), &csReply.LifecycleVersion, map[string]string{"task": csReply.TaskID})
	if err != nil {
		t.Fatalf("task.ask for completes_task preservation: %v", err)
	}
	var asked api.TaskAskResult
	decodeResult(t, askRaw, &asked)

	// Mark the paused/cancelled predecessor execution's cleanup complete so the
	// confirmed-stop barrier allows the resumed execution to activate.
	cleanupRows, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:  "cleanup",
		Equals: map[string]any{"execution_id": csReply.ExecutionID},
		Limit:  5,
	})
	if err != nil || len(cleanupRows.Rows) != 1 {
		t.Fatalf("cleanup rows for asked execution = %d (err=%v); want 1", len(cleanupRows.Rows), err)
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{
			Table:  "cleanup",
			ID:     cleanupRows.Rows[0].Values["id"].(string),
			Values: map[string]any{"state": string(kernel.CleanupComplete)},
		}},
	}); err != nil {
		t.Fatalf("complete cleanup for asked execution: %v", err)
	}

	replyRaw, err := invokeKey(t, h, testToken, "task.reply", "reply-preserve-no-complete", jsonBody(t, map[string]any{
		"question_id":               asked.QuestionID,
		"expected_question_version": asked.QuestionVersion,
		"answer":                    "staging",
	}), &asked.LifecycleVersion, map[string]string{"task": csReply.TaskID})
	if err != nil {
		t.Fatalf("task.reply for completes_task preservation: %v", err)
	}
	var replied api.TaskReplyResult
	decodeResult(t, replyRaw, &replied)

	// Succeed the resumed execution and run Progress: task MUST remain open!
	completeSingleContainerExecution(t, h, scope, id.ExecutionID(replied.ExecutionID), 0)
	progAfterReply, err := h.taskService.Progress(context.Background(), task.ProgressCommand{
		Scope:       scope,
		TaskID:      id.TaskID(csReply.TaskID),
		ExecutionID: id.ExecutionID(replied.ExecutionID),
		Outcome:     task.ProgressSucceeded,
	})
	if err != nil {
		t.Fatalf("Progress after ask/reply: %v", err)
	}
	if progAfterReply.State != task.StateOpen || progAfterReply.Applied {
		t.Fatalf("Progress after ask/reply with per-start completes_task=false = %+v; want open/not-applied", progAfterReply)
	}
	readAfterReplyRaw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": csReply.TaskID})
	if err != nil {
		t.Fatalf("task.read after ask/reply: %v", err)
	}
	var readAfterReply api.TaskReadResult
	decodeResult(t, readAfterReplyRaw, &readAfterReply)
	if readAfterReply.State != "open" {
		t.Fatalf("task state after ask/reply + succeeded execution = %q; want open", readAfterReply.State)
	}
}

func TestReviseDecisionAppendsFeedbackAndSkipsOperatorFailureInbox(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}

	reviewer := identity.Principal{
		ID:      "reviewer-human",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	}
	h.principals.principals["token-reviewer"] = reviewer
	h.seedMembership(t, scope, "mem-reviewer", reviewer.ID, 1, tenancy.RoleReviewer)

	operator := identity.Principal{
		ID:      "operator-human",
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	}
	h.principals.principals["token-operator"] = operator
	h.seedMembership(t, scope, "mem-operator", operator.ID, 1, tenancy.RoleOperator)

	reviewPipeID := applyNamedPipeline(t, h, "review-pipe", reviewFeedbackYAML)
	containerPipeID := applyNamedPipeline(t, h, "container-pipe", stageOneYAML)

	// Task A: resolved with revise -> must append feedback, stay open, NOT appear in operator inbox.
	taskARaw, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Feature needing revision",
		"pipeline_id": reviewPipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create task A: %v", err)
	}
	var taskA api.TaskCreateAndStartResult
	decodeResult(t, taskARaw, &taskA)

	// Activate action 0 (review request) on Task A.
	eng := storeexecution.NewEngine(h.store.Controller())
	preA, err := h.execs.Load(context.Background(), scope, id.ExecutionID(taskA.ExecutionID))
	if err != nil {
		t.Fatalf("Load task A exec: %v", err)
	}
	_, actA, err := flow.Reduce(preA, flow.ActivateFact{ExecutionID: preA.ID, ActionID: preA.Actions[0].ID, At: fixedNow})
	if err != nil {
		t.Fatalf("activate review A: %v", err)
	}
	if _, err := eng.Commit(context.Background(), *actA); err != nil {
		t.Fatalf("commit review A: %v", err)
	}
	preA, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(taskA.ExecutionID))

	// Reviewer resolves with decision: "revise" and feedback note + declared output.
	revBody := jsonBody(t, map[string]any{
		"expected_action_version": preA.Actions[0].Version,
		"decision":                "revise",
		"note":                    "Please split the handler and add nil checks",
		"outputs":                 map[string]string{"feedback": "split handler"},
	})
	revParams := map[string]string{"execution": taskA.ExecutionID, "action": string(preA.Actions[0].ID)}
	revRes, err := h.invokeVersion(t, "token-reviewer", "execution.request.resolve", revBody, preA.Version, revParams)
	if err != nil {
		t.Fatalf("resolve revise: %v", err)
	}
	var resolved api.ExecutionRequestResolveResult
	decodeResult(t, revRes, &resolved)

	// Idempotent replay of the identical revise resolution MUST return the identical
	// result and MUST NOT append a duplicate task_messages row.
	replayRevRes, err := h.invokeVersion(t, "token-reviewer", "execution.request.resolve", revBody, preA.Version, revParams)
	if err != nil {
		t.Fatalf("resolve revise idempotent replay: %v", err)
	}
	var replayedResolved api.ExecutionRequestResolveResult
	decodeResult(t, replayRevRes, &replayedResolved)
	if replayedResolved != resolved {
		t.Fatalf("replayed revise result = %+v; want %+v", replayedResolved, resolved)
	}
	msgRows, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:  "task_messages",
		Equals: map[string]any{"task_id": taskA.TaskID},
		Limit:  10,
	})
	if err != nil || len(msgRows.Rows) != 1 {
		t.Fatalf("task_messages rows after revise replay = %d (err=%v); want exactly 1", len(msgRows.Rows), err)
	}

	// Conflicting retry of revise (different note/outputs) MUST be refused with
	// changed_idempotency_content and leave task_messages unchanged.
	conflictBody := jsonBody(t, map[string]any{
		"expected_action_version": preA.Actions[0].Version,
		"decision":                "revise",
		"note":                    "Changed revision note",
		"outputs":                 map[string]string{"feedback": "different"},
	})
	_, err = h.invokeVersion(t, "token-reviewer", "execution.request.resolve", conflictBody, preA.Version, revParams)
	if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("conflicting revise retry err = %v; want changed_idempotency_content", err)
	}
	msgRowsAfterConflict, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:  "task_messages",
		Equals: map[string]any{"task_id": taskA.TaskID},
		Limit:  10,
	})
	if err != nil || len(msgRowsAfterConflict.Rows) != 1 {
		t.Fatalf("task_messages rows after conflicting revise = %d (err=%v); want 1", len(msgRowsAfterConflict.Rows), err)
	}

	// Inspect execution A: failed with reason needs_revision, decision=revise, NO downstream outputs promoted.
	inspRaw, err := h.invoke(t, testToken, "execution.inspect", `{}`, map[string]string{"execution": taskA.ExecutionID})
	if err != nil {
		t.Fatalf("inspect exec A: %v", err)
	}
	var insp api.ExecutionInspectResult
	decodeResult(t, inspRaw, &insp)
	if insp.State != "failed" || insp.Failure == nil || insp.Failure.Reason != kernel.FailureReasonNeedsRevision {
		t.Fatalf("exec A inspect = state:%q failure:%+v; want failed/needs_revision", insp.State, insp.Failure)
	}
	if insp.Actions[0].Decision != "revise" || insp.Actions[0].State != "failed" {
		t.Fatalf("review action = decision:%q state:%q; want revise/failed", insp.Actions[0].Decision, insp.Actions[0].State)
	}
	if insp.Actions[1].State != "cancelled" {
		t.Fatalf("successor action state = %q; want cancelled", insp.Actions[1].State)
	}
	// Verify downstream outputs on kernel aggregate are nil.
	postA, err := h.execs.Load(context.Background(), scope, id.ExecutionID(taskA.ExecutionID))
	if err != nil {
		t.Fatalf("reload exec A: %v", err)
	}
	if postA.Actions[0].Outputs != nil {
		t.Fatalf("revise must not promote Action.Outputs; got %v", postA.Actions[0].Outputs)
	}

	// Verify Task A is still open and has reviewer feedback atomically appended to messages!
	readARaw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": taskA.TaskID})
	if err != nil {
		t.Fatalf("task.read A: %v", err)
	}
	var readA api.TaskReadResult
	decodeResult(t, readARaw, &readA)
	if readA.State != "open" {
		t.Fatalf("task A state = %q; want open", readA.State)
	}
	if len(readA.Messages) != 1 {
		t.Fatalf("task A messages = %+v; want 1 revision feedback message", readA.Messages)
	}
	if readA.Messages[0].Author != string(reviewer.ID) || readA.Messages[0].Body != "Please split the handler and add nil checks" {
		t.Fatalf("task A feedback message = %+v; want author=%q body='Please split...'", readA.Messages[0], reviewer.ID)
	}

	// Starting a new execution on Task A must materialize the feedback in /kodema/task.json!
	retryRaw, err := h.invokeVersion(t, testToken, "task.start", `{}`, readA.LifecycleVersion, map[string]string{"task": taskA.TaskID})
	if err != nil {
		t.Fatalf("task.start next attempt on A: %v", err)
	}
	var retryA api.TaskStartResult
	decodeResult(t, retryRaw, &retryA)
	nextExecA, err := h.execs.Load(context.Background(), scope, id.ExecutionID(retryA.ExecutionID))
	if err != nil {
		t.Fatalf("load next exec A: %v", err)
	}
	taskJSON, present := nextExecA.Snapshot.Task()
	if !present {
		t.Fatal("next execution missing task conversation snapshot")
	}
	var materializedMsgs []api.TaskMessage
	if err := json.Unmarshal(taskJSON, &materializedMsgs); err != nil {
		t.Fatalf("unmarshal /kodema/task.json: %v", err)
	}
	if len(materializedMsgs) != 1 || materializedMsgs[0].Body != "Please split the handler and add nil checks" || materializedMsgs[0].Author != string(reviewer.ID) {
		t.Fatalf("/kodema/task.json messages = %+v; want reviewer feedback", materializedMsgs)
	}

	// Now create Task A2 (resolved with revise, NOT restarted yet) and Task B (container exit 1 system failure)
	// and Task C (request resolved with fail decision) to verify operator inbox filtering!
	taskA2Raw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-a2", jsonBody(t, map[string]any{
		"title":       "Domain revision task",
		"pipeline_id": reviewPipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create task A2: %v", err)
	}
	var taskA2 api.TaskCreateAndStartResult
	decodeResult(t, taskA2Raw, &taskA2)
	preA2, _ := h.execs.Load(context.Background(), scope, id.ExecutionID(taskA2.ExecutionID))
	_, actA2, _ := flow.Reduce(preA2, flow.ActivateFact{ExecutionID: preA2.ID, ActionID: preA2.Actions[0].ID, At: fixedNow})
	_, _ = eng.Commit(context.Background(), *actA2)
	preA2, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(taskA2.ExecutionID))
	if _, err := invokeKey(t, h, "token-reviewer", "execution.request.resolve", "resolve-a2", jsonBody(t, map[string]any{
		"expected_action_version": preA2.Actions[0].Version,
		"decision":                "revise",
		"outputs":                 map[string]string{"feedback": "use context.Context"},
	}), &preA2.Version, map[string]string{"execution": taskA2.ExecutionID, "action": string(preA2.Actions[0].ID)}); err != nil {
		t.Fatalf("resolve A2 revise: %v", err)
	}

	// Task B: container nonzero exit (system failure).
	taskBRaw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-b", jsonBody(t, map[string]any{
		"title":       "System crash task",
		"pipeline_id": containerPipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create task B: %v", err)
	}
	var taskB api.TaskCreateAndStartResult
	decodeResult(t, taskBRaw, &taskB)
	completeSingleContainerExecution(t, h, scope, id.ExecutionID(taskB.ExecutionID), 137)

	// Task C: request resolved with fail (system/terminal rejection).
	taskCRaw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-c", jsonBody(t, map[string]any{
		"title":       "Hard fail review task",
		"pipeline_id": reviewPipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create task C: %v", err)
	}
	var taskC api.TaskCreateAndStartResult
	decodeResult(t, taskCRaw, &taskC)
	preC, _ := h.execs.Load(context.Background(), scope, id.ExecutionID(taskC.ExecutionID))
	_, actC, _ := flow.Reduce(preC, flow.ActivateFact{ExecutionID: preC.ID, ActionID: preC.Actions[0].ID, At: fixedNow})
	_, _ = eng.Commit(context.Background(), *actC)
	preC, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(taskC.ExecutionID))
	if _, err := invokeKey(t, h, "token-reviewer", "execution.request.resolve", "resolve-c", jsonBody(t, map[string]any{
		"expected_action_version": preC.Actions[0].Version,
		"decision":                "fail",
		"note":                    "Policy violation",
	}), &preC.Version, map[string]string{"execution": taskC.ExecutionID, "action": string(preC.Actions[0].ID)}); err != nil {
		t.Fatalf("resolve C fail: %v", err)
	}

	// Query role/operator failure inbox: MUST contain Task B and Task C, and MUST NOT contain Task A2!
	inboxRaw, err := h.invoke(t, "token-operator", "task.inbox", `{"kinds":["failed_task"]}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("operator task.inbox: %v", err)
	}
	var opInbox api.TaskInboxResult
	decodeResult(t, inboxRaw, &opInbox)
	seenFailed := make(map[string]bool, len(opInbox.Items))
	for _, item := range opInbox.Items {
		seenFailed[item.TaskID] = true
	}
	if seenFailed[taskA2.TaskID] {
		t.Fatalf("domain revision task %s must NOT appear in operator failure inbox: %+v", taskA2.TaskID, opInbox.Items)
	}
	if !seenFailed[taskB.TaskID] {
		t.Fatalf("system failure task %s missing from operator failure inbox: %+v", taskB.TaskID, opInbox.Items)
	}
	if !seenFailed[taskC.TaskID] {
		t.Fatalf("fail-decision task %s missing from operator failure inbox: %+v", taskC.TaskID, opInbox.Items)
	}

	// Revise with empty note and empty outputs falls back cleanly to "Revision requested"
	// and appends exactly one message.
	taskEmptyRaw, err := invokeKey(t, h, testToken, "task.create_and_start", "cs-empty-revise", jsonBody(t, map[string]any{
		"title":       "Empty revise fallback task",
		"pipeline_id": reviewPipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create task empty revise: %v", err)
	}
	var taskEmpty api.TaskCreateAndStartResult
	decodeResult(t, taskEmptyRaw, &taskEmpty)
	preEmpty, err := h.execs.Load(context.Background(), scope, id.ExecutionID(taskEmpty.ExecutionID))
	if err != nil {
		t.Fatalf("Load task empty exec: %v", err)
	}
	_, actEmpty, err := flow.Reduce(preEmpty, flow.ActivateFact{ExecutionID: preEmpty.ID, ActionID: preEmpty.Actions[0].ID, At: fixedNow})
	if err != nil {
		t.Fatalf("activate empty revise: %v", err)
	}
	if _, err := eng.Commit(context.Background(), *actEmpty); err != nil {
		t.Fatalf("commit empty revise activate: %v", err)
	}
	preEmpty, _ = h.execs.Load(context.Background(), scope, id.ExecutionID(taskEmpty.ExecutionID))
	if _, err := invokeKey(t, h, "token-reviewer", "execution.request.resolve", "resolve-empty-revise", jsonBody(t, map[string]any{
		"expected_action_version": preEmpty.Actions[0].Version,
		"decision":                "revise",
	}), &preEmpty.Version, map[string]string{"execution": taskEmpty.ExecutionID, "action": string(preEmpty.Actions[0].ID)}); err != nil {
		t.Fatalf("resolve empty revise: %v", err)
	}
	readEmptyRaw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": taskEmpty.TaskID})
	if err != nil {
		t.Fatalf("task.read empty revise: %v", err)
	}
	var readEmpty api.TaskReadResult
	decodeResult(t, readEmptyRaw, &readEmpty)
	if len(readEmpty.Messages) != 1 || readEmpty.Messages[0].Body != "Revision requested" || readEmpty.Messages[0].Author != string(reviewer.ID) {
		t.Fatalf("empty revise messages = %+v; want 1 fallback message 'Revision requested' by %q", readEmpty.Messages, reviewer.ID)
	}
}

func TestTaskParentIDValidationFilteringEventsAndSpecMaterialization(t *testing.T) {
	h := newHarness(t)
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	otherScope := tenancy.Scope{TenantID: testTenant, ProjectID: "proj-other"}
	if _, err := h.store.Execute(context.Background(), tenancy.Scope{TenantID: testTenant}, store.Unit{
		Writes: []store.Write{{
			Table:  "projects",
			ID:     string(otherScope.ProjectID),
			Insert: true,
			Values: map[string]any{
				"name":       "Other Project",
				"version":    int64(1),
				"created_at": fixedNow,
			},
		}},
	}); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	otherPrincipalID := id.PrincipalID("user-other-proj")
	h.seedMembership(t, otherScope, "mem-other", otherPrincipalID, 1, tenancy.RoleEngineer)
	otherPrincipal := identity.Principal{
		ID:      otherPrincipalID,
		Kind:    identity.KindHuman,
		Scope:   otherScope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	h.principals.principals["token-other-project"] = otherPrincipal

	pipeID := applyNamedPipeline(t, h, "child-pipe", stageOneYAML)

	// Create parent task in project 1.
	parentRaw, err := h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title": "Epic parent",
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	var parent api.TaskCreateResult
	decodeResult(t, parentRaw, &parent)

	// Create task in another project.
	foreignRaw, err := h.invoke(t, "token-other-project", "task.create", jsonBody(t, map[string]any{
		"title": "Foreign parent",
	}), map[string]string{"project": "proj-other"})
	if err != nil {
		t.Fatalf("create foreign parent: %v", err)
	}
	var foreign api.TaskCreateResult
	decodeResult(t, foreignRaw, &foreign)

	// Missing parent_id is refused with not_found.
	_, err = h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":     "Orphan child",
		"parent_id": "task-does-not-exist",
	}), map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("missing parent_id err = %v; want not_found", err)
	}

	// Cross-project parent_id is refused with not_found!
	_, err = h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":     "Cross-project child",
		"parent_id": foreign.TaskID,
	}), map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("cross-project parent_id err = %v; want not_found", err)
	}

	// Cross-tenant parent_id is refused with not_found!
	otherTenantScope := tenancy.Scope{TenantID: "tenant-other", ProjectID: "proj-other-tenant"}
	if _, err := h.store.Execute(context.Background(), tenancy.Scope{TenantID: otherTenantScope.TenantID}, store.Unit{
		Writes: []store.Write{
			{
				Table:  "tenants",
				ID:     string(otherTenantScope.TenantID),
				Insert: true,
				Values: map[string]any{
					"name":           "Other Tenant",
					"version":        int64(1),
					"derivation_key": []byte(`{}`),
					"created_at":     fixedNow,
				},
			},
			{
				Table:  "projects",
				ID:     string(otherTenantScope.ProjectID),
				Insert: true,
				Values: map[string]any{
					"name":       "Other Tenant Project",
					"version":    int64(1),
					"created_at": fixedNow,
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed other tenant/project: %v", err)
	}
	otherTenantPrincipalID := id.PrincipalID("user-other-tenant")
	h.seedMembership(t, otherTenantScope, "mem-other-tenant", otherTenantPrincipalID, 1, tenancy.RoleEngineer)
	h.principals.principals["token-other-tenant"] = identity.Principal{
		ID:      otherTenantPrincipalID,
		Kind:    identity.KindHuman,
		Scope:   otherTenantScope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	crossTenantParentRaw, err := h.invoke(t, "token-other-tenant", "task.create", jsonBody(t, map[string]any{
		"title": "Other tenant parent",
	}), map[string]string{"project": string(otherTenantScope.ProjectID)})
	if err != nil {
		t.Fatalf("create cross-tenant parent: %v", err)
	}
	var crossTenantParent api.TaskCreateResult
	decodeResult(t, crossTenantParentRaw, &crossTenantParent)
	_, err = h.invoke(t, testToken, "task.create", jsonBody(t, map[string]any{
		"title":     "Cross-tenant child",
		"parent_id": crossTenantParent.TaskID,
	}), map[string]string{"project": testProject})
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("cross-tenant parent_id err = %v; want not_found", err)
	}

	// Self-parent (parent_id == task_id) is refused with invalid_input at the adapter/store unit layer!
	_, err = h.tasks.Create(context.Background(), task.CreateUnit{
		Owner:     scope,
		TaskID:    "task-self-parent",
		ParentID:  "task-self-parent",
		Title:     "Self parent task",
		Initiator: testEngineer,
		At:        fixedNow,
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("self-parent CreateUnit err = %v; want invalid_input", err)
	}

	// Create child 1 via task.create.
	child1Raw, err := invokeKey(t, h, testToken, "task.create", "create-child-1", jsonBody(t, map[string]any{
		"title":     "Subtask 1",
		"parent_id": parent.TaskID,
	}), nil, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create child 1: %v", err)
	}
	var child1 api.TaskCreateResult
	decodeResult(t, child1Raw, &child1)

	// Create child 2 via task.create_and_start.
	child2Raw, err := h.invoke(t, testToken, "task.create_and_start", jsonBody(t, map[string]any{
		"title":       "Subtask 2",
		"description": "Implement part 2",
		"parent_id":   parent.TaskID,
		"pipeline_id": pipeID,
		"inputs":      map[string]string{"repository": "shop/web"},
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("create_and_start child 2: %v", err)
	}
	var child2 api.TaskCreateAndStartResult
	decodeResult(t, child2Raw, &child2)

	// Verify task.read returns parent_id.
	readChild1Raw, err := h.invoke(t, testToken, "task.read", `{}`, map[string]string{"task": child1.TaskID})
	if err != nil {
		t.Fatalf("task.read child 1: %v", err)
	}
	var readChild1 api.TaskReadResult
	decodeResult(t, readChild1Raw, &readChild1)
	if readChild1.ParentID != parent.TaskID {
		t.Fatalf("child 1 parent_id = %q; want %q", readChild1.ParentID, parent.TaskID)
	}

	// Verify task.list with parent_id filter returns ONLY children of parent.
	listFilteredRaw, err := h.invoke(t, testToken, "task.list", jsonBody(t, map[string]any{
		"parent_id": parent.TaskID,
	}), map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.list filtered: %v", err)
	}
	var listFiltered api.TaskListResult
	decodeResult(t, listFilteredRaw, &listFiltered)
	if len(listFiltered.Tasks) != 2 {
		t.Fatalf("filtered task.list = %+v; want 2 children", listFiltered.Tasks)
	}
	for _, summary := range listFiltered.Tasks {
		if summary.ParentID != parent.TaskID {
			t.Fatalf("summary parent_id = %q; want %q", summary.ParentID, parent.TaskID)
		}
	}

	// Verify task.created event for child 1 carries parent_id in its payload.
	evPage, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:   "events",
		Equals:  map[string]any{"type": string(event.TypeTaskCreated), "subject_id": child1.TaskID},
		OrderBy: []store.Order{{Column: "publish_sequence"}},
		Limit:   5,
	})
	if err != nil || len(evPage.Rows) != 1 {
		t.Fatalf("child 1 task.created events = %d (err=%v); want 1", len(evPage.Rows), err)
	}
	rawData, _ := evPage.Rows[0].Values["data"].([]byte)
	var evData map[string]string
	if err := json.Unmarshal(rawData, &evData); err != nil || evData["parent_id"] != parent.TaskID {
		t.Fatalf("child 1 task.created event data = %q; want parent_id=%q", string(rawData), parent.TaskID)
	}

	// Verify /kodema/task/spec.json materialized on child 2's execution includes parent_id!
	exec2, err := h.execs.Load(context.Background(), scope, id.ExecutionID(child2.ExecutionID))
	if err != nil {
		t.Fatalf("load child 2 execution: %v", err)
	}
	specBytes, ok := exec2.Snapshot.TaskFiles()[workspace.TaskSpecFile]
	if !ok {
		t.Fatal("child 2 execution missing /kodema/task/spec.json")
	}
	var specDoc map[string]any
	if err := json.Unmarshal(specBytes, &specDoc); err != nil {
		t.Fatalf("unmarshal spec.json: %v", err)
	}
	if specDoc["id"] != child2.TaskID || specDoc["parent_id"] != parent.TaskID {
		t.Fatalf("spec.json = %s; want id=%q parent_id=%q", string(specBytes), child2.TaskID, parent.TaskID)
	}
}
