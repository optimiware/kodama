//go:build integration

package lifecycle

// Lifecycle S11 rows (kind).
//
// These are the real-cluster counterparts of the Podman S11 rows. They pin the
// durable outcomes a lost client response and a lost retained workspace must
// converge to over the composed factory against a rootless kind cluster:
//
//   - a lost retained workspace fails the execution visibly with the typed
//     workspace_lost failure, keeps the live workload attributable through its
//     confirmed stop, cancels the pending successor and never recreates the
//     claim or replays the action (AC6);
//   - a pipeline.start whose response is lost before a controller restart
//     replays from the durable receipt to the original execution after the
//     restart and the workload runs exactly once;
//   - an execution.cancel whose response is lost before a controller restart
//     leaves the durable cancelled outcome, and the replay after the restart
//     converges without stopping or running a second workload.
//
// Every lost-response request goes through the public Go client. The harness
// accepts the committed effect and then loses the response, exactly like the
// existing lost-response admission row; the post-restart retry uses the same
// idempotency key and the same expected version.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/factory"
)

const kindActionTimeout = "1h"

// kindInvokeStart issues exactly one pipeline.start through the public client.
// A transport failure is returned to the caller so the lost-response path can
// observe it instead of treating it as a refusal.
func kindInvokeStart(t *testing.T, transport *client.HTTPTransport, pipelineID string, expected uint64, key string) (api.PipelineStartResult, error) {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	})
	if err != nil {
		t.Fatalf("marshal start input: %v", err)
	}
	result, err := transport.Invoke(t.Context(), api.Invocation{
		Operation: "pipeline.start",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: &expected,
			IdempotencyKey:  key,
		},
	})
	if err != nil {
		return api.PipelineStartResult{}, err
	}
	var started api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil {
		t.Fatalf("decode start result: %v; body=%s", err, string(result.Body))
	}
	if err := started.Validate(); err != nil {
		t.Fatalf("start result: %v", err)
	}
	return started, nil
}

// kindInvokeCancel issues exactly one execution.cancel through the public
// client at an observed execution version.
func kindInvokeCancel(t *testing.T, transport *client.HTTPTransport, executionID string, expected uint64, key string) (api.ExecutionCancelResult, error) {
	t.Helper()
	input, err := json.Marshal(api.ExecutionCancelRequest{ExecutionID: executionID})
	if err != nil {
		t.Fatalf("marshal cancel input: %v", err)
	}
	result, err := transport.Invoke(t.Context(), api.Invocation{
		Operation: "execution.cancel",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: &expected,
			IdempotencyKey:  key,
		},
	})
	if err != nil {
		return api.ExecutionCancelResult{}, err
	}
	var cancelled api.ExecutionCancelResult
	if err := json.Unmarshal(result.Body, &cancelled); err != nil {
		t.Fatalf("decode cancel result: %v; body=%s", err, string(result.Body))
	}
	if err := cancelled.Validate(); err != nil {
		t.Fatalf("cancel result: %v", err)
	}
	return cancelled, nil
}

// kindDroppingClient returns a public client that commits the next request and
// then reports a transport failure instead of returning the response.
func kindDroppingClient(harness *factory.Harness, token string) *client.HTTPTransport {
	transport := harness.Client(token)
	transport.RoundTripper = &dropOnceRoundTripper{base: transport.RoundTripper}
	transport.Retry = client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	return transport
}

// kindSingleExecutionID returns the one committed execution of a pipeline,
// proving a lost start did not create a second execution.
func kindSingleExecutionID(t *testing.T, harness *factory.Harness, pipelineID string) string {
	t.Helper()
	var executionID string
	waitForKubernetes(t, harness, "single committed execution for "+pipelineID, time.Minute, func(context.Context) (bool, error) {
		result, publicError := harness.REST(harness.ProjectToken, "execution.list", api.ExecutionListRequest{
			ProjectID:  factory.ProjectID,
			PipelineID: pipelineID,
		}, nil, "")
		if publicError.Code != "" {
			return false, fmt.Errorf("execution.list refused: %s", publicError.Code)
		}
		var listed api.ExecutionListResult
		harness.Decode(result, &listed)
		if len(listed.Executions) != 1 {
			return false, fmt.Errorf("%d executions listed; want exactly 1", len(listed.Executions))
		}
		executionID = listed.Executions[0].ExecutionID
		return true, nil
	}, nil)
	return executionID
}

// kindWaitPodFile waits until a file exists inside the running workload
// container. The file is the workload's own readiness evidence, not a sleep.
func kindWaitPodFile(t *testing.T, harness *factory.Harness, podName, path string) {
	t.Helper()
	waitForKubernetes(t, harness, "workload file "+path, kubeWorkloadTimeout, func(ctx context.Context) (bool, error) {
		if _, err := harness.Cluster.Kubectl(ctx, "exec", podName,
			"-n", factory.KubernetesNamespace, "-c", "workload", "--", "test", "-f", path); err != nil {
			return false, nil
		}
		return true, nil
	}, func() string { return "kubectl exec test -f " + path + " never succeeded" })
}

// kindTouchPodFile creates a release file inside the running workload
// container, so a held action resumes from observed evidence.
func kindTouchPodFile(t *testing.T, harness *factory.Harness, podName, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := harness.Cluster.Kubectl(ctx, "exec", podName,
		"-n", factory.KubernetesNamespace, "-c", "workload", "--", "touch", path); err != nil {
		t.Fatalf("touch %s in pod %s: %v", path, podName, err)
	}
}

// kindWaitPodLog waits until the workload's own stdout contains want. A visible
// line is the evidence that replaces a fixed sleep before the next step.
func kindWaitPodLog(t *testing.T, harness *factory.Harness, podName, want string) {
	t.Helper()
	var last string
	waitForKubernetes(t, harness, "workload log line "+strings.TrimSpace(want), kubeWorkloadTimeout, func(ctx context.Context) (bool, error) {
		result, err := harness.Cluster.Kubectl(ctx, "logs", podName,
			"-n", factory.KubernetesNamespace, "-c", "workload")
		if err != nil {
			return false, nil
		}
		last = string(result.Stdout)
		return strings.Contains(last, want), nil
	}, func() string { return "last workload log:\n" + last })
}

// kindCancelWithLostResponse cancels one running execution, losing the response
// of the request that commits. A stale-version conflict is retried with a
// fresh read exactly like the shared cancel helper, so the committed request's
// response is always the dropped one. It returns the execution version the
// committed request used.
func kindCancelWithLostResponse(t *testing.T, harness *factory.Harness, executionID, key string) uint64 {
	t.Helper()
	version := uint64(0)
	for attempt := 1; attempt <= 20; attempt++ {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		if current.State == "cancelled" {
			if version == 0 {
				t.Fatalf("execution %s was cancelled before a lost-response request committed", executionID)
			}
			return version
		}
		if current.State != "running" {
			t.Fatalf("execution state before cancel attempt %d = %q; want running", attempt, current.State)
		}
		version = current.Version
		if _, err := kindInvokeCancel(t, kindDroppingClient(harness, harness.ProjectToken), executionID, version, key); err == nil {
			t.Fatalf("cancel attempt %d returned a response after the transport was configured to lose it", attempt)
		}
		receipts, err := harness.ReceiptCount(t.Context(), key)
		if err != nil {
			t.Fatalf("count cancel receipts: %v", err)
		}
		if receipts == 0 {
			// The request was refused (for example a version conflict) before
			// any effect; retry from a fresh observed version with the same key.
			continue
		}
		if receipts != 1 {
			t.Fatalf("cancel receipt count = %d; want exactly 1", receipts)
		}
		if state := inspectExecution(t, harness, harness.ProjectToken, executionID).State; state != "cancelled" {
			t.Fatalf("cancel receipt committed but execution state = %q; want cancelled", state)
		}
		return version
	}
	t.Fatalf("execution %s never committed a cancellation across 20 lost-response attempts", executionID)
	return 0
}

// TestKubernetesLostWorkspaceFailsWithoutReplay (AC6) removes the retained
// workspace claim while the workload owns it. The execution must fail visibly
// with the typed workspace_lost failure, confirm the stop of the one live
// workload, cancel the pending successor and complete cleanup without ever
// recreating the claim or running the action a second time.
func TestKubernetesLostWorkspaceFailsWithoutReplay(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    "build",
				Image:   harness.Image.Reference,
				Timeout: kindActionTimeout,
				Commands: []string{
					`mkdir -p /workspace`,
					`printf 'execution\n' >> /workspace/runs`,
					`printf 'runs=%s\n' "$(wc -l < /workspace/runs)"`,
					`touch /workspace/lost-workspace-ready`,
					`printf 'lost-workspace-ready\n'`,
					`while [ ! -e /workspace/lost-workspace-release ]; do sleep 1; done`,
					`printf 'lost-workspace-complete\n'`,
				},
			},
			{
				Name:    "next",
				Image:   harness.Image.Reference,
				Timeout: kindActionTimeout,
				Commands: []string{
					`printf 'next-ran\n'`,
				},
			},
		},
	})
	applied := applyPipeline(t, harness, "lost-workspace", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "lost-workspace-start")

	identity := readRunningWorkloadIdentity(t, harness, started.ExecutionID)
	kindWaitPodFile(t, harness, identity.podName, "/workspace/lost-workspace-ready")
	// The workload's own stdout proves the action ran exactly once before the
	// loss, so the later retained-log assertion has a positive control.
	kindWaitPodLog(t, harness, identity.podName, "runs=1\n")

	workspaceID := waitWorkspaceID(t, harness, started.ExecutionID)
	claimName, err := runtimekube.WorkspaceClaimName(id.WorkspaceID(workspaceID))
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}
	running := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	nextAction := actionByName(t, running, "next")

	// Lose the retained workspace: the claim is deleted externally and starts
	// terminating while the workload still references it. Nothing recreates it.
	if err := harness.KubernetesClient.DeleteClaim(t.Context(), factory.KubernetesNamespace, claimName); err != nil {
		t.Fatalf("delete retained workspace claim %s: %v", claimName, err)
	}

	// The failure is observable before cleanup finishes, so the live Job set is
	// still attributable at that point.
	var failed api.ExecutionInspectResult
	waitForKubernetes(t, harness, "visible workspace_lost failure", time.Minute, func(context.Context) (bool, error) {
		current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		failed = current
		return current.State == "failed", nil
	}, func() string {
		return fmt.Sprintf("last inspect: state=%q failure=%+v", failed.State, failed.Failure)
	})
	if failed.Failure == nil || failed.Failure.Kind != "workspace_lost" {
		t.Fatalf("execution failure = %+v; want kind workspace_lost", failed.Failure)
	}
	if failed.Failure.Reason != "workspace_missing" {
		t.Fatalf("execution failure reason = %q; want workspace_missing", failed.Failure.Reason)
	}
	if !strings.Contains(failed.Failure.Detail, workspaceID) {
		t.Fatalf("execution failure detail = %q; want the lost workspace identity %q",
			failed.Failure.Detail, workspaceID)
	}

	// No replacement workload exists: at most the one Job captured while the
	// action was running, and no Job for the pending successor.
	jobs, err := harness.KubernetesClient.ListJobs(t.Context(), factory.KubernetesNamespace,
		map[string]string{runtimekube.LabelExecution: started.ExecutionID})
	if err != nil {
		t.Fatalf("list execution Jobs after workspace loss: %v", err)
	}
	if len(jobs) > 1 {
		t.Fatalf("workspace loss left %d Jobs for the execution; want at most the original one", len(jobs))
	}
	for _, job := range jobs {
		if job.UID != identity.jobUID {
			t.Fatalf("workspace loss created a replacement Job %s UID=%s; want only %s UID=%s",
				job.Name, job.UID, identity.jobName, identity.jobUID)
		}
	}
	replays, err := harness.KubernetesClient.ListJobs(t.Context(), factory.KubernetesNamespace,
		map[string]string{
			runtimekube.LabelExecution: started.ExecutionID,
			runtimekube.LabelAction:    nextAction.ActionID,
		})
	if err != nil {
		t.Fatalf("list successor Jobs after workspace loss: %v", err)
	}
	if len(replays) != 0 {
		t.Fatalf("workspace loss dispatched %d Job(s) for the cancelled successor", len(replays))
	}

	finished := waitKubernetesExecution(t, harness, started.ExecutionID)
	if finished.State != "failed" || finished.Failure == nil || finished.Failure.Kind != "workspace_lost" {
		t.Fatalf("terminal outcome = state %q failure %+v; want failed/workspace_lost", finished.State, finished.Failure)
	}
	if finished.Cleanup.State != "complete" {
		t.Fatalf("cleanup after workspace loss = %+v; want complete", finished.Cleanup)
	}
	build := actionByName(t, finished, "build")
	if build.State != "failed" {
		t.Fatalf("build action state = %q; want failed", build.State)
	}
	if build.Stop == nil || build.Stop.State != "confirmed" {
		t.Fatalf("build action stop evidence = %+v; want the confirmed stop of the lost-workspace workload", build.Stop)
	}
	next := actionByName(t, finished, "next")
	if next.State != "cancelled" {
		t.Fatalf("successor action state = %q; want cancelled", next.State)
	}
	if next.ActivatedAt != nil || next.Deadline != nil {
		t.Fatalf("successor action was activated after workspace loss: activated=%v deadline=%v",
			next.ActivatedAt, next.Deadline)
	}

	// The retained log shows exactly one execution of the action; a replay
	// would append a second runs line or the completion marker.
	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "build")
	if got := strings.Count(logs, "runs="); got != 1 {
		t.Fatalf("retained log has %d runs lines; want exactly one:\n%s", got, logs)
	}
	if !strings.Contains(logs, "runs=1") {
		t.Fatalf("retained log does not show exactly one command execution:\n%s", logs)
	}
	if strings.Contains(logs, "runs=2") || strings.Contains(logs, "lost-workspace-complete") {
		t.Fatalf("retained log shows a replayed or completed action after workspace loss:\n%s", logs)
	}

	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count = %d; want exactly the original execution", count)
	}
	requireSubjectEventCount(t, harness, "execution.failed", started.ExecutionID, 1)

	// The claim is never recreated: the terminated claim is gone and stays gone.
	waitJobsGone(t, harness, started.ExecutionID)
	waitClaimGone(t, harness, claimName)
	if _, err := harness.KubernetesClient.GetClaim(t.Context(), factory.KubernetesNamespace, claimName); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("lost workspace claim %s was recreated or is still present: %v", claimName, err)
	}
}

// TestKubernetesLostStartResponseAcrossRestart loses the response of a
// committed pipeline.start, restarts the controller while the workload runs,
// and retries the exact request. The durable receipt must return the original
// execution and the one workload must run the command exactly once.
func TestKubernetesLostStartResponseAcrossRestart(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	applied := applyPipeline(t, harness, "lost-start", marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "survive",
			Image:   harness.Image.Reference,
			Timeout: kindActionTimeout,
			Commands: []string{
				`mkdir -p /workspace`,
				`printf 'execution\n' >> /workspace/runs`,
				`printf 'runs=%s\n' "$(wc -l < /workspace/runs)"`,
				`touch /workspace/lost-start-ready`,
				`printf 'lost-start-ready\n'`,
				`while [ ! -e /workspace/lost-start-release ]; do sleep 1; done`,
				`printf 'lost-start-complete\n'`,
			},
		}},
	}))
	expected := readPipeline(t, harness, applied.PipelineID).Version
	const key = "lost-start-response"

	// The server commits the execution and start receipt, then the response is
	// lost. The client sees only the transport failure.
	if _, err := kindInvokeStart(t, kindDroppingClient(harness, harness.ProjectToken), applied.PipelineID, expected, key); err == nil {
		t.Fatal("start with a dropped response returned a result; the lost-response path was not exercised")
	}
	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after the lost start: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count after the lost start = %d; want the one committed execution", count)
	}
	receipts, err := harness.ReceiptCount(t.Context(), key)
	if err != nil {
		t.Fatalf("count start receipts after the lost start: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("start receipt count after the lost start = %d; want exactly 1", receipts)
	}

	executionID := kindSingleExecutionID(t, harness, applied.PipelineID)
	identity := readRunningWorkloadIdentity(t, harness, executionID)
	kindWaitPodFile(t, harness, identity.podName, "/workspace/lost-start-ready")
	kindWaitPodLog(t, harness, identity.podName, "runs=1\n")

	// The restart must adopt the live workload, not stop or recreate it.
	harness.RestartController()
	assertAdoptedWorkload(t, harness, executionID, identity)

	// Retry the exact lost request after the restart: same key, same expected
	// version and the same stored pipeline selection.
	replayed, err := kindInvokeStart(t, harness.Client(harness.ProjectToken), applied.PipelineID, expected, key)
	if err != nil {
		t.Fatalf("replayed start after restart: %v", err)
	}
	if replayed.ExecutionID != executionID {
		t.Fatalf("replayed start returned execution %q; want the original %q", replayed.ExecutionID, executionID)
	}
	count, err = harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after the replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count after the start replay = %d; want exactly 1", count)
	}
	receipts, err = harness.ReceiptCount(t.Context(), key)
	if err != nil {
		t.Fatalf("count start receipts after the replay: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("start receipt count after the replay = %d; want exactly 1", receipts)
	}

	kindTouchPodFile(t, harness, identity.podName, "/workspace/lost-start-release")
	finished := waitKubernetesExecution(t, harness, executionID)
	requireSucceeded(t, finished)

	logs := executionLogs(t, harness, harness.ProjectToken, executionID, "survive")
	if got := strings.Count(logs, "runs="); got != 1 {
		t.Fatalf("retained log has %d runs lines; want exactly one:\n%s", got, logs)
	}
	if !strings.Contains(logs, "runs=1") || !strings.Contains(logs, "lost-start-complete") {
		t.Fatalf("retained log does not show the single completed workload execution:\n%s", logs)
	}
	if strings.Contains(logs, "runs=2") {
		t.Fatalf("retained log shows a replayed workload after the controller restart:\n%s", logs)
	}
	requireSubjectEventCount(t, harness, "execution.started", executionID, 1)
}

// TestKubernetesLostStopResponseAcrossRestart loses the response of a
// committed execution.cancel, restarts the controller, and retries the exact
// request. The durable cancelled outcome must survive the restart, the retry
// must converge without a second cancellation, and the workload must have run
// exactly once before it was stopped.
func TestKubernetesLostStopResponseAcrossRestart(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	applied := applyPipeline(t, harness, "lost-stop", marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "stop",
			Image:   harness.Image.Reference,
			Timeout: kindActionTimeout,
			Commands: []string{
				`mkdir -p /workspace`,
				`printf 'execution\n' >> /workspace/runs`,
				`printf 'runs=%s\n' "$(wc -l < /workspace/runs)"`,
				`touch /workspace/lost-stop-ready`,
				`printf 'lost-stop-ready\n'`,
				`sleep 3600`,
			},
		}},
	}))
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "lost-stop-start")
	identity := readRunningWorkloadIdentity(t, harness, started.ExecutionID)
	kindWaitPodFile(t, harness, identity.podName, "/workspace/lost-stop-ready")
	kindWaitPodLog(t, harness, identity.podName, "runs=1\n")

	const key = "lost-stop-response"
	expected := kindCancelWithLostResponse(t, harness, started.ExecutionID, key)

	// The cancellation committed before its response was lost: the durable
	// receipt and the terminal state are already visible.
	receipts, err := harness.ReceiptCount(t.Context(), key)
	if err != nil {
		t.Fatalf("count cancel receipts after the lost stop: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("cancel receipt count after the lost stop = %d; want exactly 1", receipts)
	}
	cancelled := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("execution state after the lost stop = %q; want cancelled", cancelled.State)
	}

	// The successor completes the durable stop; the retry converges on the same
	// cancelled outcome without a second stop or a second workload.
	harness.RestartController()
	afterRestart := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterRestart.State != "cancelled" {
		t.Fatalf("execution state after restart = %q; want the durable cancelled outcome", afterRestart.State)
	}
	replayed, err := kindInvokeCancel(t, harness.Client(harness.ProjectToken), started.ExecutionID, expected, key)
	if err != nil {
		t.Fatalf("replayed cancel after restart: %v", err)
	}
	if replayed.ExecutionVersion == 0 {
		t.Fatalf("replayed cancel returned no execution version")
	}
	receipts, err = harness.ReceiptCount(t.Context(), key)
	if err != nil {
		t.Fatalf("count cancel receipts after the replay: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("cancel receipt count after the replay = %d; want exactly 1", receipts)
	}

	finished := waitKubernetesExecution(t, harness, started.ExecutionID)
	if finished.State != "cancelled" {
		t.Fatalf("terminal state = %q; want cancelled", finished.State)
	}
	if finished.Failure != nil {
		t.Fatalf("cancelled execution carries a failure: %+v", finished.Failure)
	}
	if finished.Cleanup.State != "complete" {
		t.Fatalf("cleanup after the lost stop = %+v; want complete", finished.Cleanup)
	}

	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "stop")
	if got := strings.Count(logs, "runs="); got != 1 {
		t.Fatalf("retained log has %d runs lines; want exactly one:\n%s", got, logs)
	}
	if !strings.Contains(logs, "runs=1") || !strings.Contains(logs, "lost-stop-ready") {
		t.Fatalf("retained log does not show the single workload execution that was stopped:\n%s", logs)
	}
	if strings.Contains(logs, "runs=2") {
		t.Fatalf("retained log shows a replayed workload after the lost stop:\n%s", logs)
	}
	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count = %d; want exactly the cancelled execution", count)
	}
	requireSubjectEventCount(t, harness, "execution.cancelled", started.ExecutionID, 1)
	waitJobsGone(t, harness, started.ExecutionID)
}
