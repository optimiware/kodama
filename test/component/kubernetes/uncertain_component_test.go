//go:build component

package kubernetesconformance

import (
	"context"
	"errors"
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

// TestCancelledStartLeavesNoJobAndRetryReusesOne proves a start cancelled before
// any effect stays unknown with no Job, and that a retry converges on exactly
// one stable Job instead of replaying.
func TestCancelledStartLeavesNoJobAndRetryReusesOne(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-10")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k10", "action-k10", "workload-k10", "idem-k10")
	request := prepareRequest(identity, workspaceID, "build", "",
		testContainer([]string{`printf 'ok\n'`}, nil), nil, "tok-k10")
	if _, err := fx.Adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	cancelled, cancelStart := context.WithCancel(ctx)
	cancelStart()
	start, err := fx.Adapter.Start(cancelled, engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if start.Outcome != engine.StartUnknown {
		t.Fatalf("cancelled start outcome = %q; want unknown", start.Outcome)
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	jobs, err := fx.Client.ListJobs(ctx, testNamespace, map[string]string{
		runtimekube.LabelWorkload: string(identity.WorkloadID),
	})
	if err != nil {
		t.Fatalf("ListJobs after cancelled start: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("cancelled start created %d Job(s); want 0", len(jobs))
	}

	accepted, err := fx.Adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	if accepted.Outcome != engine.StartAccepted {
		t.Fatalf("retry start outcome = %q; want accepted", accepted.Outcome)
	}
	reused, err := fx.Adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("second retry Start: %v", err)
	}
	if reused.Outcome != engine.StartAccepted {
		t.Fatalf("second retry outcome = %q; want accepted", reused.Outcome)
	}
	oneJob(t, ctx, fx.Client, identity)
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
}

// TestStopWithoutTerminationEvidenceStaysPending proves suspending the Job is an
// acknowledged request, not a stop barrier: a pod that ignores SIGTERM and has
// printed its post-trap marker keeps the stop pending until its container is
// affirmatively gone, while the same suspended Job survives and later confirms.
func TestStopWithoutTerminationEvidenceStaysPending(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-11")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k11", "action-k11", "workload-k11", "idem-k11")
	const marker = "stop-pending-marker"
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`trap '' TERM`, `printf 'stop-pending-marker\n'`, `sleep 20`}, nil), nil, "tok-k11",
	))
	// The trap precedes the marker, so a readable marker proves the workload
	// ignores SIGTERM. Observe's Running plus a pod that merely exists does not.
	waitForWorkloadContainerRunning(t, ctx, fx, identity, marker)
	jobBefore := oneJob(t, ctx, fx.Client, identity)
	if jobBefore.UID == "" {
		t.Fatal("live Job has no metadata.uid")
	}

	evidence, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; a SIGTERM-ignoring pod must stay pending", evidence)
	}
	if evidence.Detail == "" {
		t.Fatalf("pending stop has no detail: %+v", evidence)
	}
	jobAfterStop := oneJob(t, ctx, fx.Client, identity)
	if jobAfterStop.UID != jobBefore.UID {
		t.Fatalf("Job UID changed from %q to %q; Stop must suspend the same Job", jobBefore.UID, jobAfterStop.UID)
	}
	if jobAfterStop.Spec.Suspend == nil || !*jobAfterStop.Spec.Suspend {
		t.Fatalf("Job spec.suspend = %v after Stop; want true", jobAfterStop.Spec.Suspend)
	}

	observation, err := fx.Adapter.Observe(ctx, engine.ObserveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Observe after Stop: %v", err)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v; suspension is never proof the command never ran", observation)
	}
	if observation.Kind == kernel.ObservationNeverStarted {
		t.Fatalf("observation = %+v; a suspended Job is not a never-started proof", observation)
	}

	// Convergence: once the container is gone the surviving Job confirms.
	waitForNoWorkloadPods(t, ctx, fx, identity)
	waitForStopConfirmed(t, ctx, fx, identity, workspaceID)
	jobConfirmed := oneJob(t, ctx, fx.Client, identity)
	if jobConfirmed.UID != jobBefore.UID {
		t.Fatalf("confirmed stop observed Job UID %q; want %q", jobConfirmed.UID, jobBefore.UID)
	}
	if jobConfirmed.DeletionTimestamp != nil {
		t.Fatalf("confirmed stop left the Job deleting: %v", jobConfirmed.DeletionTimestamp)
	}
}

// TestSlowStopRetainsLogsAndReleasesResources is the component-level slow-stop
// proof: a SIGTERM-ignoring workload keeps the stop pending, its retained
// output is still readable while the pod is terminating, and cleanup converges
// on resource removal and workspace release after the container finally exits.
//
// The test never treats log-source absence as stop evidence: the first Stop must
// stay unconfirmed while the container runs, and only the terminated container
// lets the idempotent Remove delete owned resources. Collection happens through
// the real adapter log seam before the destructive removal deletes the
// redaction Secret, matching the composition's collect-before-remove order.
func TestSlowStopRetainsLogsAndReleasesResources(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-13")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k13", "action-k13", "workload-k13", "idem-k13")
	const predecessorMarker = "predecessor-output"
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{
			// Install the trap before printing so the marker is post-trap
			// evidence, then keep the workload alive past the stop request.
			`trap '' TERM`,
			`printf 'predecessor-output\n'`,
			`sleep 25`,
		}, nil), nil, "tok-k13",
	))
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationRunning)
	// Observe can report Running from the Job alone while the pod is still
	// Pending (scheduling, image pull). A log read of a container that has not
	// started is correctly rejected and retried by the adapter, so this test
	// must not treat "pod exists" as "container runs": wait for the workload
	// container's running state and for the marker to be readable before Stop.
	waitForWorkloadContainerRunning(t, ctx, fx, identity, predecessorMarker)
	job := oneJob(t, ctx, fx.Client, identity)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_, _ = fx.Cluster.Kubectl(cleanupCtx, "delete", "pods", "-n", testNamespace,
			"-l", runtimekube.LabelWorkload+"="+string(identity.WorkloadID),
			"--force", "--grace-period=0")
		if err := fx.Client.DeleteJob(cleanupCtx, testNamespace, job.Name); err != nil && !errors.Is(err, runtimekube.ErrNotFound) {
			t.Logf("cleanup Job deletion: %v", err)
		}
	})

	// The requested deletion is acknowledged, not confirmed: the workload
	// ignores SIGTERM and keeps running until its own bounded timer expires.
	pending, err := fx.Adapter.Stop(ctx, engine.StopRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pending.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; a SIGTERM-ignoring workload must stay pending", pending)
	}

	// The pod is terminating but its container still runs, so the retained
	// output collected before removal remains readable. If this read does fail
	// on a real cluster, report the classified cause and the observed pod state
	// instead of a bare error: a dependency_unavailable here means the container
	// was not actually serving its log when Stop returned.
	stdout, stderr, logErr := readWorkloadLogs(ctx, fx.Adapter, identity, 1<<20)
	if logErr != nil {
		t.Fatalf("post-stop retained log read failed: %s; %s",
			classifiedFault(logErr), workloadPodDiagnostic(t, ctx, fx, identity))
	}
	if !containsLine(string(stdout), predecessorMarker) {
		t.Fatalf("post-stop retained stdout = %q; want the predecessor marker %q; %s",
			stdout, predecessorMarker, workloadPodDiagnostic(t, ctx, fx, identity))
	}
	if len(stderr) != 0 {
		t.Fatalf("post-stop retained stderr = %q; want no stderr output", stderr)
	}

	// The stop must be affirmatively confirmed before the destructive Remove
	// path. Suspension keeps the Job; the container's own exit and the fresh
	// zero-counter observation close the barrier.
	waitForStopConfirmed(t, ctx, fx, identity, workspaceID)
	jobAfterStop := oneJob(t, ctx, fx.Client, identity)
	if jobAfterStop.UID != job.UID {
		t.Fatalf("confirmed stop observed Job UID %q; want %q", jobAfterStop.UID, job.UID)
	}

	// Cleanup must converge on its own after the container exits. Removing
	// before then retains the workspace instead of deleting a live writer's
	// resources; the loop below is the Kernel's bounded retry behavior.
	var lastRemoval engine.Removal
	probe := func(ctx context.Context) error {
		removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{
			Identity: identity, WorkspaceID: workspaceID,
		})
		if err != nil {
			return err
		}
		lastRemoval = removal
		if !removal.Removed {
			return fmt.Errorf("removal pending: %s", removal.Detail)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = observationTimeout
	options.AttemptTimeout = 15 * time.Second
	options.InitialBackoff = 200 * time.Millisecond
	options.MaxBackoff = 2 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "slow-stop resource removal", probe, options); err != nil {
		t.Fatalf("slow-stop cleanup did not converge: %v (last removal %+v)", err, lastRemoval)
	}

	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
}

// TestDeletedJobIsNotAnAbsentWorkload proves a Job deleted out from under the
// adapter never turns into a confirmed stop or a nonexecution proof.
func TestDeletedJobIsNotAnAbsentWorkload(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-12")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k12", "action-k12", "workload-k12", "idem-k12")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`trap '' TERM`, `sleep 20`}, nil), nil, "tok-k12",
	))
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationRunning)
	waitForPod(t, ctx, fx.Client, identity)
	job := oneJob(t, ctx, fx.Client, identity)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_, _ = fx.Cluster.Kubectl(cleanupCtx, "delete", "pods", "-n", testNamespace,
			"-l", runtimekube.LabelWorkload+"="+string(identity.WorkloadID),
			"--force", "--grace-period=0")
		if err := fx.Client.DeleteJob(cleanupCtx, testNamespace, job.Name); err != nil && !errors.Is(err, runtimekube.ErrNotFound) {
			t.Logf("cleanup Job deletion: %v", err)
		}
	})

	if err := fx.Client.DeleteJob(ctx, testNamespace, job.Name); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	observation, err := fx.Adapter.Observe(ctx, engine.ObserveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Observe after external deletion: %v", err)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v; an externally deleted Job is not proof of nonexecution", observation)
	}
}

// workloadRunningTimeout bounds the wait for the workload pod to be scheduled
// and for its container to start. Observe reports Running from the Job while
// the pod can still be Pending (scheduling, image pull), so the running
// container needs its own bounded wait inside the test body's
// observationTimeout.
const workloadRunningTimeout = 4 * time.Minute

// waitForWorkloadContainerRunning blocks until exactly one workload pod is
// Running, its workload container's state.running is set and the retained
// marker is readable through the adapter log seam. Observe's Running plus a
// pod that merely exists is not enough: a log read of a container that is
// waiting to start is rejected and correctly retried by the adapter, so a
// post-Stop read would prove nothing about retention.
func waitForWorkloadContainerRunning(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	identity engine.EffectIdentity,
	marker string,
) {
	t.Helper()
	var last runtimekube.Pod
	var lastState string
	var lastLog string
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
		if last.Phase != "Running" {
			return fmt.Errorf("pod phase = %q; want Running", last.Phase)
		}
		document, err := fx.Cluster.GetJSON(ctx, "get", "pod", last.Name, "-n", testNamespace)
		if err != nil {
			return fmt.Errorf("pod status: %w", err)
		}
		state, running := workloadContainerState(document)
		lastState = state
		if !running {
			return fmt.Errorf("workload container state = %q; want running", state)
		}
		stdout, _, err := readWorkloadLogs(ctx, fx.Adapter, identity, 1<<20)
		if err != nil {
			lastLog = classifiedFault(err)
			return fmt.Errorf("marker log read: %w", err)
		}
		if !containsLine(string(stdout), marker) {
			return fmt.Errorf("stdout = %q; want the marker before Stop", stdout)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = workloadRunningTimeout
	options.AttemptTimeout = 10 * time.Second
	options.InitialBackoff = 200 * time.Millisecond
	options.MaxBackoff = 2 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "running workload container", probe, options); err != nil {
		t.Fatalf("wait for running workload container: %v; last pod %q container=%q last log read=%q; %s",
			err, last.Phase, lastState, lastLog, workloadPodDiagnostic(t, ctx, fx, identity))
	}
}

// readWorkloadLogs paginates the adapter log seam without failing the test so
// callers can report the classified fault instead of a bare read error.
func readWorkloadLogs(
	ctx context.Context,
	adapter *runtimekube.Adapter,
	identity engine.EffectIdentity,
	limit int,
) (stdout, stderr []byte, err error) {
	var offset int64
	for page := 0; page < 10000; page++ {
		batch, err := adapter.Logs(ctx, engine.LogsRequest{
			Identity:    identity,
			AfterOffset: offset,
			Limit:       limit,
		})
		if err != nil {
			return stdout, stderr, fmt.Errorf("log read at offset %d: %w", offset, err)
		}
		for _, chunk := range batch.Chunks {
			switch chunk.Stream {
			case engine.LogStdout:
				stdout = append(stdout, chunk.Data...)
			case engine.LogStderr:
				stderr = append(stderr, chunk.Data...)
			}
		}
		if batch.NextOffset == offset {
			return stdout, stderr, nil
		}
		offset = batch.NextOffset
	}
	return stdout, stderr, errors.New("log pagination did not terminate")
}

// classifiedFault names the stable public class and message of a fault so a
// real-cluster failure states its classified cause (the log fault vocabulary)
// rather than only "read failed". The private cause stays out, keeping the
// message free of credential material.
func classifiedFault(err error) string {
	class, ok := fault.ClassOf(err)
	if !ok {
		return "unclassified fault: " + err.Error()
	}
	message, _ := fault.MessageOf(err)
	return fmt.Sprintf("%s: %s", class, message)
}

// workloadPodDiagnostic renders bounded, secret-free pod state for a failure
// message: phase, deletion timestamp, status reason/message and the workload
// container's running/waiting/terminated state.
func workloadPodDiagnostic(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	identity engine.EffectIdentity,
) string {
	t.Helper()
	pods, err := fx.Client.ListPods(ctx, testNamespace, map[string]string{
		runtimekube.LabelWorkload: string(identity.WorkloadID),
	})
	if err != nil {
		return fmt.Sprintf("pod listing failed: %v", err)
	}
	if len(pods) != 1 {
		return fmt.Sprintf("%d workload pods observed; no single pod status to report", len(pods))
	}
	pod := pods[0]
	document, err := fx.Cluster.GetJSON(ctx, "get", "pod", pod.Name, "-n", testNamespace)
	if err != nil {
		return fmt.Sprintf("pod %s phase=%q; status unavailable: %v", pod.Name, pod.Phase, err)
	}
	state, _ := workloadContainerState(document)
	status, _ := document["status"].(map[string]any)
	fields := []string{
		fmt.Sprintf("pod %s phase=%q", pod.Name, pod.Phase),
		"container=" + state,
	}
	if reason := podDocumentText(status, "reason"); reason != "" {
		fields = append(fields, "reason="+reason)
	}
	if message := podDocumentText(status, "message"); message != "" {
		fields = append(fields, "message="+message)
	}
	if pod.DeletionTimestamp != nil {
		fields = append(fields, "deletionTimestamp=set")
	}
	return strings.Join(fields, " ")
}

// workloadContainerState describes the workload container's state in a pod
// document and reports whether state.running is set.
func workloadContainerState(document map[string]any) (string, bool) {
	status, _ := document["status"].(map[string]any)
	containers, _ := status["containerStatuses"].([]any)
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok || podDocumentText(container, "name") != "workload" {
			continue
		}
		states, _ := container["state"].(map[string]any)
		if _, ok := states["running"]; ok {
			return "running", true
		}
		if waiting, ok := states["waiting"].(map[string]any); ok {
			return "waiting(" + podDocumentText(waiting, "reason") + ")", false
		}
		if terminated, ok := states["terminated"].(map[string]any); ok {
			return "terminated(" + podDocumentText(terminated, "reason") + ")", false
		}
		return "unknown", false
	}
	return "absent", false
}

// podDocumentText reads one string field from a decoded kubectl document.
func podDocumentText(document map[string]any, key string) string {
	if document == nil {
		return ""
	}
	value, _ := document[key].(string)
	return value
}
