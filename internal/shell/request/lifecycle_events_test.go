package request_test

// Emitter ownership for the two request.Service paths that carry a Kernel
// flow.Commit:
//
//   - activation owns action.requested (subject: the execution) and appends the
//     projected pending->running action.transition;
//   - first settlement owns request.resolved and appends the projected
//     execution/action transitions of the answer reduction, while a
//     receipt-first replay carries a zero commit and appends nothing.
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

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// lifecycleSecretMarker is answer/subject content that must never reach a
// projected lifecycle fact.
const lifecycleSecretMarker = "lifecycle-secret-answer-value"

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
// value (answers, subjects, credentials, free-form errors).
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

func TestActivateUnitCarriesCommitAndAppendsProjectedTransition(t *testing.T) {
	fixture, scope, aggregate, subject := activationFixture(t, kernel.ConfirmedStop("termination observed"))
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")

	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject.subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{"activate":1}`),
	})
	requireNoError(t, err, "Activate")
	if fixture.activations.callCount() != 1 {
		t.Fatalf("activation port calls = %d; want one", fixture.activations.callCount())
	}
	unit := fixture.activations.captured()[0]

	// The activation unit carries the Kernel reduction it projects.
	_, want, err := flow.Reduce(aggregate, flow.ActivateFact{
		ExecutionID: aggregate.ID,
		ActionID:    aggregate.Actions[1].ID,
		At:          testNow,
	})
	requireNoError(t, err, "independent activation reduction")
	if want == nil {
		t.Fatal("independent activation reduction produced no commit")
	}
	if !reflect.DeepEqual(unit.Commit, *want) {
		t.Fatalf("activation commit = %+v; want the reducer's %+v", unit.Commit, *want)
	}

	// The owner's action.requested stays first and unchanged; the projected
	// pending->running action.transition follows.
	requirePublishedSequence(t, unit.Events,
		"action.requested@exec-1",
		"action.transition@act-1",
	)
	if unit.Events[0].Source.PrincipalID != testEngineActor {
		t.Fatalf("owner event source = %q; want the engine caller %q", unit.Events[0].Source.PrincipalID, testEngineActor)
	}
	projected := unit.Events[1]
	if projected.Source != event.SystemSource {
		t.Fatalf("projected event source = %+v; want the system source", projected.Source)
	}
	if !projected.At.Equal(testNow) || projected.Scope != scope {
		t.Fatalf("projected event time/scope = %v/%+v; want %v/%+v", projected.At, projected.Scope, testNow, scope)
	}
	if projected.SubjectVersion != aggregate.Actions[1].Version+1 {
		t.Fatalf("projected action subject version = %d; want %d", projected.SubjectVersion, aggregate.Actions[1].Version+1)
	}
	requireProjectedRemainder(t, unit.Events, 1, lifecyclePreImageOf(scope, aggregate), unit.Commit,
		event.OwnedEvent{Type: event.TypeActionRequested})
	requireFactsExclude(t, unit.Events, lifecycleSecretMarker, "git.ref.update")
}

func TestResolvePassAppendsSucceededExecutionAndActionTransition(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	response := kernel.Outputs{"approved": lifecycleSecretMarker}

	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, response))
	requireNoError(t, err, "Resolve pass")
	unit := fixture.resolutions.captured()[0]

	// A passing answer on the last action settles both the action and the
	// execution; the owner's request.resolved stays first.
	requirePublishedSequence(t, unit.Events,
		"request.resolved@exec-1",
		"execution.succeeded@exec-1",
		"execution.cleanup@exec-1",
		"action.transition@act-0",
	)
	for _, envelope := range unit.Events[1:] {
		if envelope.Source != event.SystemSource {
			t.Fatalf("projected event %q source = %+v; want the system source", publishedIdentity(envelope), envelope.Source)
		}
		if !envelope.At.Equal(fixture.clock.now) || envelope.Scope != scope {
			t.Fatalf("projected event %q time/scope = %v/%+v", publishedIdentity(envelope), envelope.At, envelope.Scope)
		}
	}
	if unit.Events[1].SubjectVersion != aggregate.Version+1 {
		t.Fatalf("execution subject version = %d; want %d", unit.Events[1].SubjectVersion, aggregate.Version+1)
	}
	if unit.Events[3].SubjectVersion != aggregate.Actions[0].Version+1 {
		t.Fatalf("action subject version = %d; want %d", unit.Events[3].SubjectVersion, aggregate.Actions[0].Version+1)
	}
	requireProjectedRemainder(t, unit.Events, 1, lifecyclePreImageOf(scope, aggregate), unit.Commit,
		event.OwnedEvent{Type: event.TypeRequestResolved, SubjectID: string(aggregate.ID)})
	requireFactsExclude(t, unit.Events, lifecycleSecretMarker)
}

func TestResolvePassKeepsExecutionRunningWhenActionsRemain(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(
		requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
		containerAction("act-1", "deploy", 1, time.Hour, "result"),
	)
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)

	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
	requireNoError(t, err, "Resolve pass with successor")
	unit := fixture.resolutions.captured()[0]

	// A non-final pass settles only the action: no execution.succeeded.
	requirePublishedSequence(t, unit.Events,
		"request.resolved@exec-1",
		"action.transition@act-0",
	)
	requireProjectedRemainder(t, unit.Events, 1, lifecyclePreImageOf(scope, aggregate), unit.Commit,
		event.OwnedEvent{Type: event.TypeRequestResolved, SubjectID: string(aggregate.ID)})
}

func TestResolveFailAppendsFailedExecutionAndEveryActionTransition(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	aggregate := runningRequestExecution(
		requestAction("act-0", "review", 0, 2*time.Hour, "role/reviewer", subject, "approved"),
		containerAction("act-1", "deploy", 1, time.Hour, "result"),
	)
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)

	_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionFail, kernel.Outputs{"approved": lifecycleSecretMarker}))
	requireNoError(t, err, "Resolve fail")
	unit := fixture.resolutions.captured()[0]

	// A failing answer settles the execution failed and cancels the pending
	// successor; nothing is dropped and nothing repeats.
	requirePublishedSequence(t, unit.Events,
		"request.resolved@exec-1",
		"execution.failed@exec-1",
		"execution.cleanup@exec-1",
		"action.transition@act-0",
		"action.transition@act-1",
	)
	requireProjectedRemainder(t, unit.Events, 1, lifecyclePreImageOf(scope, aggregate), unit.Commit,
		event.OwnedEvent{Type: event.TypeRequestResolved, SubjectID: string(aggregate.ID)})
	requireFactsExclude(t, unit.Events, lifecycleSecretMarker, "request action was answered with a failing decision")
}

func TestResolveReplayUnitAppendsNoLifecycleEvents(t *testing.T) {
	fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
	fixture.clock.now = testNow.Add(time.Minute)
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
	command := resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"})

	_, err := fixture.service.Resolve(context.Background(), command)
	requireNoError(t, err, "first Resolve")
	_, err = fixture.service.Resolve(context.Background(), command)
	requireNoError(t, err, "replayed Resolve")
	units := fixture.resolutions.captured()
	if len(units) != 2 {
		t.Fatalf("resolve port calls = %d; want two", len(units))
	}
	if !reflect.DeepEqual(units[1].Commit, flow.Commit{}) {
		t.Fatalf("replay unit carried a commit: %+v", units[1].Commit)
	}
	// A receipt-first replay applies nothing, so only the owner's own event is
	// published again.
	requirePublishedSequence(t, units[1].Events, "request.resolved@exec-1")
}

func TestActivateFailsClosedWhenProjectionRefusesInconsistentPreImage(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	subject := literalSubject(map[string]string{"operation": "git.ref.update"})
	container := containerAction("act-0", "build", 0, time.Hour, "candidate")
	container.State = kernel.ActionSucceeded
	container.Version = 2
	container.Stop = kernel.ConfirmedStop("termination observed")
	review := requestAction("act-1", "review", 1, 2*time.Hour, "role/reviewer", subject, "approved")
	// An out-of-order action index is an inconsistent pre-image the projection
	// must refuse; the service must fail closed instead of committing a unit
	// whose events it could not build.
	review.Index = 9
	aggregate := baseExecution(container, review)
	aggregate.State = kernel.ExecutionRunning
	aggregate.CurrentAction = -1
	fixture := newTestService(t)
	fixture.store.Seed(scope, aggregate)
	assignment, err := request.ParseAssignment("role/reviewer")
	requireNoError(t, err, "ParseAssignment")

	_, err = fixture.service.Activate(context.Background(), request.ActivateCommand{
		Caller:                   testEngineCaller(t),
		Scope:                    scope,
		ExecutionID:              aggregate.ID,
		ActionID:                 aggregate.Actions[1].ID,
		ExpectedExecutionVersion: aggregate.Version,
		ExpectedActionVersion:    aggregate.Actions[1].Version,
		Subject:                  &subject,
		Assignment:               assignment,
		Timeout:                  2 * time.Hour,
		CallerKey:                "activate-key-1",
		Content:                  receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if fixture.activations.callCount() != 0 {
		t.Fatalf("activation port calls = %d; want zero on a projection refusal", fixture.activations.callCount())
	}
	if fixture.store.commitCount() != 0 {
		t.Fatalf("commits = %d; want zero on a projection refusal", fixture.store.commitCount())
	}
}

func TestResolveProjectionIdentitiesAreDeterministicAcrossReplays(t *testing.T) {
	run := func() []event.Envelope {
		fixture, scope, aggregate, _ := newResolveFixture(t, "role/reviewer", "approved")
		fixture.clock.now = testNow.Add(time.Minute)
		actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleReviewer)
		_, err := fixture.service.Resolve(context.Background(), resolveCommand(scope, aggregate, 0, actor, request.DecisionPass, kernel.Outputs{"approved": "yes"}))
		requireNoError(t, err, "Resolve")
		return fixture.resolutions.captured()[0].Events[1:]
	}
	first := run()
	second := run()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replayed resolve projection = %+v; want the deterministic %+v", second, first)
	}
	for _, envelope := range first {
		if !strings.HasPrefix(string(envelope.ID), "event-") {
			t.Fatalf("projected event id %q is not deterministic-shaped", envelope.ID)
		}
	}
}
