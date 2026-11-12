//go:build qualification

package production

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStageResilienceControllerPodDeletion is Q02 resilience probe (a). It
// starts a longer task-free inline execution through the public API, deletes
// the kodama controller pod from kodama-system while the execution runs, and
// measures the replacement's Ready time and public API availability. The
// execution must converge without replay: the workload's side-effect marker
// appears exactly once, the action deadline is unchanged and the retained
// workspace identity stays stable.
//
// The frozen public surface exposes a workspace identity only through
// api.ExecutionCleanup, so this probe pins the terminal cleanup identity and
// proves workspace continuity separately through a file the workload writes
// before the restart and still observes afterwards.
//
// Inputs: Q02_TENANT_NAMESPACE is the derived namespace kodama-tenant-<tenant>
// (stage: kodama-tenant-tenant-a), never the bare tenant id. The workload image
// needs a POSIX sh, printf, grep and sleep and must run as root or have a
// writable /workspace; a wrong image fails with the fixed diagnostic
// "q02: /workspace is not writable by the workload user".
func TestStageResilienceControllerPodDeletion(t *testing.T) {
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceControllerPodDeletion")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	onceMarker := "resilience-controller-once-" + nonce
	workspaceMarker := "resilience-controller-workspace-" + nonce
	document := resilienceControllerPipeline(config.Image, onceMarker, workspaceMarker)

	startedAt := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-controller-"+nonce)
	if err != nil {
		t.Fatalf("start the controller-deletion execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-controller-start", startedAt, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})

	running, _, err := resilienceWaitRunning(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("wait for execution %q to run: %v", executionID, err)
	}
	runningObserved := time.Now().UTC()
	action, ok := resilienceAction(running, "hold")
	if !ok {
		t.Fatalf("running execution %q has no hold action: %+v", executionID, running.Actions)
	}
	if action.Deadline == nil {
		t.Fatal("the running action has no deadline")
	}
	if _, err := resilienceWaitWorkloadPod(ctx, stage, executionID, 2*time.Minute); err != nil {
		t.Fatalf("the controller-deletion workload for execution %q did not start: %v", executionID, err)
	}
	// The workload ID is not exposed while running, but every job pod carries
	// the execution label (internal/shell/runtime/kube LabelExecution). Capture
	// fixed pod facts before deletion so a pod loss can be told from a kill.
	workloadSelector := "kodama.dev/execution=" + executionID
	beforeFacts, beforeFactsErr := stage.Cluster.PodFacts(ctx, config.TenantNamespace, workloadSelector)
	beforeFactsObserved := time.Now().UTC()
	if beforeFactsErr != nil {
		t.Logf("read workload pod facts before the controller deletion for execution %q: %v", executionID, beforeFactsErr)
	}
	resilienceRecordPodFacts(probe, executionID, "before", beforeFacts, beforeFactsObserved)
	deadline := *action.Deadline
	deadlineSeconds := action.TimeoutSeconds
	runningVersion := running.Version

	beforePods, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceKodamaSelector)
	if err != nil {
		t.Fatalf("read the controller pods before deletion for execution %q: %v", executionID, err)
	}
	if len(beforePods) != 1 {
		t.Fatalf("controller pods before deletion for execution %q = %v, want the one-replica composition", executionID, beforePods)
	}
	before := make(map[string]bool, len(beforePods))
	for _, name := range beforePods {
		before[name] = true
	}

	deleteStarted := time.Now().UTC()
	if err := stage.Cluster.DeletePods(ctx, ReleaseNamespace, resilienceKodamaSelector); err != nil {
		t.Fatalf("delete the controller pod for execution %q: %v", executionID, err)
	}
	replacement := ""
	_, err = (resilienceWaiter{Timeout: 4 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceKodamaSelector)
		if err != nil {
			return false, nil
		}
		for _, name := range names {
			if before[name] {
				continue
			}
			if err := stage.Cluster.WaitPodReady(ctx, ReleaseNamespace, name, 3*time.Second); err != nil {
				continue
			}
			replacement = name
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("no Ready replacement controller pod appeared for execution %q: %v", executionID, err)
	}
	readyObserved := time.Now().UTC()

	// The pod deletion also tears down the run-stage port-forward. Re-open a
	// guarded same-port forward once a Ready replacement exists and wait for an
	// authenticated read; availability is measured to that first success.
	stage = resilienceEnsureAPIAccess(t, stage)
	apiObserved := time.Now().UTC()

	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 4*time.Minute)
	if err != nil {
		t.Fatalf("execution %q did not converge after the controller deletion: %v (workload facts before: %s; pod read error: %v)",
			executionID, err, resilienceFormatPodFacts("before", beforeFacts), beforeFactsErr)
	}
	terminalObserved := time.Now().UTC()
	afterFacts, afterFactsErr := stage.Cluster.PodFacts(ctx, config.TenantNamespace, workloadSelector)
	afterFactsObserved := time.Now().UTC()
	if afterFactsErr != nil {
		t.Logf("read workload pod facts after the controller deletion for execution %q: %v", executionID, afterFactsErr)
	}
	resilienceRecordPodFacts(probe, executionID, "after", afterFacts, afterFactsObserved)
	if final.State != "succeeded" {
		t.Fatalf("execution %q state after the controller deletion = %q, want succeeded (failure %+v; workload facts before: %s; workload facts after: %s; pod read errors: before=%v after=%v)",
			executionID, final.State, final.Failure,
			resilienceFormatPodFacts("before", beforeFacts), resilienceFormatPodFacts("after", afterFacts),
			beforeFactsErr, afterFactsErr)
	}
	if final.ExecutionID != executionID {
		t.Fatalf("execution identity changed: %q -> %q", executionID, final.ExecutionID)
	}
	finalAction, ok := resilienceAction(final, "hold")
	if !ok {
		t.Fatalf("terminal execution %q has no hold action: %+v", executionID, final.Actions)
	}
	if finalAction.Deadline == nil || !finalAction.Deadline.Equal(deadline) {
		t.Fatalf("action deadline for execution %q changed across the controller deletion: %v -> %v", executionID, deadline, finalAction.Deadline)
	}
	if finalAction.TimeoutSeconds != deadlineSeconds {
		t.Fatalf("action timeout for execution %q changed across the controller deletion: %d -> %d", executionID, deadlineSeconds, finalAction.TimeoutSeconds)
	}
	if final.Version < runningVersion {
		t.Fatalf("execution %q version went backwards: %d -> %d", executionID, runningVersion, final.Version)
	}

	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("execution %q cleanup did not complete: %v", executionID, err)
	}
	cleanupObserved := time.Now().UTC()
	workspaceID := completed.Cleanup.WorkspaceID
	if workspaceID == "" {
		t.Fatalf("terminal cleanup for execution %q did not name the workspace identity", executionID)
	}
	again, err := resilienceInspect(ctx, stage, executionID)
	if err != nil {
		t.Fatalf("re-read completed execution %q: %v", executionID, err)
	}
	if again.Cleanup.WorkspaceID != workspaceID {
		t.Fatalf("workspace identity for execution %q changed: %q -> %q", executionID, workspaceID, again.Cleanup.WorkspaceID)
	}

	logs, err := resilienceReadAllLogs(ctx, stage, executionID, finalAction.ActionID)
	if err != nil {
		t.Fatalf("read the retained workload logs for execution %q: %v", executionID, err)
	}
	onceCount, err := resilienceCountMarker(logs, onceMarker)
	if err != nil {
		t.Fatalf("count the side-effect marker for execution %q: %v", executionID, err)
	}
	if onceCount != 1 {
		t.Fatalf("execution %q side-effect marker appears %d times; a replay would duplicate it", executionID, onceCount)
	}
	workspaceCount, err := resilienceCountMarker(logs, workspaceMarker)
	if err != nil {
		t.Fatalf("count the workspace marker for execution %q: %v", executionID, err)
	}
	if workspaceCount != 1 {
		t.Fatalf("execution %q retained workspace marker appears %d times; the workload did not continue in its workspace", executionID, workspaceCount)
	}
	if !strings.Contains(string(logs), onceMarker) {
		t.Fatalf("the retained logs for execution %q do not contain the action side-effect marker", executionID)
	}

	resilienceAssertNoOrphans(t, stage, executionID, workspaceID)

	probe.Record("resilience-controller-api-availability", deleteStarted, apiObserved, map[string]any{
		"execution_id": executionID,
	})
	probe.Record("resilience-controller-pod-ready", deleteStarted, readyObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  replacement,
	})
	probe.Record("resilience-controller-execution-terminal", runningObserved, terminalObserved, map[string]any{
		"execution_id": executionID,
	})
	probe.Record("resilience-controller-cleanup", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  workspaceID,
	})
}
