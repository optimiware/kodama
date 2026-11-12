// Observation, stop and log evidence for the Process runtime adapter.
//
// The rules here keep uncertainty visible: absence and a signal acknowledgment
// never collapse into exit or stop confirmation, and only affirmative
// never-created evidence reports never_started.
package process

import (
	"fmt"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// validateIdentity refuses an unusable persisted identity before any effect.
func validateIdentity(identity engine.EffectIdentity) error {
	if _, err := id.ParseWorkloadID(string(identity.WorkloadID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "workload id is invalid", err)
	}
	if _, err := id.ParseExecutionID(string(identity.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(identity.ActionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "action id is invalid", err)
	}
	if strings.TrimSpace(identity.IdempotencyKey) == "" {
		return fault.New(fault.InvalidInput, "idempotency key is empty")
	}
	return nil
}

// validateWorkspaceID refuses an unusable workspace identity before any effect.
func validateWorkspaceID(workspaceID id.WorkspaceID) error {
	if _, err := id.ParseWorkspaceID(string(workspaceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "workspace id is invalid", err)
	}
	return nil
}

// validatePrepareContext refuses an unusable action slug or task identity before
// any effect. An empty task id is valid and means the execution is task-free.
func validatePrepareContext(actionName id.ActionName, taskID id.TaskID) error {
	if _, err := id.ParseActionName(string(actionName)); err != nil {
		return fault.Wrap(fault.InvalidInput, "action name is invalid", err)
	}
	if taskID != "" {
		if _, err := id.ParseTaskID(string(taskID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "task id is invalid", err)
		}
	}
	return nil
}

// observationFor maps persisted spawn state and process-group evidence to the
// declared Kernel vocabulary. Only affirmative evidence is proof.
func observationFor(state groupState, status GroupStatus) kernel.Observation {
	switch state.State {
	case groupStarted:
		return observationForStatus(status)
	case groupNeverStarted:
		return kernel.NeverStarted(nonEmptyProof(state.Detail))
	case groupSpawning, groupUnknown:
		return observationForUncertainSpawn(state, status)
	}
	if status.NeverCreated {
		return kernel.NeverStarted(nonEmptyProof(state.Detail))
	}
	return observationForStatus(status)
}

// observationForUncertainSpawn reports what the observer affirmatively knows
// about a persisted spawn intent or lost spawn response. A running group is
// running, an affirmatively terminated group reports its leader exit, and only
// affirmative never-created evidence proves nonexecution. Zero or ambiguous
// evidence stays unknown and never collapses into absence.
func observationForUncertainSpawn(state groupState, status GroupStatus) kernel.Observation {
	if status.NeverCreated {
		return kernel.NeverStarted(nonEmptyProof(state.Detail))
	}
	if !status.Present && !status.MembersAlive && !status.LeaderExited && !status.GroupGone {
		return kernel.UnknownObservation()
	}
	return observationForStatus(status)
}

// observationForStatus reports exit only for affirmative whole-group
// termination with a leader exit status.
func observationForStatus(status GroupStatus) kernel.Observation {
	switch {
	case status.GroupGone && status.LeaderExited:
		return kernel.ExitedObservation(status.ExitCode, kernel.ConfirmedStop(
			fmt.Sprintf("process group affirmatively terminated with exit code %d", status.ExitCode),
		))
	case status.GroupGone:
		// Whole-group termination without a leader status is still not an
		// exit status.
		return kernel.UnknownObservation()
	case status.MembersAlive || status.Present || status.LeaderExited:
		return kernel.RunningObservation()
	default:
		return kernel.AbsentObservation()
	}
}

// nonEmptyProof never fabricates an empty never-started proof.
func nonEmptyProof(detail string) string {
	if proof := strings.TrimSpace(detail); proof != "" {
		return proof
	}
	return "the process executor affirmatively created no process"
}

// redactionValues returns every configured value that must be removed from log
// output, longest first so a shorter value cannot partially obscure a longer
// one.
func redactionValues(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		filtered = append(filtered, value)
	}
	sort.Slice(filtered, func(left, right int) bool {
		if len(filtered[left]) != len(filtered[right]) {
			return len(filtered[left]) > len(filtered[right])
		}
		return filtered[left] < filtered[right]
	})
	return filtered
}

// logSegment is one decoded piece of a log page: a raw byte range and the
// bytes it contributes after redaction (a raw copy or the fixed marker).
type logSegment struct {
	start  int
	end    int
	marker bool
	data   []byte
}

// scanLogSegments splits a page into raw runs and configured-value matches,
// left to right and longest value first (redactionValues orders them that way).
// A value is consumed atomically, so no page boundary can be placed inside one.
func scanLogSegments(data []byte, values []string) []logSegment {
	segments := make([]logSegment, 0, len(data))
	for index := 0; index < len(data); {
		matched := false
		for _, value := range values {
			if value == "" || len(value) > len(data)-index {
				continue
			}
			if string(data[index:index+len(value)]) == value {
				segments = append(segments, logSegment{
					start:  index,
					end:    index + len(value),
					marker: true,
					data:   []byte(Redacted),
				})
				index += len(value)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		segments = append(segments, logSegment{
			start: index,
			end:   index + 1,
			data:  []byte{data[index]},
		})
		index++
	}
	return segments
}

// couldExtend reports whether tail is a proper prefix of a configured value, so
// a longer match could still start there once the log grows past this page.
func couldExtend(tail []byte, values []string) bool {
	for _, value := range values {
		if len(value) <= len(tail) {
			continue
		}
		if hasStringPrefix(value, tail) {
			return true
		}
	}
	return false
}

// hasStringPrefix reports whether value starts with prefix without allocating.
func hasStringPrefix(value string, prefix []byte) bool {
	if len(prefix) > len(value) {
		return false
	}
	for index := range prefix {
		if value[index] != prefix[index] {
			return false
		}
	}
	return true
}

// redactLogPage redacts one bounded page without splitting a configured value
// across the page boundary or dropping raw bytes when marker expansion exceeds
// limit. It returns the raw prefix the cursor may advance over and the redacted
// bytes to emit.
func redactLogPage(raw []byte, values []string, limit int, mayGrow bool) ([]byte, []byte) {
	segments := scanLogSegments(raw, values)
	end := len(segments)
	hold := end
	if mayGrow {
		for index, segment := range segments {
			if couldExtend(raw[segment.start:], values) {
				end = index
				hold = index
				break
			}
		}
	}

	size := 0
	for _, segment := range segments[:end] {
		size += len(segment.data)
	}
	for end > 0 && size > limit {
		end--
		size -= len(segments[end].data)
	}
	if end == 0 {
		if len(segments) == 0 || hold == 0 {
			return nil, nil
		}
		first := segments[0]
		data := first.data
		if len(data) > limit {
			data = data[:limit]
		}
		return raw[:first.end], data
	}

	consumed := raw[:segments[end-1].end]
	data := make([]byte, 0, size)
	for _, segment := range segments[:end] {
		data = append(data, segment.data...)
	}
	return consumed, data
}

// logCursorStreamBits splits the single log continuation token into one
// position per stream: the high 32 bits carry stdout and the low 32 bits carry
// stderr.
const logCursorStreamBits = 32

const logCursorStreamMask int64 = (1 << logCursorStreamBits) - 1

const (
	// maxStdoutLogCursorPosition bounds the signed high 32-bit stdout half so
	// encodeLogCursor never sets the sign bit and returns a negative cursor.
	maxStdoutLogCursorPosition int64 = (1 << 31) - 1
	// maxStderrLogCursorPosition bounds the unsigned low 32-bit stderr half so
	// stderr never overflows into the stdout bits.
	maxStderrLogCursorPosition int64 = (1 << 32) - 1
)

// decodeLogCursor returns the per-stream positions encoded in one token.
func decodeLogCursor(cursor int64) (stdout, stderr int64) {
	return cursor >> logCursorStreamBits, cursor & logCursorStreamMask
}

// encodeLogCursor packs per-stream positions into one continuation token.
func encodeLogCursor(stdout, stderr int64) int64 {
	return stdout<<logCursorStreamBits | stderr
}

// nextStreamLogPosition advances one stream's cursor position while refusing
// any position that cannot be represented losslessly in the continuation token.
func nextStreamLogPosition(stream engine.LogStream, offset int64, consumed int) (int64, error) {
	bound := maxStdoutLogCursorPosition
	if stream == engine.LogStderr {
		bound = maxStderrLogCursorPosition
	}
	if offset < 0 || offset > bound || consumed < 0 || int64(consumed) > bound-offset {
		return 0, fault.New(fault.InvalidInput,
			"log position exceeds the continuation-token range")
	}
	return offset + int64(consumed), nil
}
