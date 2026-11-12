package domaincontract

// The lifecycle projection.
//
// flow.Commit carries no events, so the lifecycle events of an engine-driven
// execution transition are derived from the committed reduction. The
// transition→event projection is one pure, deterministic Shell function:
//
//	event.ProjectLifecycle(pre event.ExecutionPreImage, commit flow.Commit)
//	    ([]event.LifecycleEvent, error)
//
// Given the version-guarded pre-image of one execution and the committed
// Kernel reduction, it yields the ordered lifecycle event specs whose state
// change that commit is. It reads no clock, generates no random identity and
// imports no Kernel builder: the occurrence time is Commit.At, the source is
// the trusted system producer and every event id derives deterministically
// from (execution id, committed version, type, subject).
//
// These vectors exercise the declared projection only. No store, adapter,
// process or broker is involved.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var (
	projectionScope = tenancy.Scope{
		TenantID:  id.TenantID("tenant-1"),
		ProjectID: id.ProjectID("project-1"),
	}
	projectionAt = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	projectionExecution = id.ExecutionID("execution-1")
	containerAction     = id.ActionID("action-container")
	requestAction       = id.ActionID("action-request")
	firstSuccessor      = id.ActionID("action-successor-1")
	secondSuccessor     = id.ActionID("action-successor-2")
)

func intPointer(value int) *int { return &value }

func actionPreImage(
	index int,
	actionID id.ActionID,
	kind definition.ActionKind,
	state kernel.ActionState,
	version uint64,
) event.ActionPreImage {
	return event.ActionPreImage{
		ActionID: actionID,
		Index:    index,
		Kind:     kind,
		State:    state,
		Version:  version,
	}
}

func executionPreImage(
	state kernel.ExecutionState,
	version uint64,
	actions ...event.ActionPreImage,
) event.ExecutionPreImage {
	return event.ExecutionPreImage{
		Scope:       projectionScope,
		ExecutionID: projectionExecution,
		Version:     version,
		State:       state,
		Actions:     actions,
	}
}

// wantLifecycleEvent is one expected entry of the projected, ordered list.
type wantLifecycleEvent struct {
	eventType      event.Type
	subjectID      string
	subjectVersion uint64
}

type projectionCase struct {
	name   string
	pre    event.ExecutionPreImage
	commit flow.Commit
	want   []wantLifecycleEvent
}

func projectionCases() []projectionCase {
	return []projectionCase{
		{
			// The one event class that marks leaving `queued` for `running`.
			name: "queued to running emits execution.started",
			pre: executionPreImage(
				kernel.ExecutionQueued, 3,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 3,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 containerAction,
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				NextCurrentAction:        intPointer(0),
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				At:                       projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionStarted, string(projectionExecution), 4},
				{event.TypeActionTransition, string(containerAction), 2},
			},
		},
		{
			name: "running to succeeded emits one terminal event",
			pre: executionPreImage(
				kernel.ExecutionRunning, 7,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 2),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 7,
				NextExecutionState:       kernel.ExecutionSucceeded,
				ActionID:                 containerAction,
				ExpectedActionVersion:    2,
				NextActionState:          kernel.ActionSucceeded,
				NextCurrentAction:        intPointer(-1),
				Output: flow.OutputChange{
					Action: flow.OutputPromote,
					Values: kernel.Outputs{"result": "ok"},
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionSucceeded, string(projectionExecution), 8},
				{event.TypeActionTransition, string(containerAction), 3},
			},
		},
		{
			// The commit lists the pending cancellations in reverse index order;
			// the projection must still publish them in action-index order.
			name: "running to failed cancels pending successors by index",
			pre: executionPreImage(
				kernel.ExecutionRunning, 5,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 4),
				actionPreImage(1, firstSuccessor, definition.ActionKindContainer, kernel.ActionPending, 1),
				actionPreImage(2, secondSuccessor, definition.ActionKindRequest, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 5,
				NextExecutionState:       kernel.ExecutionFailed,
				ActionID:                 containerAction,
				ExpectedActionVersion:    4,
				NextActionState:          kernel.ActionFailed,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: secondSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction: intPointer(-1),
				Output:            flow.OutputChange{Action: flow.OutputDiscard},
				Failure: &kernel.Failure{
					Kind:   kernel.FailureRuntime,
					Reason: "workload_exited",
					Detail: "workload exited with status 1",
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionFailed, string(projectionExecution), 6},
				{event.TypeActionTransition, string(containerAction), 5},
				{event.TypeActionTransition, string(firstSuccessor), 2},
				{event.TypeActionTransition, string(secondSuccessor), 2},
			},
		},
		{
			// A pending cancellation at a lower index precedes a later primary
			// action: the total order is execution level, then action index.
			name: "action index orders a pending cancellation before the primary action",
			pre: executionPreImage(
				kernel.ExecutionRunning, 9,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionSucceeded, 2),
				actionPreImage(1, firstSuccessor, definition.ActionKindContainer, kernel.ActionPending, 1),
				actionPreImage(2, secondSuccessor, definition.ActionKindContainer, kernel.ActionRunning, 3),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 9,
				NextExecutionState:       kernel.ExecutionFailed,
				ActionID:                 secondSuccessor,
				ExpectedActionVersion:    3,
				NextActionState:          kernel.ActionFailed,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction: intPointer(-1),
				Output:            flow.OutputChange{Action: flow.OutputDiscard},
				Failure: &kernel.Failure{
					Kind:   kernel.FailureRuntime,
					Reason: "workload_exited",
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionFailed, string(projectionExecution), 10},
				{event.TypeActionTransition, string(firstSuccessor), 2},
				{event.TypeActionTransition, string(secondSuccessor), 4},
			},
		},
		{
			// Cancelling before anything started emits no execution.started.
			name: "queued cancellation emits no started event",
			pre: executionPreImage(
				kernel.ExecutionQueued, 2,
				actionPreImage(0, requestAction, definition.ActionKindRequest, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 2,
				NextExecutionState:       kernel.ExecutionCancelled,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: requestAction, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction:    intPointer(-1),
				Output:               flow.OutputChange{Action: flow.OutputDiscard},
				InvalidateCredential: true,
				Cleanup: &flow.CleanupIntent{
					WorkloadID:  id.WorkloadID("workload-1"),
					WorkspaceID: id.WorkspaceID("workspace-1"),
					Reason:      "cancelled",
				},
				CleanupUpdate: &flow.CleanupUpdate{
					State:       kernel.CleanupPending,
					WorkspaceID: id.WorkspaceID("workspace-1"),
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionCancelled, string(projectionExecution), 3},
				{event.TypeCleanup, string(projectionExecution), 3},
				{event.TypeActionTransition, string(requestAction), 2},
			},
		},
		{
			// A request action activation is published as the public
			// action.requested type; request.activated is only the internal
			// alias and must never appear in a projected spec.
			name: "request activation publishes action.requested",
			pre: executionPreImage(
				kernel.ExecutionRunning, 4,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionSucceeded, 3),
				actionPreImage(1, requestAction, definition.ActionKindRequest, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 4,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 requestAction,
				ExpectedActionVersion:    1,
				NextActionState:          kernel.ActionRunning,
				NextCurrentAction:        intPointer(1),
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				InvalidateCredential:     true,
				At:                       projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeActionTransition, string(requestAction), 2},
				{event.TypeActionRequested, string(requestAction), 2},
			},
		},
		{
			// A passing answer settles the request action and the execution in
			// the same commit: one terminal event, never two.
			name: "request settlement succeeds the execution once",
			pre: executionPreImage(
				kernel.ExecutionRunning, 8,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionSucceeded, 2),
				actionPreImage(1, requestAction, definition.ActionKindRequest, kernel.ActionRunning, 3),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 8,
				NextExecutionState:       kernel.ExecutionSucceeded,
				ActionID:                 requestAction,
				ExpectedActionVersion:    3,
				NextActionState:          kernel.ActionSucceeded,
				NextCurrentAction:        intPointer(-1),
				Output: flow.OutputChange{
					Action: flow.OutputPromote,
					Values: kernel.Outputs{"decision": "pass"},
				},
				Answer: &kernel.Answer{
					Decision: kernel.DecisionPass,
					Outputs:  kernel.Outputs{"decision": "pass"},
					Actor:    id.PrincipalID("principal-1"),
					At:       projectionAt,
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionSucceeded, string(projectionExecution), 9},
				{event.TypeActionTransition, string(requestAction), 4},
			},
		},
		{
			// Cleanup progress after the immutable outcome is its own
			// execution-level fact; the terminal event is not repeated.
			name: "cleanup progress after a terminal outcome",
			pre: executionPreImage(
				kernel.ExecutionFailed, 9,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionFailed, 4),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 9,
				NextExecutionState:       kernel.ExecutionFailed,
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				CleanupUpdate: &flow.CleanupUpdate{
					State:       kernel.CleanupComplete,
					WorkloadID:  id.WorkloadID("workload-1"),
					WorkspaceID: id.WorkspaceID("workspace-1"),
					Attempts:    2,
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeCleanup, string(projectionExecution), 10},
			},
		},
		{
			// Workspace loss before start fails the execution without a
			// started event and cancels every still-pending action.
			name: "queued workspace loss fails without a started event",
			pre: executionPreImage(
				kernel.ExecutionQueued, 1,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 1,
				NextExecutionState:       kernel.ExecutionFailed,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: containerAction, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction: intPointer(-1),
				Output:            flow.OutputChange{Action: flow.OutputDiscard},
				Failure: &kernel.Failure{
					Kind:   kernel.FailureWorkspaceLost,
					Reason: "workspace_missing",
				},
				CleanupUpdate: &flow.CleanupUpdate{
					State:       kernel.CleanupPending,
					WorkspaceID: id.WorkspaceID("workspace-1"),
				},
				At: projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionFailed, string(projectionExecution), 2},
				{event.TypeCleanup, string(projectionExecution), 2},
				{event.TypeActionTransition, string(containerAction), 2},
			},
		},
		{
			// Output staging rewrites no lifecycle state, so it publishes
			// nothing: lifecycle events follow durable transitions, never
			// workload observations or staged values.
			name: "output staging emits no lifecycle event",
			pre: executionPreImage(
				kernel.ExecutionRunning, 6,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 3),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 6,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 containerAction,
				ExpectedActionVersion:    3,
				NextActionState:          kernel.ActionRunning,
				Output: flow.OutputChange{
					Action: flow.OutputStage,
					Values: kernel.Outputs{"result": "staged"},
				},
				At: projectionAt,
			},
		},
		{
			// A stop barrier is a request, never confirmation: recording it
			// changes no lifecycle state and publishes no event.
			name: "unconfirmed stop evidence emits no lifecycle event",
			pre: executionPreImage(
				kernel.ExecutionRunning, 6,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 3),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 6,
				NextExecutionState:       kernel.ExecutionRunning,
				ActionID:                 containerAction,
				ExpectedActionVersion:    3,
				NextActionState:          kernel.ActionRunning,
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				StopEvidence:             pointerTo(kernel.PendingStop("termination requested")),
				At:                       projectionAt,
			},
		},
		{
			// Confirmed stop evidence settles the action in the same commit:
			// the settlement events publish, and no separate "stop" event
			// exists because the barrier is evidence, not a lifecycle state.
			name: "confirmed stop settles with the terminal events",
			pre: executionPreImage(
				kernel.ExecutionRunning, 4,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 2),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 4,
				NextExecutionState:       kernel.ExecutionSucceeded,
				ActionID:                 containerAction,
				ExpectedActionVersion:    2,
				NextActionState:          kernel.ActionSucceeded,
				NextCurrentAction:        intPointer(-1),
				Output:                   flow.OutputChange{Action: flow.OutputPromote},
				StopEvidence:             pointerTo(kernel.ConfirmedStop("container exited with status 0")),
				At:                       projectionAt,
			},
			want: []wantLifecycleEvent{
				{event.TypeExecutionSucceeded, string(projectionExecution), 5},
				{event.TypeActionTransition, string(containerAction), 3},
			},
		},
		{
			// Cleanup intent without progress is not a progress event.
			name: "cleanup intent alone emits no lifecycle event",
			pre: executionPreImage(
				kernel.ExecutionRunning, 6,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 3),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 6,
				NextExecutionState:       kernel.ExecutionRunning,
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				Cleanup: &flow.CleanupIntent{
					WorkloadID:  id.WorkloadID("workload-1"),
					WorkspaceID: id.WorkspaceID("workspace-1"),
					Reason:      "cancelled",
				},
				At: projectionAt,
			},
		},
		{
			// A replayed equal commit (identical state, identical versions)
			// converges with no duplicate event.
			name: "equal replay of a settled transition emits nothing",
			pre: executionPreImage(
				kernel.ExecutionFailed, 9,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionFailed, 4),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 9,
				NextExecutionState:       kernel.ExecutionFailed,
				ActionID:                 containerAction,
				ExpectedActionVersion:    4,
				NextActionState:          kernel.ActionFailed,
				Output:                   flow.OutputChange{Action: flow.OutputNone},
				At:                       projectionAt,
			},
		},
	}
}

func pointerTo[T any](value T) *T { return &value }

// TestProjectLifecycleGolden pins the projection table: which transition class
// yields which event type, on which subject and committed version, in which
// total order.
func TestProjectLifecycleGolden(t *testing.T) {
	for _, testCase := range projectionCases() {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := event.ProjectLifecycle(testCase.pre, testCase.commit)
			if err != nil {
				t.Fatalf("ProjectLifecycle refused a consistent commit: %v", err)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("ProjectLifecycle yielded %d events %s; want %d %s",
					len(got), describeSpecs(got), len(testCase.want), describeWants(testCase.want))
			}
			for index, want := range testCase.want {
				spec := got[index]
				if spec.Type != want.eventType {
					t.Fatalf("event %d type = %q; want %q (all: %s)", index, spec.Type, want.eventType, describeSpecs(got))
				}
				if spec.SubjectID != want.subjectID {
					t.Fatalf("event %d subject = %q; want %q", index, spec.SubjectID, want.subjectID)
				}
				if spec.SubjectVersion != want.subjectVersion {
					t.Fatalf("event %d subject version = %d; want %d", index, spec.SubjectVersion, want.subjectVersion)
				}
			}
			seen := make(map[id.EventID]bool, len(got))
			for _, spec := range got {
				checkLifecycleSpec(t, spec, testCase.pre, testCase.commit)
				if seen[spec.ID] {
					t.Fatalf("event id %q is not unique inside one projection", spec.ID)
				}
				seen[spec.ID] = true
			}
		})
	}
}

// checkLifecycleSpec pins the shared facts of every projected event spec.
func checkLifecycleSpec(t *testing.T, spec event.LifecycleEvent, pre event.ExecutionPreImage, commit flow.Commit) {
	t.Helper()
	if spec.Type == event.TypeRequestActivated {
		t.Fatalf("projected event carries the internal alias %q; the public type is %q",
			event.TypeRequestActivated, event.TypeActionRequested)
	}
	canonical, ok := api.CanonicalEventType(string(spec.Type))
	if !ok {
		t.Fatalf("projected event type %q is not a lifecycle type", spec.Type)
	}
	if canonical != string(spec.Type) {
		t.Fatalf("projected event type %q is an alias of %q; a projected event must carry the canonical type",
			spec.Type, canonical)
	}
	if _, err := id.ParseEventID(string(spec.ID)); err != nil {
		t.Fatalf("projected event id %q is not a usable event identity: %v", spec.ID, err)
	}
	if spec.SubjectID == "" {
		t.Fatal("projected event has an empty subject id")
	}
	if spec.Scope != pre.Scope {
		t.Fatalf("projected event scope = %#v; want the pre-image scope %#v", spec.Scope, pre.Scope)
	}
	if !spec.At.Equal(commit.At) {
		t.Fatalf("projected occurrence time = %s; want the commit time %s", spec.At, commit.At)
	}
	if spec.Source != event.SystemSource {
		t.Fatalf("projected source = %#v; want the trusted system source %#v", spec.Source, event.SystemSource)
	}
	if spec.Source.Kind != event.SourceShell {
		t.Fatalf("projected source kind = %q; want %q", spec.Source.Kind, event.SourceShell)
	}
	if spec.Source.PrincipalID == "" {
		t.Fatal("projected source principal is empty")
	}
	if len(spec.Data) > event.DefaultBounds.MaxDataBytes {
		t.Fatalf("projected facts are %d bytes; the event bound is %d", len(spec.Data), event.DefaultBounds.MaxDataBytes)
	}
	if len(spec.Data) > 0 {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(spec.Data, &object); err != nil {
			t.Fatalf("projected facts are not a JSON object: %v (%s)", err, spec.Data)
		}
		if object == nil {
			t.Fatalf("projected facts are not a JSON object: %s", spec.Data)
		}
	}
	if spec.Type == event.TypeExecutionFailed {
		if commit.Failure == nil {
			t.Fatal("a failed execution event was projected without a typed commit failure")
		}
		if !bytes.Contains(spec.Data, []byte(string(commit.Failure.Kind))) {
			t.Fatalf("failed execution facts %s do not name the failure kind %q", spec.Data, commit.Failure.Kind)
		}
	}
	if spec.Type == event.TypeCleanup {
		if commit.CleanupUpdate == nil {
			t.Fatal("a cleanup event was projected without committed cleanup progress")
		}
		if !bytes.Contains(spec.Data, []byte(string(commit.CleanupUpdate.State))) {
			t.Fatalf("cleanup facts %s do not name the cleanup state %q", spec.Data, commit.CleanupUpdate.State)
		}
	}
	// A spec must be directly publishable: the trusted source, scope, time,
	// subject and facts pass the durable envelope validation unchanged.
	if _, err := event.NewEnvelope(spec.ID, spec.Source, spec.Scope, spec.At, event.EnvelopeInput{
		Type:           spec.Type,
		SubjectID:      spec.SubjectID,
		SubjectVersion: spec.SubjectVersion,
		Data:           spec.Data,
	}); err != nil {
		t.Fatalf("projected spec does not form a valid envelope: %v", err)
	}
}

func describeWants(wants []wantLifecycleEvent) string {
	parts := make([]string, 0, len(wants))
	for _, want := range wants {
		parts = append(parts, string(want.eventType)+"@"+want.subjectID)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// TestProjectLifecycleEventIDsAreDeterministic pins the identity rule: the id
// derives only from (execution id, committed version, type, subject), so a
// replay reproduces it, while a different version, type or subject is a
// different event.
func TestProjectLifecycleEventIDsAreDeterministic(t *testing.T) {
	activation := projectionCases()[5]
	first, err := event.ProjectLifecycle(activation.pre, activation.commit)
	if err != nil {
		t.Fatalf("ProjectLifecycle refused the activation case: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("activation case yielded %d events; want 2", len(first))
	}
	second, err := event.ProjectLifecycle(activation.pre, activation.commit)
	if err != nil {
		t.Fatalf("replayed projection was refused: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replayed projection differs:\n first: %#v\nsecond: %#v", first, second)
	}

	// Same subject and same committed version, different type: the internal
	// alias must not collapse the two distinct facts onto one identity.
	if first[0].SubjectID != first[1].SubjectID || first[0].SubjectVersion != first[1].SubjectVersion {
		t.Fatalf("activation specs do not share a subject/version: %#v", first)
	}
	if first[0].Type == first[1].Type {
		t.Fatalf("activation specs share the type %q", first[0].Type)
	}
	if first[0].ID == first[1].ID {
		t.Fatalf("distinct types %q and %q share the event id %q", first[0].Type, first[1].Type, first[0].ID)
	}

	// A later committed execution version is a different execution event even
	// when every other input field is identical.
	succeeded := projectionCases()[1]
	older, err := event.ProjectLifecycle(succeeded.pre, succeeded.commit)
	if err != nil {
		t.Fatalf("ProjectLifecycle refused the succeeded case: %v", err)
	}
	nextPre := succeeded.pre
	nextPre.Version++
	nextCommit := succeeded.commit
	nextCommit.ExpectedExecutionVersion = succeeded.pre.Version + 1
	newer, err := event.ProjectLifecycle(nextPre, nextCommit)
	if err != nil {
		t.Fatalf("next-version projection was refused: %v", err)
	}
	if len(older) == 0 || len(newer) != len(older) {
		t.Fatalf("version projections yielded %d and %d events; want equal non-zero", len(older), len(newer))
	}
	if older[0].Type != event.TypeExecutionSucceeded || newer[0].Type != event.TypeExecutionSucceeded {
		t.Fatalf("execution-level events are %q and %q; want %q", older[0].Type, newer[0].Type, event.TypeExecutionSucceeded)
	}
	if older[0].SubjectVersion == newer[0].SubjectVersion {
		t.Fatalf("execution events share subject version %d across two committed versions", older[0].SubjectVersion)
	}
	if older[0].ID == newer[0].ID {
		t.Fatalf("execution event keeps id %q across committed versions %d and %d",
			older[0].ID, older[0].SubjectVersion, newer[0].SubjectVersion)
	}

	// Two actions changed by one commit at the same action version are
	// different subjects and therefore different events.
	failureCase := projectionCases()[3]
	failed, err := event.ProjectLifecycle(failureCase.pre, failureCase.commit)
	if err != nil {
		t.Fatalf("ProjectLifecycle refused the multi-action case: %v", err)
	}
	byAction := make(map[string]id.EventID, len(failed))
	for _, spec := range failed {
		if spec.Type != event.TypeActionTransition {
			continue
		}
		if previous, ok := byAction[spec.SubjectID]; ok {
			t.Fatalf("action %q was projected twice with ids %q and %q", spec.SubjectID, previous, spec.ID)
		}
		byAction[spec.SubjectID] = spec.ID
	}
	if len(byAction) != 2 {
		t.Fatalf("multi-action case projected %d action subjects; want 2", len(byAction))
	}
	for subject, eventID := range byAction {
		for otherSubject, otherID := range byAction {
			if subject == otherSubject {
				continue
			}
			if eventID == otherID {
				t.Fatalf("subjects %q and %q share the event id %q", subject, otherSubject, eventID)
			}
		}
	}
}

// TestProjectLifecycleIsPure pins that the projection mutates nothing and
// shares no mutable state between calls.
func TestProjectLifecycleIsPure(t *testing.T) {
	testCase := projectionCases()[2]
	preCopy := testCase.pre
	preCopy.Actions = append([]event.ActionPreImage(nil), testCase.pre.Actions...)
	commitCopy := testCase.commit
	commitCopy.PendingCancellations = append([]flow.ActionUpdate(nil), testCase.commit.PendingCancellations...)

	first, err := event.ProjectLifecycle(testCase.pre, testCase.commit)
	if err != nil {
		t.Fatalf("ProjectLifecycle refused a consistent commit: %v", err)
	}
	if !reflect.DeepEqual(testCase.pre, preCopy) {
		t.Fatalf("ProjectLifecycle mutated the pre-image: got %#v; want %#v", testCase.pre, preCopy)
	}
	if !reflect.DeepEqual(testCase.commit, commitCopy) {
		t.Fatalf("ProjectLifecycle mutated the commit: got %#v; want %#v", testCase.commit, commitCopy)
	}

	// Damaging the returned slice and its facts must not change the next
	// projection: the result is owned by the caller.
	for index := range first {
		if len(first[index].Data) > 0 {
			first[index].Data[0] ^= 0xff
		}
		first[index].Type = event.TypeCustomObservation
	}
	second, err := event.ProjectLifecycle(testCase.pre, testCase.commit)
	if err != nil {
		t.Fatalf("second projection was refused: %v", err)
	}
	if len(second) != len(commitCopy.PendingCancellations)+2 {
		t.Fatalf("second projection yielded %d events; want %d", len(second), len(commitCopy.PendingCancellations)+2)
	}
	if second[0].Type != event.TypeExecutionFailed {
		t.Fatalf("second projection starts with %q; want %q", second[0].Type, event.TypeExecutionFailed)
	}
}

// TestProjectLifecycleFactsAreBoundedAndSecretFree pins that the projected
// facts never carry submitted output content, an answer body, a resolved
// subject literal, credential or token material, or free-form error text.
func TestProjectLifecycleFactsAreBoundedAndSecretFree(t *testing.T) {
	const (
		outputSecret  = "submitted-output-value-do-not-publish"
		tokenSecret   = "kodex.execution-1.token-material"
		subjectSecret = "refs/heads/private-subject"
		detailSecret  = "operator detail carrying kodex.execution-1.token-material"
	)
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"branch": {Kind: definition.SubjectLiteral, Literal: subjectSecret},
	})
	pre := executionPreImage(
		kernel.ExecutionRunning, 5,
		actionPreImage(0, requestAction, definition.ActionKindRequest, kernel.ActionRunning, 2),
	)
	commit := flow.Commit{
		ExecutionID:              projectionExecution,
		ExpectedExecutionVersion: 5,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 requestAction,
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionFailed,
		NextCurrentAction:        intPointer(-1),
		Output: flow.OutputChange{
			Action: flow.OutputDiscard,
			Values: kernel.Outputs{"result": outputSecret},
		},
		InvalidateCredential: true,
		Subject:              &subject,
		Answer: &kernel.Answer{
			Decision: kernel.DecisionFail,
			Outputs:  kernel.Outputs{"result": outputSecret},
			Actor:    id.PrincipalID("principal-1"),
			At:       projectionAt,
		},
		Failure: &kernel.Failure{
			Kind:   kernel.FailureRuntime,
			Reason: "runtime",
			Detail: detailSecret,
		},
		CleanupUpdate: &flow.CleanupUpdate{
			State:       kernel.CleanupPending,
			WorkspaceID: id.WorkspaceID("workspace-1"),
			Attempts:    1,
			LastError:   detailSecret,
		},
		At: projectionAt,
	}
	got, err := event.ProjectLifecycle(pre, commit)
	if err != nil {
		t.Fatalf("ProjectLifecycle refused a consistent commit: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("projection yielded no events for a failing settlement")
	}
	forbidden := []string{outputSecret, tokenSecret, subjectSecret, detailSecret}
	for _, spec := range got {
		checkLifecycleSpec(t, spec, pre, commit)
		for _, secret := range forbidden {
			if bytes.Contains(spec.Data, []byte(secret)) {
				t.Fatalf("%q facts leak %q: %s", spec.Type, secret, spec.Data)
			}
		}
		for _, key := range []string{"outputs", "staged_outputs", "answer", "credential", "token", "secret", "env"} {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(spec.Data, &object); err != nil {
				continue
			}
			if _, present := object[key]; present {
				t.Fatalf("%q facts carry the forbidden %q field: %s", spec.Type, key, spec.Data)
			}
		}
	}
}

// TestProjectLifecycleRefusesInconsistentCommits pins the version guard and the
// pre-image/commit consistency refusals. A refused projection yields no event.
func TestProjectLifecycleRefusesInconsistentCommits(t *testing.T) {
	basePre := executionPreImage(
		kernel.ExecutionRunning, 7,
		actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 2),
	)
	baseCommit := flow.Commit{
		ExecutionID:              projectionExecution,
		ExpectedExecutionVersion: 7,
		NextExecutionState:       kernel.ExecutionSucceeded,
		ActionID:                 containerAction,
		ExpectedActionVersion:    2,
		NextActionState:          kernel.ActionSucceeded,
		NextCurrentAction:        intPointer(-1),
		Output:                   flow.OutputChange{Action: flow.OutputNone},
		At:                       projectionAt,
	}
	if _, err := event.ProjectLifecycle(basePre, baseCommit); err != nil {
		t.Fatalf("baseline commit was refused: %v", err)
	}

	pendingPre := executionPreImage(
		kernel.ExecutionRunning, 7,
		actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 2),
		actionPreImage(1, firstSuccessor, definition.ActionKindContainer, kernel.ActionPending, 1),
	)

	cases := []struct {
		name   string
		pre    event.ExecutionPreImage
		commit flow.Commit
	}{
		{
			name: "execution identity mismatch",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ExecutionID = id.ExecutionID("execution-2")
				return commit
			}(),
		},
		{
			name: "stale expected version",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ExpectedExecutionVersion = 6
				return commit
			}(),
		},
		{
			name: "future expected version",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ExpectedExecutionVersion = 8
				return commit
			}(),
		},
		{
			name: "unknown primary action",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ActionID = id.ActionID("action-missing")
				return commit
			}(),
		},
		{
			name: "primary action version mismatch",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ExpectedActionVersion = 9
				return commit
			}(),
		},
		{
			name: "unnamed action state change",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.ActionID = ""
				commit.ExpectedActionVersion = 0
				return commit
			}(),
		},
		{
			name: "unknown pending cancellation action",
			pre:  pendingPre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.PendingCancellations = []flow.ActionUpdate{
					{ActionID: id.ActionID("action-missing"), ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				}
				return commit
			}(),
		},
		{
			name: "pending cancellation version mismatch",
			pre:  pendingPre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.PendingCancellations = []flow.ActionUpdate{
					{ActionID: firstSuccessor, ExpectedVersion: 2, NextState: kernel.ActionCancelled},
				}
				return commit
			}(),
		},
		{
			name: "pending cancellation repeats the primary action",
			pre:  pendingPre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.PendingCancellations = []flow.ActionUpdate{
					{ActionID: containerAction, ExpectedVersion: 2, NextState: kernel.ActionCancelled},
				}
				return commit
			}(),
		},
		{
			name: "duplicate pending cancellation",
			pre:  pendingPre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.PendingCancellations = []flow.ActionUpdate{
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				}
				return commit
			}(),
		},
		{
			name: "unknown next execution state",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.NextExecutionState = kernel.ExecutionState("paused")
				return commit
			}(),
		},
		{
			name: "unknown next action state",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.NextActionState = kernel.ActionState("paused")
				return commit
			}(),
		},
		{
			name: "zero occurrence time",
			pre:  basePre,
			commit: func() flow.Commit {
				commit := baseCommit
				commit.At = time.Time{}
				return commit
			}(),
		},
		{
			name: "invalid pre-image scope",
			pre: func() event.ExecutionPreImage {
				pre := basePre
				pre.Scope = tenancy.Scope{}
				return pre
			}(),
			commit: baseCommit,
		},
		{
			name: "unknown pre-image execution state",
			pre: func() event.ExecutionPreImage {
				pre := basePre
				pre.State = kernel.ExecutionState("paused")
				return pre
			}(),
			commit: baseCommit,
		},
		{
			name: "pre-image action index mismatch",
			pre: func() event.ExecutionPreImage {
				pre := basePre
				pre.Actions = append([]event.ActionPreImage(nil), basePre.Actions...)
				pre.Actions[0].Index = 1
				return pre
			}(),
			commit: baseCommit,
		},
		{
			name: "terminal execution rewritten",
			pre: executionPreImage(
				kernel.ExecutionFailed, 7,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionFailed, 2),
			),
			commit: func() flow.Commit {
				commit := baseCommit
				commit.NextExecutionState = kernel.ExecutionRunning
				return commit
			}(),
		},
		{
			name: "terminal action rewritten",
			pre: executionPreImage(
				kernel.ExecutionRunning, 7,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionSucceeded, 2),
			),
			commit: func() flow.Commit {
				commit := baseCommit
				commit.NextActionState = kernel.ActionRunning
				return commit
			}(),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := event.ProjectLifecycle(testCase.pre, testCase.commit)
			if err == nil {
				t.Fatalf("inconsistent commit was accepted with events %s", describeSpecs(got))
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("refusal class = %v; want %q", class, fault.InvalidInput)
			}
			if len(got) != 0 {
				t.Fatalf("refused projection returned %d partial events %s", len(got), describeSpecs(got))
			}
		})
	}
}
