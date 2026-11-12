//go:build integration

package lifecycle

// Controller-restart boundary.
//
// Deleting the controller pod must not delete the running workload: App.Close
// does not stop tracked workloads, so a workload Job is not deleted and its pod
// is not garbage-collected when a controller closes. These tests pin that
// boundary over the real factory composition:
//
//   - closing a controller leaves every durable workload running, so the
//     replacement controller observes and adopts the SAME Kubernetes Job and
//     Pod UIDs, workspace and persisted deadline;
//   - the command runs exactly once (a single workspace execution counter), the
//     execution eventually succeeds and its retained logs stay readable;
//   - an explicit cancellation still stops the adopted workload after the
//     restart, so only stop-on-close is excluded and not the stop controls.
//
// RestartController rebuilds the App and every Shell adapter over the same
// PostgreSQL, NATS, keyfile, log sink, workload image, kind cluster/Podman
// service and public edge; it never re-creates infrastructure and never removes
// a workload.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
)

// restartActionTimeout keeps the action deadline clear of the Kubernetes Job
// runtime and of a controller rebuild.
const restartActionTimeout = "1h"

// restartHoldSeconds is long enough for a controller stop, rebuild and public
// edge readiness while the workload is still running. It is not a settle sleep:
// every assertion before and after the restart is a polled barrier.
const restartHoldSeconds = 45

// restartPipeline writes an execution counter into the shared workspace, holds
// the action open across the controller restart, then reports the counter. A
// replay would append a second line and the retained log would show runs=2.
func restartPipeline(t *testing.T, harness *factory.Harness) []byte {
	t.Helper()
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "restart",
			Image:   harness.Image.Reference,
			Timeout: restartActionTimeout,
			Commands: []string{fmt.Sprintf(`mkdir -p /workspace
printf 'execution\n' >> /workspace/runs
printf 'restart-start\n'
sleep %d
printf 'runs=%%s\n' "$(wc -l < /workspace/runs)"
printf 'restart-complete\n'`, restartHoldSeconds)},
		}},
	})
}

// workloadIdentity is the immutable identity of one live Kubernetes workload.
type workloadIdentity struct {
	jobName string
	jobUID  string
	podName string
	podUID  string
}

// readRunningWorkloadIdentity returns the single Job/Pod identity of one
// execution when the workload container is observably running. It is a barrier
// that fails the test if the workload never reaches that state.
func readRunningWorkloadIdentity(t *testing.T, harness *factory.Harness, executionID string) workloadIdentity {
	t.Helper()
	var identity workloadIdentity
	waitForKubernetes(t, harness, "running workload Job and Pod", kubeWorkloadTimeout, func(ctx context.Context) (bool, error) {
		jobs, err := kubectlItems(ctx, harness, "jobs", executionID)
		if err != nil {
			return false, err
		}
		if len(jobs) != 1 {
			return false, fmt.Errorf("saw %d Job(s); want 1", len(jobs))
		}
		pods, err := kubectlItems(ctx, harness, "pods", executionID)
		if err != nil {
			return false, err
		}
		if len(pods) != 1 {
			return false, fmt.Errorf("saw %d Pod(s); want 1", len(pods))
		}
		job, pod := jobs[0], pods[0]
		metadata := nestedMap(t, pod, "metadata")
		if _, deleting := metadata["deletionTimestamp"]; deleting {
			return false, fmt.Errorf("workload pod %s is already deleting", nestedString(t, metadata, "name"))
		}
		phase := nestedString(t, nestedMap(t, pod, "status"), "phase")
		if phase != "Running" {
			return false, fmt.Errorf("pod phase = %q; want Running", phase)
		}
		if !podContainerRunning(t, pod) {
			return false, fmt.Errorf("workload container is not running yet")
		}
		identity = workloadIdentity{
			jobName: nestedString(t, nestedMap(t, job, "metadata"), "name"),
			jobUID:  nestedString(t, nestedMap(t, job, "metadata"), "uid"),
			podName: nestedString(t, metadata, "name"),
			podUID:  nestedString(t, metadata, "uid"),
		}
		return true, nil
	}, func() string { return debugKubernetesExecution(t, harness, executionID) })
	return identity
}

// podContainerRunning reports whether the workload container has a running
// state (not merely a Running pod phase, which a terminating container can also
// report).
func podContainerRunning(t *testing.T, pod map[string]any) bool {
	t.Helper()
	statuses, ok := nestedMap(t, pod, "status")["containerStatuses"].([]any)
	if !ok {
		return false
	}
	for _, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		state, ok := status["state"].(map[string]any)
		if !ok {
			continue
		}
		if _, running := state["running"]; running {
			return true
		}
	}
	return false
}

// assertAdoptedWorkload proves the post-restart controller observes the same
// live Job and Pod rather than deleting and recreating them.
func assertAdoptedWorkload(t *testing.T, harness *factory.Harness, executionID string, before workloadIdentity) {
	t.Helper()
	after := readRunningWorkloadIdentity(t, harness, executionID)
	if after != before {
		t.Fatalf("controller restart replaced the workload identity: before %+v after %+v", before, after)
	}
}

// TestKubernetesControllerRestart is the real-cluster proof: a controller
// restart over a running Kubernetes Job must adopt the same Job/Pod UID,
// workspace and persisted deadline, run the command exactly once, succeed and
// retain its logs.
func TestKubernetesControllerRestart(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	if harness.Cluster == nil || harness.KubernetesClient == nil {
		t.Fatalf("Kubernetes harness did not expose its cluster and client")
	}
	applied := applyPipeline(t, harness, "controller-restart", restartPipeline(t, harness))
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "controller-restart-start")

	// Capture the durable facts while the workload is live.
	identity := readRunningWorkloadIdentity(t, harness, started.ExecutionID)
	workspaceID := waitWorkspaceID(t, harness, started.ExecutionID)
	claimName, err := runtimekube.WorkspaceClaimName(id.WorkspaceID(workspaceID))
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}
	before := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	beforeAction := actionByName(t, before, "restart")
	if beforeAction.State != "running" {
		t.Fatalf("action before restart = %q; want running", beforeAction.State)
	}
	if beforeAction.Deadline == nil {
		t.Fatalf("running action has no persisted deadline")
	}

	// The controller restart keeps infrastructure, durable state and the
	// workload alive. The old controller's close path must not stop anything.
	harness.RestartController()

	// The replacement controller observes the same running identity. This is
	// the assertion that fails when shutdown deletes the Job: the Pod would be
	// gone or deleting and the execution would fail with exit 137.
	assertAdoptedWorkload(t, harness, started.ExecutionID, identity)

	afterRestart := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterRestart.State != "running" {
		t.Fatalf("execution state after controller restart = %q; want running", afterRestart.State)
	}
	afterAction := actionByName(t, afterRestart, "restart")
	if afterAction.State != "running" {
		t.Fatalf("action state after controller restart = %q; want running", afterAction.State)
	}
	if afterAction.Deadline == nil || !afterAction.Deadline.Equal(*beforeAction.Deadline) {
		t.Fatalf("action deadline changed across restart: before %v after %v",
			beforeAction.Deadline, afterAction.Deadline)
	}
	reopened := waitWorkspaceID(t, harness, started.ExecutionID)
	if reopened != workspaceID {
		t.Fatalf("workspace identity changed across restart: before %q after %q", workspaceID, reopened)
	}
	claim := waitClaim(t, harness, claimName)
	assertClaimSpec(t, harness, claim, workspaceID, claimName)

	// The single command finishes, the execution succeeds and cleanup completes.
	execution := waitKubernetesExecution(t, harness, started.ExecutionID)
	requireSucceeded(t, execution)

	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "restart")
	if !strings.Contains(logs, "restart-complete") {
		t.Fatalf("retained logs after restart = %q; want the completion marker", logs)
	}
	if !strings.Contains(logs, "runs=1") {
		t.Fatalf("retained logs after restart = %q; want exactly one command execution (runs=1)", logs)
	}
	if strings.Contains(logs, "runs=2") {
		t.Fatalf("retained logs show a replayed command after restart: %q", logs)
	}
}

// TestKubernetesCancellationStopsAdoptedWorkload proves the shutdown policy
// excludes only stop-on-close: after a restart, an explicit execution
// cancellation still stops the adopted Job and settles the execution cancelled.
func TestKubernetesCancellationStopsAdoptedWorkload(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	if harness.Cluster == nil || harness.KubernetesClient == nil {
		t.Fatalf("Kubernetes harness did not expose its cluster and client")
	}
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "hold",
			Image:   harness.Image.Reference,
			Timeout: restartActionTimeout,
			Commands: []string{`printf 'hold-start\n'
sleep 300`},
		}},
	})
	applied := applyPipeline(t, harness, "restart-cancel", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "restart-cancel-start")
	identity := readRunningWorkloadIdentity(t, harness, started.ExecutionID)

	harness.RestartController()
	assertAdoptedWorkload(t, harness, started.ExecutionID, identity)

	cancelExecutionOnce(t, harness, harness.ProjectToken, started.ExecutionID, "restart-cancel")
	execution := waitKubernetesExecution(t, harness, started.ExecutionID)
	if execution.State != "cancelled" {
		t.Fatalf("execution after cancellation = %q; want cancelled", execution.State)
	}
	waitJobsGone(t, harness, started.ExecutionID)
}

// TestPodmanControllerRestartPreservesRunningWorkload is the Podman half of the
// durable-runtime policy: a container outlives the controller and the restarted
// composition adopts it without re-running the command.
func TestPodmanControllerRestartPreservesRunningWorkload(t *testing.T) {
	harness := factory.Start(t)
	applied := applyPipeline(t, harness, "podman-restart", restartPipeline(t, harness))
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "podman-restart-start")

	ctx := t.Context()
	var container string
	harness.Eventually(t, 2*time.Minute, func() bool {
		containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
		if err != nil || len(containers) != 1 {
			return false
		}
		container = containers[0]
		return true
	})
	harness.RestartController()

	// The same deterministic container must still exist immediately after the
	// restart; deleting it on close would remove the running workload.
	containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
	if err != nil {
		t.Fatalf("list workload containers after restart: %v", err)
	}
	if len(containers) != 1 || containers[0] != container {
		t.Fatalf("workload containers after restart = %v; want [%s]", containers, container)
	}

	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, execution)
	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "restart")
	if !strings.Contains(logs, "restart-complete") {
		t.Fatalf("retained logs after restart = %q; want the completion marker", logs)
	}
	if !strings.Contains(logs, "runs=1") {
		t.Fatalf("retained logs after restart = %q; want exactly one command execution (runs=1)", logs)
	}
}

// TestPodmanCancellationAfterRestartStopsWorkload proves an adopted Podman
// workload is still stoppable through the persisted cancellation control.
func TestPodmanCancellationAfterRestartStopsWorkload(t *testing.T) {
	harness := factory.Start(t)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "hold",
			Image:   harness.Image.Reference,
			Timeout: restartActionTimeout,
			Commands: []string{`printf 'hold-start\n'
sleep 300`},
		}},
	})
	applied := applyPipeline(t, harness, "podman-restart-cancel", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "podman-restart-cancel-start")

	ctx := t.Context()
	var container string
	harness.Eventually(t, 2*time.Minute, func() bool {
		containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
		if err != nil || len(containers) != 1 {
			return false
		}
		container = containers[0]
		return true
	})
	harness.RestartController()

	cancelExecutionOnce(t, harness, harness.ProjectToken, started.ExecutionID, "podman-restart-cancel")
	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if execution.State != "cancelled" {
		t.Fatalf("execution after cancellation = %q; want cancelled", execution.State)
	}
	containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
	if err != nil {
		t.Fatalf("list workload containers after cancellation: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("cancelled adopted workload left containers %v", containers)
	}
	if container == "" {
		t.Fatal("the test never observed the running workload it cancelled")
	}
}

// cancelExecutionOnce cancels one execution with the human project token,
// retrying a stale expected version so a concurrent Kernel observation cannot
// make the cancellation flaky.
func cancelExecutionOnce(t *testing.T, harness *factory.Harness, token, executionID, key string) {
	t.Helper()
	for range 20 {
		execution := inspectExecution(t, harness, token, executionID)
		switch execution.State {
		case "succeeded", "failed", "cancelled":
			return
		}
		_, publicError := harness.REST(token, "execution.cancel",
			api.ExecutionCancelRequest{ExecutionID: executionID}, &execution.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		return
	}
	t.Fatalf("execution.cancel did not converge on a current execution version")
}
