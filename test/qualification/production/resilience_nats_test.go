//go:build qualification

package production

import (
	"context"
	"testing"
	"time"
)

// TestStageResilienceNATSPodDeletion is Q02 resilience probe (b). It runs a
// longer execution, deletes the stack's nats-0 pod, measures the restart to a
// Ready replacement and the time the committed event stream needs to catch up
// to the execution's terminal event. The full committed-event read before and
// after proves no event id was lost or duplicated.
func TestStageResilienceNATSPodDeletion(t *testing.T) {
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceNATSPodDeletion")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	document := resilienceNATSPipeline(config.Image, "resilience-nats-"+nonce)

	startedAt := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-nats-"+nonce)
	if err != nil {
		t.Fatalf("start the NATS-deletion execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-nats-start", startedAt, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})
	running, _, err := resilienceWaitRunning(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("wait for execution %q to run: %v", executionID, err)
	}
	runningObserved := time.Now().UTC()
	if _, ok := resilienceAction(running, "broker-work"); !ok {
		t.Fatalf("running execution %q has no broker-work action: %+v", executionID, running.Actions)
	}
	if _, err := resilienceWaitWorkloadPod(ctx, stage, executionID, 2*time.Minute); err != nil {
		t.Fatalf("the NATS-deletion workload for execution %q did not start: %v", executionID, err)
	}

	beforeEvents, err := resilienceReadAllEvents(ctx, stage)
	if err != nil {
		t.Fatalf("read the committed events before the NATS deletion for execution %q: %v", executionID, err)
	}
	beforeIDs := resilienceEventIDs(beforeEvents)

	beforeUID, err := stage.Cluster.PodUID(ctx, ReleaseNamespace, resilienceNATSPod)
	if err != nil {
		t.Fatalf("read the NATS pod identity before deletion for execution %q: %v", executionID, err)
	}

	deleteStarted := time.Now().UTC()
	if err := stage.Cluster.DeletePod(ctx, ReleaseNamespace, resilienceNATSPod); err != nil {
		t.Fatalf("delete the NATS pod during execution %q: %v", executionID, err)
	}

	_, err = (resilienceWaiter{Timeout: 4 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		uid, err := stage.Cluster.PodUID(ctx, ReleaseNamespace, resilienceNATSPod)
		if err != nil || uid == beforeUID {
			return false, nil
		}
		if err := stage.Cluster.WaitPodReady(ctx, ReleaseNamespace, resilienceNATSPod, 5*time.Second); err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("the NATS pod did not restart Ready during execution %q: %v", executionID, err)
	}
	restartObserved := time.Now().UTC()

	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 4*time.Minute)
	if err != nil {
		t.Fatalf("execution %q did not converge after the NATS restart: %v", executionID, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "succeeded" {
		t.Fatalf("execution %q state after the NATS restart = %q, want succeeded (failure %+v)", executionID, final.State, final.Failure)
	}
	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("execution %q cleanup did not complete: %v", executionID, err)
	}
	cleanupObserved := time.Now().UTC()

	afterEvents, err := resilienceReadAllEvents(ctx, stage)
	if err != nil {
		t.Fatalf("read the committed events after the NATS restart for execution %q: %v", executionID, err)
	}
	delta := resilienceEventDelta(beforeIDs, resilienceEventIDs(afterEvents))
	if len(delta.Lost) != 0 {
		t.Fatalf("committed events were lost across the NATS restart: %v", delta.Lost)
	}
	if len(delta.Duplicated) != 0 {
		t.Fatalf("committed events were duplicated across the NATS restart: %v", delta.Duplicated)
	}
	if delta.After < delta.Before {
		t.Fatalf("committed event count for execution %q shrank: %d -> %d", executionID, delta.Before, delta.After)
	}
	terminalEventID := ""
	for _, event := range afterEvents {
		if event.SubjectID != executionID {
			continue
		}
		if event.Type == "execution.succeeded" {
			terminalEventID = event.ID
		}
	}
	if terminalEventID == "" {
		t.Fatalf("the committed event stream has no execution.succeeded event for %q", executionID)
	}

	if completed.Cleanup.WorkspaceID == "" {
		t.Fatalf("terminal cleanup for execution %q did not name the workspace identity", executionID)
	}
	resilienceAssertNoOrphans(t, stage, executionID, completed.Cleanup.WorkspaceID)

	fields := map[string]any{
		"execution_id": executionID,
		"resource_id":  resilienceNATSPod,
		"event_id":     terminalEventID,
	}
	probe.Record("resilience-nats-restart", deleteStarted, restartObserved, fields)
	probe.Record("resilience-nats-catch-up", runningObserved, terminalObserved, map[string]any{
		"execution_id": executionID,
		"event_id":     terminalEventID,
	})
	probe.Record("resilience-nats-cleanup", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  completed.Cleanup.WorkspaceID,
	})
}
