//go:build integration

package lifecycle

// Real-cluster proof: cancelling an execution whose first pod is still Pending
// must converge.
//
// A Stop that deleted the Job would let the background cascade delete the
// unscheduled pod at once, and Stop would then see zero pods with no
// terminated-container evidence: the cleanup would stay pending forever. The
// guarded suspension design keeps the same Job, so the controller's Suspended
// acknowledgment and a present zero terminating count confirm the stop after
// the Pending pod is gone, and the composition removes every owned object and
// releases the PVC.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestKubernetesPendingCancellationCompletesCleanup requires an observed Pending
// Job/pod before execution.cancel, then requires explicit cleanup.state=complete,
// an immutable cancelled outcome and the eventual absence of the Job, Pod,
// Secret, ConfigMap and PVC.
func TestKubernetesPendingCancellationCompletesCleanup(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	ctx := t.Context()

	// Cordon the only node so the workload pod is deterministically Pending and
	// never starts a container. The fixture condition is restored in cleanup.
	node := pendingCancelNodeName(t, harness)
	if _, err := harness.Cluster.Kubectl(ctx, "cordon", node); err != nil {
		t.Fatalf("cordon node %s: %v", node, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := harness.Cluster.Kubectl(cleanupCtx, "uncordon", node); err != nil && cleanupCtx.Err() == nil {
			t.Logf("uncordon node %s during cleanup: %v", node, err)
		}
	})

	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "resist",
			Image:    harness.Image.Reference,
			Timeout:  uncertainStopActionTimeout,
			Commands: []string{"trap '' TERM", "echo pending-cancel-marker", "sleep 300"},
		}},
	})
	applied := applyPipeline(t, harness, "kubernetes-pending-cancel", document)
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "pending cancellation cleanup",
		PipelineID: applied.PipelineID,
	}, "pending-cancel-create")
	executionID := created.ExecutionID
	if executionID == "" {
		t.Fatalf("task.create_and_start returned an empty execution id: %+v", created)
	}

	// Require the Pending Job/pod observation before cancelling: a cancel that
	// raced ahead of the pod would not exercise the Pending-pod sequence.
	var workspaceID string
	var lastObservation string
	waitForKubernetes(t, harness, "pending workload Job and pod", 5*time.Minute, func(ctx context.Context) (bool, error) {
		jobs, err := harness.KubernetesClient.ListJobs(ctx, harness.KubernetesNamespace, map[string]string{
			runtimekube.LabelExecution: executionID,
		})
		if err != nil {
			lastObservation = err.Error()
			return false, nil
		}
		if len(jobs) != 1 {
			lastObservation = fmt.Sprintf("%d Jobs observed; want exactly 1", len(jobs))
			return false, nil
		}
		job := jobs[0]
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			lastObservation = "Job is already suspended before cancel"
			return false, nil
		}
		pods, err := harness.KubernetesClient.ListPods(ctx, harness.KubernetesNamespace, map[string]string{
			runtimekube.LabelExecution: executionID,
		})
		if err != nil {
			lastObservation = err.Error()
			return false, nil
		}
		if len(pods) != 1 {
			lastObservation = fmt.Sprintf("%d Pods observed; want exactly 1", len(pods))
			return false, nil
		}
		pod := pods[0]
		if pod.Phase != "Pending" || pod.NodeName != "" {
			lastObservation = fmt.Sprintf("pod phase=%q node=%q; want an unscheduled Pending pod", pod.Phase, pod.NodeName)
			return false, nil
		}
		workspaceID = pod.Labels[runtimekube.LabelWorkspace]
		if workspaceID == "" {
			lastObservation = "Pending pod carries no workspace label"
			return false, nil
		}
		lastObservation = fmt.Sprintf("Job %s UID=%s and Pending Pod %s", job.Name, job.UID, pod.Name)
		return true, nil
	}, func() string { return "last observation: " + lastObservation })

	claimName, err := runtimekube.WorkspaceClaimName(id.WorkspaceID(workspaceID))
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}

	cancelExecutionCurrent(t, harness, harness.ProjectToken, executionID, "pending-cancel")

	// Require explicit cleanup.state=complete, not a "none" claim. The
	// suspension stop confirms only after the Pending pod object is gone, so
	// the workspace release that follows is not racing a terminating writer;
	// the retry schedule is still paced in real time because Kubernetes
	// finishes the PVC deletion finalizer asynchronously.
	finished := waitUncertainStopExecution(t, harness, executionID)
	if finished.State != "cancelled" {
		t.Fatalf("terminal state = %q; want cancelled", finished.State)
	}
	if finished.Failure != nil {
		t.Fatalf("cancelled execution carries a failure: %+v", finished.Failure)
	}
	if finished.Cleanup.State != "complete" {
		t.Fatalf("cleanup state = %q; want explicit complete", finished.Cleanup.State)
	}

	// The cancelled outcome is immutable and the cleanup record stays complete.
	immutable := inspectExecution(t, harness, harness.ProjectToken, executionID)
	if immutable.State != "cancelled" || immutable.Failure != nil {
		t.Fatalf("execution outcome changed after cleanup: state=%q failure=%+v", immutable.State, immutable.Failure)
	}
	if immutable.Cleanup.State != "complete" {
		t.Fatalf("cleanup state regressed to %q after completion", immutable.Cleanup.State)
	}

	// Every owned object and the retained workspace claim are gone.
	waitJobsGone(t, harness, executionID)
	waitExecutionEphemeraGone(t, harness, executionID)
	waitClaimGone(t, harness, claimName)
}

// pendingCancelNodeName returns the single integration kind node's name.
func pendingCancelNodeName(t *testing.T, harness *factory.Harness) string {
	t.Helper()
	document := kubectlJSON(t, harness, "get", "nodes")
	items, ok := document["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("node count = %d; want exactly one kind node", len(items))
	}
	node, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("node document is %T; want an object", items[0])
	}
	metadata, _ := node["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	if name == "" {
		t.Fatal("integration node has no name")
	}
	return name
}
