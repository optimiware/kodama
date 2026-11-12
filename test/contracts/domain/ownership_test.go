package domaincontract

// Emitter ownership across every flow.Commit-carrying Shell path.
//
// A Shell unit that commits a Kernel flow.Commit appends the projection minus
// the event classes it already emits itself:
//
//	event.ExcludeOwned(event.ProjectLifecycle(pre, commit), owned...)
//
// The published order is the unit's own events in their declared order, then
// the retained projection in its total order. Each transition is published
// exactly once per commit, and no class is dropped on a Shell path.

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/request"
)

// ownershipCase is one Shell path: its own event classes, the Kernel commit it
// carries, the classes it excludes and the exact published order.
type ownershipCase struct {
	name   string
	own    []publishedEvent
	pre    event.ExecutionPreImage
	commit flow.Commit
	owned  []event.OwnedEvent
	want   []publishedEvent
}

func ownershipCases() []ownershipCase {
	return []ownershipCase{
		{
			// A passing answer settles the request action and, when it is last,
			// the execution. The request service owns request.resolved; it must
			// append the projected execution and action transitions.
			name: "answer pass appends execution.succeeded and the request action.transition",
			own:  []publishedEvent{{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)}},
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
			owned: []event.OwnedEvent{{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)}},
			want: []publishedEvent{
				{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)},
				{Type: event.TypeExecutionSucceeded, SubjectID: string(projectionExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(requestAction)},
			},
		},
		{
			// A failing answer settles the action and execution failed and
			// cancels every still-pending successor. Nothing may be dropped.
			name: "answer fail appends execution.failed and every action.transition",
			own:  []publishedEvent{{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)}},
			pre: executionPreImage(
				kernel.ExecutionRunning, 8,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionSucceeded, 2),
				actionPreImage(1, requestAction, definition.ActionKindRequest, kernel.ActionRunning, 3),
				actionPreImage(2, firstSuccessor, definition.ActionKindContainer, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 8,
				NextExecutionState:       kernel.ExecutionFailed,
				ActionID:                 requestAction,
				ExpectedActionVersion:    3,
				NextActionState:          kernel.ActionFailed,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction: intPointer(-1),
				Output:            flow.OutputChange{Action: flow.OutputDiscard},
				Answer: &kernel.Answer{
					Decision: kernel.DecisionFail,
					Actor:    id.PrincipalID("principal-1"),
					At:       projectionAt,
				},
				Failure: &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "answer_rejected"},
				At:      projectionAt,
			},
			owned: []event.OwnedEvent{{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)}},
			want: []publishedEvent{
				{Type: event.TypeRequestResolved, SubjectID: string(projectionExecution)},
				{Type: event.TypeExecutionFailed, SubjectID: string(projectionExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(requestAction)},
				{Type: event.TypeActionTransition, SubjectID: string(firstSuccessor)},
			},
		},
		{
			// Explicit cancel owns execution.cancelled; it must append the
			// cleanup progress and every cancelled action transition.
			name: "operator cancel appends cleanup and every action.transition",
			own:  []publishedEvent{{Type: event.TypeExecutionCancelled, SubjectID: string(projectionExecution)}},
			pre: executionPreImage(
				kernel.ExecutionRunning, 5,
				actionPreImage(0, containerAction, definition.ActionKindContainer, kernel.ActionRunning, 4),
				actionPreImage(1, firstSuccessor, definition.ActionKindContainer, kernel.ActionPending, 1),
			),
			commit: flow.Commit{
				ExecutionID:              projectionExecution,
				ExpectedExecutionVersion: 5,
				NextExecutionState:       kernel.ExecutionCancelled,
				ActionID:                 containerAction,
				ExpectedActionVersion:    4,
				NextActionState:          kernel.ActionCancelled,
				PendingCancellations: []flow.ActionUpdate{
					{ActionID: firstSuccessor, ExpectedVersion: 1, NextState: kernel.ActionCancelled},
				},
				NextCurrentAction:    intPointer(-1),
				Output:               flow.OutputChange{Action: flow.OutputDiscard},
				InvalidateCredential: true,
				CleanupUpdate: &flow.CleanupUpdate{
					State:       kernel.CleanupPending,
					WorkspaceID: id.WorkspaceID("workspace-1"),
				},
				At: projectionAt,
			},
			owned: []event.OwnedEvent{{Type: event.TypeExecutionCancelled}},
			want: []publishedEvent{
				{Type: event.TypeExecutionCancelled, SubjectID: string(projectionExecution)},
				{Type: event.TypeCleanup, SubjectID: string(projectionExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(containerAction)},
				{Type: event.TypeActionTransition, SubjectID: string(firstSuccessor)},
			},
		},
		{
			// The request activation path owns action.requested (whose subject
			// is the execution id), so it excludes the projected
			// action.requested by type and appends only action.transition.
			name: "request activation appends action.transition and excludes action.requested by type",
			own:  []publishedEvent{{Type: event.TypeActionRequested, SubjectID: string(projectionExecution)}},
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
			owned: []event.OwnedEvent{{Type: event.TypeActionRequested}},
			want: []publishedEvent{
				{Type: event.TypeActionRequested, SubjectID: string(projectionExecution)},
				{Type: event.TypeActionTransition, SubjectID: string(requestAction)},
			},
		},
	}
}

// TestEmitterOwnershipPublishesEachTransitionOnce pins three settlement paths
// (answer pass, answer fail, operator cancel) plus request activation: the
// union of the owner's own event and the projection minus the owned classes,
// de-duplicated by (type, subject), publishes every transition exactly once in
// the fixed order (own events, then the retained projection).
func TestEmitterOwnershipPublishesEachTransitionOnce(t *testing.T) {
	for _, testCase := range ownershipCases() {
		t.Run(testCase.name, func(t *testing.T) {
			projected, err := event.ProjectLifecycle(testCase.pre, testCase.commit)
			if err != nil {
				t.Fatalf("ProjectLifecycle refused a consistent commit: %v", err)
			}
			before := append([]event.LifecycleEvent(nil), projected...)
			appended := event.ExcludeOwned(projected, testCase.owned...)
			if !reflect.DeepEqual(projected, before) {
				t.Fatalf("ExcludeOwned mutated its input: got %s; want %s", describeSpecs(projected), describeSpecs(before))
			}
			again := event.ExcludeOwned(projected, testCase.owned...)
			if !reflect.DeepEqual(appended, again) {
				t.Fatalf("ExcludeOwned is not deterministic:\n first: %s\nsecond: %s", describeSpecs(appended), describeSpecs(again))
			}

			published := make([]publishedEvent, 0, len(testCase.own)+len(appended))
			published = append(published, testCase.own...)
			for _, spec := range appended {
				published = append(published, publishedEvent{Type: spec.Type, SubjectID: spec.SubjectID})
			}
			if !reflect.DeepEqual(published, testCase.want) {
				t.Fatalf("published order = %s; want %s", describePublished(published), describePublished(testCase.want))
			}

			counts := make(map[publishedEvent]int, len(published))
			for _, spec := range published {
				counts[spec]++
			}
			for spec, count := range counts {
				if count != 1 {
					t.Fatalf("%q@%q is published %d times; want exactly once", spec.Type, spec.SubjectID, count)
				}
			}
			for _, spec := range appended {
				for _, own := range testCase.own {
					if spec.Type == own.Type && spec.SubjectID == own.SubjectID {
						t.Fatalf("appended projection repeats the owner's own event %q@%q", spec.Type, spec.SubjectID)
					}
				}
			}
		})
	}
}

// TestExcludeOwnedMatchesAndDeduplicates pins the helper contract: exact
// subject matching, wildcard-by-type matching for an empty subject, duplicate
// removal by (type, subject) and a pure result that shares no mutable state.
func TestExcludeOwnedMatchesAndDeduplicates(t *testing.T) {
	specs := []event.LifecycleEvent{
		{Type: event.TypeExecutionSucceeded, SubjectID: "execution-1", Data: []byte("{}")},
		{Type: event.TypeExecutionSucceeded, SubjectID: "execution-1", Data: []byte("{}")},
		{Type: event.TypeActionTransition, SubjectID: "action-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-2"},
		{Type: event.TypeActionRequested, SubjectID: "action-1"},
		{Type: event.TypeActionRequested, SubjectID: "action-2"},
	}

	exact := event.ExcludeOwned(specs, event.OwnedEvent{Type: event.TypeActionTransition, SubjectID: "action-1"})
	wantExact := []publishedEvent{
		{Type: event.TypeExecutionSucceeded, SubjectID: "execution-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-2"},
		{Type: event.TypeActionRequested, SubjectID: "action-1"},
		{Type: event.TypeActionRequested, SubjectID: "action-2"},
	}
	if got := asPublished(exact); !reflect.DeepEqual(got, wantExact) {
		t.Fatalf("exact-subject exclusion = %s; want %s", describePublished(got), describePublished(wantExact))
	}

	wildcard := event.ExcludeOwned(specs, event.OwnedEvent{Type: event.TypeActionRequested})
	wantWildcard := []publishedEvent{
		{Type: event.TypeExecutionSucceeded, SubjectID: "execution-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-2"},
	}
	if got := asPublished(wildcard); !reflect.DeepEqual(got, wantWildcard) {
		t.Fatalf("wildcard exclusion = %s; want %s", describePublished(got), describePublished(wantWildcard))
	}

	deduped := event.ExcludeOwned(specs)
	wantDeduped := []publishedEvent{
		{Type: event.TypeExecutionSucceeded, SubjectID: "execution-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-1"},
		{Type: event.TypeActionTransition, SubjectID: "action-2"},
		{Type: event.TypeActionRequested, SubjectID: "action-1"},
		{Type: event.TypeActionRequested, SubjectID: "action-2"},
	}
	if got := asPublished(deduped); !reflect.DeepEqual(got, wantDeduped) {
		t.Fatalf("duplicate removal = %s; want %s", describePublished(got), describePublished(wantDeduped))
	}

	// The returned facts are owned by the caller: damaging them must not change
	// the input or a later call.
	exact[0].Data[0] = 'x'
	exact[0].Type = event.TypeCustomObservation
	again := event.ExcludeOwned(specs, event.OwnedEvent{Type: event.TypeActionTransition, SubjectID: "action-1"})
	if string(again[0].Data) != "{}" {
		t.Fatalf("ExcludeOwned shared fact bytes with its input: %q", again[0].Data)
	}
	if specs[0].Type != event.TypeExecutionSucceeded {
		t.Fatalf("ExcludeOwned mutated the input slice: %q", specs[0].Type)
	}

	if got := event.ExcludeOwned(nil); got != nil {
		t.Fatalf("ExcludeOwned(nil) = %#v; want nil", got)
	}
}

// TestFlowCommitUnitsCarryTheReduction pins that every flow.Commit-carrying
// Shell unit carries the Kernel reduction it projects.
func TestFlowCommitUnitsCarryTheReduction(t *testing.T) {
	units := []struct {
		name  string
		value any
	}{
		{name: "execution.TransitionUnit", value: execution.TransitionUnit{}},
		{name: "request.ActivationUnit", value: request.ActivationUnit{}},
		{name: "request.ResolveUnit", value: request.ResolveUnit{}},
	}
	commitType := reflect.TypeOf(flow.Commit{})
	for _, unit := range units {
		field, ok := reflect.TypeOf(unit.value).FieldByName("Commit")
		if !ok {
			t.Errorf("%s does not declare Commit flow.Commit", unit.name)
			continue
		}
		if field.Type != commitType {
			t.Errorf("%s.Commit type = %v; want %v", unit.name, field.Type, commitType)
		}
	}
}

func asPublished(events []event.LifecycleEvent) []publishedEvent {
	published := make([]publishedEvent, 0, len(events))
	for _, spec := range events {
		published = append(published, publishedEvent{Type: spec.Type, SubjectID: spec.SubjectID})
	}
	return published
}
