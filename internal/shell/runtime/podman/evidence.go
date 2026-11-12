// Observation, stop, ownership and redaction evidence for the Podman adapter.
//
// The rules here keep uncertainty visible: an absent container, an unreachable
// daemon, a created container whose start may still be delayed and an exited
// main process without whole-container evidence never collapse into
// nonexecution or stop confirmation.
package podman

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/workspace"
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
// any effect. An empty task id is valid and means the execution is task-free; an
// action slug is always required.
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

// checkContainerIdentity reports whether the observed container belongs to the
// persisted effect identity. A container created for another identity is never
// silently reused or removed.
func checkContainerIdentity(state ContainerState, identity engine.EffectIdentity) error {
	switch {
	case state.Labels[LabelWorkload] != string(identity.WorkloadID):
		return fault.New(fault.Forbidden, "container is not owned by this workload identity")
	case state.Labels[LabelExecution] != string(identity.ExecutionID):
		return fault.New(fault.Forbidden, "container is not owned by this execution identity")
	case state.Labels[LabelAction] != string(identity.ActionID):
		return fault.New(fault.Forbidden, "container is not owned by this action identity")
	case state.Labels[LabelIdempotencyKey] != identity.IdempotencyKey:
		return fault.New(fault.ChangedIdempotencyContent, "container carries a different idempotency key")
	}
	return nil
}

// checkContainerWorkspace reports whether the observed container is bound to the
// requested retained workspace.
func checkContainerWorkspace(state ContainerState, workspaceID id.WorkspaceID) error {
	if state.Labels[LabelWorkspace] != string(workspaceID) {
		return fault.New(fault.Forbidden, "container is bound to a different workspace")
	}
	return nil
}

// checkExistingContainer reports whether an already existing container belongs
// to this exact effect identity: same labels, same retained workspace and the
// image pinned by the completed preparation.
func checkExistingContainer(state ContainerState, identity engine.EffectIdentity, workspaceID id.WorkspaceID, image string) error {
	if err := checkContainerIdentity(state, identity); err != nil {
		return err
	}
	if err := checkContainerWorkspace(state, workspaceID); err != nil {
		return err
	}
	if state.Image != "" && state.Image != image {
		return fault.New(fault.ChangedIdempotencyContent, "existing container uses a different image")
	}
	return nil
}

// observationFor maps libpod container evidence to the declared Kernel
// vocabulary. A created container is affirmative never-started proof only when
// no start attempt was durably recorded and none is pending; an attempted start
// whose outcome was lost stays uncertain.
func observationFor(state ContainerState, attempted bool) kernel.Observation {
	switch state.Status {
	case ContainerStatusCreated:
		if attempted || state.Started || state.StartPending {
			// A start was attempted, accepted or may still be delivered: the
			// command may run, so nonexecution is not proven.
			return kernel.UnknownObservation()
		}
		return kernel.NeverStarted(fmt.Sprintf(
			"container %q exists in status %q with no start attempt, no accepted start and no pending start",
			state.Name, state.Status))
	case ContainerStatusRunning:
		return kernel.RunningObservation()
	case ContainerStatusExited:
		if state.WholeStopped {
			return kernel.ExitedObservation(state.ExitCode, kernel.ConfirmedStop(fmt.Sprintf(
				"container %q exited with code %d and every container process stopped",
				state.Name, state.ExitCode)))
		}
		return kernel.ExitedObservation(state.ExitCode, kernel.PendingStop(fmt.Sprintf(
			"container %q exited with code %d without whole-container termination evidence",
			state.Name, state.ExitCode)))
	default:
		return kernel.UnknownObservation()
	}
}

// removable reports whether cleanup may proceed for one observed container.
// Only affirmative never-started evidence (no recorded start attempt) or
// affirmative whole-container termination qualifies; a running, attempted-start,
// delayed-start, ambiguous or partially stopped container retains every owned
// resource.
func removable(state ContainerState, attempted bool) bool {
	switch state.Status {
	case ContainerStatusCreated:
		return !attempted && !state.Started && !state.StartPending
	case ContainerStatusExited:
		return state.WholeStopped
	default:
		return false
	}
}

// describeContainerState renders an operator-facing, non-secret state summary.
func describeContainerState(state ContainerState) string {
	return fmt.Sprintf("status %q started=%t start_pending=%t whole_stopped=%t",
		state.Status, state.Started, state.StartPending, state.WholeStopped)
}

// redactionValues returns every configured value that must be removed from log
// output, longest first so a shorter value cannot partially obscure a longer
// one. A missing or unreadable source is an error: log output is never emitted
// without redaction.
func (a *Adapter) redactionValues(ctx context.Context, workload id.WorkloadID, record preparationRecord) ([]string, error) {
	logicalPaths := make([]string, 0, len(record.Secrets)+1)
	for _, name := range record.Secrets {
		path, err := workspace.SecretPath(name)
		if err != nil {
			return nil, err
		}
		logicalPaths = append(logicalPaths, path)
	}
	logicalPaths = append(logicalPaths, workspace.ReservedCredentialPath)

	values := make([]string, 0, len(logicalPaths))
	for _, logical := range logicalPaths {
		data, err := a.files.ReadFile(ctx, hostPath(a.root, workload, logical))
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		values = append(values, string(data))
	}
	sort.Slice(values, func(left, right int) bool {
		if len(values[left]) != len(values[right]) {
			return len(values[left]) > len(values[right])
		}
		return values[left] < values[right]
	})
	return values, nil
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

// logCursorStreamBits splits the single log continuation token into one position
// per stream: the high 32 bits carry stdout and the low 32 bits carry stderr.
// Reading a stream advances only that stream's position, so a later read never
// skips the quieter stream.
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
