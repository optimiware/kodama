package domaincontract

// The pure cancellation-commit rule.
//
// The task store unit must never write `executions.state=cancelled` directly.
// It reads the task's current execution aggregate through
// `task.ExecutionAggregateReader` and applies this rule before writing:
//
//	task.ValidateCancellationCommit(
//	    expectedExecutionVersion uint64,
//	    taskCurrentExecution     id.ExecutionID,
//	    pre                      kernel.Execution,
//	    commit                   *flow.Commit,
//	) error
//
// The rule is deliberately small and total:
//
//   - a NON-TERMINAL predecessor requires a commit whose execution id and
//     expected execution version match the aggregate, the unit's declared
//     expected version and the task's current_execution pointer;
//   - a TERMINAL predecessor carries NO commit: its row is preserved, not
//     rewritten;
//   - ExpectedExecutionVersion == 0 means "no predecessor" and requires no
//     commit; a live aggregate with a zero expected version is refused;
//   - an execution-identity disagreement (task pointer or commit id) is
//     invalid_input; any expected-version disagreement is version_conflict
//     (the same stale-version class the task service uses for
//     lifecycle/question versions), including a zero expected version against
//     a live aggregate.
//
// The reference rule is pure: it reads no clock and performs no I/O.

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/task"
)

type validatorCase struct {
	name            string
	expectedVersion uint64
	taskCurrent     id.ExecutionID
	pre             kernel.Execution
	commit          *flow.Commit
	wantClass       fault.Class // empty means accepted
}

func validatorCases(t *testing.T) []validatorCase {
	t.Helper()
	running := runningExecution()
	queued := queuedExecution()
	terminal := terminalCancelledExecution()
	runningCommit := cancelCommitFor(t, running)
	queuedCommit := cancelCommitFor(t, queued)

	clone := func(commit *flow.Commit) *flow.Commit {
		copied := *commit
		return &copied
	}
	otherExecution := clone(runningCommit)
	otherExecution.ExecutionID = cancelOtherExec
	staleCommitVersion := clone(runningCommit)
	staleCommitVersion.ExpectedExecutionVersion = running.Version - 1
	terminalCommit := clone(runningCommit)
	terminalCommit.ExpectedExecutionVersion = terminal.Version

	return []validatorCase{
		{
			name:            "running predecessor with the matching cancel commit",
			expectedVersion: running.Version,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          runningCommit,
		},
		{
			name:            "queued predecessor with the matching cancel commit",
			expectedVersion: queued.Version,
			taskCurrent:     queued.ID,
			pre:             queued,
			commit:          queuedCommit,
		},
		{
			name:            "non-terminal predecessor without a commit is refused",
			expectedVersion: running.Version,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          nil,
			wantClass:       fault.InvalidInput,
		},
		{
			name:            "terminal predecessor carries no commit",
			expectedVersion: terminal.Version,
			taskCurrent:     terminal.ID,
			pre:             terminal,
			commit:          nil,
		},
		{
			name:            "terminal predecessor with a commit is refused",
			expectedVersion: terminal.Version,
			taskCurrent:     terminal.ID,
			pre:             terminal,
			commit:          terminalCommit,
			wantClass:       fault.InvalidInput,
		},
		{
			name:            "no predecessor requires no commit",
			expectedVersion: 0,
			taskCurrent:     "",
			pre:             kernel.Execution{},
			commit:          nil,
		},
		{
			name:            "no predecessor with a commit is refused",
			expectedVersion: 0,
			taskCurrent:     "",
			pre:             kernel.Execution{},
			commit:          runningCommit,
			wantClass:       fault.InvalidInput,
		},
		{
			name:            "zero expected version with a live predecessor is refused",
			expectedVersion: 0,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          runningCommit,
			wantClass:       fault.VersionConflict,
		},
		{
			name:            "task current-execution pointer mismatch is refused",
			expectedVersion: running.Version,
			taskCurrent:     cancelOtherExec,
			pre:             running,
			commit:          runningCommit,
			wantClass:       fault.InvalidInput,
		},
		{
			name:            "commit execution identity mismatch is refused",
			expectedVersion: running.Version,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          otherExecution,
			wantClass:       fault.InvalidInput,
		},
		{
			name:            "stale unit expected version is refused",
			expectedVersion: running.Version - 1,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          runningCommit,
			wantClass:       fault.VersionConflict,
		},
		{
			name:            "commit expected version mismatch is refused",
			expectedVersion: running.Version,
			taskCurrent:     running.ID,
			pre:             running,
			commit:          staleCommitVersion,
			wantClass:       fault.VersionConflict,
		},
		{
			name:            "terminal predecessor with a stale unit version is refused",
			expectedVersion: terminal.Version - 1,
			taskCurrent:     terminal.ID,
			pre:             terminal,
			commit:          nil,
			wantClass:       fault.VersionConflict,
		},
		{
			name:            "terminal predecessor pointer mismatch is refused",
			expectedVersion: terminal.Version,
			taskCurrent:     cancelOtherExec,
			pre:             terminal,
			commit:          nil,
			wantClass:       fault.InvalidInput,
		},
	}
}

// TestValidateCancellationCommit pins the accept/refuse boundary of the pure
// reference rule, including the exact error classes: identity disagreement is
// invalid_input and version disagreement is version_conflict. A refusal must
// never be reported as an accepted no-op, and an accepted unit must not be
// reported as a failure.
func TestValidateCancellationCommit(t *testing.T) {
	for _, testCase := range validatorCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			err := task.ValidateCancellationCommit(
				testCase.expectedVersion,
				testCase.taskCurrent,
				testCase.pre,
				testCase.commit,
			)
			if testCase.wantClass == "" {
				if err != nil {
					t.Fatalf("ValidateCancellationCommit refused a consistent unit: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCancellationCommit accepted an inconsistent unit; want %s", testCase.wantClass)
			}
			class, ok := fault.ClassOf(err)
			if !ok {
				t.Fatalf("refusal %v carries no stable fault class; want %s", err, testCase.wantClass)
			}
			if class != testCase.wantClass {
				t.Fatalf("refusal class = %s (%v); want %s", class, err, testCase.wantClass)
			}
		})
	}
}
