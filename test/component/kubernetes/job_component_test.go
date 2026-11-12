//go:build component

package kubernetesconformance

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// podRunningTimeout bounds the wait for the workload pod to be scheduled and
// started. Observe can report Running from the Job alone while the pod is still
// Pending (image pull, scheduling), so the pod phase needs its own bounded wait
// inside the test body's observationTimeout.
const podRunningTimeout = 4 * time.Minute

// TestJobCompletesWithPodEvidence proves a real Job runs once, reports an
// affirmative exit with pod evidence and exposes both log streams.
func TestJobCompletesWithPodEvidence(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-1")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k1", "action-k1", "workload-k1", "idem-k1")
	commands := []string{
		`printf 'done\n' > /workspace/result.txt`,
		`printf 'hello-stdout\n'`,
		`printf 'hello-stderr\n' >&2`,
	}
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "task-k1",
		testContainer(commands, nil), nil, "tok-k1",
	))

	observation := waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	code, ok := observation.ExitStatus()
	if !ok || code != 0 {
		t.Fatalf("exit status = %d/%v; want 0/true (%+v)", code, ok, observation)
	}
	if !observation.StopConfirmed() {
		t.Fatalf("exited observation has no confirmed stop: %+v", observation)
	}

	job := waitForJobSettled(t, ctx, fx.Client, identity)
	if job.Status.Succeeded != 1 || job.Status.Failed != 0 {
		t.Fatalf("job status = %+v; want succeeded 1 failed 0", job.Status)
	}
	pod := onePod(t, ctx, fx.Client, identity)
	if pod.Phase != "Succeeded" {
		t.Fatalf("pod phase = %q; want Succeeded", pod.Phase)
	}
	if pod.NodeName == "" {
		t.Fatal("pod has no node name")
	}
	terminated := false
	for _, container := range pod.Containers {
		if container.Name == "workload" {
			if !container.Terminated || container.ExitCode != 0 {
				t.Fatalf("workload container = %+v; want terminated exit 0", container)
			}
			terminated = true
		}
	}
	if !terminated {
		t.Fatalf("pod has no workload container status: %+v", pod.Containers)
	}

	stdout, stderr := readAllLogs(t, ctx, fx.Adapter, identity, 1<<20)
	if !containsLine(string(stdout), "hello-stdout") {
		t.Fatalf("stdout = %q; want the workload line", stdout)
	}
	if !containsLine(string(stdout), "hello-stderr") || len(stderr) != 0 {
		t.Fatalf("combined log = %q, stderr = %q; want both lines in the combined log", stdout, stderr)
	}
}

// TestJobFailureIsObservedNotReplayed proves a nonzero exit is observed exactly
// once and never replayed: BackoffLimit zero and PodReplacementPolicy Failed
// leave one failed pod.
func TestJobFailureIsObservedNotReplayed(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-2")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k2", "action-k2", "workload-k2", "idem-k2")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`exit 7`}, nil), nil, "tok-k2",
	))

	observation := waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	code, ok := observation.ExitStatus()
	if !ok || code != 7 {
		t.Fatalf("exit status = %d/%v; want 7/true (%+v)", code, ok, observation)
	}
	if !observation.StopConfirmed() {
		t.Fatalf("failed exit is not a confirmed stop: %+v", observation)
	}

	job := waitForJobSettled(t, ctx, fx.Client, identity)
	if job.Status.Failed != 1 || job.Status.Succeeded != 0 {
		t.Fatalf("job status = %+v; want failed 1 succeeded 0", job.Status)
	}
	pod := onePod(t, ctx, fx.Client, identity)
	if pod.Phase != "Failed" {
		t.Fatalf("pod phase = %q; want Failed", pod.Phase)
	}
	terminated := false
	for _, container := range pod.Containers {
		if container.Name == "workload" {
			if !container.Terminated || container.ExitCode != 7 {
				t.Fatalf("workload container = %+v; want terminated exit 7", container)
			}
			terminated = true
		}
	}
	if !terminated {
		t.Fatalf("pod has no workload container status: %+v", pod.Containers)
	}
	// onePod and oneJob already prove there is exactly one object per identity;
	// BackoffLimit zero and PodReplacementPolicy Failed make a replay impossible.
}

// TestFreshAdapterAdoptsRunningJob proves a replacement driver and adapter (the
// shape of a restarted controller) adopt a live Job by observing the same
// stable identity, without preparing it again, deleting it or replaying the
// command. This is the runtime-level half of controller-restart safety:
// the composition must never stop a durable workload on close, and a fresh
// adapter must be able to observe the surviving Job.
func TestFreshAdapterAdoptsRunningJob(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-adopt")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-adopt", "action-adopt", "workload-adopt", "idem-adopt")
	// Register cleanup before the workload exists, so a failure before or during
	// Start still removes the objects this test caused. The fixture's cluster
	// delete is later in the LIFO chain and would also remove the namespace, but
	// the test-owned Job and Pod are deleted explicitly and first.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		selector := runtimekube.LabelWorkload + "=" + string(identity.WorkloadID)
		for _, resource := range []string{"pods", "jobs"} {
			result, err := fx.Cluster.Kubectl(cleanupCtx, "delete", resource,
				"-n", fx.Namespace, "-l", selector, "--ignore-not-found", "--wait=false")
			if err != nil {
				t.Logf("cleanup %s for workload %s: %v (%s)", resource, identity.WorkloadID, err, strings.TrimSpace(string(result.Stderr)))
			}
		}
	})
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`printf 'adopt\n'; sleep 120`}, nil), nil, "tok-adopt",
	))
	// Observe may report Running from the Job alone while the pod is still
	// Pending (image pull or scheduling), so the pod itself must reach Running
	// before the identity snapshot is taken.
	podBefore := waitForPodRunning(t, ctx, fx, fx.Client, identity)
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationRunning)
	jobBefore := oneJob(t, ctx, fx.Client, identity)
	jobUIDBefore := objectUID(t, ctx, fx, "job", jobBefore.Name)
	podUIDBefore := objectUID(t, ctx, fx, "pod", podBefore.Name)

	// A fresh driver/adapter over the same cluster, as a restarted controller
	// builds. Its in-memory prepared map is empty: adoption must come from the
	// durable Job and Pod identity, not from previous controller state.
	kubeconfig, err := fx.Cluster.KubeconfigJSON(ctx)
	if err != nil {
		t.Fatalf("export kubeconfig: %v", err)
	}
	client, err := driverkube.NewCluster(driverkube.ClusterConfig{
		Kubeconfig: kubeconfig,
		Timeout:    time.Minute,
	})
	if err != nil {
		t.Fatalf("build fresh Kubernetes driver: %v", err)
	}
	fresh, err := runtimekube.New(runtimekube.Config{
		Cluster:   client,
		Namespace: fx.Namespace,
		TenantID:  fx.Tenant,
		KodamaURL: testKodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("build fresh Kubernetes adapter: %v", err)
	}
	waitForObservation(t, ctx, fresh, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationRunning)

	jobAfter := oneJob(t, ctx, client, identity)
	if jobAfter.Name != jobBefore.Name {
		t.Fatalf("fresh adapter observed Job %q; want the adopted %q", jobAfter.Name, jobBefore.Name)
	}
	if uid := objectUID(t, ctx, fx, "job", jobAfter.Name); uid != jobUIDBefore {
		t.Fatalf("Job %s UID = %q; want the adopted UID %q", jobAfter.Name, uid, jobUIDBefore)
	}
	podAfter := waitForPodRunning(t, ctx, fx, client, identity)
	if podAfter.Name != podBefore.Name {
		t.Fatalf("fresh adapter observed Pod %q; want the adopted %q", podAfter.Name, podBefore.Name)
	}
	if podAfter.DeletionTimestamp != nil {
		t.Fatalf("adopted pod %s is deleting: %v", podAfter.Name, podAfter.DeletionTimestamp)
	}
	if podAfter.Phase != "Running" {
		t.Fatalf("adopted pod phase = %q; want Running", podAfter.Phase)
	}
	if uid := objectUID(t, ctx, fx, "pod", podAfter.Name); uid != podUIDBefore {
		t.Fatalf("Pod %s UID = %q; want the adopted UID %q", podAfter.Name, uid, podUIDBefore)
	}

	// A restarted controller may retry the same Start. The durable Job must be
	// adopted: Start is accepted with the existing stable Job, and no second Job
	// or replacement Pod appears.
	start, err := fresh.Start(ctx, engine.StartRequest{
		Identity: identity, WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("fresh Start on the adopted workload: %v", err)
	}
	if start.Outcome != engine.StartAccepted {
		t.Fatalf("fresh Start outcome = %q; want accepted (%s)", start.Outcome, start.Detail)
	}
	if !strings.Contains(start.Detail, "reused existing stable Job") {
		t.Fatalf("fresh Start detail = %q; want reuse of the adopted stable Job", start.Detail)
	}
	jobAfterStart := oneJob(t, ctx, client, identity)
	if jobAfterStart.Name != jobBefore.Name {
		t.Fatalf("fresh Start observed Job %q; want the adopted %q", jobAfterStart.Name, jobBefore.Name)
	}
	if uid := objectUID(t, ctx, fx, "job", jobAfterStart.Name); uid != jobUIDBefore {
		t.Fatalf("Job %s UID after fresh Start = %q; want the adopted UID %q", jobAfterStart.Name, uid, jobUIDBefore)
	}
	podAfterStart := waitForPodRunning(t, ctx, fx, client, identity)
	if podAfterStart.Name != podBefore.Name {
		t.Fatalf("fresh Start observed Pod %q; want the adopted %q", podAfterStart.Name, podBefore.Name)
	}
	if uid := objectUID(t, ctx, fx, "pod", podAfterStart.Name); uid != podUIDBefore {
		t.Fatalf("Pod %s UID after fresh Start = %q; want the adopted UID %q", podAfterStart.Name, uid, podUIDBefore)
	}
}

// waitForPodRunning blocks until exactly one workload pod is Running and live.
// The runtime's Observe maps a Pending pod to Running, so only the pod phase
// proves the workload actually started.
func waitForPodRunning(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	client *driverkube.Cluster,
	identity engine.EffectIdentity,
) runtimekube.Pod {
	t.Helper()
	var last runtimekube.Pod
	probe := func(ctx context.Context) error {
		pods, err := client.ListPods(ctx, fx.Namespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(pods) != 1 {
			return fmt.Errorf("%d workload pods observed; want exactly 1", len(pods))
		}
		last = pods[0]
		if last.DeletionTimestamp != nil {
			return fmt.Errorf("workload pod %s carries a deletion timestamp", last.Name)
		}
		if last.Phase != "Running" {
			return fmt.Errorf("pod phase = %q; want Running", last.Phase)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = podRunningTimeout
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "running workload pod", probe, options); err != nil {
		t.Fatalf("wait for running workload pod: %v; %s", err, podStatusDiagnostic(t, ctx, fx, client, identity, last))
	}
	return last
}

// objectUID returns metadata.uid for one object, read through the same cluster.
// UID is immutable, so equal UIDs before and after the adapter swap prove the
// fresh adapter observed the same durable object instead of a replacement.
func objectUID(t *testing.T, ctx context.Context, fx *fixture, kind, name string) string {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", kind, name, "-n", fx.Namespace)
	if err != nil {
		t.Fatalf("read %s %s UID: %v", kind, name, err)
	}
	metadata, _ := document["metadata"].(map[string]any)
	uid := documentString(metadata, "uid")
	if uid == "" {
		t.Fatalf("%s %s has no metadata.uid", kind, name)
	}
	return uid
}

// podStatusDiagnostic renders the phase and each container state of the pod
// that never reached Running. It reads status fields only, never environment
// values or secret material, so a failure names the waiting reason safely.
func podStatusDiagnostic(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	client *driverkube.Cluster,
	identity engine.EffectIdentity,
	last runtimekube.Pod,
) string {
	t.Helper()
	name := last.Name
	if name == "" {
		pods, err := client.ListPods(ctx, fx.Namespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return fmt.Sprintf("pod listing failed: %v", err)
		}
		if len(pods) != 1 {
			return fmt.Sprintf("%d workload pods observed; no single pod status to report", len(pods))
		}
		name = pods[0].Name
	}
	document, err := fx.Cluster.GetJSON(ctx, "get", "pod", name, "-n", fx.Namespace)
	if err != nil {
		return fmt.Sprintf("pod %s status unavailable: %v", name, err)
	}
	status, _ := document["status"].(map[string]any)
	spec, _ := document["spec"].(map[string]any)
	fields := []string{fmt.Sprintf("pod %s phase=%q", name, documentString(status, "phase"))}
	if reason := documentString(status, "reason"); reason != "" {
		fields = append(fields, "reason="+reason)
	}
	if message := documentString(status, "message"); message != "" {
		fields = append(fields, "message="+message)
	}
	if node := documentString(spec, "nodeName"); node != "" {
		fields = append(fields, "node="+node)
	}
	if containers, ok := status["containerStatuses"].([]any); ok {
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fields = append(fields, fmt.Sprintf(
				"container %s state=%s lastState=%s restarts=%v",
				documentString(container, "name"),
				containerState(container["state"]),
				containerState(container["lastState"]),
				container["restartCount"],
			))
		}
	}
	return strings.Join(fields, " ")
}

// containerState renders one Kubernetes container state map.
func containerState(raw any) string {
	state, ok := raw.(map[string]any)
	if !ok {
		return "unknown"
	}
	if waiting, ok := state["waiting"].(map[string]any); ok {
		return fmt.Sprintf("waiting(reason=%q message=%q)",
			documentString(waiting, "reason"), documentString(waiting, "message"))
	}
	if _, ok := state["running"].(map[string]any); ok {
		return "running"
	}
	if terminated, ok := state["terminated"].(map[string]any); ok {
		return fmt.Sprintf("terminated(reason=%q exitCode=%v)",
			documentString(terminated, "reason"), terminated["exitCode"])
	}
	return "unknown"
}

func documentString(document map[string]any, key string) string {
	value, _ := document[key].(string)
	return value
}
