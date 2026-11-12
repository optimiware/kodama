//go:build e2e

package harness

import (
	"fmt"

	"github.com/optimiware/kodama/api"
)

// Status is the interpreted observation of one execution.inspect result.
type Status struct {
	State             string
	Terminal          bool
	Succeeded         bool
	CleanupState      string
	CleanupComplete   bool
	RunningActions    int
	ActiveActionIndex int
}

// Interpret checks one inspection result against the declared state vocabulary
// and the single-active-action invariant.
func Interpret(result api.ExecutionInspectResult) (Status, error) {
	if err := result.Validate(); err != nil {
		return Status{}, fmt.Errorf("inspection result is invalid: %w", err)
	}
	status := Status{
		State:             result.State,
		CleanupState:      result.Cleanup.State,
		CleanupComplete:   result.Cleanup.State == "complete",
		ActiveActionIndex: -1,
	}
	switch result.State {
	case "succeeded":
		status.Terminal = true
		status.Succeeded = true
	case "failed", "cancelled":
		status.Terminal = true
	case "queued", "running":
	default:
		return Status{}, fmt.Errorf("unknown execution state %q", result.State)
	}
	switch result.Cleanup.State {
	case "none", "pending", "complete":
	default:
		return Status{}, fmt.Errorf("unknown cleanup state %q", result.Cleanup.State)
	}
	for index, action := range result.Actions {
		if action.Index != index {
			return Status{}, fmt.Errorf("action %d reports index %d; actions must be ordered by index", index, action.Index)
		}
		if action.State == "running" {
			status.RunningActions++
			status.ActiveActionIndex = index
		}
	}
	if status.RunningActions > 1 {
		return Status{}, fmt.Errorf("execution has %d running actions; at most one may be active", status.RunningActions)
	}
	return status, nil
}

// IsTerminalState reports whether a declared execution state is terminal.
func IsTerminalState(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	case "queued", "running":
		return false
	default:
		return false
	}
}
