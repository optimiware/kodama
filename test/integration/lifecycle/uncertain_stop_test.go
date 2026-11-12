//go:build integration

package lifecycle

// Real Kubernetes regression: an uncertain predecessor stop must
// block the replacement handoff at the activation commit, not only at the
// notification edge.
//
// The pipeline's workload ignores SIGTERM until its own bounded timer expires.
// An ask cancels the running predecessor and requests a Job deletion; while the
// pod is terminating without terminated-container evidence the stop is
// unconfirmed. A reply commits a fresh queued execution. It must remain queued,
// unprepared and without any Job/Pod/Secret until the stop confirms; only then
// may the replacement start, and it starts exactly once.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/factory"
)

// uncertainStopReplyBudget bounds the version_conflict retry of one reply. The
// predecessor's stop is confirmed by its own 25-second timer, so a budget well
// below that keeps the reply inside the unconfirmed window the test proves.
const uncertainStopReplyBudget = 10 * time.Second

// replyTaskCurrentVersion replies to an outstanding question from a fresh task
// read on every attempt.
//
// While a predecessor's stop is unconfirmed the recovery scanner keeps writing
// it: every scan pass observes the terminating pod, records the still-pending
// stop evidence and advances the predecessor's execution version (see
// internal/kernel/engine/cleanup.go confirmStop/commitStopEvidence). The task
// service re-reads that row as an internal authority guard when it composes the
// reply (internal/shell/task/service.go Reply), so a reply whose guard was
// composed just before such a commit is refused with version_conflict. That is
// the documented compare-and-commit race, not a stale client version: a refused
// reply commits nothing, so re-reading the task and retrying the same
// idempotency key is safe. Only version_conflict is retried, inside a bounded
// budget; any other refusal fails at once. The returned trace names the
// versions every attempt used so a real-cluster failure is diagnosable.
func replyTaskCurrentVersion(t *testing.T, harness *factory.Harness, readToken, replyToken, taskID, questionID, answer, key string) (api.TaskReplyResult, string) {
	t.Helper()
	const traceLimit = 25
	deadline := time.Now().Add(uncertainStopReplyBudget)
	var trace []string
	for attempt := 1; ; attempt++ {
		current := readTask(t, harness, readToken, taskID)
		if current.State != "waiting" || current.Question == nil || current.Question.QuestionID != questionID {
			t.Fatalf("%s reply attempt %d: task %s state=%q question=%+v; want waiting on question %q (attempts: %s)",
				key, attempt, taskID, current.State, current.Question, questionID, strings.Join(trace, "; "))
		}
		predecessor := inspectExecution(t, harness, readToken, current.CurrentExecution)
		line := fmt.Sprintf("attempt %d: lifecycle_version=%d question_version=%d predecessor_execution_version=%d",
			attempt, current.LifecycleVersion, current.Question.Version, predecessor.Version)
		result, publicError := harness.REST(replyToken, "task.reply", api.TaskReplyRequest{
			TaskID:                  taskID,
			QuestionID:              questionID,
			ExpectedQuestionVersion: current.Question.Version,
			Answer:                  answer,
		}, &current.LifecycleVersion, key)
		if publicError.Code == "" {
			var replied api.TaskReplyResult
			harness.Decode(result, &replied)
			trace = appendBoundedTrace(trace, line+" -> committed", traceLimit)
			t.Logf("%s reply committed on %s", key, line)
			return replied, strings.Join(trace, "; ")
		}
		trace = appendBoundedTrace(trace, line+" -> "+publicError.Code, traceLimit)
		if publicError.Code != api.CodeVersionConflict {
			t.Fatalf("%s reply attempt %d refused: %s (%s); attempts: %s",
				key, attempt, publicError.Code, publicError.Message, strings.Join(trace, "; "))
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s reply did not converge within %s: last %s (%s); attempts: %s",
				key, uncertainStopReplyBudget, publicError.Code, publicError.Message, strings.Join(trace, "; "))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// appendBoundedTrace keeps the last traceLimit attempt lines, so a long retry
// still reports a bounded, diagnosable tail.
func appendBoundedTrace(trace []string, line string, limit int) []string {
	trace = append(trace, line)
	if len(trace) > limit {
		trace = trace[len(trace)-limit:]
	}
	return trace
}

// uncertainStopActionTimeout keeps the action well beyond the stop barrier and
// the Kubernetes termination grace period.
const uncertainStopActionTimeout = "1h"

const (
	// uncertainStopCleanupTimeout bounds the terminal cleanup barrier. It matches
	// the shared waitExecution cleanup deadline.
	uncertainStopCleanupTimeout = time.Minute
	// uncertainStopRetryBaseDelay and uncertainStopRetryMaxDelay mirror the
	// producer's durable cleanup retry schedule (1s, 2s, 4s, 8s, 16s, capped at
	// 30s). The factory clock is frozen, so a persisted next_retry_at
	// (commit.At + the backoff of the failed attempt) only becomes due once the
	// harness clock moves past it.
	uncertainStopRetryBaseDelay = time.Second
	uncertainStopRetryMaxDelay  = 30 * time.Second
	// uncertainStopPollInterval is the short poll between inspections.
	uncertainStopPollInterval = 10 * time.Millisecond
)

// uncertainStopRetryDelay returns the producer's durable delay after one spent
// cleanup attempt. The retry is accelerated through the frozen factory clock,
// but only after the same delay has elapsed in real time: Kubernetes' own PVC
// deletion finalizer is asynchronous, so collapsing 1s+2s+4s+8s into a few
// milliseconds would spend the whole five-attempt bound while pvc-protection is
// still finishing a perfectly healthy release.
func uncertainStopRetryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	delay := uncertainStopRetryBaseDelay
	for step := 1; step < attempts; step++ {
		if delay >= uncertainStopRetryMaxDelay {
			break
		}
		delay *= 2
	}
	if delay > uncertainStopRetryMaxDelay {
		delay = uncertainStopRetryMaxDelay
	}
	return delay
}

// waitForPredecessorTrap blocks until exactly one execution pod is Running, its
// workload container's state.running is set and the post-trap marker is
// readable from its log. The marker is emitted after `trap ” TERM`, so its
// presence is the readiness evidence that replaces a fixed sleep.
func waitForPredecessorTrap(t *testing.T, harness *factory.Harness, executionID, marker string) string {
	t.Helper()
	var podName string
	var lastErr error
	waitForKubernetes(t, harness, "first workload post-trap marker", 5*time.Minute, func(ctx context.Context) (bool, error) {
		pods, err := harness.KubernetesClient.ListPods(ctx, harness.KubernetesNamespace, map[string]string{
			kube.LabelExecution: executionID,
		})
		if err != nil {
			lastErr = err
			return false, nil
		}
		if len(pods) != 1 {
			lastErr = fmt.Errorf("%d execution pods observed; want exactly 1", len(pods))
			return false, nil
		}
		pod := pods[0]
		if pod.Phase != "Running" {
			lastErr = fmt.Errorf("pod phase = %q; want Running", pod.Phase)
			return false, nil
		}
		if pod.DeletionTimestamp != nil {
			lastErr = errors.New("pod is already terminating")
			return false, nil
		}
		document, err := harness.Cluster.GetJSON(ctx, "get", "pod", pod.Name, "-n", harness.KubernetesNamespace)
		if err != nil {
			lastErr = fmt.Errorf("pod status: %w", err)
			return false, nil
		}
		if !workloadContainerRunning(document) {
			lastErr = errors.New("workload container is not running")
			return false, nil
		}
		result, err := harness.Cluster.Kubectl(ctx, "logs", pod.Name, "-n", harness.KubernetesNamespace, "-c", "workload")
		if err != nil {
			lastErr = fmt.Errorf("read workload log before Stop: %w", err)
			return false, nil
		}
		if !strings.Contains(string(result.Stdout), marker+"\n") {
			lastErr = fmt.Errorf("post-trap marker %q is not readable yet", marker)
			return false, nil
		}
		podName = pod.Name
		return true, nil
	}, func() string {
		return fmt.Sprintf("last error: %v", lastErr)
	})
	return podName
}

// workloadContainerRunning reports whether the workload container's
// state.running is set in one decoded pod document.
func workloadContainerRunning(document map[string]any) bool {
	status, _ := document["status"].(map[string]any)
	containers, _ := status["containerStatuses"].([]any)
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := container["name"].(string); name != "workload" {
			continue
		}
		states, _ := container["state"].(map[string]any)
		_, running := states["running"]
		return running
	}
	return false
}

// uncertainStopPipeline renders the TERM-resistant first execution and the
// marker-only replacement. The reply's answer is part of the admitted task
// snapshot, so the same action distinguishes them by reading /kodama/task.json.
func uncertainStopPipeline(t *testing.T, image, answer, firstMarker, replacementMarker, timerMarker string) []byte {
	t.Helper()
	command := "if grep -q '" + answer + "' /kodama/task.json 2>/dev/null; then\n" +
		"  echo " + replacementMarker + "\n" +
		"  exit 0\n" +
		"fi\n" +
		"trap '' TERM\n" +
		"echo " + firstMarker + "\n" +
		"sleep 25\n" +
		"echo " + timerMarker
	return marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "resist",
			Image:    image,
			Timeout:  uncertainStopActionTimeout,
			Commands: []string{command},
		}},
	})
}

// waitUncertainStopExecution waits for one execution to reach a terminal state
// and for its independent cleanup record to settle, advancing the factory clock
// while a fired cleanup attempt is waiting out its durable retry backoff.
//
// The factory clock is frozen unless Advanced, so a failed cleanup attempt
// persists next_retry_at = commit.At + the backoff and would never become due;
// the helper advances the clock past it only after the same real delay has
// elapsed. A healthy release does not spend attempt 1 on an in-progress claim
// deletion: Workspace.Release accepts an owned, unreferenced terminating claim,
// or a claim still carrying a deletion timestamp right after its own accepted
// versioned delete, as an accepted deletion in progress, so the PVC deletion
// finalizer completing asynchronously is not a failed attempt. The pacing
// therefore applies only to a genuinely failed attempt (for example a claim
// that is still referenced); advancing the durable schedule faster than wall
// clock would burn the whole five-attempt bound before the cluster could finish
// deleting the claim. Advancing unconditionally would also move action
// deadlines and credential lifetimes during preparation. The wait stays bounded
// by uncertainStopCleanupTimeout.
//
// Settlement semantics match the shared waitExecution this helper replaces for
// this test: "complete" settles immediately; "none" is accepted only after a
// short grace and only when the execution has no active container action and
// the live state proves the claim (no run-labelled container and no retained
// workspace volume). The helper keeps its own name and constant names so a
// merge with kubernetes_test.go's barrier cannot clash.
func waitUncertainStopExecution(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	token := harness.ProjectToken
	var last api.ExecutionInspectResult
	var resources executionResources
	defer func() {
		if t.Failed() {
			t.Logf("execution %s last inspect: state=%q version=%d cleanup=%+v failure=%+v actions=%+v",
				last.ExecutionID, last.State, last.Version, last.Cleanup, last.Failure, last.Actions)
			t.Logf("execution %s last live resources: %s", executionID, resources)
			dumpExecutionLogs(t, harness, token, executionID, last)
		}
	}()
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		switch last.State {
		case "succeeded", "failed", "cancelled":
			return true
		default:
			return false
		}
	})

	const noneGrace = 2 * time.Second
	probeAfter := time.Now().Add(noneGrace)
	probedNone := false
	pacedAttempts := 0
	deadline := time.Now().Add(uncertainStopCleanupTimeout)
	for {
		inspected := false
		result, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code == "" {
			harness.Decode(result, &last)
			inspected = true
			switch last.Cleanup.State {
			case "complete":
				return last
			case "none":
				if !probedNone && !time.Now().Before(probeAfter) {
					probedNone = true
					resources = probeExecutionResources(t.Context(), harness, executionID)
					if cleanupNoneCanBeTrue(last) && resources.clean() {
						return last
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			if !probedNone {
				resources = probeExecutionResources(t.Context(), harness, executionID)
			}
			t.Fatalf("execution %s reached state %q but cleanup did not settle within %s: cleanup=%+v; live resources: %s\ncomposition log:\n%s",
				executionID, last.State, uncertainStopCleanupTimeout, last.Cleanup, resources, factory.SanitizeDiagnostic(harness.Logs()))
		}
		if inspected && last.Cleanup.State == "pending" && last.Cleanup.Attempts > pacedAttempts {
			// One failed attempt is paced exactly once. Wait the producer's
			// real backoff before moving the frozen clock past next_retry_at,
			// so the external deletion this attempt waits on has the same
			// wall-clock opportunity it gets in production.
			delay := uncertainStopRetryDelay(last.Cleanup.Attempts)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-t.Context().Done():
				timer.Stop()
			}
			pacedAttempts = last.Cleanup.Attempts
			harness.Clock.Advance(delay)
		}
		time.Sleep(uncertainStopPollInterval)
	}
}

// TestKubernetesUncertainStopBlocksHandoff is the real-cluster proof that a
// replacement stays unactivated while its predecessor's Pod stop is
// unconfirmed and is released exactly once after confirmation.
func TestKubernetesUncertainStopBlocksHandoff(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	ctx := t.Context()
	nonce := factory.ProbeContainerName("uncertain-stop")
	answer := "answer-" + nonce
	firstMarker := "predecessor-" + nonce
	replacementMarker := "replacement-" + nonce
	timerMarker := "timer-" + nonce

	document := uncertainStopPipeline(t, harness.Image.Reference, answer, firstMarker, replacementMarker, timerMarker)
	applied := applyPipeline(t, harness, "kubernetes-uncertain-stop", document)
	created := createAndStartTask(t, harness, harness.ProjectToken, api.TaskCreateAndStartRequest{
		ProjectID:  factory.ProjectID,
		Title:      "uncertain stop handoff",
		PipelineID: applied.PipelineID,
	}, "uncertain-stop-create")
	if created.ExecutionID == "" {
		t.Fatalf("task.create_and_start returned an empty execution id: %+v", created)
	}
	firstExecution := created.ExecutionID

	// Wait until the first workload container is actually running and has
	// printed its marker. The marker is printed after the shell installs its
	// TERM trap, so a readable marker is the post-trap evidence the stop needs;
	// pod phase alone (or a fixed sleep) does not prove that.
	waitForPredecessorTrap(t, harness, firstExecution, firstMarker)
	waiting := askForReply(t, harness, raceTask{taskID: created.TaskID, executionID: firstExecution}, "uncertain-stop-ask")

	// The stop requested by the ask is unconfirmed while the pod is terminating
	// without terminated-container evidence.
	var observedUnconfirmed bool
	waitForKubernetes(t, harness, "unconfirmed predecessor stop", 2*time.Minute, func(context.Context) (bool, error) {
		predecessor := inspectExecution(t, harness, harness.ProjectToken, firstExecution)
		work := actionByName(t, predecessor, "resist")
		if work.Stop != nil && work.Stop.State == "unconfirmed" {
			observedUnconfirmed = true
			return true, nil
		}
		return false, nil
	}, func() string { return "predecessor stop never reported unconfirmed" })
	if !observedUnconfirmed {
		t.Fatal("the TERM-resistant stop barrier was not exercised")
	}

	// The scanner advances the predecessor's execution version while it keeps
	// recording the still-pending stop, so the reply is issued from a fresh
	// read and retries only the version_conflict guard race.
	replied, replyTrace := replyTaskCurrentVersion(t, harness, harness.ProjectToken, harness.ReviewerToken,
		created.TaskID, waiting.Question.QuestionID, answer, "uncertain-stop-reply")
	if replied.ExecutionID == "" || replied.ExecutionID == firstExecution {
		t.Fatalf("reply execution = %q; want a fresh execution distinct from %q (attempts: %s)",
			replied.ExecutionID, firstExecution, replyTrace)
	}

	// The reply must have committed while the stop was still unconfirmed;
	// otherwise this run does not exercise the barrier.
	predecessorAtReply := inspectExecution(t, harness, harness.ProjectToken, firstExecution)
	stopAtReply := actionByName(t, predecessorAtReply, "resist").Stop
	if stopAtReply == nil || stopAtReply.State != "unconfirmed" {
		t.Fatalf("predecessor stop at the committed reply = %+v; want unconfirmed (attempts: %s)",
			stopAtReply, replyTrace)
	}

	// While the predecessor stop is unconfirmed the replacement must stay
	// queued with no activation timestamp, deadline, Job, Pod or credential
	// Secret. The predecessor's own cleanup keeps the scanner busy.
	replacement := inspectExecution(t, harness, harness.ProjectToken, replied.ExecutionID)
	if replacement.State != "queued" {
		t.Fatalf("replacement state while the stop is unconfirmed = %q; want queued (actions=%+v, attempts: %s)",
			replacement.State, replacement.Actions, replyTrace)
	}
	replacementAction := actionByName(t, replacement, "resist")
	if replacementAction.State != "pending" || replacementAction.ActivatedAt != nil || replacementAction.Deadline != nil {
		t.Fatalf("replacement action while the stop is unconfirmed = %+v; "+
			"want pending with no activation timestamp or deadline", replacementAction)
	}
	for _, kind := range []string{"jobs", "pods", "secrets"} {
		items, err := kubectlItems(ctx, harness, kind, replied.ExecutionID)
		if err != nil {
			t.Fatalf("list replacement %s: %v", kind, err)
		}
		if len(items) != 0 {
			t.Fatalf("replacement has %d %s before the predecessor stop confirmed", len(items), kind)
		}
	}

	// The predecessor's bounded timer or the Kubernetes termination grace
	// confirms the stop. Only then may the replacement be dispatched.
	waitUncertainStopExecution(t, harness, firstExecution)
	finished := waitUncertainStopExecution(t, harness, replied.ExecutionID)
	requireSucceeded(t, finished)
	if done := waitTaskState(t, harness, harness.ProjectToken, created.TaskID, "done"); done.CurrentExecution != replied.ExecutionID {
		t.Fatalf("done task current execution = %q; want %q", done.CurrentExecution, replied.ExecutionID)
	}
	requireSubjectEventCount(t, harness, "execution.started", replied.ExecutionID, 1)
	requireSubjectEventCount(t, harness, "execution.cancelled", firstExecution, 1)

	// Each execution ran its own action exactly once: the replacement marker is
	// present only in the replacement's retained stdout and the predecessor
	// marker only in the predecessor's.
	predecessorFinal := inspectExecution(t, harness, harness.ProjectToken, firstExecution)
	predecessorAction := actionByName(t, predecessorFinal, "resist")
	predecessorLogs := readActionLog(t, harness, harness.ProjectToken, firstExecution, predecessorAction.ActionID, "stdout")
	if !strings.Contains(predecessorLogs, firstMarker) {
		t.Fatalf("predecessor retained stdout does not contain its own marker:\n%s", predecessorLogs)
	}
	if strings.Contains(predecessorLogs, replacementMarker) {
		t.Fatalf("predecessor retained stdout contains the replacement marker; the replacement ran early")
	}
	replacementLogs := readActionLog(t, harness, harness.ProjectToken, replied.ExecutionID, replacementAction.ActionID, "stdout")
	if !strings.Contains(replacementLogs, replacementMarker) {
		t.Fatalf("replacement retained stdout does not contain its own marker:\n%s", replacementLogs)
	}
	if strings.Contains(replacementLogs, firstMarker) {
		t.Fatalf("replacement retained stdout contains the predecessor marker; work was replayed")
	}
}
