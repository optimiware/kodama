package task_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/task"
)

func progressCommand(execution id.ExecutionID, outcome task.ProgressOutcome) task.ProgressCommand {
	return task.ProgressCommand{
		Scope:       projectScope(),
		TaskID:      taskID,
		ExecutionID: execution,
		Outcome:     outcome,
	}
}

// TestProgressCurrentSuccessCompletesOpenTask proves only the current execution
// succeeding while the task is open completes it, and the completion is one
// versioned progress unit.
func TestProgressCurrentSuccessCompletesOpenTask(t *testing.T) {
	env := newTestEnv(t)
	env.reader.put(baseTask())

	result, err := env.service.Progress(t.Context(), progressCommand(executionOne, task.ProgressSucceeded))
	requireNoError(t, err, "Progress current success")

	if result.State != task.StateDone || !result.Applied || result.Replayed {
		t.Fatalf("progress result = %+v; want done/applied/fresh", result)
	}
	unit := env.ports.lastProgress(t)
	if unit.Owner != projectScope() || unit.TaskID != taskID || unit.ExecutionID != executionOne {
		t.Fatalf("progress unit identity = %+v/%q/%q", unit.Owner, unit.TaskID, unit.ExecutionID)
	}
	if unit.ExpectedLifecycleVersion != 4 {
		t.Fatalf("progress expected lifecycle = %d; want 4", unit.ExpectedLifecycleVersion)
	}
	if !unit.At.Equal(testNow) {
		t.Fatalf("progress time = %v; want %v", unit.At, testNow)
	}
	if env.ports.totalCalls() != 1 {
		t.Fatalf("commit port calls = %d; want one progress unit", env.ports.totalCalls())
	}
}

// TestProgressIgnoresOlderDuplicateFailedAndTerminal proves older success,
// duplicates, failures and terminal states never overwrite current state and
// never call the progress port.
func TestProgressIgnoresOlderDuplicateFailedAndTerminal(t *testing.T) {
	older := baseTask()
	older.CurrentExecution = executionTwo
	older.ExecutionHistory = []id.ExecutionID{executionOne, executionTwo}

	cases := map[string]struct {
		task      task.Task
		execution id.ExecutionID
		outcome   task.ProgressOutcome
		wantState task.State
	}{
		"older success": {
			task: older, execution: executionOne, outcome: task.ProgressSucceeded, wantState: task.StateOpen,
		},
		"duplicate success after done": {
			task: doneTask(), execution: executionOne, outcome: task.ProgressSucceeded, wantState: task.StateDone,
		},
		"waiting never overwritten": {
			task: waitingTask(t), execution: executionOne, outcome: task.ProgressSucceeded, wantState: task.StateWaiting,
		},
		"cancelled never overwritten": {
			task: cancelledTask(), execution: executionOne, outcome: task.ProgressSucceeded, wantState: task.StateCancelled,
		},
		"current failure stays open": {
			task: baseTask(), execution: executionOne, outcome: task.ProgressFailed, wantState: task.StateOpen,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			env.reader.put(testCase.task)
			result, err := env.service.Progress(t.Context(), progressCommand(testCase.execution, testCase.outcome))
			requireNoError(t, err, name)
			if result.State != testCase.wantState {
				t.Fatalf("state = %q; want %q", result.State, testCase.wantState)
			}
			if result.Applied {
				t.Fatalf("progress was applied for %s", name)
			}
			requireNoPortCalls(t, env.ports)
		})
	}
}

// TestProgressRetryPreservesFailedHistory proves an operator retry after a
// failure is a fresh current execution; the old execution's success cannot
// complete the new work, while the new current execution can.
func TestProgressRetryPreservesFailedHistory(t *testing.T) {
	env := newTestEnv(t)
	value := baseTask()
	value.ExecutionHistory = []id.ExecutionID{executionOne, executionTwo}
	value.CurrentExecution = executionTwo
	env.reader.put(value)

	old, err := env.service.Progress(t.Context(), progressCommand(executionOne, task.ProgressSucceeded))
	requireNoError(t, err, "old success")
	if old.Applied || old.State != task.StateOpen {
		t.Fatalf("old success result = %+v; want open/not applied", old)
	}
	requireNoPortCalls(t, env.ports)

	current, err := env.service.Progress(t.Context(), progressCommand(executionTwo, task.ProgressSucceeded))
	requireNoError(t, err, "current success")
	if !current.Applied || current.State != task.StateDone {
		t.Fatalf("current success result = %+v; want done/applied", current)
	}
	if len(env.ports.progresses) != 1 || env.ports.lastProgress(t).ExecutionID != executionTwo {
		t.Fatalf("progress units = %+v; want only the current execution", env.ports.progresses)
	}
}

// TestProgressPropagatesPortReplayAndConflict proves a lost-response retry
// returns the original replay and a stale lifecycle version is refused at the
// commit boundary.
func TestProgressPropagatesPortReplayAndConflict(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(baseTask())
		env.ports.setProgress(func(unit task.ProgressUnit) (task.ProgressResult, error) {
			return task.ProgressResult{LifecycleVersion: 5, Replayed: true}, nil
		})
		result, err := env.service.Progress(t.Context(), progressCommand(executionOne, task.ProgressSucceeded))
		requireNoError(t, err, "Progress replay")
		if !result.Applied || !result.Replayed || result.State != task.StateDone {
			t.Fatalf("replay result = %+v; want done/applied/replayed", result)
		}
	})
	t.Run("stale version", func(t *testing.T) {
		env := newTestEnv(t)
		env.reader.put(baseTask())
		env.ports.setProgress(func(unit task.ProgressUnit) (task.ProgressResult, error) {
			return task.ProgressResult{}, fault.New(fault.VersionConflict, "stale task version")
		})
		_, err := env.service.Progress(t.Context(), progressCommand(executionOne, task.ProgressSucceeded))
		requireFaultClass(t, err, fault.VersionConflict)
		if len(env.ports.progresses) != 1 {
			t.Fatalf("progress port calls = %d; want the version check at the port", len(env.ports.progresses))
		}
	})
}

// TestProgressRefusesCrossScope proves another scope's task id is never read or
// settled.
func TestProgressRefusesCrossScope(t *testing.T) {
	env := newTestEnv(t)
	value := baseTask()
	value.Scope = otherScope()
	env.reader.put(value)

	_, err := env.service.Progress(t.Context(), progressCommand(executionOne, task.ProgressSucceeded))
	requireFaultClass(t, err, fault.NotFound)
	requireNoPortCalls(t, env.ports)
}
