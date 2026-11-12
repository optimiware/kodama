//go:build component

package kubernetesconformance

// Guarded stop convergence on a real kind cluster.
//
// Stop suspends the one stable Job instead of deleting it, and confirms only
// from fresh evidence: the same Job UID, no deletion timestamp, spec.suspend
// true, an acknowledged Suspended/Complete/Failed condition, active 0, a
// present terminating 0 and a successful workload-pod list with no pod. These
// tests prove three real boundaries:
//
//   - an unscheduled Pending pod is removed by suspension while the same
//     non-deleting Job survives long enough to confirm;
//   - a SIGTERM-ignoring running container keeps the stop pending even after
//     active and ready fall to zero, and convergence happens after the pod
//     disappears;
//   - a reconstructed adapter confirms from the surviving Job, not from
//     in-process preparation state or the lost PATCH answer.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// newFixtureAdapter reconstructs the production adapter over the same cluster,
// as a restarted process would. It deliberately carries no prepared-workload
// state.
func newFixtureAdapter(t *testing.T, fx *fixture) *runtimekube.Adapter {
	t.Helper()
	return newFixtureAdapterWithTTL(t, fx, 120)
}

// newFixtureAdapterWithTTL reconstructs the production adapter with an explicit
// finished-Job TTL, so a test can use a TTL short enough to observe.
func newFixtureAdapterWithTTL(t *testing.T, fx *fixture, ttlSeconds int) *runtimekube.Adapter {
	t.Helper()
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   fx.Client,
		Namespace: testNamespace,
		TenantID:  testTenant(),
		KodamaURL: testKodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: ttlSeconds,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("reconstruct Kubernetes adapter: %v", err)
	}
	return adapter
}

// TestPendingJobStopConfirmsAfterSuspension proves the Pending cancellation
// mechanism: suspension removes the unscheduled pod while the same Job survives
// as a durable non-deleting suspended object, and the stop confirms only after
// the counters settle.
func TestPendingJobStopConfirmsAfterSuspension(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-stop-pending")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})

	node := onlyNodeName(t, ctx, fx)
	cordonNode(t, ctx, fx, node)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		uncordonNode(t, cleanupCtx, fx, node)
	})

	identity := testIdentity("exec-stop-pending", "action-stop-pending", "workload-stop-pending", "idem-stop-pending")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`printf 'pending-stop-marker\n'`, `sleep 300`}, nil), nil, "tok-stop-pending",
	))
	pod := waitForUnscheduledPod(t, ctx, fx, identity)
	if pod.NodeName != "" {
		t.Fatalf("Pending pod is already scheduled on node %q", pod.NodeName)
	}
	jobBefore := oneJob(t, ctx, fx.Client, identity)
	if jobBefore.UID == "" {
		t.Fatal("live Job has no metadata.uid")
	}

	pending, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pending.Detail == "" {
		t.Fatalf("stop evidence has no detail: %+v", pending)
	}

	// The suspension PATCH must never delete or replace the Job: the durable
	// object is what lets a later pass confirm instead of losing the only stop
	// evidence together with the pod.
	jobAfterStop := oneJob(t, ctx, fx.Client, identity)
	if jobAfterStop.UID != jobBefore.UID {
		t.Fatalf("Job UID changed from %q to %q; Stop must suspend the same Job", jobBefore.UID, jobAfterStop.UID)
	}
	if jobAfterStop.Spec.Suspend == nil || !*jobAfterStop.Spec.Suspend {
		t.Fatalf("Job spec.suspend = %v after Stop; want true", jobAfterStop.Spec.Suspend)
	}
	if jobAfterStop.DeletionTimestamp != nil {
		t.Fatalf("Stop left a deletion timestamp on the surviving Job: %v", jobAfterStop.DeletionTimestamp)
	}

	// The controller removes the never-started pod and acknowledges the
	// suspension. Only then may the guarded branch confirm.
	waitForNoWorkloadPods(t, ctx, fx, identity)
	confirmed := waitForStopConfirmed(t, ctx, fx, identity, workspaceID)
	if confirmed.Detail == "" {
		t.Fatal("confirmed stop has no detail")
	}

	jobConfirmed := oneJob(t, ctx, fx.Client, identity)
	if jobConfirmed.UID != jobBefore.UID {
		t.Fatalf("confirmed stop observed Job UID %q; want the original %q", jobConfirmed.UID, jobBefore.UID)
	}
	if jobConfirmed.DeletionTimestamp != nil {
		t.Fatalf("confirmed stop left the Job deleting: %v", jobConfirmed.DeletionTimestamp)
	}
	if jobConfirmed.Spec.Suspend == nil || !*jobConfirmed.Spec.Suspend {
		t.Fatalf("confirmed stop observed spec.suspend = %v; want true", jobConfirmed.Spec.Suspend)
	}

	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove after confirmed stop: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the suspended Job's resources removed", removal)
	}
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
}

// TestSuspendedStopConvergesAfterPodDisappears proves a live SIGTERM-ignoring
// workload keeps the stop pending even when active counts reach zero, and that
// the same suspended Job confirms once the pod is gone.
func TestSuspendedStopConvergesAfterPodDisappears(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-stop-running")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-stop-running", "action-stop-running", "workload-stop-running", "idem-stop-running")
	const marker = "live-stop-marker"
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		// The trap is installed before the marker, so a readable marker proves
		// the workload ignores SIGTERM and must not be assumed stopped.
		testContainer([]string{`trap '' TERM`, `printf 'live-stop-marker\n'`, `sleep 20`}, nil), nil, "tok-stop-running",
	))
	waitForWorkloadContainerRunning(t, ctx, fx, identity, marker)
	jobBefore := oneJob(t, ctx, fx.Client, identity)

	pending, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pending.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; a live SIGTERM-ignoring container must stay pending", pending)
	}
	jobAfterStop := oneJob(t, ctx, fx.Client, identity)
	if jobAfterStop.UID != jobBefore.UID {
		t.Fatalf("Job UID changed from %q to %q", jobBefore.UID, jobAfterStop.UID)
	}

	// Wait for the false-confirmation window: the Job reports active 0 while
	// the workload container still runs.
	waitForActiveZeroWhileRunning(t, ctx, fx, identity)
	stillPending, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop while active is zero but the container runs: %v", err)
	}
	if stillPending.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; active 0 is not termination evidence", stillPending)
	}

	waitForNoWorkloadPods(t, ctx, fx, identity)
	confirmed := waitForStopConfirmed(t, ctx, fx, identity, workspaceID)
	if confirmed.Detail == "" {
		t.Fatal("confirmed stop has no detail")
	}
	jobConfirmed := oneJob(t, ctx, fx.Client, identity)
	if jobConfirmed.UID != jobBefore.UID {
		t.Fatalf("confirmed stop observed Job UID %q; want %q", jobConfirmed.UID, jobBefore.UID)
	}
	if jobConfirmed.DeletionTimestamp != nil {
		t.Fatalf("confirmed stop left the Job deleting: %v", jobConfirmed.DeletionTimestamp)
	}

	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want removed", removal)
	}
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
}

// TestSuspendedStopRecoveryUsesSurvivingJob proves a successful suspension whose
// response was lost is recovered by a reconstructed adapter from the durable
// Job and fresh pod evidence.
func TestSuspendedStopRecoveryUsesSurvivingJob(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-stop-recovery")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-stop-recovery", "action-stop-recovery", "workload-stop-recovery", "idem-stop-recovery")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`sleep 120`}, nil), nil, "tok-stop-recovery",
	))
	waitForPodRunning(t, ctx, fx, fx.Client, identity)
	jobBefore := oneJob(t, ctx, fx.Client, identity)
	if jobBefore.UID == "" {
		t.Fatal("live Job has no metadata.uid")
	}

	// The effect happened but its answer was lost: issue the same UID-bound
	// suspension through the driver and discard the returned observation.
	if _, err := fx.Client.SuspendJob(ctx, fx.Namespace, jobBefore.Name, jobBefore.UID); err != nil {
		t.Fatalf("suspension whose response is discarded: %v", err)
	}
	waitForNoWorkloadPods(t, ctx, fx, identity)
	waitForJobSuspended(t, ctx, fx, jobBefore.Name, jobBefore.UID)
	waitForJobCountersZero(t, ctx, fx, jobBefore.Name, jobBefore.UID)

	// A restarted process has no in-process preparation state and no PATCH
	// answer; it must confirm from the surviving Job.
	recovered := newFixtureAdapter(t, fx)
	evidence, err := recovered.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("recovered Stop: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("recovered stop evidence = %+v; want confirmation from the surviving Job", evidence)
	}
	jobAfter := oneJob(t, ctx, fx.Client, identity)
	if jobAfter.UID != jobBefore.UID {
		t.Fatalf("recovered Stop observed Job UID %q; want %q", jobAfter.UID, jobBefore.UID)
	}
	if jobAfter.DeletionTimestamp != nil {
		t.Fatalf("recovered Stop left the Job deleting: %v", jobAfter.DeletionTimestamp)
	}
	if jobAfter.Spec.Suspend == nil || !*jobAfter.Spec.Suspend {
		t.Fatalf("recovered Stop observed spec.suspend = %v; want true", jobAfter.Spec.Suspend)
	}

	removal, err := recovered.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("recovered Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want removed", removal)
	}
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
}

// TestSuspendJobUIDMismatchIsRejectedAndClassified pins what a real API server
// does with a UID-mismatched suspension PATCH: it must be refused with a
// definite, non-uncertain classification and must not change the Job. The
// concrete class is recorded here.
func TestSuspendJobUIDMismatchIsRejectedAndClassified(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-stop-mismatch")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-stop-mismatch", "action-stop-mismatch", "workload-stop-mismatch", "idem-stop-mismatch")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`sleep 30`}, nil), nil, "tok-stop-mismatch",
	))
	job := oneJob(t, ctx, fx.Client, identity)
	if job.UID == "" {
		t.Fatal("live Job has no metadata.uid")
	}

	_, err := fx.Client.SuspendJob(ctx, fx.Namespace, job.Name, job.UID+"-mismatch")
	if err == nil {
		t.Fatal("a UID-mismatched suspension PATCH succeeded")
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("mismatched-UID PATCH error carries no fault class: %v", err)
	}
	message, _ := fault.MessageOf(err)
	t.Logf("mismatched-UID suspension PATCH observed classification %s (%s)", class, message)
	// Pinned on kind 0.33.0 / Kubernetes v1.37.0: the API server refuses the
	// merge PATCH with status 422 (immutable metadata.uid), the driver classifies
	// that definite 4xx as dependency_unavailable, and Stop must preserve that
	// definite class rather than reporting an uncertain effect.
	if class != fault.DependencyUnavailable {
		t.Fatalf("mismatched-UID PATCH classification = %s; want dependency_unavailable (%s)", class, message)
	}
	if !strings.Contains(message, "status 422") {
		t.Fatalf("mismatched-UID PATCH rejection = %q; want the observed status 422 refusal", message)
	}

	after := oneJob(t, ctx, fx.Client, identity)
	if after.UID != job.UID {
		t.Fatalf("Job UID changed to %q after a refused PATCH", after.UID)
	}
	if after.Spec.Suspend != nil && *after.Spec.Suspend {
		t.Fatal("a refused UID-mismatched PATCH still suspended the Job")
	}
	if after.DeletionTimestamp != nil {
		t.Fatalf("a refused UID-mismatched PATCH left the Job deleting: %v", after.DeletionTimestamp)
	}
}

// TestRemoveRefusesWhileWorkloadPodLive proves Remove keeps its live-writer
// guard independently of Stop.
func TestRemoveRefusesWhileWorkloadPodLive(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-remove-live")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-remove-live", "action-remove-live", "workload-remove-live", "idem-remove-live")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`trap '' TERM`, `sleep 60`}, nil), nil, "tok-remove-live",
	))
	waitForPodRunning(t, ctx, fx, fx.Client, identity)
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}

	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removal.Removed {
		t.Fatal("Remove released the workspace while a live workload pod could still write to it")
	}
	if !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("removal detail = %q; want the retained claim named", removal.Detail)
	}
	oneJob(t, ctx, fx.Client, identity)
	if _, err := fx.Client.GetClaim(ctx, fx.Namespace, claimName); err != nil {
		t.Fatalf("Remove deleted the retained claim while the workload is live: %v", err)
	}
}

// onlyNodeName returns the single kind node's name.
func onlyNodeName(t *testing.T, ctx context.Context, fx *fixture) string {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "nodes")
	if err != nil {
		t.Fatalf("read nodes: %v", err)
	}
	items, ok := document["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("node count = %d; want exactly one kind node", len(items))
	}
	node, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("node document is %T; want an object", items[0])
	}
	metadata, ok := node["metadata"].(map[string]any)
	if !ok {
		t.Fatal("node document has no metadata object")
	}
	name, _ := metadata["name"].(string)
	if name == "" {
		t.Fatal("node has no name")
	}
	return name
}

// cordonNode makes the only node unschedulable so a new Job pod stays Pending.
func cordonNode(t *testing.T, ctx context.Context, fx *fixture, node string) {
	t.Helper()
	if _, err := fx.Cluster.Kubectl(ctx, "cordon", node); err != nil {
		t.Fatalf("cordon node %s: %v", node, err)
	}
}

// uncordonNode restores the fixture condition during cleanup.
func uncordonNode(t *testing.T, ctx context.Context, fx *fixture, node string) {
	t.Helper()
	if _, err := fx.Cluster.Kubectl(ctx, "uncordon", node); err != nil && ctx.Err() == nil {
		t.Logf("uncordon node %s during cleanup: %v", node, err)
	}
}

// waitForUnscheduledPod blocks until one workload pod is Pending with no node
// assigned: it never reached a container runtime.
func waitForUnscheduledPod(t *testing.T, ctx context.Context, fx *fixture, identity engine.EffectIdentity) runtimekube.Pod {
	t.Helper()
	var last runtimekube.Pod
	var lastDetail string
	probe := func(ctx context.Context) error {
		pods, err := fx.Client.ListPods(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(pods) != 1 {
			return fmt.Errorf("%d workload pods observed; want exactly 1", len(pods))
		}
		last = pods[0]
		if last.Phase != "Pending" {
			return fmt.Errorf("pod phase = %q; want Pending", last.Phase)
		}
		document, err := fx.Cluster.GetJSON(ctx, "get", "pod", last.Name, "-n", testNamespace)
		if err != nil {
			return fmt.Errorf("pod spec: %w", err)
		}
		spec, _ := document["spec"].(map[string]any)
		nodeName, _ := spec["nodeName"].(string)
		lastDetail = fmt.Sprintf("phase=%q node=%q", last.Phase, nodeName)
		if last.NodeName != "" || nodeName != "" {
			return fmt.Errorf("pod is scheduled: %s", lastDetail)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 5 * time.Minute
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "unscheduled workload pod", probe, options); err != nil {
		t.Fatalf("wait for an unscheduled Pending pod: %v (last %s)", err, lastDetail)
	}
	return last
}

// waitForActiveZeroWhileRunning blocks until the suspended Job reports active 0
// and a nonzero terminating count while the workload container still runs.
func waitForActiveZeroWhileRunning(t *testing.T, ctx context.Context, fx *fixture, identity engine.EffectIdentity) {
	t.Helper()
	var lastJob runtimekube.Job
	var lastState string
	probe := func(ctx context.Context) error {
		jobs, err := fx.Client.ListJobs(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(jobs) != 1 {
			return fmt.Errorf("%d workload Jobs observed; want exactly 1", len(jobs))
		}
		lastJob = jobs[0]
		if lastJob.Status.Active != 0 {
			return fmt.Errorf("Job active = %d; want 0", lastJob.Status.Active)
		}
		if lastJob.Status.Terminating == nil || *lastJob.Status.Terminating == 0 {
			return fmt.Errorf("Job terminating = %v; want a nonzero terminating count", lastJob.Status.Terminating)
		}
		pods, err := fx.Client.ListPods(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(pods) != 1 {
			return fmt.Errorf("%d workload pods observed; want exactly 1", len(pods))
		}
		document, err := fx.Cluster.GetJSON(ctx, "get", "pod", pods[0].Name, "-n", testNamespace)
		if err != nil {
			return fmt.Errorf("pod status: %w", err)
		}
		state, running := workloadContainerState(document)
		lastState = state
		if !running {
			return fmt.Errorf("workload container state = %q; want running", state)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 90 * time.Second
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "zero active counters with a running container", probe, options); err != nil {
		t.Fatalf("wait for the false-confirmation window: %v (last Job %+v container %q)", err, lastJob.Status, lastState)
	}
}

// waitForNoWorkloadPods blocks until the workload selector returns no pod.
func waitForNoWorkloadPods(t *testing.T, ctx context.Context, fx *fixture, identity engine.EffectIdentity) {
	t.Helper()
	var last int
	probe := func(ctx context.Context) error {
		pods, err := fx.Client.ListPods(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		last = len(pods)
		if last != 0 {
			return fmt.Errorf("%d workload pods observed; want none", last)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 2 * time.Minute
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "workload pod removal", probe, options); err != nil {
		t.Fatalf("wait for the workload pod to disappear: %v (last %d)", err, last)
	}
}

// waitForStopConfirmed retries the production Stop until it reports affirmative
// confirmation.
func waitForStopConfirmed(t *testing.T, ctx context.Context, fx *fixture, identity engine.EffectIdentity, workspaceID id.WorkspaceID) kernel.StopEvidence {
	t.Helper()
	var last kernel.StopEvidence
	probe := func(ctx context.Context) error {
		evidence, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		last = evidence
		if !evidence.IsConfirmed() {
			return fmt.Errorf("stop still pending: %s", evidence.Detail)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 3 * time.Minute
	options.AttemptTimeout = 20 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "confirmed stop", probe, options); err != nil {
		t.Fatalf("stop did not confirm: %v (last %+v)", err, last)
	}
	return last
}
