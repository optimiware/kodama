//go:build e2e

package smoke

import "github.com/optimiware/kodama/api"

// ExecutionIDs returns every execution identity a cleanup owns: the complete
// durable task history in order, then the current execution, then any
// caller-supplied id. Empty and duplicate ids are dropped, so a scenario that
// failed before or after a fresh execution was queued still enumerates exactly
// the executions it must cancel and wait for.
func ExecutionIDs(task api.TaskReadResult, extra ...string) []string {
	seen := make(map[string]bool, len(task.ExecutionHistory)+len(extra)+1)
	ids := make([]string, 0, len(task.ExecutionHistory)+len(extra)+1)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, id := range task.ExecutionHistory {
		add(id)
	}
	add(task.CurrentExecution)
	for _, id := range extra {
		add(id)
	}
	return ids
}
