//go:build qualification

package production

// Q02 failure probes (g): workspace loss, workload/node loss, optional
// dependency outage and terminal-cleanup crash attribution. Every probe keeps
// the no-fallback/no-replay invariant: a lost authority or workspace may make
// an execution fail, but it must never make the Shell replay a command, adopt
// a replacement workspace or fall back to another identity.
//
// The workspace probe deletes a real retained claim and is therefore gated on
// an explicit disposable-stage confirmation. Without it the probe skips and
// names every input it needs; it never skips silently and never passes by
// running nothing.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// resilienceExplicitInput names one probe input that is validated in the probe
// itself rather than in Config. It is never persisted to evidence.
type resilienceExplicitInput struct {
	name  string
	value string
}

// resilienceRequireAll skips with every missing stage input and every missing
// destructive-probe confirmation named in one place. An empty value or a
// mismatched confirmation token is missing; the probe never enables a
// destructive path from an accidental value.
func resilienceRequireAll(t *testing.T, config Config, inputs []Input, explicit ...resilienceExplicitInput) {
	t.Helper()
	names := make([]string, 0, len(inputs)+len(explicit))
	for _, input := range config.Missing(inputs...) {
		names = append(names, string(input))
	}
	for _, input := range explicit {
		if input.value == "" {
			names = append(names, input.name)
		}
	}
	if len(names) != 0 {
		if os.Getenv("Q02_REQUIRE_STAGE") == "1" {
			t.Fatalf("required Q02 stage input(s) are missing: %s", strings.Join(names, ", "))
		}
		t.Skipf("unconfigured Q02 stage input(s): %s", strings.Join(names, ", "))
	}
}

// resilienceDescribeFailure renders one bounded failure for a probe diagnostic.
func resilienceDescribeFailure(failure *api.ExecutionFailure) string {
	if failure == nil {
		return "<none>"
	}
	return failure.Kind + ": " + failure.Reason + " " + failure.Detail
}

// TestStageResilienceWorkspaceLoss is Q02 failure probe (g1). It starts an
// execution, deletes the exact retained workspace claim its running workload
// mounts, and requires the execution to fail visibly. The successor action must
// never be activated, the first action's marker must appear exactly once (no
// replay), terminal cleanup must name the workspace/workload identities even
// when the claim is gone, and a fresh authorized attempt must still succeed.
//
// Destructive: it deletes one tenant claim on the stage. It runs only when
// Q02_WORKSPACE_LOSS_NAMESPACE names the assigned tenant namespace and
// Q02_WORKSPACE_LOSS_CONFIRM is exactly destroy-claim.
func TestStageResilienceWorkspaceLoss(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	namespace := strings.TrimSpace(os.Getenv("Q02_WORKSPACE_LOSS_NAMESPACE"))
	confirm := strings.TrimSpace(os.Getenv("Q02_WORKSPACE_LOSS_CONFIRM"))
	if confirm != "destroy-claim" {
		confirm = ""
	}
	resilienceRequireAll(t, config, resilienceResilienceInputs(),
		resilienceExplicitInput{name: "Q02_WORKSPACE_LOSS_NAMESPACE", value: namespace},
		resilienceExplicitInput{name: "Q02_WORKSPACE_LOSS_CONFIRM=destroy-claim", value: confirm},
	)
	if namespace != config.TenantNamespace {
		t.Fatalf("Q02_WORKSPACE_LOSS_NAMESPACE %q is not the assigned tenant namespace %q", namespace, config.TenantNamespace)
	}

	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceWorkspaceLoss")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	onceMarker := "resilience-workspace-once-" + nonce
	afterMarker := "resilience-workspace-after-" + nonce
	document := resilienceWorkspaceLossPipeline(config.Image, onceMarker, afterMarker)

	startedAt := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-workspace-"+nonce)
	if err != nil {
		t.Fatalf("start the workspace-loss execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-workspace-start", startedAt, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})

	running, _, err := resilienceWaitRunning(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("wait for execution %q to run: %v", executionID, err)
	}
	if _, ok := resilienceAction(running, "hold"); !ok {
		t.Fatalf("running execution %q has no hold action: %+v", executionID, running.Actions)
	}
	pod, err := resilienceWaitWorkloadPod(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("the workspace-loss workload for execution %q did not start: %v", executionID, err)
	}
	claim, err := stage.Cluster.PodClaimName(ctx, namespace, pod)
	if err != nil {
		t.Fatalf("read the workspace claim of pod %q for execution %q: %v", pod, executionID, err)
	}

	lossStarted := time.Now().UTC()
	if err := stage.Cluster.RequestClaimDeletion(ctx, namespace, claim); err != nil {
		t.Logf("claim deletion request for %q returned %v; verifying the observed loss", claim, err)
	}
	_, err = (resilienceWaiter{Timeout: time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		return stage.Cluster.ClaimGoneOrTerminating(ctx, namespace, claim)
	})
	if err != nil {
		t.Fatalf("workspace claim %q for execution %q is neither gone nor terminating: %v", claim, executionID, err)
	}
	lossObserved := time.Now().UTC()

	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 5*time.Minute)
	if err != nil {
		t.Fatalf("execution %q did not settle after losing workspace claim %q: %v", executionID, claim, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "failed" {
		t.Fatalf("execution %q after workspace loss = %q, want failed (failure %s); a lost workspace must not continue or be recreated",
			executionID, final.State, resilienceDescribeFailure(final.Failure))
	}
	if final.Failure == nil {
		t.Fatalf("execution %q failed after workspace loss without a visible failure", executionID)
	}
	after, ok := resilienceAction(final, "after")
	if !ok {
		t.Fatalf("terminal execution %q has no after action: %+v", executionID, final.Actions)
	}
	if after.ActivatedAt != nil || after.State == "succeeded" {
		t.Fatalf("successor action after workspace loss = state %q activated_at %v; the lost execution must not advance",
			after.State, after.ActivatedAt)
	}

	hold, ok := resilienceAction(final, "hold")
	if !ok {
		t.Fatalf("terminal execution %q has no hold action: %+v", executionID, final.Actions)
	}
	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("cleanup for the lost-workspace execution %q did not settle: %v", executionID, err)
	}
	cleanupObserved := time.Now().UTC()
	if completed.Cleanup.WorkspaceID == "" {
		t.Fatalf("cleanup for the lost-workspace execution %q did not name the workspace identity", executionID)
	}
	if completed.Cleanup.WorkloadID == "" {
		t.Fatalf("cleanup for the lost-workspace execution %q did not name the workload identity", executionID)
	}
	logs, err := resilienceReadAllLogs(ctx, stage, executionID, hold.ActionID)
	if err != nil {
		t.Fatalf("read the retained workspace-loss logs for execution %q: %v", executionID, err)
	}
	if count, err := resilienceCountMarker(logs, onceMarker); err != nil || count != 1 {
		t.Fatalf("workspace-loss marker count for execution %q = %d, err = %v; want exactly one (a replay would duplicate it)",
			executionID, count, err)
	}
	if count, err := resilienceCountMarker(logs, afterMarker); err != nil || count != 0 {
		t.Fatalf("successor marker count for execution %q = %d, err = %v; the lost workspace must not run the successor",
			executionID, count, err)
	}

	// A failed execution with a lost workspace must not poison the tenant: a
	// fresh authorized attempt gets a new workspace and succeeds.
	recoveryMarker := "resilience-workspace-recovery-" + nonce
	recoveryStarted := time.Now().UTC()
	recovery, err := resilienceStartInline(ctx, stage, resilienceRecoveryPipeline(config.Image, recoveryMarker), "resilience-workspace-recovery-"+nonce)
	if err != nil {
		t.Fatalf("start the fresh attempt after workspace loss for execution %q: %v", executionID, err)
	}
	recovered, _, err := resilienceWaitTerminal(ctx, stage, recovery.ExecutionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("fresh execution %q after workspace loss did not settle: %v", recovery.ExecutionID, err)
	}
	if recovered.State != "succeeded" {
		t.Fatalf("fresh execution %q after workspace loss = %q, want succeeded (failure %s)",
			recovery.ExecutionID, recovered.State, resilienceDescribeFailure(recovered.Failure))
	}
	recoveryObserved := time.Now().UTC()

	probe.Record("resilience-workspace-loss", lossStarted, lossObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  claim,
	})
	probe.Record("resilience-workspace-failure", lossObserved, terminalObserved, map[string]any{
		"execution_id": executionID,
	})
	probe.Record("resilience-workspace-cleanup", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  completed.Cleanup.WorkspaceID,
	})
	probe.Record("resilience-workspace-recovery", recoveryStarted, recoveryObserved, map[string]any{
		"execution_id": recovery.ExecutionID,
	})
}

// TestStageResilienceWorkloadPodLoss is Q02 failure probe (g2), the node-loss
// emulation allowed by the confinement guard: it force-deletes the running
// workload pod with no TERM, exactly what a node disconnect looks like to the
// Job. The execution must fail visibly, the command marker must appear exactly
// once (the Job has backoffLimit 0, so Kubernetes must not replace the pod to
// replay it), cleanup must stay attributable and a fresh attempt must work.
func TestStageResilienceWorkloadPodLoss(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceWorkloadPodLoss")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	beforeMarker := "resilience-pod-before-" + nonce
	afterMarker := "resilience-pod-after-" + nonce
	document := resilienceWorkloadPodLossPipeline(config.Image, beforeMarker, afterMarker)

	startedAt := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, document, "resilience-pod-loss-"+nonce)
	if err != nil {
		t.Fatalf("start the workload-pod-loss execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-pod-loss-start", startedAt, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})

	running, _, err := resilienceWaitRunning(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("wait for execution %q to run: %v", executionID, err)
	}
	if _, ok := resilienceAction(running, "hold"); !ok {
		t.Fatalf("running execution %q has no hold action: %+v", executionID, running.Actions)
	}
	pod, err := resilienceWaitWorkloadPod(ctx, stage, executionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("the pod-loss workload for execution %q did not start: %v", executionID, err)
	}
	uid, err := stage.Cluster.PodUID(ctx, config.TenantNamespace, pod)
	if err != nil {
		t.Fatalf("read the workload pod identity for execution %q: %v", executionID, err)
	}

	lossStarted := time.Now().UTC()
	if err := stage.Cluster.DeletePodForce(ctx, config.TenantNamespace, pod); err != nil {
		t.Fatalf("force-delete workload pod %q for execution %q: %v", pod, executionID, err)
	}
	_, err = (resilienceWaiter{Timeout: time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, config.TenantNamespace, "kodama.dev/execution="+executionID)
		if err != nil {
			return false, nil
		}
		return len(names) == 0, nil
	})
	lossObserved := time.Now().UTC()
	if err != nil {
		t.Fatalf("workload pod %q for execution %q was not deleted: %v", pod, executionID, err)
	}

	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 5*time.Minute)
	if err != nil {
		t.Fatalf("execution %q did not settle after losing workload pod %q: %v", executionID, uid, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "failed" {
		t.Fatalf("execution %q after losing workload pod %q = %q, want failed (failure %s); a lost workload must not become success",
			executionID, uid, final.State, resilienceDescribeFailure(final.Failure))
	}
	if final.Failure == nil {
		t.Fatalf("execution %q failed after losing its workload without a visible failure", executionID)
	}
	hold, ok := resilienceAction(final, "hold")
	if !ok {
		t.Fatalf("terminal execution %q has no hold action: %+v", executionID, final.Actions)
	}
	never, ok := resilienceAction(final, "never")
	if !ok {
		t.Fatalf("terminal execution %q has no never action: %+v", executionID, final.Actions)
	}
	if never.ActivatedAt != nil || never.State == "succeeded" {
		t.Fatalf("action after workload loss = state %q activated_at %v; the lost execution must not advance",
			never.State, never.ActivatedAt)
	}

	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("cleanup for the lost-workload execution %q did not settle: %v", executionID, err)
	}
	cleanupObserved := time.Now().UTC()
	if completed.Cleanup.WorkspaceID == "" || completed.Cleanup.WorkloadID == "" {
		t.Fatalf("cleanup for the lost-workload execution %q lost an identity: %+v", executionID, completed.Cleanup)
	}

	logs, err := resilienceReadAllLogs(ctx, stage, executionID, hold.ActionID)
	if err != nil {
		t.Fatalf("read the retained pod-loss logs for execution %q: %v", executionID, err)
	}
	// False-failure risk: a force-deleted pod may not flush its final stdout
	// chunk, so the retained logs can legitimately lose the pre-kill marker.
	// A duplicate is the replay signal this probe exists to catch, so only
	// more than one occurrence fails; zero occurrences is log loss, not a
	// changed outcome (the absent afterMarker stays a hard requirement).
	if count, err := resilienceCountMarker(logs, beforeMarker); err != nil {
		t.Fatalf("count the pod-loss marker for execution %q: %v", executionID, err)
	} else if count > 1 {
		t.Fatalf("pod-loss marker count for execution %q = %d; want at most one (a replacement pod would duplicate it)",
			executionID, count)
	} else if count == 0 {
		t.Logf("the retained pod-loss logs for execution %q lost the pre-kill marker; a force-deleted pod may not flush its final chunk, so the absence is treated as log loss rather than a replay",
			executionID)
	}
	if count, err := resilienceCountMarker(logs, afterMarker); err != nil || count != 0 {
		t.Fatalf("post-sleep marker count for execution %q = %d, err = %v; a replayed command would finish the sleep",
			executionID, count, err)
	}
	resilienceAssertNoOrphans(t, stage, executionID, completed.Cleanup.WorkspaceID)

	recoveryMarker := "resilience-pod-recovery-" + nonce
	recoveryStarted := time.Now().UTC()
	recovery, err := resilienceStartInline(ctx, stage, resilienceRecoveryPipeline(config.Image, recoveryMarker), "resilience-pod-recovery-"+nonce)
	if err != nil {
		t.Fatalf("start the fresh attempt after workload loss for execution %q: %v", executionID, err)
	}
	recovered, _, err := resilienceWaitTerminal(ctx, stage, recovery.ExecutionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("fresh execution %q after workload loss did not settle: %v", recovery.ExecutionID, err)
	}
	if recovered.State != "succeeded" {
		t.Fatalf("fresh execution %q after workload loss = %q, want succeeded (failure %s)",
			recovery.ExecutionID, recovered.State, resilienceDescribeFailure(recovered.Failure))
	}
	recoveryObserved := time.Now().UTC()

	probe.Record("resilience-pod-loss", lossStarted, lossObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  uid,
	})
	probe.Record("resilience-pod-failure", lossObserved, terminalObserved, map[string]any{
		"execution_id": executionID,
	})
	probe.Record("resilience-pod-cleanup", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  completed.Cleanup.WorkspaceID,
	})
	probe.Record("resilience-pod-recovery", recoveryStarted, recoveryObserved, map[string]any{
		"execution_id": recovery.ExecutionID,
	})
}

// TestStageResilienceCleanupCrashAttribution is Q02 failure probe (g3). It
// crashes the controller immediately after a terminal execution commits, while
// terminal cleanup is pending or in progress, and requires the replacement
// controller to finish that same cleanup with the same workspace/workload
// identities, no duplicated command and no orphaned Job, pod or claim.
//
// Attribution needs three facts. A pre-crash inspect must observe the pending
// obligation with a non-empty workspace identity. The crash must be a real one:
// the controller pods are force-deleted with --grace-period=0 --force, because
// a graceful delete lets the terminating process finish the pending cleanup and
// the probe would pass even when the replacement did nothing. After the old
// pods are gone the obligation is inspected again: if cleanup is still pending,
// only the replacement can finish it; if it is already complete, the completion
// happened in the gap and the probe skips with the named reason
// resilience-cleanup-crash-not-attributable instead of passing vacuously.
func TestStageResilienceCleanupCrashAttribution(t *testing.T) {
	config := loadStageConfig(t)
	requireStageInputs(t, config, resilienceResilienceInputs()...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceCleanupCrashAttribution")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	nonce := resilienceNonce()
	marker := "resilience-cleanup-crash-" + nonce
	startedAt := time.Now().UTC()
	startedExecution, err := resilienceStartInline(ctx, stage, resilienceCleanupCrashPipeline(config.Image, marker), "resilience-cleanup-crash-"+nonce)
	if err != nil {
		t.Fatalf("start the cleanup-crash execution: %v", err)
	}
	executionID := startedExecution.ExecutionID
	probe.Record("resilience-cleanup-crash-start", startedAt, time.Now().UTC(), map[string]any{
		"execution_id": executionID,
	})

	if _, _, err := resilienceWaitRunning(ctx, stage, executionID, 2*time.Minute); err != nil {
		t.Fatalf("wait for cleanup-crash execution %q to run: %v", executionID, err)
	}
	if _, err := resilienceWaitWorkloadPod(ctx, stage, executionID, 2*time.Minute); err != nil {
		t.Fatalf("cleanup-crash workload pod for execution %q did not start: %v", executionID, err)
	}
	// Allow the container to install its TERM trap and emit its single marker
	// before cancelling, so Stop remains unconfirmed across the crash window
	// while pre-stop log checkpointing retains the marker.
	time.Sleep(2 * time.Second)

	beforePods, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceKodamaSelector)
	if err != nil || len(beforePods) != 1 {
		t.Fatalf("controller pods before the cleanup crash for execution %q = %v, err = %v; want one", executionID, beforePods, err)
	}
	if _, err := resilienceCancelCurrent(ctx, stage, executionID, "resilience-cleanup-crash-cancel-"+nonce); err != nil {
		t.Fatalf("cancel cleanup-crash execution %q: %v", executionID, err)
	}
	final, _, err := resilienceWaitTerminal(ctx, stage, executionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("cleanup-crash execution %q did not settle: %v", executionID, err)
	}
	terminalObserved := time.Now().UTC()
	if final.State != "cancelled" {
		t.Fatalf("cleanup-crash execution %q = %q, want cancelled", executionID, final.State)
	}
	// The terminal commit persists the cleanup obligation as pending with its
	// workspace and workload identities before any cleanup work runs
	// (internal/shell/store/units/execution commitCleanup). Only a read taken
	// now, while that obligation is still outstanding, can prove the crash
	// landed on an interrupted cleanup: final.Cleanup is an earlier poll, and
	// comparing two reads taken after the crash says nothing about the
	// pre-crash state. "pending" is the only in-progress cleanup state in the
	// frozen none|pending|complete vocabulary; "none" would mean the execution
	// never had a cleanup obligation, so this probe would be vacuous too.
	snapshot, err := resilienceInspect(ctx, stage, executionID)
	if err != nil {
		t.Fatalf("read the pre-crash cleanup state of execution %q: %v", executionID, err)
	}
	switch snapshot.Cleanup.State {
	case "pending":
	case "complete":
		t.Skipf("resilience-cleanup-crash-vacuous: cleanup for execution %q completed before the controller deletion; cleanup cannot be attributed to an interruption", executionID)
	default:
		t.Fatalf("cleanup for execution %q before the controller deletion = %q, want pending (an outstanding cleanup obligation): %+v",
			executionID, snapshot.Cleanup.State, snapshot.Cleanup)
	}
	// The pending obligation always carries the workspace identity: every
	// cleanup write passes execution.WorkspaceID. An empty one is corrupt, and
	// the identity comparison must fail rather than silently accept anything.
	if snapshot.Cleanup.WorkspaceID == "" {
		t.Fatalf("pending cleanup for execution %q carries no workspace identity; the terminal commit persists it, so this record cannot attribute the interrupted cleanup: %+v",
			executionID, snapshot.Cleanup)
	}

	crashStarted := time.Now().UTC()
	// Force-delete every controller pod: no TERM, no grace period and no chance
	// for the old process to finish the pending cleanup while terminating. A
	// graceful deletion would let the crashed controller complete the work and
	// the probe could not tell which controller performed it.
	for _, name := range beforePods {
		if err := stage.Cluster.DeletePodForce(ctx, ReleaseNamespace, name); err != nil {
			t.Fatalf("force-delete controller pod %q during cleanup of execution %q: %v", name, executionID, err)
		}
	}
	before := make(map[string]bool, len(beforePods))
	for _, name := range beforePods {
		before[name] = true
	}
	// The old controller must be gone before the replacement is allowed to be
	// credited. With a forced deletion this is immediate; a pod that outlives
	// the bound leaves the attribution unprovable.
	_, err = (resilienceWaiter{Timeout: 2 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceKodamaSelector)
		if err != nil {
			return false, nil
		}
		for _, name := range names {
			if before[name] {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("the force-deleted controller pod(s) %v for execution %q were still present after the crash; cleanup cannot be attributed to the replacement",
			beforePods, executionID)
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
		t.Fatalf("no Ready replacement controller appeared after the cleanup crash for execution %q: %v", executionID, err)
	}
	readyObserved := time.Now().UTC()
	stage = resilienceEnsureAPIAccess(t, stage)

	// The replacement is now the only controller process. Re-read the persisted
	// obligation as early as it serves the API: a still-pending record proves
	// the interrupted cleanup survived the crash and can only be finished by
	// the replacement. A record that is already complete finished in the gap
	// (before the force deletion took effect or before this first read), so the
	// probe skips with a named reason instead of crediting the replacement.
	resumed, err := resilienceInspect(ctx, stage, executionID)
	if err != nil {
		t.Fatalf("read the post-crash cleanup state of execution %q: %v", executionID, err)
	}
	reason, err := resilienceCleanupCrashVerdict(resumed.Cleanup.State)
	if err != nil {
		t.Fatalf("post-crash cleanup for execution %q is not the interrupted obligation: %v; record %+v",
			executionID, err, resumed.Cleanup)
	}
	if reason != "" {
		t.Skipf("%s: cleanup for execution %q completed in the gap before the replacement %q could report an interrupted obligation (controller pods %v were force-deleted); the completion cannot be attributed to the replacement",
			reason, executionID, replacement, beforePods)
	}
	if err := resilienceCleanupObligationError(snapshot.Cleanup, resumed.Cleanup); err != nil {
		t.Fatalf("cleanup for execution %q is not the pre-crash obligation after the controller crash: %v; pre-crash %+v, post-crash %+v",
			executionID, err, snapshot.Cleanup, resumed.Cleanup)
	}

	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, executionID, 4*time.Minute)
	if err != nil {
		t.Fatalf("cleanup for execution %q did not complete after the controller crash (state before the crash %q): %v",
			executionID, snapshot.Cleanup.State, err)
	}
	cleanupObserved := time.Now().UTC()
	if completed.Cleanup.WorkspaceID == "" || completed.Cleanup.WorkloadID == "" {
		t.Fatalf("cleanup after the controller crash lost an identity: %+v", completed.Cleanup)
	}
	// The pre-crash identities are the ones the terminal commit persisted. The
	// replacement must finish that same cleanup, never replace or re-derive the
	// identities, so a non-empty pre-crash identity has to survive unchanged.
	if err := resilienceCleanupObligationError(snapshot.Cleanup, completed.Cleanup); err != nil {
		t.Fatalf("completed cleanup for execution %q is not the pre-crash obligation: %v; pre-crash %+v, completed %+v",
			executionID, err, snapshot.Cleanup, completed.Cleanup)
	}
	surviving, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, resilienceKodamaSelector)
	if err != nil {
		t.Fatalf("read the controller pods after the attributed cleanup of execution %q: %v", executionID, err)
	}
	if len(surviving) != 1 || surviving[0] != replacement {
		t.Fatalf("controller pods after the attributed cleanup of execution %q = %v, want only the replacement %q",
			executionID, surviving, replacement)
	}
	action, ok := resilienceAction(completed, "short-work")
	if !ok {
		t.Fatalf("execution %q has no short-work action: %+v", executionID, completed.Actions)
	}
	logs, err := resilienceReadAllLogs(ctx, stage, executionID, action.ActionID)
	if err != nil {
		t.Fatalf("read the retained cleanup-crash logs for execution %q: %v", executionID, err)
	}
	if count, err := resilienceCountMarker(logs, marker); err != nil || count != 1 {
		t.Fatalf("cleanup-crash marker count for execution %q = %d, err = %v; want exactly one", executionID, count, err)
	}
	resilienceAssertNoOrphans(t, stage, executionID, completed.Cleanup.WorkspaceID)

	probe.Record("resilience-cleanup-crash", crashStarted, readyObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  replacement,
	})
	probe.Record("resilience-cleanup-crash-complete", terminalObserved, cleanupObserved, map[string]any{
		"execution_id": executionID,
		"resource_id":  completed.Cleanup.WorkspaceID,
	})
}

// TestStageResilienceGitDependencyOutage is Q02 failure probe (g4). It stops
// the tenant's optional Git StatefulSet, proves ordinary task-free work still
// commits and progresses without the dependency, proves the outage is visible
// as the exact zero-replica object (not a silently recreated replacement), then
// restores the saved replica count in body with a read-back and measures the
// restart. Optional dependencies must never block the core factory, and no
// command may be replayed to cover the outage.
//
// This probe covers core independence from the optional Git dependency and the
// observed zero-replica outage/restore window. It does not exercise a Git-backed
// push flow (no pipeline pushes to or clones from the repository), so it never
// requires Q02_GIT_REPOSITORY_A; only the StatefulSet that serves Git is used.
func TestStageResilienceGitDependencyOutage(t *testing.T) {
	config := loadStageConfig(t)
	requireStageInputs(t, config, append(resilienceResilienceInputs(), InputTenantID, InputGitServiceA)...)
	stage := resilienceEnsureAPIAccess(t, resilienceConnect(t, config))
	probe := resilienceBeginProbe(t, config, "TestStageResilienceGitDependencyOutage")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	guard := resilienceNewReplicaGuard(stage.Cluster)
	saved, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", config.GitServiceA)
	if err != nil {
		t.Fatalf("save the tenant Git replica count: %v", err)
	}
	if saved < 1 {
		t.Fatalf("tenant Git statefulset %q is already at %d replicas; restore it manually before the probe",
			config.GitServiceA, saved)
	}
	// Restore on cleanup before the first scale so a mid-probe failure still
	// reaches the saved count.
	resilienceRestoreOnCleanup(t, probe, guard)

	outageStarted := time.Now().UTC()
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", config.GitServiceA, 0); err != nil {
		t.Fatalf("scale tenant Git statefulset %q to zero: %v", config.GitServiceA, err)
	}
	selector := "app.kubernetes.io/name=kodama,app.kubernetes.io/component=git-service,kodama.dev/tenant=" + config.TenantID
	_, err = (resilienceWaiter{Timeout: 2 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, selector)
		if err != nil {
			return false, nil
		}
		return len(names) == 0, nil
	})
	if err != nil {
		t.Fatalf("tenant Git pods did not stop after scaling %q to zero: %v", config.GitServiceA, err)
	}
	outageObserved := time.Now().UTC()

	// The core factory must progress while its optional dependency is absent.
	nonce := resilienceNonce()
	marker := "resilience-git-outage-" + nonce
	workStarted := time.Now().UTC()
	work, err := resilienceStartInline(ctx, stage, resilienceGitOutagePipeline(config.Image, marker), "resilience-git-outage-"+nonce)
	if err != nil {
		t.Fatalf("start ordinary work during the Git outage: %v", err)
	}
	final, _, err := resilienceWaitTerminal(ctx, stage, work.ExecutionID, 3*time.Minute)
	if err != nil {
		t.Fatalf("ordinary execution %q did not settle during the Git outage: %v", work.ExecutionID, err)
	}
	workObserved := time.Now().UTC()
	if final.State != "succeeded" {
		t.Fatalf("ordinary execution %q during the Git outage = %q, want succeeded (failure %s); an optional dependency must not block the core factory",
			work.ExecutionID, final.State, resilienceDescribeFailure(final.Failure))
	}
	action, ok := resilienceAction(final, "plain-work")
	if !ok {
		t.Fatalf("ordinary execution %q has no plain-work action: %+v", work.ExecutionID, final.Actions)
	}
	// Retained log chunks are drained when the workspace is removed, so the
	// cleanup must complete before the marker can be read; reading earlier sees
	// an empty retained log and would report a false zero.
	completed, _, err := resilienceWaitCleanupComplete(ctx, stage, work.ExecutionID, 2*time.Minute)
	if err != nil {
		t.Fatalf("cleanup for the Git-outage execution %q did not complete: %v", work.ExecutionID, err)
	}
	logs, err := resilienceReadAllLogs(ctx, stage, work.ExecutionID, action.ActionID)
	if err != nil {
		t.Fatalf("read the retained Git-outage logs for execution %q: %v", work.ExecutionID, err)
	}
	if count, err := resilienceCountMarker(logs, marker); err != nil || count != 1 {
		t.Fatalf("Git-outage marker count for execution %q = %d, err = %v; want exactly one", work.ExecutionID, count, err)
	}
	resilienceAssertNoOrphans(t, stage, work.ExecutionID, completed.Cleanup.WorkspaceID)

	// Restore in body, even if the ordinary work failed, then read the observed
	// replica count back. A successful scale request alone is not restoration.
	restoration, restoreErr := resilienceCloseOutage(ctx, guard, resilienceBrokerRestoreBudget)
	if restoreErr != nil {
		t.Fatalf("tenant Git restore failed: %v; manual restore: kubectl --context %s -n %s scale statefulsets %s --replicas=%d",
			restoreErr, config.KubeContext, ReleaseNamespace, config.GitServiceA, saved)
	}
	_, err = (resilienceWaiter{Timeout: 3 * time.Minute}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, ReleaseNamespace, selector)
		if err != nil || len(names) == 0 {
			return false, nil
		}
		return stage.Cluster.WaitPodReady(ctx, ReleaseNamespace, names[0], 5*time.Second) == nil, nil
	})
	if err != nil {
		t.Fatalf("tenant Git pods did not return Ready after the restore: %v", err)
	}
	readyObserved := time.Now().UTC()

	probe.Record("resilience-git-outage", outageStarted, outageObserved, map[string]any{
		"resource_id": config.GitServiceA,
	})
	probe.Record("resilience-git-independent-work", workStarted, workObserved, map[string]any{
		"execution_id": work.ExecutionID,
	})
	probe.Record("resilience-git-restore", outageObserved, readyObserved, map[string]any{
		"resource_id": config.GitServiceA,
	})
	if restoration != "restored" {
		t.Fatalf("tenant Git restoration = %q, want restored", restoration)
	}
}
