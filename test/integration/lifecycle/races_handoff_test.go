//go:build integration

package lifecycle

// Real-stack race schedules for handoffs, admission authority and settlement.
//
// Coverage map (docs/specs/06-shell-lifecycles.md S-HANDOFF/S-PROGRESS;
// docs/specs/11-verification.md AC 17/18):
//   - task.reply versus a runtime stop of its predecessor (gated Libpod stop);
//   - task.start/admission versus a membership authority/version change
//     (membership advisory-lock absence guard);
//   - an older execution's success versus a new current execution on the same
//     task (task-row hold).
//
// The schedule shape is the same one races_test.go uses: a test-owned hold
// blocks the exact lock the two product commits must take, the test observes
// through pg_stat_activity that the intended winner and then the loser are
// queued behind that hold, and only then releases it. No sleep decides an
// ordering. Every loser is refused by a store guard after it already passed its
// service pre-checks, so the test distinguishes a store refusal from an early
// service refusal. Each contested subtest also asserts the durable final state,
// that the loser left no receipt or event, and that cleanup/credential
// invariants still hold.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestRaceReplyStop races task.reply against the runtime stop confirmation of
// its predecessor execution. The runtime stop is gated inside the composition's
// real Libpod client, so the test controls when the stop completes; the
// execution-row hold then decides whether the stop confirmation or the reply
// commits first.
func TestRaceReplyStop(t *testing.T) {
	t.Run("sequential reply after a confirmed stop dispatches a fresh execution", func(t *testing.T) {
		testReplyAfterConfirmedStop(t, factory.Start(t))
	})
	t.Run("sequential reply carrying the pre-handoff lifecycle is refused", func(t *testing.T) {
		testReplyStaleLifecycle(t, factory.Start(t))
	})
	t.Run("contested stop first refuses the queued authenticated reply at the store guard", func(t *testing.T) {
		gate := factory.NewStopGate()
		t.Cleanup(gate.Release)
		testReplyStopContested(t, factory.Start(t, factory.WithStopGate(gate)), gate)
	})
	t.Run("reply first commits a replacement that stays queued until the stop is confirmed", func(t *testing.T) {
		gate := factory.NewStopGate()
		t.Cleanup(gate.Release)
		testReplyBeforeUnconfirmedStop(t, factory.Start(t, factory.WithStopGate(gate)), gate)
	})
}

// testReplyBeforeUnconfirmedStop is the reply-first schedule: the ask
// requests a runtime stop, the stop is held at the composition's Libpod client
// so it is unconfirmed, and the reply therefore commits a fresh execution while
// the predecessor is not stopped. The replacement must be durable but must not
// be activated, prepared or given a workload until the held stop confirms; then
// the authoritative scanner releases it exactly once.
func testReplyBeforeUnconfirmedStop(t *testing.T, harness *factory.Harness, gate *factory.StopGate) {
	ctx := t.Context()
	task := startGatedReplyTask(t, harness, "race-reply-before-stop")

	gate.Arm()
	waiting := askForReply(t, harness, task, "race-reply-before-stop-ask")
	awaitGate(t, gate, "reply-before-stop")

	// The reply passes its service reads while the predecessor stop is still
	// unconfirmed, so the replacement is committed and queued. It must not
	// dispatch: no activation timestamp, deadline, workload container or
	// retained workspace may exist while the stop is held.
	replied := replyTask(t, harness, harness.ReviewerToken, task.taskID, &waiting.LifecycleVersion, api.TaskReplyRequest{
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  "Restore candidate B.",
	}, "race-reply-before-stop-reply")
	if replied.ExecutionID == "" || replied.ExecutionID == task.executionID {
		t.Fatalf("reply execution = %q; want a fresh execution distinct from %q",
			replied.ExecutionID, task.executionID)
	}

	predecessor := inspectExecution(t, harness, harness.ProjectToken, task.executionID)
	work := actionByName(t, predecessor, "work")
	if work.Stop != nil && work.Stop.State == "confirmed" {
		t.Fatalf("predecessor stop = %+v; the held stop must remain unconfirmed", work.Stop)
	}

	// Hold the schedule open long enough to observe the replacement without
	// any race against the release. The stop gate is the ordering barrier; the
	// loop only proves the held state is stable while it is armed.
	deadline := time.Now().Add(time.Second)
	for {
		replacement := inspectExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
		if replacement.State != "queued" {
			t.Fatalf("replacement state while the predecessor stop is unconfirmed = %q; "+
				"want queued (actions=%+v)", replacement.State, replacement.Actions)
		}
		action := actionByName(t, replacement, "work")
		if action.State != "pending" || action.ActivatedAt != nil || action.Deadline != nil {
			t.Fatalf("replacement action while the predecessor stop is unconfirmed = %+v; "+
				"want pending with no activation timestamp or deadline", action)
		}
		resources := probeExecutionResources(ctx, harness, replied.ExecutionID)
		if resources.err != nil {
			t.Fatalf("probe held replacement resources: %v", resources.err)
		}
		if len(resources.containers) != 0 || resources.workspaceVolume {
			t.Fatalf("held replacement has a workload or retained workspace: %s", resources)
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Releasing the gated stop confirms the predecessor and the authoritative
	// scanner dispatches the queued replacement exactly once.
	gate.Release()
	waitExecution(t, harness, harness.ProjectToken, task.executionID)
	container, _ := waitBlockingWorkload(t, harness, replied.ExecutionID)
	if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release replacement workload: %v", err)
	}
	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	requireSucceeded(t, fresh)
	if done := waitTaskState(t, harness, harness.ProjectToken, task.taskID, "done"); done.CurrentExecution != replied.ExecutionID {
		t.Fatalf("done task current execution = %q; want %q", done.CurrentExecution, replied.ExecutionID)
	}
	requireSubjectEventCount(t, harness, "execution.started", replied.ExecutionID, 1)
	raceReceiptCount(t, harness, "race-reply-before-stop-reply", 1, "reply")
	assertExecutionResourcesReleased(t, harness, task.executionID)
	assertExecutionResourcesReleased(t, harness, replied.ExecutionID)
}

// startGatedReplyTask creates and starts one task over the blocking pipeline and
// waits until the workload is running with a readable credential.
func startGatedReplyTask(t *testing.T, harness *factory.Harness, name string) raceTask {
	t.Helper()
	task := startRaceTask(t, harness, name, name)
	waitWorkloadObservation(t, harness, task.executionID)
	return task
}

// askForReply asks one reviewer-assigned question from the current task
// lifecycle and returns the waiting task observation.
func askForReply(t *testing.T, harness *factory.Harness, task raceTask, key string) api.TaskReadResult {
	t.Helper()
	asked := askTaskCurrent(t, harness, harness.ProjectToken, task.taskID, api.TaskAskRequest{
		Question:   "Which candidate should be restored?",
		Assignment: "role/reviewer",
	}, key)
	if asked.QuestionVersion == 0 {
		t.Fatalf("%s ask result = %+v; want a committed question", key, asked)
	}
	waiting := raceReadTask(t, harness, task.taskID)
	if waiting.State != "waiting" || waiting.Question == nil {
		t.Fatalf("%s task after ask = %+v; want waiting with an outstanding question", key, waiting)
	}
	return waiting
}

// testReplyAfterConfirmedStop is the positive control: once the predecessor's
// stop is confirmed, the reply builds a dispatching unit and its fresh execution
// runs to success. Without the confirmed stop the same reply must not dispatch,
// which the contested schedule asserts.
func testReplyAfterConfirmedStop(t *testing.T, harness *factory.Harness) {
	task := startGatedReplyTask(t, harness, "race-reply-stop-confirmed")

	before := raceReadTask(t, harness, task.taskID)
	askForReply(t, harness, task, "race-reply-stop-confirmed-ask")
	predecessor := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if predecessor.State != "cancelled" {
		t.Fatalf("predecessor state after ask = %q; want cancelled", predecessor.State)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "cancelled predecessor")

	waiting := raceReadTask(t, harness, task.taskID)
	replied := replyTask(t, harness, harness.ReviewerToken, task.taskID, &waiting.LifecycleVersion, api.TaskReplyRequest{
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  "Restore candidate B.",
	}, "race-reply-stop-confirmed-reply")
	if replied.ExecutionID == "" || replied.ExecutionID == task.executionID {
		t.Fatalf("reply execution = %q; want a fresh execution distinct from %q",
			replied.ExecutionID, task.executionID)
	}

	container, _ := waitBlockingWorkload(t, harness, replied.ExecutionID)
	if err := harness.TouchContainerFile(t.Context(), container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release reply workload: %v", err)
	}
	fresh := waitExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	requireSucceeded(t, fresh)
	if done := waitTaskState(t, harness, harness.ProjectToken, task.taskID, "done"); done.CurrentExecution != replied.ExecutionID {
		t.Fatalf("done task current execution = %q; want %q", done.CurrentExecution, replied.ExecutionID)
	}

	// The handoff answered exactly one question and left exactly two
	// executions; the predecessor's terminal event is unchanged by the reply.
	requireSubjectEventCount(t, harness, "task.reply", task.taskID, 1)
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 1)
	requireSubjectEventCount(t, harness, "execution.cancelled", task.executionID, 1)
	requireSubjectEventCount(t, harness, "execution.succeeded", replied.ExecutionID, 1)
	raceReceiptCount(t, harness, "race-reply-stop-confirmed-reply", 1, "reply")
	assertExecutionResourcesReleased(t, harness, task.executionID)
	assertExecutionResourcesReleased(t, harness, replied.ExecutionID)

	// A committed answer removes the outstanding question: a second reply with
	// the same question identity is refused and commits nothing.
	_, publicError, err := harness.Invoke(t.Context(), harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  task.taskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  "Restore candidate C.",
	}, &before.LifecycleVersion, "race-reply-stop-confirmed-replay")
	if err != nil {
		t.Fatalf("second reply transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("second reply code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
}

// testReplyStaleLifecycle keeps the earlier sequential stale-loser coverage: a
// reply that carries the lifecycle version observed before the ask is refused by
// the task version guard and commits nothing.
func testReplyStaleLifecycle(t *testing.T, harness *factory.Harness) {
	task := startGatedReplyTask(t, harness, "race-reply-stop-stale")
	before := raceReadTask(t, harness, task.taskID)
	waiting := askForReply(t, harness, task, "race-reply-stop-stale-ask")

	predecessor := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if predecessor.State != "cancelled" {
		t.Fatalf("predecessor state after ask = %q; want cancelled", predecessor.State)
	}

	_, publicError, err := harness.Invoke(t.Context(), harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  task.taskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  "Restore candidate B.",
	}, &before.LifecycleVersion, "race-reply-stop-stale-reply")
	if err != nil {
		t.Fatalf("stale reply transport: %v", err)
	}
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale reply code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}

	after := raceReadTask(t, harness, task.taskID)
	if after.State != "waiting" || after.LifecycleVersion != waiting.LifecycleVersion || after.Question == nil ||
		after.Question.QuestionID != waiting.Question.QuestionID {
		t.Fatalf("stale reply changed the waiting task: before=%+v after=%+v", waiting, after)
	}
	if after.CurrentExecution != task.executionID {
		t.Fatalf("stale reply changed the current execution to %q; want %q", after.CurrentExecution, task.executionID)
	}
	raceReceiptCount(t, harness, "race-reply-stop-stale-reply", 0, "stale reply")
	requireSubjectEventCount(t, harness, "task.reply", task.taskID, 0)
	count, err := harness.ExecutionCount(t.Context(), task.pipelineID)
	if err != nil {
		t.Fatalf("count executions after stale reply: %v", err)
	}
	if count != 1 {
		t.Fatalf("stale reply queued executions: count=%d want=1", count)
	}
}

// testReplyStopContested gates the runtime stop of the predecessor, releases the
// stop so its confirmation commit queues behind an execution-row hold, then
// issues the reply from the still-waiting task. The reply passes its service
// reads (the predecessor stop is still unconfirmed, so it would not dispatch)
// and queues its own commit behind the same row. The hold is released only after
// both distinct product backends are observed, so the stop confirmation wins and
// the reply is refused by the store's expected-execution-version guard.
func testReplyStopContested(t *testing.T, harness *factory.Harness, gate *factory.StopGate) {
	ctx := t.Context()
	task := startGatedReplyTask(t, harness, "race-reply-stop-contested")

	// Arm the gate before the ask can request cleanup, then prove the engine
	// really reached the gated Libpod stop instead of finishing first.
	gate.Arm()
	waiting := askForReply(t, harness, task, "race-reply-stop-contested-ask")
	awaitGate(t, gate, "reply-stop contested")

	hold, err := harness.HoldExecutionRows(ctx, task.executionID)
	if err != nil {
		t.Fatalf("hold predecessor execution row: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "executions", "reply-stop contested")

	// Release the gated stop. Its confirmation commit is the intended winner
	// and must be the first queued product backend on the execution row.
	gate.Release()
	stopBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "executions", raceContentionTimeout)
	if err != nil {
		t.Fatalf("stop confirmation did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, stopBlocked, "executions", "stop winner")
	t.Logf("stop confirmation queued behind the hold: pid=%d wait=%s/%s query=%q",
		stopBlocked.PID, stopBlocked.WaitEventType, stopBlocked.WaitEvent, stopBlocked.Query)

	// The execution row is still the pre-stop version and the stop evidence is
	// unconfirmed, so the reply passes every service read and queues its commit
	// second.
	reply := invokeAsync(t, harness, harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  task.taskID,
		QuestionID:              waiting.Question.QuestionID,
		ExpectedQuestionVersion: waiting.Question.Version,
		Answer:                  "Restore candidate B.",
	}, &waiting.LifecycleVersion, "race-reply-stop-contested-reply")
	waiters := awaitContention(t, ctx, harness, hold, "executions", 2, reply, "reply loser")
	requireContendingWaiters(t, waiters, hold, stopBlocked, "executions", "stop winner", "reply loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release reply-stop contested hold: %v", err)
	}
	loser := awaitInvocation(t, reply, 2*time.Minute, "reply loser")
	requireStoreVersionConflict(t, loser, "reply loser")

	// The stop confirmation committed and the reply wrote nothing: the task is
	// still waiting on the same question at the same lifecycle version, and no
	// fresh execution exists.
	after := raceReadTask(t, harness, task.taskID)
	if after.State != "waiting" || after.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("losing reply changed the task: before=%+v after=%+v", waiting, after)
	}
	if after.Question == nil || after.Question.QuestionID != waiting.Question.QuestionID ||
		after.Question.Version != waiting.Question.Version {
		t.Fatalf("losing reply changed the outstanding question: before=%+v after=%+v", waiting.Question, after.Question)
	}
	if after.CurrentExecution != task.executionID {
		t.Fatalf("losing reply changed the current execution to %q; want %q", after.CurrentExecution, task.executionID)
	}
	count, err := harness.ExecutionCount(ctx, task.pipelineID)
	if err != nil {
		t.Fatalf("count executions after losing reply: %v", err)
	}
	if count != 1 {
		t.Fatalf("losing reply queued executions: count=%d want=1", count)
	}
	raceReceiptCount(t, harness, "race-reply-stop-contested-reply", 0, "reply loser")
	requireSubjectEventCount(t, harness, "task.reply", task.taskID, 0)

	// The winner really confirmed the stop: the predecessor is terminal, its
	// stop evidence is confirmed, cleanup completed and the credential is gone.
	predecessor := waitExecution(t, harness, harness.ProjectToken, task.executionID)
	if predecessor.State != "cancelled" {
		t.Fatalf("predecessor state after losing reply = %q; want cancelled", predecessor.State)
	}
	work := actionByName(t, predecessor, "work")
	if work.Stop == nil || work.Stop.State != "confirmed" {
		t.Fatalf("predecessor stop evidence = %+v; want confirmed termination evidence", work.Stop)
	}
	requireRevokedExecutionCredential(t, harness, task.token, task.executionID)
	requireExecutionCredentialMaterial(t, harness, task.executionID, true, "stopped predecessor")
	requireSubjectEventCount(t, harness, "execution.cancelled", task.executionID, 1)
	requireSubjectEventCount(t, harness, "task.question", task.taskID, 1)
	assertExecutionResourcesReleased(t, harness, task.executionID)
}

// TestRaceAdmissionAuthority races task.start (admission) against a membership
// change. The membership change is a first project-scope grant whose absence
// guard and the admission's absent tenant-wide membership pin take the same
// transaction-scoped advisory lock; the test holds that advisory lock directly.
func TestRaceAdmissionAuthority(t *testing.T) {
	t.Run("sequential authority loss refuses a later start", func(t *testing.T) {
		testStartAfterDemotion(t, factory.Start(t))
	})
	t.Run("contested membership change first refuses the queued admission at the absence guard", func(t *testing.T) {
		testAdmissionAuthorityContested(t, factory.Start(t))
	})
}

// admissionTask authors one blocking pipeline and creates (but does not start)
// one task in it.
func admissionTask(t *testing.T, harness *factory.Harness, name string) (api.PipelineApplyResult, api.TaskCreateResult) {
	t.Helper()
	applied := applyRaceBlockingPipeline(t, harness, name)
	created := createTask(t, harness, harness.ProjectToken, api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      name,
		PipelineID: applied.PipelineID,
	}, name+"-create")
	return applied, created
}

// testStartAfterDemotion is the sequential authority-loss variant: after the
// engineer's project membership is replaced with viewer, a start by that
// principal is refused before any commit.
func testStartAfterDemotion(t *testing.T, harness *factory.Harness) {
	applied, created := admissionTask(t, harness, "race-admission-demoted")

	// The harness grants the limited principals one project role each, so the
	// engineer row is at version 1.
	one := uint64(1)
	harness.MustREST(harness.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: factory.EngineerPrincipal,
		Roles:       []string{"viewer"},
	}, &one, "race-admission-demoted-grant")

	task := raceReadTask(t, harness, created.TaskID)
	_, publicError, err := harness.Invoke(t.Context(), harness.EngineerToken, "task.start", api.TaskStartRequest{
		TaskID: created.TaskID,
	}, &task.LifecycleVersion, "race-admission-demoted-start")
	if err != nil {
		t.Fatalf("demoted start transport: %v", err)
	}
	if publicError.Code != api.CodeForbidden {
		t.Fatalf("demoted start code = %q (%s); want %q", publicError.Code, publicError.Message, api.CodeForbidden)
	}

	after := raceReadTask(t, harness, created.TaskID)
	if after.State != "open" || after.LifecycleVersion != task.LifecycleVersion || after.CurrentExecution != "" {
		t.Fatalf("refused demoted start changed the task: before=%+v after=%+v", task, after)
	}
	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after demoted start: %v", err)
	}
	if count != 0 {
		t.Fatalf("refused demoted start queued %d executions; want 0", count)
	}
	raceReceiptCount(t, harness, "race-admission-demoted-start", 0, "demoted start")
}

// testAdmissionAuthorityContested holds the engineer's tenant-wide membership
// advisory key. The first tenant-wide grant takes the same key first; the
// admission then passes its service pre-check (the engineer's project row still
// grants engineer) and queues behind it. Releasing the hold commits the grant
// first, so the admission's in-transaction absence guard observes the new row
// and refuses with version_conflict, committing no execution and no receipt.
func testAdmissionAuthorityContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied, created := admissionTask(t, harness, "race-admission-authority")
	before := raceReadTask(t, harness, created.TaskID)

	// The admission principal is project-scoped with no exact-project
	// membership; its tenant-wide engineer row is fixture state because the
	// public API has no tenant-wide membership write. The admission therefore
	// pins the absent exact-project row, which the public project-scoped first
	// grant below also serializes on.
	if err := harness.SeedTenantWideMembership(ctx, factory.AdmissionPrincipal, []string{"engineer"}); err != nil {
		t.Fatalf("seed tenant-wide admission membership: %v", err)
	}

	hold, err := harness.HoldMembershipAdvisory(ctx, factory.MembershipKey{
		TenantID:    factory.TenantID,
		ProjectID:   factory.ProjectID,
		PrincipalID: factory.AdmissionPrincipal,
	})
	if err != nil {
		t.Fatalf("hold membership advisory lock: %v", err)
	}
	requireNoBlockedAdvisoryStatements(t, harness, hold, "admission-authority contested")

	// The project-scoped first grant for the admission principal is the
	// intended winner.
	grant := invokeAsync(t, harness, harness.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: factory.AdmissionPrincipal,
		Roles:       []string{"viewer"},
	}, nil, "race-admission-authority-grant")
	grantBlocked, err := harness.WaitForBlockedAdvisoryStatement(ctx, hold, raceContentionTimeout)
	if err != nil {
		t.Fatalf("membership grant did not queue behind the advisory hold: %v", err)
	}
	requireQueuedAdvisoryLock(t, grantBlocked, "membership grant winner")

	// The admission's service pre-check runs while the grant is queued and
	// uncommitted, so its tenant-wide engineer row still authorizes it.
	start := invokeAsync(t, harness, harness.AdmissionToken, "task.start", api.TaskStartRequest{
		TaskID: created.TaskID,
	}, &before.LifecycleVersion, "race-admission-authority-start")
	waiters := awaitAdvisoryContention(t, ctx, harness, hold, 2, start, "admission loser")
	requireContendingAdvisoryWaiters(t, waiters, hold, grantBlocked, "membership grant winner", "admission loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release admission-authority advisory hold: %v", err)
	}
	winner := awaitInvocation(t, grant, 2*time.Minute, "membership grant winner")
	requireInvocationSuccess(t, winner, "membership grant winner")
	var granted api.MembershipGrantResult
	harness.Decode(winner.result, &granted)
	if granted.Version == 0 {
		t.Fatalf("membership grant result = %+v; want a committed version", granted)
	}
	loser := awaitInvocation(t, start, 2*time.Minute, "admission loser")
	requireStoreVersionConflict(t, loser, "admission loser")

	after := raceReadTask(t, harness, created.TaskID)
	if after.State != "open" || after.LifecycleVersion != before.LifecycleVersion || after.CurrentExecution != "" {
		t.Fatalf("losing admission changed the task: before=%+v after=%+v", before, after)
	}
	count, err := harness.ExecutionCount(ctx, applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after losing admission: %v", err)
	}
	if count != 0 {
		t.Fatalf("losing admission queued %d executions; want 0", count)
	}
	raceReceiptCount(t, harness, "race-admission-authority-start", 0, "admission loser")
	raceReceiptCount(t, harness, "race-admission-authority-grant", 1, "membership grant winner")

	// Positive control: the committed grant does not revoke the tenant-wide
	// engineer authority, so the same start now succeeds and runs to
	// settlement.
	current := raceReadTask(t, harness, created.TaskID)
	started := startTask(t, harness, harness.AdmissionToken, created.TaskID, &current.LifecycleVersion, "race-admission-authority-control")
	container, _ := waitBlockingWorkload(t, harness, started.ExecutionID)
	if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release control workload: %v", err)
	}
	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	raceReceiptCount(t, harness, "race-admission-authority-control", 1, "control start")
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// TestRaceOldSuccessNewExecution races an older execution's success against a
// new current execution on the same task. The new start is queued first behind
// a task-row hold; the older execution then succeeds, so the settlement loop's
// current-only completion queues second. Releasing the hold commits the start
// first, and the older success must be ignored by the task's
// lifecycle-version/current-execution guard instead of marking the task done.
func TestRaceOldSuccessNewExecution(t *testing.T) {
	harness := factory.Start(t)
	testOldSuccessNewExecutionContested(t, harness)
}

// testOldSuccessNewExecutionContested runs the contested old-success/new-current
// schedule and then proves the newer current execution's own success still
// completes the task.
func testOldSuccessNewExecutionContested(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	older := startRaceTask(t, harness, "race-old-success-new", "race-old-success-new")
	waitWorkloadObservation(t, harness, older.executionID)
	before := raceReadTask(t, harness, older.taskID)
	if before.CurrentExecution != older.executionID {
		t.Fatalf("task current execution = %q; want the running older %q", before.CurrentExecution, older.executionID)
	}

	hold, err := harness.HoldTaskRow(ctx, older.taskID)
	if err != nil {
		t.Fatalf("hold task row: %v", err)
	}
	requireNoBlockedStatements(t, harness, hold, "tasks", "old-success contested")

	// The replacement start is admitted while the older execution still runs
	// and is the intended winner: it queues first behind the task row.
	start := invokeAsync(t, harness, harness.ProjectToken, "task.start", api.TaskStartRequest{
		TaskID: older.taskID,
	}, &before.LifecycleVersion, "race-old-success-new-start")
	startBlocked, err := harness.WaitForBlockedStatement(ctx, hold, "tasks", raceContentionTimeout)
	if err != nil {
		t.Fatalf("replacement start did not queue behind the hold: %v", err)
	}
	requireQueuedRowLock(t, startBlocked, "tasks", "replacement start winner")
	t.Logf("replacement start queued behind the hold: pid=%d wait=%s/%s query=%q",
		startBlocked.PID, startBlocked.WaitEventType, startBlocked.WaitEvent, startBlocked.Query)

	// Only now does the older workload succeed. Its execution-terminal commit
	// does not touch the task row; the settlement loop then reports the older
	// success and queues its current-only completion second.
	if err := harness.TouchContainerFile(ctx, older.container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release older workload: %v", err)
	}
	olderFinished := waitExecution(t, harness, harness.ProjectToken, older.executionID)
	requireSucceeded(t, olderFinished)
	waiters := awaitContention(t, ctx, harness, hold, "tasks", 2, nil, "older success loser")
	requireContendingWaiters(t, waiters, hold, startBlocked, "tasks", "replacement start winner", "older success loser")

	if err := hold.Release(ctx); err != nil {
		t.Fatalf("release old-success contested hold: %v", err)
	}
	winner := awaitInvocation(t, start, 2*time.Minute, "replacement start winner")
	requireInvocationSuccess(t, winner, "replacement start winner")
	var started api.TaskStartResult
	harness.Decode(winner.result, &started)
	if started.ExecutionID == "" || started.ExecutionID == older.executionID {
		t.Fatalf("replacement start result = %+v; want a fresh execution distinct from %q",
			started, older.executionID)
	}

	// The queued older-success completion has left the task lock queue; its
	// task guard expected the older execution to still be current, so it wrote
	// nothing. The task is still open with the new current execution.
	waitNoBlockedStatements(t, harness, "tasks", "older success loser")
	after := raceReadTask(t, harness, older.taskID)
	if after.State != "open" {
		t.Fatalf("older execution success settled the task: state = %q; want open", after.State)
	}
	if after.CurrentExecution != started.ExecutionID {
		t.Fatalf("task current execution = %q; want the new %q", after.CurrentExecution, started.ExecutionID)
	}
	if after.LifecycleVersion != started.LifecycleVersion {
		t.Fatalf("task lifecycle version = %d; want exactly the replacement start's %d (the older success wrote nothing)",
			after.LifecycleVersion, started.LifecycleVersion)
	}
	if len(after.ExecutionHistory) != 2 || after.ExecutionHistory[0] != older.executionID ||
		after.ExecutionHistory[1] != started.ExecutionID {
		t.Fatalf("task execution history = %v; want [%s %s]", after.ExecutionHistory, older.executionID, started.ExecutionID)
	}
	count, err := harness.ExecutionCount(ctx, older.pipelineID)
	if err != nil {
		t.Fatalf("count executions after older success: %v", err)
	}
	if count != 2 {
		t.Fatalf("execution count = %d; want exactly the older and the newer execution", count)
	}
	raceReceiptCount(t, harness, "race-old-success-new-start", 1, "replacement start winner")
	requireSubjectEventCount(t, harness, "execution.succeeded", older.executionID, 1)

	// The new current execution dispatches after the older one's confirmed
	// stop/cleanup and its own success marks the task done.
	container, _ := waitBlockingWorkload(t, harness, started.ExecutionID)
	if err := harness.TouchContainerFile(ctx, container, raceWorkloadReleasePath); err != nil {
		t.Fatalf("release newer workload: %v", err)
	}
	newerFinished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, newerFinished)
	done := waitTaskState(t, harness, harness.ProjectToken, older.taskID, "done")
	if done.CurrentExecution != started.ExecutionID {
		t.Fatalf("done task current execution = %q; want %q", done.CurrentExecution, started.ExecutionID)
	}
	if done.LifecycleVersion <= started.LifecycleVersion {
		t.Fatalf("done task lifecycle version = %d; want an advance past the start's %d",
			done.LifecycleVersion, started.LifecycleVersion)
	}
	requireSubjectEventCount(t, harness, "execution.succeeded", started.ExecutionID, 1)
	assertExecutionResourcesReleased(t, harness, older.executionID)
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// awaitGate waits for the composition to reach the gated Libpod stop call.
func awaitGate(t *testing.T, gate *factory.StopGate, label string) {
	t.Helper()
	select {
	case <-gate.Reached():
	case <-time.After(raceContentionTimeout):
		t.Fatalf("%s: the composition never issued the gated Libpod stop", label)
	}
}

// requireNoBlockedAdvisoryStatements proves the advisory hold is quiet before
// the winner is issued.
func requireNoBlockedAdvisoryStatements(t *testing.T, harness *factory.Harness, hold *factory.AdvisoryHold, label string) {
	t.Helper()
	observed, err := harness.BlockedAdvisoryStatements(t.Context(), hold)
	if err != nil {
		t.Fatalf("%s: read blocked advisory statements before the race: %v", label, err)
	}
	if len(observed) != 0 {
		t.Fatalf("%s: %d product backend(s) were already queued behind the advisory hold before the winner was issued: %+v",
			label, len(observed), observed)
	}
}

// awaitAdvisoryContention waits for want advisory waiters but fails with the
// loser's public result if it returns before it ever queues.
func awaitAdvisoryContention(t *testing.T, ctx context.Context, harness *factory.Harness, hold *factory.AdvisoryHold, want int, loser <-chan raceInvocation, loserLabel string) []factory.BlockedStatement {
	t.Helper()
	type barrierResult struct {
		waiters []factory.BlockedStatement
		err     error
	}
	done := make(chan barrierResult, 1)
	go func() {
		waiters, err := harness.WaitForBlockedAdvisoryStatements(ctx, hold, want, raceContentionTimeout)
		done <- barrierResult{waiters: waiters, err: err}
	}()
	var loserCase <-chan raceInvocation
	if loser != nil {
		loserCase = loser
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("%s did not contend: %v", loserLabel, result.err)
		}
		return result.waiters
	case outcome := <-loserCase:
		if outcome.err != nil {
			t.Fatalf("%s returned before contending: transport %v", loserLabel, outcome.err)
		}
		t.Fatalf("%s returned before contending: code=%q message=%q; want a queued store transaction",
			loserLabel, outcome.publicError.Code, outcome.publicError.Message)
		return nil
	}
}

// requireQueuedAdvisoryLock proves the observed waiter is the product's
// transaction-scoped membership advisory lock.
func requireQueuedAdvisoryLock(t *testing.T, blocked factory.BlockedStatement, label string) {
	t.Helper()
	query := strings.ToLower(blocked.Query)
	if blocked.PID == 0 {
		t.Fatalf("%s blocked statement = %+v; want a queued advisory lock", label, blocked)
	}
	if !strings.Contains(query, "pg_advisory_xact_lock") || !strings.Contains(query, "hashtextextended") {
		t.Fatalf("%s blocked statement query = %q; want the product's transaction-scoped membership advisory lock",
			label, blocked.Query)
	}
}

// requireContendingAdvisoryWaiters proves two distinct product backends queue
// on the same advisory key, the intended winner first.
func requireContendingAdvisoryWaiters(t *testing.T, waiters []factory.BlockedStatement, hold *factory.AdvisoryHold, winner factory.BlockedStatement, winnerLabel, loserLabel string) {
	t.Helper()
	if len(waiters) != 2 {
		t.Fatalf("%s/%s: observed %d contending advisory waiters; want exactly 2: %+v",
			winnerLabel, loserLabel, len(waiters), waiters)
	}
	if waiters[0].PID != winner.PID {
		t.Fatalf("%s/%s: first queued backend pid=%d, want the already-observed winner pid=%d: %+v",
			winnerLabel, loserLabel, waiters[0].PID, winner.PID, waiters)
	}
	if waiters[1].PID == winner.PID {
		t.Fatalf("%s/%s: the second waiter is the winner backend pid=%d; want a distinct loser backend: %+v",
			winnerLabel, loserLabel, waiters[1].PID, waiters)
	}
	if !slices.Contains(waiters[0].Blockers, hold.PID()) {
		t.Fatalf("%s/%s: winner blockers=%v do not include the advisory hold pid=%d",
			winnerLabel, loserLabel, waiters[0].Blockers, hold.PID())
	}
	if !slices.Contains(waiters[1].Blockers, waiters[0].PID) && !slices.Contains(waiters[1].Blockers, hold.PID()) {
		t.Fatalf("%s/%s: loser blockers=%v include neither the winner pid=%d nor the hold pid=%d",
			winnerLabel, loserLabel, waiters[1].Blockers, waiters[0].PID, hold.PID())
	}
	if waiters[1].QueryStart.Before(waiters[0].QueryStart) {
		t.Fatalf("%s/%s: loser queued at %s before the winner at %s; want winner-first FIFO order: %+v",
			winnerLabel, loserLabel, waiters[1].QueryStart.UTC(), waiters[0].QueryStart.UTC(), waiters)
	}
	requireQueuedAdvisoryLock(t, waiters[0], winnerLabel)
	requireQueuedAdvisoryLock(t, waiters[1], loserLabel)
	t.Logf("%s then %s queued behind the same advisory key: winner pid=%d blockers=%v query_start=%s; loser pid=%d blockers=%v query_start=%s",
		winnerLabel, loserLabel, waiters[0].PID, waiters[0].Blockers, waiters[0].QueryStart.UTC(),
		waiters[1].PID, waiters[1].Blockers, waiters[1].QueryStart.UTC())
}

// waitNoBlockedStatements waits until no product backend is queued on table.
// It is the loser-quiescence barrier: the queued transaction is either still
// waiting or has already run its guard, so an empty queue means the outcome is
// decided and the durable row can be asserted.
func waitNoBlockedStatements(t *testing.T, harness *factory.Harness, table, label string) {
	t.Helper()
	var last []factory.BlockedStatement
	harness.Eventually(t, raceContentionTimeout, func() bool {
		observed, err := harness.BlockedStatementsWithoutHold(t.Context(), table)
		if err != nil {
			return false
		}
		last = observed
		return len(observed) == 0
	})
	if len(last) != 0 {
		t.Fatalf("%s: %d backend(s) are still queued on %s: %+v", label, len(last), table, last)
	}
}
