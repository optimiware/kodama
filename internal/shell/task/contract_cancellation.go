package task

// The task cancellation reduction rule.
//
// Both task cancel paths (`AskUnit` and `CancelUnit`) route through the Kernel
// cancel path in ONE atomic receipt-first store unit. The store unit never
// writes `executions.state = cancelled` directly: it reads the task's current
// execution aggregate through `ExecutionAggregateReader`, builds the Kernel
// cancel fact, and applies the `flow.Commit` that `flow.Reduce` returns. This
// file is the pure reference rule a consumer (and the task service) can pin
// without a store.

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// ValidateCancellationCommit refuses a task cancel unit whose declared
// execution identity or version disagrees with the aggregate the store unit
// read, and refuses a live predecessor without a commit. It is pure: it reads
// no clock and performs no I/O.
//
// The rule is total over the four inputs:
//
//   - a present aggregate (`pre.ID != ""`) is the predecessor the unit was
//     built from: the task's current-execution pointer and, when present, the
//     commit's execution id must both equal `pre.ID` (`invalid_input`);
//   - the unit's expected execution version must equal the aggregate version
//     (`version_conflict`), so a zero expected version against a live
//     aggregate is refused because the declared version and the aggregate
//     disagree;
//   - a TERMINAL predecessor carries NO commit: its row is preserved, not
//     rewritten; a commit on a terminal predecessor is `invalid_input`;
//   - a NON-TERMINAL predecessor requires a commit whose expected execution
//     version is the unit's declared expected version (`invalid_input` when
//     missing, `version_conflict` when stale);
//   - no aggregate (`pre.ID == ""`) means "no predecessor" and accepts only a
//     nil commit with a zero expected version; any other combination is
//     `invalid_input`.
func ValidateCancellationCommit(
	expectedExecutionVersion uint64,
	taskCurrentExecution id.ExecutionID,
	pre kernel.Execution,
	commit *flow.Commit,
) error {
	if pre.ID == "" {
		// No predecessor aggregate: nothing to cancel.
		if commit != nil {
			return fault.New(fault.InvalidInput,
				"task cancellation carries a commit but the task has no predecessor execution")
		}
		if expectedExecutionVersion != 0 {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("task cancellation declares execution version %d but no predecessor execution exists",
					expectedExecutionVersion))
		}
		return nil
	}

	// Identity: the pointer the unit was built from must be the aggregate the
	// unit read, and the commit must target that same execution.
	if taskCurrentExecution != pre.ID {
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("task current execution %q does not match the read aggregate %q",
				taskCurrentExecution, pre.ID))
	}
	if commit != nil && commit.ExecutionID != pre.ID {
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("cancellation commit targets execution %q, not the task's current execution %q",
				commit.ExecutionID, pre.ID))
	}

	// Version: the unit's declared expected version is the aggregate version.
	if expectedExecutionVersion != pre.Version {
		return fault.New(fault.VersionConflict,
			fmt.Sprintf("task cancellation expected execution version %d but the aggregate is version %d",
				expectedExecutionVersion, pre.Version))
	}

	if pre.State.Terminal() {
		// A terminal predecessor is preserved; there is nothing left to
		// cancel and the unit must not rewrite its row.
		if commit != nil {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("task cancellation carries a commit for the terminal execution %q", pre.ID))
		}
		return nil
	}

	// A live predecessor must be cancelled through the Kernel reduction.
	if commit == nil {
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("task cancellation of the live execution %q carries no cancel commit", pre.ID))
	}
	if commit.ExpectedExecutionVersion != expectedExecutionVersion {
		return fault.New(fault.VersionConflict,
			fmt.Sprintf("cancellation commit expected version %d does not match the declared version %d",
				commit.ExpectedExecutionVersion, expectedExecutionVersion))
	}
	return nil
}
