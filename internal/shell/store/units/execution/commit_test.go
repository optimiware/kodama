package executionstore

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// cleanupNextRetryAt reads the persisted cleanup schedule and attempt count of
// one execution directly from the row, so a test asserts the durable values
// rather than a Load-derived projection.
func cleanupNextRetryAt(t *testing.T, backend *memory.Store, scope tenancy.Scope, executionID id.ExecutionID) (time.Time, int) {
	t.Helper()
	row, err := backend.Read(context.Background(), scope, "cleanup", cleanupID(executionID))
	if err != nil {
		t.Fatalf("read cleanup row: %v", err)
	}
	attempts, err := rowInt(row.Values, "attempts")
	if err != nil {
		t.Fatalf("cleanup attempts: %v", err)
	}
	retryAt, err := rowTime(row.Values, "next_retry_at")
	if err != nil {
		t.Fatalf("cleanup next_retry_at: %v", err)
	}
	return retryAt, attempts
}

// cleanupProgressCommit is the execution-only progress commit the Kernel's
// cleanup retry path issues: it keeps the execution state and rewrites the
// cleanup record.
func cleanupProgressCommit(executionID id.ExecutionID, version uint64, state kernel.ExecutionState, attempts int, lastError string, at time.Time) flow.Commit {
	return flow.Commit{
		ExecutionID:              executionID,
		ExpectedExecutionVersion: version,
		NextExecutionState:       state,
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkloadID:  "wl-1",
			WorkspaceID: "ws-1",
			Attempts:    attempts,
			LastError:   lastError,
		},
		At: at,
	}
}

// executionOnlyCommit is a valid execution-only commit shape a test mutates.
func executionOnlyCommit() flow.Commit {
	return flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		NextCurrentAction:        new(-1),
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		At:                       testNow,
	}
}

// TestEngineCommitRefusesActionScopedFieldsOnExecutionOnlyCommit asserts every
// action-scoped field an execution-only commit could carry is invalid_input and
// persists nothing, rather than being silently dropped.
func TestEngineCommitRefusesActionScopedFieldsOnExecutionOnlyCommit(t *testing.T) {
	cases := map[string]func(*flow.Commit){
		"next action state":       func(commit *flow.Commit) { commit.NextActionState = kernel.ActionRunning },
		"expected action version": func(commit *flow.Commit) { commit.ExpectedActionVersion = 1 },
		"stage output": func(commit *flow.Commit) {
			commit.Output = flow.OutputChange{Action: flow.OutputStage, Values: kernel.Outputs{"a": "b"}}
		},
		"promote output": func(commit *flow.Commit) {
			commit.Output = flow.OutputChange{Action: flow.OutputPromote, Values: kernel.Outputs{"a": "b"}}
		},
		"launch": func(commit *flow.Commit) {
			commit.Launch = &kernel.LaunchIntent{WorkloadID: "wl-1", State: kernel.LaunchNotIssued, IdempotencyKey: "key"}
		},
		"observation": func(commit *flow.Commit) {
			observation := kernel.ExitedObservation(0, kernel.ConfirmedStop("exit"))
			commit.Observation = &observation
		},
		"subject": func(commit *flow.Commit) { subject := definition.NewSubject(nil); commit.Subject = &subject },
		"answer": func(commit *flow.Commit) {
			commit.Answer = &kernel.Answer{Decision: kernel.DecisionPass, Actor: "user-1", At: testNow}
		},
		"stop evidence":             func(commit *flow.Commit) { stop := kernel.ConfirmedStop("stopped"); commit.StopEvidence = &stop },
		"next current action index": func(commit *flow.Commit) { commit.NextCurrentAction = new(0) },
		"next current action range": func(commit *flow.Commit) { commit.NextCurrentAction = new(-2) },
		"unknown execution state":   func(commit *flow.Commit) { commit.NextExecutionState = kernel.ExecutionState("bogus") },
		"unknown output action":     func(commit *flow.Commit) { commit.Output = flow.OutputChange{Action: flow.OutputAction("bogus")} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			engineState, adapter, backend := newEngine(t)
			startedExecution(t, adapter, backend, projectScope(), "exec-1", containerAction())
			commit := executionOnlyCommit()
			mutate(&commit)

			_, err := engineState.Commit(context.Background(), commit)
			requireClass(t, err, fault.InvalidInput)
			aggregate := loadExecution(t, adapter, projectScope(), "exec-1")
			if aggregate.Version != 1 || aggregate.State != kernel.ExecutionQueued || aggregate.CurrentAction != -1 {
				t.Fatalf("refused commit changed state: %+v", aggregate)
			}
			if aggregate.Actions[0].State != kernel.ActionPending {
				t.Fatalf("refused commit changed the action: %+v", aggregate.Actions[0])
			}
		})
	}
}

// TestEngineCommitRefusesDivergentNextCurrentAction asserts an action commit's
// explicit current-action value must equal the index Load derives from the
// single running row, and a valid value commits.
func TestEngineCommitRefusesDivergentNextCurrentAction(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	actions := startedExecution(t, adapter, backend, projectScope(), "exec-1", containerAction())

	for name, next := range map[string]int{"other index": 1, "out of range": 5, "negative": -2} {
		t.Run(name, func(t *testing.T) {
			commit := flow.Commit{
				ExecutionID:              "exec-1",
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 actions[0],
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				NextCurrentAction:        new(next),
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				At:                       testNow,
			}
			_, err := engineState.Commit(context.Background(), commit)
			requireClass(t, err, fault.InvalidInput)
			if aggregate := loadExecution(t, adapter, projectScope(), "exec-1"); aggregate.Version != 1 {
				t.Fatalf("refused commit changed the execution: %+v", aggregate)
			}
		})
	}

	valid := flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionRunning,
		ActionID:                 actions[0],
		ExpectedActionVersion:    1,
		NextActionState:          kernel.ActionRunning,
		NextCurrentAction:        new(0),
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       testNow,
	}
	aggregate, err := engineState.Commit(context.Background(), valid)
	if err != nil {
		t.Fatalf("valid commit: %v", err)
	}
	if aggregate.CurrentAction != 0 || aggregate.Actions[0].State != kernel.ActionRunning {
		t.Fatalf("post-commit aggregate = %+v; want current action 0 running", aggregate)
	}
}

// TestEngineCommitAcceptsAValidExecutionOnlyCommit asserts the positive control
// for the execution-only shape: the cancel commits and Load derives -1.
func TestEngineCommitAcceptsAValidExecutionOnlyCommit(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	startedExecution(t, adapter, backend, projectScope(), "exec-1", containerAction())

	aggregate, err := engineState.Commit(context.Background(), executionOnlyCommit())
	if err != nil {
		t.Fatalf("execution-only commit: %v", err)
	}
	if aggregate.State != kernel.ExecutionCancelled || aggregate.CurrentAction != -1 {
		t.Fatalf("post-commit aggregate = %+v; want cancelled with no current action", aggregate)
	}
}

// TestPlanCleanupSchedulesFailedAttemptsWithCappedBackoff pins the durable
// retry schedule of a failed cleanup attempt: the new next_retry_at is
// commit.At plus a deterministic capped exponential delay derived from the NEW
// attempt count (1 s, 2 s, 4 s, 8 s, 16 s, then 30 s). A short scan interval
// therefore cannot spend the whole attempt budget in one burst, while the
// persisted delay survives a restart because it is a column.
func TestPlanCleanupSchedulesFailedAttemptsWithCappedBackoff(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	ctx := context.Background()

	cases := []struct {
		attempts int
		delay    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 16 * time.Second},
		{6, 30 * time.Second},
		{9, 30 * time.Second},
	}
	version := uint64(1)
	for _, testCase := range cases {
		at := testNow.Add(time.Duration(testCase.attempts) * time.Minute)
		aggregate, err := engineState.Commit(ctx, cleanupProgressCommit("exec-1", version, kernel.ExecutionQueued, testCase.attempts, "dependency_unavailable: log read failed", at))
		if err != nil {
			t.Fatalf("attempt %d commit: %v", testCase.attempts, err)
		}
		version = aggregate.Version
		retryAt, attempts := cleanupNextRetryAt(t, backend, scope, "exec-1")
		if attempts != testCase.attempts {
			t.Fatalf("attempts = %d; want %d", attempts, testCase.attempts)
		}
		if want := at.Add(testCase.delay); !retryAt.Equal(want) {
			t.Fatalf("attempt %d next_retry_at = %s; want %s", testCase.attempts, retryAt, want)
		}
	}
}

// TestPlanCleanupKeepsAPromptRetryWhenAttemptsDoNotIncrease asserts a progress
// update that does not spend an attempt (an unconfirmed-stop pending
// acknowledgment, or the exhaustion diagnostic) remains promptly eligible at
// commit.At, so stop observation still progresses every scan pass.
func TestPlanCleanupKeepsAPromptRetryWhenAttemptsDoNotIncrease(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	ctx := context.Background()

	failedAt := testNow.Add(time.Minute)
	aggregate, err := engineState.Commit(ctx, cleanupProgressCommit("exec-1", 1, kernel.ExecutionQueued, 3, "dependency_unavailable: log read failed", failedAt))
	if err != nil {
		t.Fatalf("failure commit: %v", err)
	}
	if retryAt, _ := cleanupNextRetryAt(t, backend, scope, "exec-1"); !retryAt.Equal(failedAt.Add(4 * time.Second)) {
		t.Fatalf("failed attempt next_retry_at = %s; want the delayed retry", retryAt)
	}

	// The same attempt count is rewritten without spending a new attempt.
	acknowledgedAt := failedAt.Add(time.Hour)
	if _, err := engineState.Commit(ctx, cleanupProgressCommit("exec-1", aggregate.Version, kernel.ExecutionQueued, 3, "dependency_unavailable: log read failed", acknowledgedAt)); err != nil {
		t.Fatalf("acknowledgment commit: %v", err)
	}
	retryAt, attempts := cleanupNextRetryAt(t, backend, scope, "exec-1")
	if attempts != 3 {
		t.Fatalf("attempts = %d; want the unchanged count 3", attempts)
	}
	if !retryAt.Equal(acknowledgedAt) {
		t.Fatalf("unchanged-attempt next_retry_at = %s; want promptly eligible at %s", retryAt, acknowledgedAt)
	}
}

// TestPlanCleanupIntentIsPromptAndUnrelatedCommitsPreserveTheSchedule asserts
// the first cleanup intent is immediately eligible, and a commit that carries
// no cleanup update preserves the persisted retry schedule and attempt count.
func TestPlanCleanupIntentIsPromptAndUnrelatedCommitsPreserveTheSchedule(t *testing.T) {
	engineState, adapter, backend := newEngine(t)
	scope := projectScope()
	startedExecution(t, adapter, backend, scope, "exec-1", containerAction())
	ctx := context.Background()

	intentAt := testNow.Add(time.Minute)
	aggregate, err := engineState.Commit(ctx, flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: 1,
		NextExecutionState:       kernel.ExecutionCancelled,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Cleanup:                  &flow.CleanupIntent{WorkloadID: "wl-1", WorkspaceID: "ws-1", Reason: "cancelled"},
		CleanupUpdate:            &flow.CleanupUpdate{State: kernel.CleanupPending, WorkloadID: "wl-1", WorkspaceID: "ws-1"},
		At:                       intentAt,
	})
	if err != nil {
		t.Fatalf("cleanup intent: %v", err)
	}
	retryAt, attempts := cleanupNextRetryAt(t, backend, scope, "exec-1")
	if !retryAt.Equal(intentAt) || attempts != 0 {
		t.Fatalf("initial intent = (%s, %d); want promptly eligible at %s with 0 attempts", retryAt, attempts, intentAt)
	}

	failedAt := intentAt.Add(time.Minute)
	aggregate, err = engineState.Commit(ctx, cleanupProgressCommit("exec-1", aggregate.Version, kernel.ExecutionCancelled, 2, "workspace release pending", failedAt))
	if err != nil {
		t.Fatalf("failure commit: %v", err)
	}
	scheduledAt := failedAt.Add(2 * time.Second)
	if retryAt, _ := cleanupNextRetryAt(t, backend, scope, "exec-1"); !retryAt.Equal(scheduledAt) {
		t.Fatalf("scheduled retry = %s; want %s", retryAt, scheduledAt)
	}

	// An unrelated execution-only commit carries no cleanup update.
	if _, err := engineState.Commit(ctx, flow.Commit{
		ExecutionID:              "exec-1",
		ExpectedExecutionVersion: aggregate.Version,
		NextExecutionState:       kernel.ExecutionCancelled,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		At:                       failedAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("unrelated commit: %v", err)
	}
	retryAt, attempts = cleanupNextRetryAt(t, backend, scope, "exec-1")
	if attempts != 2 || !retryAt.Equal(scheduledAt) {
		t.Fatalf("unrelated commit changed the schedule: (%s, %d); want (%s, 2)", retryAt, attempts, scheduledAt)
	}
}
