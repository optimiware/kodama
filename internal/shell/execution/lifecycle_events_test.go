package execution_test

// Emitter ownership for the two execution.Service paths that carry a Kernel
// flow.Commit:
//
//   - explicit operator cancel owns execution.cancelled and appends the
//     projected cleanup progress and every cancelled action.transition;
//   - output staging changes no lifecycle state, so the owner's action.transition
//     is the complete publication for that commit.
//
// The unit's own events are not altered and the appended projection is the pure
// event.ExcludeOwned(event.ProjectLifecycle(pre, commit), owned...) remainder
// built from the state the service already read.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// lifecycleSecretMarker is staged output content that must never reach a
// projected lifecycle fact.
const lifecycleSecretMarker = "lifecycle-secret-output-value"

// publishedIdentity is the (type, subject) publication identity.
func publishedIdentity(envelope event.Envelope) string {
	return string(envelope.Type) + "@" + envelope.SubjectID
}

// requirePublishedSequence asserts the exact ordered publication list, that
// every envelope is valid and that no (type, subject) is published twice.
func requirePublishedSequence(t *testing.T, events []event.Envelope, want ...string) {
	t.Helper()
	got := make([]string, len(events))
	seen := make(map[string]bool, len(events))
	for index, envelope := range events {
		identity := publishedIdentity(envelope)
		got[index] = identity
		if seen[identity] {
			t.Fatalf("event %q is published twice; sequence %v", identity, got)
		}
		seen[identity] = true
		if err := envelope.Validate(); err != nil {
			t.Fatalf("published event %q is not valid: %v", identity, err)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published sequence = %v; want %v", got, want)
	}
}

// lifecyclePreImageOf builds the version-guarded pre-image from an aggregate
// exactly as the service reads it before its guarded decision.
func lifecyclePreImageOf(scope tenancy.Scope, aggregate kernel.Execution) event.ExecutionPreImage {
	actions := make([]event.ActionPreImage, len(aggregate.Actions))
	for position, action := range aggregate.Actions {
		actions[position] = event.ActionPreImage{
			ActionID: action.ID,
			Index:    action.Index,
			Kind:     action.Kind,
			State:    action.State,
			Version:  action.Version,
		}
	}
	return event.ExecutionPreImage{
		Scope:       scope,
		ExecutionID: aggregate.ID,
		Version:     aggregate.Version,
		State:       aggregate.State,
		Actions:     actions,
	}
}

// requireProjectedRemainder asserts the events after the owner's own events are
// exactly the retained lifecycle projection for the unit's commit.
func requireProjectedRemainder(
	t *testing.T,
	events []event.Envelope,
	own int,
	pre event.ExecutionPreImage,
	commit flow.Commit,
	owned ...event.OwnedEvent,
) {
	t.Helper()
	projected, err := event.ProjectLifecycle(pre, commit)
	requireNoError(t, err, "ProjectLifecycle")
	remainder := event.ExcludeOwned(projected, owned...)
	if len(events) != own+len(remainder) {
		t.Fatalf("published events = %d; want %d owner + %d projected", len(events), own, len(remainder))
	}
	for index, spec := range remainder {
		want, err := event.NewEnvelope(spec.ID, spec.Source, spec.Scope, spec.At, event.EnvelopeInput{
			Type:           spec.Type,
			SubjectID:      spec.SubjectID,
			SubjectVersion: spec.SubjectVersion,
			Data:           spec.Data,
		})
		requireNoError(t, err, "NewEnvelope")
		if got := events[own+index]; !reflect.DeepEqual(got, want) {
			t.Fatalf("projected event %d = %+v; want %+v", own+index, got, want)
		}
	}
}

// requireFactsExclude asserts no published event fact carries any forbidden
// value (outputs, answers, subjects, credentials, free-form errors).
func requireFactsExclude(t *testing.T, events []event.Envelope, forbidden ...string) {
	t.Helper()
	for _, envelope := range events {
		data := string(envelope.Data)
		for _, value := range forbidden {
			if strings.Contains(data, value) {
				t.Fatalf("event %q facts %q contain forbidden value %q", publishedIdentity(envelope), data, value)
			}
		}
	}
}

func TestCancelUnitAppendsProjectedLifecycleEvents(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	aggregate.Actions = append(aggregate.Actions, containerAction("act-1", "test", 1, time.Hour, "result"))
	aggregate.Actions[0].StagedOutputs = kernel.Outputs{"candidate": lifecycleSecretMarker}
	aggregate.Actions[0].StagedOutputsSet = true
	fixture.store.Seed(scope, aggregate)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator)

	_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ExpectedExecutionVersion: aggregate.Version,
		CallerKey:                "cancel-key-1",
		Content:                  receiptContent(`{"cancel":1}`),
	})
	requireNoError(t, err, "Cancel")
	if fixture.transitions.callCount() != 1 {
		t.Fatalf("transition port calls = %d; want one", fixture.transitions.callCount())
	}
	unit := fixture.transitions.captured()[0]

	// The owner's execution.cancelled stays first and unchanged; the projected
	// cleanup progress and both cancelled action transitions follow in total
	// order.
	requirePublishedSequence(t, unit.Events,
		"execution.cancelled@exec-1",
		"execution.cleanup@exec-1",
		"action.transition@act-0",
		"action.transition@act-1",
	)
	if unit.Events[0].Source.PrincipalID != actor.ID {
		t.Fatalf("owner event source = %q; want the caller %q", unit.Events[0].Source.PrincipalID, actor.ID)
	}
	for _, envelope := range unit.Events[1:] {
		if envelope.Source != event.SystemSource {
			t.Fatalf("projected event %q source = %+v; want the system source", publishedIdentity(envelope), envelope.Source)
		}
		if !envelope.At.Equal(testNow) || envelope.Scope != scope {
			t.Fatalf("projected event %q time/scope = %v/%+v; want %v/%+v", publishedIdentity(envelope), envelope.At, envelope.Scope, testNow, scope)
		}
	}
	if unit.Events[1].SubjectVersion != aggregate.Version+1 {
		t.Fatalf("cleanup subject version = %d; want %d", unit.Events[1].SubjectVersion, aggregate.Version+1)
	}
	if unit.Events[2].SubjectVersion != aggregate.Actions[0].Version+1 {
		t.Fatalf("primary action subject version = %d; want %d", unit.Events[2].SubjectVersion, aggregate.Actions[0].Version+1)
	}
	if unit.Events[3].SubjectVersion != aggregate.Actions[1].Version+1 {
		t.Fatalf("pending action subject version = %d; want %d", unit.Events[3].SubjectVersion, aggregate.Actions[1].Version+1)
	}
	requireProjectedRemainder(t, unit.Events, 1, lifecyclePreImageOf(scope, aggregate), unit.Commit,
		event.OwnedEvent{Type: event.TypeExecutionCancelled})
	requireFactsExclude(t, unit.Events, lifecycleSecretMarker)
}

func TestStageOutputsUnitAppendsNoLifecycleProjection(t *testing.T) {
	fixture := newTestService(t)
	scope := projectScope("tenant-1", "project-1")
	aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
	fixture.store.Seed(scope, aggregate)
	actor := executionActor("tenant-1", "project-1", "principal-exec", aggregate.ID)

	_, err := fixture.service.StageOutputs(context.Background(), execution.OutputsCommand{
		Actor:                    actor,
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[0].ID,
		ExpectedExecutionVersion: aggregate.Version,
		Outputs:                  kernel.Outputs{"candidate": lifecycleSecretMarker},
		CallerKey:                "outputs-key-1",
	})
	requireNoError(t, err, "StageOutputs")
	if fixture.transitions.callCount() != 1 {
		t.Fatalf("transition port calls = %d; want one", fixture.transitions.callCount())
	}
	unit := fixture.transitions.captured()[0]

	// Output staging changes no durable lifecycle state, so the owner's
	// action.transition is the complete publication for this commit.
	requirePublishedSequence(t, unit.Events, "action.transition@exec-1")
	if unit.Events[0].Source.PrincipalID != actor.ID {
		t.Fatalf("owner event source = %q; want the caller %q", unit.Events[0].Source.PrincipalID, actor.ID)
	}
	projected, err := event.ProjectLifecycle(lifecyclePreImageOf(scope, aggregate), unit.Commit)
	requireNoError(t, err, "ProjectLifecycle")
	if len(projected) != 0 {
		t.Fatalf("output staging projected %d lifecycle events; want none", len(projected))
	}
	requireFactsExclude(t, unit.Events, lifecycleSecretMarker)
}

func TestCancelProjectionIdentitiesAreDeterministicAcrossReplays(t *testing.T) {
	run := func() []event.Envelope {
		fixture := newTestService(t)
		scope := projectScope("tenant-1", "project-1")
		aggregate := runningContainerExecution(containerAction("act-0", "build", 0, time.Hour, "candidate"))
		aggregate.Actions = append(aggregate.Actions, containerAction("act-1", "test", 1, time.Hour, "result"))
		fixture.store.Seed(scope, aggregate)
		_, err := fixture.service.Cancel(context.Background(), execution.CancelCommand{
			Actor:                    humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleOperator),
			Scope:                    scope,
			ExecutionID:              aggregate.ID,
			ExpectedExecutionVersion: aggregate.Version,
			CallerKey:                "cancel-key-1",
			Content:                  receiptContent(`{}`),
		})
		requireNoError(t, err, "Cancel")
		return fixture.transitions.captured()[0].Events
	}
	first := run()
	second := run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replayed cancel events = %+v; want the deterministic %+v", second, first)
	}
	for _, envelope := range first[1:] {
		if !strings.HasPrefix(string(envelope.ID), "event-") {
			t.Fatalf("projected event id %q is not deterministic-shaped", envelope.ID)
		}
	}
}
