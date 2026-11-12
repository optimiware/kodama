//go:build qualification

package production

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestStageResilienceUncertainStopBlocksHandoff is Q02 resilience probe (d).
// A TERM-resistant workload ignores the Job deletion signal until its own
// bounded sleep expires. An ask cancels that execution and a reply queues the
// replacement; the replacement must stay queued while the predecessor's
// action stop is unconfirmed, and its cleanup identities, attempts and pending
// stop reason must be attributable. The workload is self-cleaning: it exits on
// its own timer without any external kill.
func TestStageResilienceUncertainStopBlocksHandoff(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceUncertainStopBlocksHandoff")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	answer := "resilience-answer-" + nonce
	firstMarker := "resilience-handoff-first-" + nonce
	replacementMarker := "resilience-handoff-replacement-" + nonce
	document := resilienceHandoffPipeline(config.Image, answer, firstMarker, replacementMarker, "resilience-handoff-own-timer-"+nonce)

	applied, err := resilienceApplyPipeline(ctx, stage, "resilience-handoff-"+nonce, document, "resilience-handoff-apply-"+nonce)
	if err != nil {
		t.Fatalf("apply the handoff pipeline: %v", err)
	}
	taskCreatedAt := time.Now().UTC()
	result, err := resilienceMutateResult(ctx, stage, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID:  config.ProjectID,
		Title:      "resilience uncertain stop handoff",
		PipelineID: applied.PipelineID,
	}, "resilience-handoff-create-"+nonce)
	if err != nil {
		t.Fatalf("create and start the handoff task: %v", err)
	}
	created, err := resilienceDecodeResult[api.TaskCreateAndStartResult](result, "task.create_and_start")
	if err != nil {
		t.Fatalf("decode the handoff task: %v", err)
	}
	firstExecution := created.ExecutionID
	probe.Record("resilience-handoff-first-start", taskCreatedAt, time.Now().UTC(), map[string]any{
		"execution_id": firstExecution,
	})

	firstRunning, _, err := resilienceWaitRunning(ctx, stage, firstExecution, 2*time.Minute)
	if err != nil {
		t.Fatalf("wait for first execution %q to run: %v", firstExecution, err)
	}
	firstStopable, ok := resilienceAction(firstRunning, "resist")
	if !ok || firstStopable.Deadline == nil {
		t.Fatalf("first execution %q has no running resist action with a deadline: %+v", firstExecution, firstRunning.Actions)
	}
	if _, err := resilienceWaitWorkloadPod(ctx, stage, firstExecution, 2*time.Minute); err != nil {
		t.Fatalf("the TERM-resistant workload for execution %q did not start: %v", firstExecution, err)
	}
	// Let the shell reach its trap and sleep before the ask requests a stop.
	time.Sleep(time.Second)

	askObserved := time.Now().UTC()
	if _, err := resilienceAskCurrent(ctx, stage, created.TaskID, "Continue after the stop barrier?", "role/engineer", "resilience-handoff-ask-"+nonce); err != nil {
		t.Fatalf("ask the handoff question for task %q: %v", created.TaskID, err)
	}
	waiting, err := resilienceReadTask(ctx, stage, created.TaskID)
	if err != nil {
		t.Fatalf("read waiting task %q: %v", created.TaskID, err)
	}
	if waiting.State != "waiting" || waiting.Question == nil {
		t.Fatalf("task %q after ask = %+v, want waiting with an outstanding question", created.TaskID, waiting)
	}

	replied, err := resilienceReplyCurrent(ctx, stage, created.TaskID, answer, "resilience-handoff-reply-"+nonce)
	if err != nil {
		t.Fatalf("reply to the handoff question for task %q: %v", created.TaskID, err)
	}
	replacementExecution := replied.ExecutionID
	if replacementExecution == "" || replacementExecution == firstExecution {
		t.Fatalf("reply execution for task %q = %q, want a fresh execution after %q", created.TaskID, replacementExecution, firstExecution)
	}
	replyObserved := time.Now().UTC()
	probe.Record("resilience-handoff-replacement-start", replyObserved, time.Now().UTC(), map[string]any{
		"execution_id": replacementExecution,
	})

	var (
		observedUnconfirmed bool
		pendingCleanup      api.ExecutionCleanup
		pendingStop         *api.ExecutionStopEvidence
		confirmedObserved   time.Time
		replacementFinal    api.ExecutionInspectResult
		predecessorFinal    api.ExecutionInspectResult
	)
	_, waitErr := (resilienceWaiter{Interval: 200 * time.Millisecond, Timeout: 5 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		// Read the replacement first: a stop confirmation landing between the
		// two reads must never look like a replacement that ran early.
		replacement, err := resilienceInspect(ctx, stage, replacementExecution)
		if err != nil {
			return false, err
		}
		predecessor, err := resilienceInspect(ctx, stage, firstExecution)
		if err != nil {
			return false, err
		}
		replacementFinal = replacement
		predecessorFinal = predecessor

		stopState := ""
		var stop *api.ExecutionStopEvidence
		if action, found := resilienceAction(predecessor, "resist"); found {
			stop = action.Stop
			if stop != nil {
				stopState = stop.State
			}
		}
		switch stopState {
		case "unconfirmed":
			observedUnconfirmed = true
			if pendingCleanup.State == "" {
				pendingCleanup = predecessor.Cleanup
				pendingStop = stop
				resilienceRecordBarrierObservation(probe, replacement, predecessor, stopState, time.Now().UTC())
			}
			if err := resilienceCheckStopBarrier(stopState, replacement.Actions); err != nil {
				resilienceRecordBarrierObservation(probe, replacement, predecessor, stopState, time.Now().UTC())
				return false, fmt.Errorf("replacement execution %q violated the NEW WORK stop barrier: %w (replacement execution state=%q, predecessor stop=%q, predecessor cleanup=%+v, actions: %s)",
					replacementExecution, err, replacement.State, stopState, predecessor.Cleanup, resilienceFormatActions(replacement.Actions))
			}
		case "confirmed":
			if confirmedObserved.IsZero() {
				confirmedObserved = time.Now().UTC()
			}
		}
		return resilienceTerminalInspection(predecessor) && predecessor.Cleanup.State == "complete" &&
			replacement.State == "succeeded", nil
	})
	if waitErr != nil {
		t.Fatalf("handoff predecessor %q / replacement %q did not settle: %v", firstExecution, replacementExecution, waitErr)
	}

	if !observedUnconfirmed {
		t.Fatal("never observed the predecessor's stop as unconfirmed; the TERM-resistant stop barrier was not exercised")
	}
	if pendingStop == nil || pendingStop.Detail == "" {
		t.Fatalf("pending stop evidence = %+v, want an attributable unconfirmed detail", pendingStop)
	}
	if pendingCleanup.WorkloadID == "" || pendingCleanup.WorkspaceID == "" || pendingCleanup.Attempts < 0 {
		t.Fatalf("pending cleanup = %+v, want workload/workspace identities and a non-negative attempt count", pendingCleanup)
	}

	predecessorFinal, _, err = resilienceWaitCleanupComplete(ctx, stage, firstExecution, 3*time.Minute)
	if err != nil {
		t.Fatalf("predecessor cleanup for execution %q did not complete on its own timer: %v", firstExecution, err)
	}
	if predecessorFinal.State != "cancelled" {
		t.Fatalf("predecessor execution %q state = %q, want cancelled", firstExecution, predecessorFinal.State)
	}
	if predecessorFinal.Cleanup.WorkloadID != pendingCleanup.WorkloadID ||
		predecessorFinal.Cleanup.WorkspaceID != pendingCleanup.WorkspaceID {
		t.Fatalf("cleanup identities for execution %q changed across the stop barrier: %+v -> %+v", firstExecution, pendingCleanup, predecessorFinal.Cleanup)
	}
	if predecessorFinal.Cleanup.Attempts < pendingCleanup.Attempts {
		t.Fatalf("cleanup attempts for execution %q went backwards: %d -> %d", firstExecution, pendingCleanup.Attempts, predecessorFinal.Cleanup.Attempts)
	}
	finalStop, ok := resilienceAction(predecessorFinal, "resist")
	if !ok || finalStop.Stop == nil || finalStop.Stop.State != "confirmed" {
		t.Fatalf("predecessor %q stop evidence after cleanup = %+v, want confirmed", firstExecution, finalStop.Stop)
	}

	replacementFinal, _, err = resilienceWaitTerminal(ctx, stage, replacementExecution, 3*time.Minute)
	if err != nil {
		t.Fatalf("replacement execution %q did not settle: %v", replacementExecution, err)
	}
	replacementTerminalObserved := time.Now().UTC()
	if replacementFinal.State != "succeeded" {
		t.Fatalf("replacement execution %q state = %q, want succeeded (failure %+v)", replacementExecution, replacementFinal.State, replacementFinal.Failure)
	}
	replacementAction, ok := resilienceAction(replacementFinal, "resist")
	if !ok {
		t.Fatalf("replacement execution %q has no resist action: %+v", replacementExecution, replacementFinal.Actions)
	}
	replacementClean, _, err := resilienceWaitCleanupComplete(ctx, stage, replacementExecution, 2*time.Minute)
	if err != nil {
		t.Fatalf("replacement execution %q cleanup did not complete: %v", replacementExecution, err)
	}
	replacementCleanupObserved := time.Now().UTC()
	replacementLogs, err := resilienceReadAllLogs(ctx, stage, replacementExecution, replacementAction.ActionID)
	if err != nil {
		t.Fatalf("read replacement %q workload logs: %v", replacementExecution, err)
	}
	if count, err := resilienceCountMarker(replacementLogs, replacementMarker); err != nil || count != 1 {
		t.Fatalf("replacement %q marker count = %d, err = %v; want exactly one", replacementExecution, count, err)
	}
	if count, err := resilienceCountMarker(replacementLogs, firstMarker); err != nil || count != 0 {
		t.Fatalf("the predecessor marker appears %d times in replacement %q logs, err = %v; want zero", count, replacementExecution, err)
	}

	finishedTask, err := resilienceWaitTaskSettled(ctx, stage, created.TaskID, replacementExecution, "done", time.Minute)
	if err != nil {
		t.Fatalf("task %q did not settle done for replacement %q: %v", created.TaskID, replacementExecution, err)
	}
	if finishedTask.State != "done" || finishedTask.CurrentExecution != replacementExecution {
		t.Fatalf("task %q after replacement %q = %+v, want done with the current execution", created.TaskID, replacementExecution, finishedTask)
	}

	predecessorLogs, err := resilienceReadAllLogs(ctx, stage, firstExecution, firstStopable.ActionID)
	if err != nil {
		t.Fatalf("read predecessor %q workload logs: %v", firstExecution, err)
	}
	if count, err := resilienceCountMarker(predecessorLogs, firstMarker); err != nil || count != 1 {
		t.Fatalf("predecessor %q marker count = %d, err = %v; want exactly one", firstExecution, count, err)
	}
	if count, err := resilienceCountMarker(predecessorLogs, replacementMarker); err != nil || count != 0 {
		t.Fatalf("the replacement marker appears %d times in predecessor %q logs, err = %v; want zero", count, firstExecution, err)
	}

	resilienceAssertNoOrphans(t, stage, firstExecution, predecessorFinal.Cleanup.WorkspaceID)
	resilienceAssertNoOrphans(t, stage, replacementExecution, replacementClean.Cleanup.WorkspaceID)

	if confirmedObserved.IsZero() {
		confirmedObserved = time.Now().UTC()
	}
	probe.Record("resilience-handoff-pending", askObserved, confirmedObserved, map[string]any{
		"execution_id": firstExecution,
		"resource_id":  pendingCleanup.WorkspaceID,
	})
	probe.Record("resilience-handoff-queue", replyObserved, confirmedObserved, map[string]any{
		"execution_id": replacementExecution,
	})
	probe.Record("resilience-handoff-replacement", replyObserved, replacementTerminalObserved, map[string]any{
		"execution_id": replacementExecution,
	})
	probe.Record("resilience-handoff-cleanup", replacementTerminalObserved, replacementCleanupObserved, map[string]any{
		"execution_id": replacementExecution,
		"resource_id":  replacementClean.Cleanup.WorkspaceID,
	})
}

// resilienceTerminalInspection reports whether an execution reached a frozen terminal
// state.
func resilienceTerminalInspection(inspection api.ExecutionInspectResult) bool {
	switch inspection.State {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}
