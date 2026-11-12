//go:build qualification

package production

import (
	"context"
	"testing"
	"time"
)

// TestStageResilienceCleanupAttribution is Q02 resilience probe (e). It runs a
// short execution to completion and proves its terminal cleanup is
// attributable: cleanup completes with a workspace identity and leaves no Job,
// pod or workspace claim labelled with that execution or workspace in the
// assigned tenant namespace.
func TestStageResilienceCleanupAttribution(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceCleanupAttribution")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	marker := "resilience-cleanup-" + nonce
	document := resilienceCleanupPipeline(config.Image, marker)

	executionStarted := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-cleanup-"+nonce)
	if err != nil {
		t.Fatalf("start the cleanup execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-cleanup-start", executionStarted, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})

	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("cleanup execution %q did not settle: %v", executionID, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "succeeded" {
		t.Fatalf("cleanup execution %q state = %q, want succeeded (failure %+v)", executionID, final.State, final.Failure)
	}
	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("cleanup record for execution %q did not complete: %v", executionID, err)
	}
	cleanupObserved := time.Now().UTC()
	if completed.Cleanup.WorkspaceID == "" {
		t.Fatalf("terminal cleanup for execution %q did not name the workspace identity", executionID)
	}
	if completed.Cleanup.WorkloadID == "" {
		t.Fatalf("terminal cleanup for execution %q did not name the workload identity", executionID)
	}

	action, ok := resilienceAction(final, "short-work")
	if !ok {
		t.Fatalf("terminal execution %q has no short-work action: %+v", executionID, final.Actions)
	}
	logs, err := resilienceReadAllLogs(ctx, stage, executionID, action.ActionID)
	if err != nil {
		t.Fatalf("read the cleanup workload logs for execution %q: %v", executionID, err)
	}
	if count, err := resilienceCountMarker(logs, marker); err != nil || count != 1 {
		t.Fatalf("cleanup workload marker count for execution %q = %d, err = %v; want exactly one", executionID, count, err)
	}

	resilienceAssertNoOrphans(t, stage, executionID, completed.Cleanup.WorkspaceID)

	probe.Record("resilience-cleanup-execution", executionStarted, terminalObserved, map[string]any{
		"execution_id": executionID,
	})
	probe.Record("resilience-cleanup-terminal", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  completed.Cleanup.WorkspaceID,
	})
}
