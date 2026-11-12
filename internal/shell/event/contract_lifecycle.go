package event

// The lifecycle projection and emitter ownership.
//
// Transitions commit durable execution/action state through the Kernel reducer,
// but flow.Commit carries no event and the cross-scope store controller refuses
// event writes. ProjectLifecycle is the one pure seam that turns such a commit
// into the ordered lifecycle event specs whose state change that commit is. It
// reads no clock, generates no random identity and imports no Kernel builder:
// the occurrence time is Commit.At, the source is the trusted system producer
// and every event id derives deterministically from (execution id, committed
// version, type, subject).
//
// Every Shell unit that carries a flow.Commit uses the same projection. Such a
// unit appends ExcludeOwned(ProjectLifecycle(pre, commit), owned...) to the
// events it already emits itself: ExcludeOwned removes the classes the unit
// owns (request.resolved, execution.cancelled, action.requested, ...) and
// de-duplicates the remainder by (type, subject), so no event is published
// twice and no transition class is dropped on a Shell path.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// systemPrincipal is the trusted Shell producer of a projected lifecycle
// event. The projection never attributes an engine commit to a caller.
const systemPrincipal = id.PrincipalID("system")

// SystemSource is the trusted system producer stamped on every projected
// lifecycle event.
var SystemSource = Source{Kind: SourceShell, PrincipalID: systemPrincipal}

// maxLifecycleReasonBytes bounds the projected failure reason class. The
// free-form Failure.Detail is never projected.
const maxLifecycleReasonBytes = 128

// ActionPreImage is the version-guarded pre-image of one action inside an
// execution, as read before the commit is applied.
type ActionPreImage struct {
	ActionID id.ActionID
	Index    int
	Kind     definition.ActionKind
	State    kernel.ActionState
	Version  uint64
}

// ExecutionPreImage is the version-guarded pre-image of one execution, as read
// before the commit is applied. Actions are in action-index order.
type ExecutionPreImage struct {
	Scope       tenancy.Scope
	ExecutionID id.ExecutionID
	Version     uint64
	State       kernel.ExecutionState
	Actions     []ActionPreImage
}

// LifecycleEvent is one projected lifecycle event spec. It is directly
// publishable: the trusted source, scope, time, subject and bounded facts form
// a valid event.Envelope through NewEnvelope.
type LifecycleEvent struct {
	ID             id.EventID
	Type           Type
	Source         Source
	At             time.Time
	Scope          tenancy.Scope
	SubjectID      string
	SubjectVersion uint64
	Data           []byte
}

// OwnedEvent names one lifecycle event class a Shell path already emits
// itself. An empty SubjectID matches every subject of that type; a non-empty
// SubjectID matches exactly that subject.
type OwnedEvent struct {
	Type      Type
	SubjectID string
}

// ownedKey is the publication identity ExcludeOwned de-duplicates by.
type ownedKey struct {
	eventType Type
	subjectID string
}

// ExcludeOwned returns the projection a Shell path must append after removing
// the event classes it already emits itself. It is pure and deterministic: it
// never mutates events, returns a new slice whose facts are copied, removes
// every OwnedEvent match (an empty SubjectID matches every subject of that
// type) and de-duplicates the remainder by (type, subject), keeping the first
// occurrence in projection order. A nil or empty input yields nil.
func ExcludeOwned(events []LifecycleEvent, owned ...OwnedEvent) []LifecycleEvent {
	if len(events) == 0 {
		return nil
	}
	kept := make([]LifecycleEvent, 0, len(events))
	seen := make(map[ownedKey]bool, len(events))
	for _, spec := range events {
		if ownedEventMatches(spec, owned) {
			continue
		}
		key := ownedKey{eventType: spec.Type, subjectID: spec.SubjectID}
		if seen[key] {
			continue
		}
		seen[key] = true
		cloned := spec
		if spec.Data != nil {
			cloned.Data = append([]byte(nil), spec.Data...)
		}
		kept = append(kept, cloned)
	}
	return kept
}

// ownedEventMatches reports whether one spec is an owned class. An empty owned
// subject is the wildcard for every subject of that type.
func ownedEventMatches(spec LifecycleEvent, owned []OwnedEvent) bool {
	for _, candidate := range owned {
		if candidate.Type != spec.Type {
			continue
		}
		if candidate.SubjectID == "" || candidate.SubjectID == spec.SubjectID {
			return true
		}
	}
	return false
}

// executionFacts are the bounded, secret-free facts of an execution-level
// state change.
type executionFacts struct {
	ExecutionID   string                `json:"execution_id"`
	PreviousState kernel.ExecutionState `json:"previous_state"`
	State         kernel.ExecutionState `json:"state"`
	Version       uint64                `json:"version"`
}

// failureFacts are the bounded, secret-free facts of a terminal failure. They
// name the failure class and the reason class, never the free-form detail.
type failureFacts struct {
	ExecutionID   string                `json:"execution_id"`
	PreviousState kernel.ExecutionState `json:"previous_state"`
	State         kernel.ExecutionState `json:"state"`
	Version       uint64                `json:"version"`
	FailureKind   kernel.FailureKind    `json:"failure_kind"`
	Reason        string                `json:"reason,omitempty"`
}

// cleanupFacts are the bounded, secret-free facts of committed cleanup
// progress.
type cleanupFacts struct {
	ExecutionID  string              `json:"execution_id"`
	CleanupState kernel.CleanupState `json:"cleanup_state"`
	Attempts     int                 `json:"attempts,omitempty"`
}

// actionFacts are the bounded, secret-free facts of one action-level event.
type actionFacts struct {
	ActionID      string                `json:"action_id"`
	Index         int                   `json:"index"`
	Kind          definition.ActionKind `json:"kind"`
	PreviousState kernel.ActionState    `json:"previous_state"`
	State         kernel.ActionState    `json:"state"`
	Version       uint64                `json:"version"`
}

// actionChange is one committed action state change.
type actionChange struct {
	action ActionPreImage
	next   kernel.ActionState
}

// ProjectLifecycle projects one version-guarded Kernel commit into the ordered
// lifecycle events it represents. It is the same pure projection for the
// Kernel engine adapter and for a Shell unit that carries the commit; the
// Shell unit appends ExcludeOwned(result, owned...) to its own events.
//
// Ordering is total and stable: execution-level events first (the state change
// then cleanup progress), followed by action-level events in action-index
// order. A request action activation publishes action.transition and then the
// public action.requested; the internal request.activated alias is never
// projected. A commit that changes no durable lifecycle state yields no event.
//
// Every inconsistent pre-image/commit pair is refused with invalid_input and
// no partial event: an identity mismatch, a stale or future expected version,
// an unknown action/index, a version mismatch, an unnamed action state change,
// a duplicate or repeating pending cancellation, an unknown state, a rewritten
// terminal state or a zero occurrence time.
func ProjectLifecycle(pre ExecutionPreImage, commit flow.Commit) ([]LifecycleEvent, error) {
	if err := validatePreImage(pre); err != nil {
		return nil, err
	}
	actions := make(map[id.ActionID]ActionPreImage, len(pre.Actions))
	for _, action := range pre.Actions {
		actions[action.ActionID] = action
	}
	if err := validateCommit(pre, commit, actions); err != nil {
		return nil, err
	}

	committedVersion := pre.Version + 1
	events := make([]LifecycleEvent, 0, len(pre.Actions)+2)
	if commit.NextExecutionState != pre.State {
		eventType, data, err := executionTransitionEvent(pre, commit, committedVersion)
		if err != nil {
			return nil, err
		}
		events = append(events, projectedEvent(pre, commit, eventType, string(pre.ExecutionID), committedVersion, data))
	}
	if commit.CleanupUpdate != nil && commit.CleanupUpdate.State != kernel.CleanupNone {
		data, err := json.Marshal(cleanupFacts{
			ExecutionID:  string(pre.ExecutionID),
			CleanupState: commit.CleanupUpdate.State,
			Attempts:     commit.CleanupUpdate.Attempts,
		})
		if err != nil {
			return nil, fault.Wrap(fault.InternalFailure, "cleanup lifecycle facts could not be encoded", err)
		}
		events = append(events, projectedEvent(pre, commit, TypeCleanup, string(pre.ExecutionID), committedVersion, data))
	}
	for _, change := range orderedActionChanges(commit, actions) {
		committedActionVersion := change.action.Version + 1
		if change.next != change.action.State {
			data, err := json.Marshal(actionFacts{
				ActionID:      string(change.action.ActionID),
				Index:         change.action.Index,
				Kind:          change.action.Kind,
				PreviousState: change.action.State,
				State:         change.next,
				Version:       committedActionVersion,
			})
			if err != nil {
				return nil, fault.Wrap(fault.InternalFailure, "action lifecycle facts could not be encoded", err)
			}
			events = append(events, projectedEvent(pre, commit, TypeActionTransition, string(change.action.ActionID), committedActionVersion, data))
		}
		if change.action.State == kernel.ActionPending &&
			change.next == kernel.ActionRunning &&
			change.action.Kind == definition.ActionKindRequest {
			data, err := json.Marshal(actionFacts{
				ActionID:      string(change.action.ActionID),
				Index:         change.action.Index,
				Kind:          change.action.Kind,
				PreviousState: change.action.State,
				State:         change.next,
				Version:       committedActionVersion,
			})
			if err != nil {
				return nil, fault.Wrap(fault.InternalFailure, "request activation facts could not be encoded", err)
			}
			events = append(events, projectedEvent(pre, commit, TypeActionRequested, string(change.action.ActionID), committedActionVersion, data))
		}
	}
	return events, nil
}

// executionTransitionEvent maps an execution state change to its one event type
// and bounded facts.
func executionTransitionEvent(pre ExecutionPreImage, commit flow.Commit, committedVersion uint64) (Type, []byte, error) {
	eventType := TypeExecutionStarted
	switch commit.NextExecutionState {
	case kernel.ExecutionRunning:
		eventType = TypeExecutionStarted
	case kernel.ExecutionSucceeded:
		eventType = TypeExecutionSucceeded
	case kernel.ExecutionFailed:
		eventType = TypeExecutionFailed
	case kernel.ExecutionCancelled:
		eventType = TypeExecutionCancelled
	default:
		return "", nil, fault.New(fault.InvalidInput, fmt.Sprintf("execution transition to %q has no lifecycle event", commit.NextExecutionState))
	}

	if eventType != TypeExecutionFailed {
		data, err := json.Marshal(executionFacts{
			ExecutionID:   string(pre.ExecutionID),
			PreviousState: pre.State,
			State:         commit.NextExecutionState,
			Version:       committedVersion,
		})
		if err != nil {
			return "", nil, fault.Wrap(fault.InternalFailure, "execution lifecycle facts could not be encoded", err)
		}
		return eventType, data, nil
	}

	reason := ""
	if commit.Failure != nil && len(commit.Failure.Reason) <= maxLifecycleReasonBytes {
		reason = commit.Failure.Reason
	}
	data, err := json.Marshal(failureFacts{
		ExecutionID:   string(pre.ExecutionID),
		PreviousState: pre.State,
		State:         commit.NextExecutionState,
		Version:       committedVersion,
		FailureKind:   commit.Failure.Kind,
		Reason:        reason,
	})
	if err != nil {
		return "", nil, fault.Wrap(fault.InternalFailure, "failure lifecycle facts could not be encoded", err)
	}
	return eventType, data, nil
}

// orderedActionChanges returns the primary action change and every pending
// cancellation in action-index order, independent of the commit's list order.
func orderedActionChanges(commit flow.Commit, actions map[id.ActionID]ActionPreImage) []actionChange {
	changes := make([]actionChange, 0, len(commit.PendingCancellations)+1)
	if commit.ActionID != "" {
		changes = append(changes, actionChange{
			action: actions[commit.ActionID],
			next:   commit.NextActionState,
		})
	}
	for _, update := range commit.PendingCancellations {
		changes = append(changes, actionChange{
			action: actions[update.ActionID],
			next:   update.NextState,
		})
	}
	sort.SliceStable(changes, func(left, right int) bool {
		return changes[left].action.Index < changes[right].action.Index
	})
	return changes
}

// projectedEvent assembles one event spec with a deterministic identity.
func projectedEvent(
	pre ExecutionPreImage,
	commit flow.Commit,
	eventType Type,
	subjectID string,
	subjectVersion uint64,
	data []byte,
) LifecycleEvent {
	return LifecycleEvent{
		ID:             lifecycleEventID(pre.ExecutionID, subjectVersion, eventType, subjectID),
		Type:           eventType,
		Source:         SystemSource,
		At:             commit.At,
		Scope:          pre.Scope,
		SubjectID:      subjectID,
		SubjectVersion: subjectVersion,
		Data:           data,
	}
}

// lifecycleEventID derives a collision-resistant event identity from the
// execution id, the subject's committed version, the event type and the
// subject. Length-prefixed canonical fields keep the tuple unambiguous.
func lifecycleEventID(executionID id.ExecutionID, version uint64, eventType Type, subjectID string) id.EventID {
	canonical := make([]byte, 0, 128)
	canonical = canonicalField(canonical, string(executionID))
	canonical = canonicalField(canonical, strconv.FormatUint(version, 10))
	canonical = canonicalField(canonical, string(eventType))
	canonical = canonicalField(canonical, subjectID)
	sum := sha256.Sum256(canonical)
	return id.EventID("event-" + hex.EncodeToString(sum[:]))
}

func canonicalField(dst []byte, value string) []byte {
	dst = strconv.AppendInt(dst, int64(len(value)), 10)
	dst = append(dst, ':')
	return append(dst, value...)
}

// validatePreImage checks the trusted pre-image shape.
func validatePreImage(pre ExecutionPreImage) error {
	if err := pre.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseExecutionID(string(pre.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "execution pre-image id is invalid", err)
	}
	if pre.Version == 0 {
		return fault.New(fault.InvalidInput, "execution pre-image version is zero")
	}
	if !pre.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("execution pre-image state %q is unknown", pre.State))
	}
	seen := make(map[id.ActionID]bool, len(pre.Actions))
	for position, action := range pre.Actions {
		if action.Index != position {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action pre-image index %d is out of order at position %d", action.Index, position))
		}
		if _, err := id.ParseActionID(string(action.ActionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "action pre-image id is invalid", err)
		}
		if !action.Kind.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action pre-image kind %q is unknown", action.Kind))
		}
		if !action.State.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action pre-image state %q is unknown", action.State))
		}
		if action.Version == 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action pre-image %q version is zero", action.ActionID))
		}
		if seen[action.ActionID] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action pre-image repeats action %q", action.ActionID))
		}
		seen[action.ActionID] = true
	}
	return nil
}

// validateCommit checks that the commit applies exactly one consistent,
// representable transition to the pre-image.
func validateCommit(pre ExecutionPreImage, commit flow.Commit, actions map[id.ActionID]ActionPreImage) error {
	if commit.ExecutionID != pre.ExecutionID {
		return fault.New(fault.InvalidInput, "commit execution id does not match the pre-image")
	}
	if commit.ExpectedExecutionVersion != pre.Version {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"commit expected execution version %d does not match the pre-image version %d",
			commit.ExpectedExecutionVersion, pre.Version,
		))
	}
	if commit.At.IsZero() {
		return fault.New(fault.InvalidInput, "commit occurrence time is zero")
	}
	if !commit.NextExecutionState.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("commit next execution state %q is unknown", commit.NextExecutionState))
	}
	if !executionTransitionAllowed(pre.State, commit.NextExecutionState) {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"execution transition %q to %q is not representable",
			pre.State, commit.NextExecutionState,
		))
	}
	if commit.NextExecutionState == kernel.ExecutionFailed && pre.State != kernel.ExecutionFailed {
		if commit.Failure == nil {
			return fault.New(fault.InvalidInput, "failing commit carries no typed failure")
		}
		if !commit.Failure.Kind.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("commit failure kind %q is unknown", commit.Failure.Kind))
		}
	}
	if commit.CleanupUpdate != nil && !commit.CleanupUpdate.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("commit cleanup state %q is unknown", commit.CleanupUpdate.State))
	}

	if commit.ActionID == "" {
		if commit.ExpectedActionVersion != 0 {
			return fault.New(fault.InvalidInput, "unnamed action carries an expected action version")
		}
		if commit.NextActionState != "" {
			return fault.New(fault.InvalidInput, "unnamed action carries a next action state")
		}
	} else {
		action, ok := actions[commit.ActionID]
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("commit names unknown action %q", commit.ActionID))
		}
		if commit.ExpectedActionVersion != action.Version {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"commit expected action version %d does not match action %q version %d",
				commit.ExpectedActionVersion, commit.ActionID, action.Version,
			))
		}
		if !commit.NextActionState.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("commit next action state %q is unknown", commit.NextActionState))
		}
		if !actionTransitionAllowed(action.State, commit.NextActionState) {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"action transition %q to %q is not representable",
				action.State, commit.NextActionState,
			))
		}
	}

	cancelled := make(map[id.ActionID]bool, len(commit.PendingCancellations))
	for _, update := range commit.PendingCancellations {
		if _, err := id.ParseActionID(string(update.ActionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "pending cancellation action id is invalid", err)
		}
		if commit.ActionID != "" && update.ActionID == commit.ActionID {
			return fault.New(fault.InvalidInput, "pending cancellation repeats the primary action")
		}
		action, ok := actions[update.ActionID]
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("pending cancellation names unknown action %q", update.ActionID))
		}
		if update.ExpectedVersion != action.Version {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"pending cancellation expected version %d does not match action %q version %d",
				update.ExpectedVersion, update.ActionID, action.Version,
			))
		}
		if update.NextState != kernel.ActionCancelled {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"pending cancellation of action %q carries next state %q; only cancelled is representable",
				update.ActionID, update.NextState,
			))
		}
		if action.State != kernel.ActionPending {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"pending cancellation names action %q in state %q; only a pending action can be cancelled",
				update.ActionID, action.State,
			))
		}
		if cancelled[update.ActionID] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("pending cancellation repeats action %q", update.ActionID))
		}
		cancelled[update.ActionID] = true
	}
	return nil
}

// executionTransitionAllowed reports whether the reducer can produce the
// execution transition. A same-state commit is a no-op or cleanup progress and
// is representable; a terminal state is never rewritten.
func executionTransitionAllowed(from, to kernel.ExecutionState) bool {
	if from == to {
		return true
	}
	switch from {
	case kernel.ExecutionQueued:
		return to == kernel.ExecutionRunning || to == kernel.ExecutionFailed || to == kernel.ExecutionCancelled
	case kernel.ExecutionRunning:
		return to == kernel.ExecutionSucceeded || to == kernel.ExecutionFailed || to == kernel.ExecutionCancelled
	default:
		return false
	}
}

// actionTransitionAllowed reports whether the reducer can produce the action
// transition. A same-state commit is a no-op; a terminal state is never
// rewritten.
func actionTransitionAllowed(from, to kernel.ActionState) bool {
	if from == to {
		return true
	}
	switch from {
	case kernel.ActionPending:
		return to == kernel.ActionRunning || to == kernel.ActionFailed || to == kernel.ActionCancelled
	case kernel.ActionRunning:
		return to == kernel.ActionSucceeded || to == kernel.ActionFailed || to == kernel.ActionCancelled
	default:
		return false
	}
}
