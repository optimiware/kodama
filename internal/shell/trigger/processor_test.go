package trigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// pushFixture is one processor wired to deterministic fakes. Every test starts
// from an isolated fixture so no durable state leaks between cases.
type pushFixture struct {
	events     *fakeEvents
	candidates *fakeCandidates
	selections *fakeSelections
	admission  *fakeAdmission
	start      *fakeStart
	completion *fakeCompletion
	processor  *trigger.Processor
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	fixture := &pushFixture{
		events:     &fakeEvents{},
		candidates: &fakeCandidates{},
		selections: newFakeSelections(),
		admission:  &fakeAdmission{},
		completion: &fakeCompletion{},
	}
	fixture.start = newFakeStart(fixture.selections)
	fixture.processor = newProcessor(t, fixture.events, fixture.candidates, fixture.selections, fixture.admission, fixture.start, fixture.completion)
	return fixture
}

func (fixture *pushFixture) process(t *testing.T) trigger.ProcessResult {
	t.Helper()
	result, err := fixture.processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return result
}

func hasEvent(ids []id.EventID, want id.EventID) bool {
	return slices.Contains(ids, want)
}

func hasExecution(ids []id.ExecutionID, want id.ExecutionID) bool {
	return slices.Contains(ids, want)
}

// assertRefusalPayload checks the published pipeline.start_refused data is
// exactly the stable reason class plus the pipeline/event selection identity:
// no error message, credential or free-form field can be present.
func assertRefusalPayload(t *testing.T, refusal event.Envelope, wantReason string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(refusal.Data, &fields); err != nil {
		t.Fatalf("refusal data is not JSON: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("refusal data fields = %v; want exactly reason, pipeline_id and event_id", fields)
	}
	for _, key := range []string{"reason", "pipeline_id", "event_id"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("refusal data is missing %q: %s", key, refusal.Data)
		}
	}
	var payload struct {
		Reason     string `json:"reason"`
		PipelineID string `json:"pipeline_id"`
		EventID    string `json:"event_id"`
	}
	if err := json.Unmarshal(refusal.Data, &payload); err != nil {
		t.Fatalf("refusal data does not decode: %v", err)
	}
	if payload.Reason != wantReason {
		t.Errorf("refusal reason = %q; want %q", payload.Reason, wantReason)
	}
	if payload.PipelineID != "pipeline-a" || payload.EventID != "event-1" {
		t.Errorf("refusal selection identity = %q/%q; want pipeline-a/event-1", payload.PipelineID, payload.EventID)
	}
}

func TestNewProcessorRequiresEveryDependency(t *testing.T) {
	build := func() trigger.ProcessorConfig {
		selections := newFakeSelections()
		return trigger.ProcessorConfig{
			Events:         &fakeEvents{},
			Candidates:     &fakeCandidates{},
			Selections:     selections,
			Select:         selections,
			Admission:      &fakeAdmission{},
			Start:          newFakeStart(selections),
			Completion:     &fakeCompletion{},
			Clock:          fakeClock{},
			IDs:            &fakeIDs{},
			DefaultTimeout: 5 * time.Minute,
		}
	}
	if _, err := trigger.NewProcessor(build()); err != nil {
		t.Fatalf("NewProcessor with every dependency: %v", err)
	}

	cases := []struct {
		name  string
		clear func(*trigger.ProcessorConfig)
	}{
		{name: "events", clear: func(config *trigger.ProcessorConfig) { config.Events = nil }},
		{name: "candidates", clear: func(config *trigger.ProcessorConfig) { config.Candidates = nil }},
		{name: "selections", clear: func(config *trigger.ProcessorConfig) { config.Selections = nil }},
		{name: "select", clear: func(config *trigger.ProcessorConfig) { config.Select = nil }},
		{name: "admission", clear: func(config *trigger.ProcessorConfig) { config.Admission = nil }},
		{name: "start", clear: func(config *trigger.ProcessorConfig) { config.Start = nil }},
		{name: "completion", clear: func(config *trigger.ProcessorConfig) { config.Completion = nil }},
		{name: "clock", clear: func(config *trigger.ProcessorConfig) { config.Clock = nil }},
		{name: "ids", clear: func(config *trigger.ProcessorConfig) { config.IDs = nil }},
		{name: "zero default timeout", clear: func(config *trigger.ProcessorConfig) { config.DefaultTimeout = 0 }},
		{name: "negative default timeout", clear: func(config *trigger.ProcessorConfig) { config.DefaultTimeout = -time.Minute }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := build()
			testCase.clear(&config)
			if _, err := trigger.NewProcessor(config); err == nil {
				t.Fatalf("NewProcessor accepted a nil %s dependency", testCase.name)
			}
		})
	}
}

// TestEventTableAlonePersistsStartsAndRefusals is the NATS-absent case: the
// processor is configured with the committed event table only and still
// persists a start and a permanent refusal in one pass.
func TestEventTableAlonePersistsStartsAndRefusals(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(
		testCandidate(
			testDefinition("pipeline-start", pushYAML("repo-1", "svc-start", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-start", []string{"refs/heads/main"}),
			testBinding(scope, "repo-1", "git-svc", 1),
		),
		testCandidate(
			testDefinition("pipeline-refuse", pushYAML("repo-1", "svc-refuse", []string{"refs/heads/release"}, []string{"ref"}, ""), 1, "repo-1", "svc-refuse", []string{"refs/heads/release"}),
			testBinding(scope, "repo-1", "git-svc", 1),
		),
	)
	fixture.events.setRows(
		pushEvent(1, "event-main", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"),
		pushEvent(2, "event-release", "repo-1", "git-svc", "refs/heads/release", "base-1", "cand-2"),
	)
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		if unit.PipelineID == pipeline.PipelineID("pipeline-refuse") {
			return fixture.start.refuse(unit), nil
		}
		return fixture.start.defaultStart(unit)
	})

	result := fixture.process(t)

	if len(result.Started) != 1 || result.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("started = %v; want the single queued execution exec-1", result.Started)
	}
	if !hasEvent(result.Refused, id.EventID("event-release")) {
		t.Fatalf("refused = %v; want the disabled-service event recorded", result.Refused)
	}
	if !fixture.completion.has(1) || !fixture.completion.has(2) {
		t.Fatalf("completed = %v; want both durable outcomes advanced", fixture.completion.sequences())
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; want exactly one", fixture.start.executionCount())
	}
	for _, cursor := range fixture.events.cursors() {
		if cursor != 0 {
			t.Fatalf("Pending cursor = %d; the processor must rescan from the zero sequence every pass", cursor)
		}
	}
}

// TestEditReplayUsesFirstSelection edits the YAML bytes, the automatic-start
// service, the declared inputs and the binding after the first selection was
// committed. The replay must use the persisted first selection verbatim and
// must not create a second execution.
func TestEditReplayUsesFirstSelection(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	firstYAML := pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"repository", "ref", "commit", "previous_commit"}, "")
	firstBinding := testBinding(scope, "repo-1", "git-svc", 1)
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", firstYAML, 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		firstBinding,
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	first := fixture.process(t)
	if !hasExecution(first.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; want the first selection to queue exec-1", first.Started)
	}
	if fixture.selections.selectCount() == 0 {
		t.Fatal("the first pass did not persist a first selection")
	}
	original := fixture.selections.selectCall(0)
	firstResult, ok := fixture.selections.get(keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1")))
	if !ok {
		t.Fatal("first selection result was not persisted")
	}

	editedYAML := pushYAML("repo-1", "svc-b", []string{"refs/heads/main"}, []string{"ref"}, "edited")
	editedBinding := testBinding(scope, "repo-1", "git-svc", 2)
	editedBinding.TrustedOrigins = []string{"https://edited.example.com"}
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", editedYAML, 2, "repo-1", "svc-b", []string{"refs/heads/main"}),
		editedBinding,
	))

	second := fixture.process(t)

	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; an edit must never create a second execution", fixture.start.executionCount())
	}
	if ids := fixture.start.executionIDs(); len(ids) != 1 || ids[0] != id.ExecutionID("exec-1") {
		t.Fatalf("execution ids = %v; want the preserved exec-1", ids)
	}
	for index := 0; index < fixture.selections.selectCount(); index++ {
		unit := fixture.selections.selectCall(index)
		if string(unit.Selection.YAML) != firstYAML {
			t.Fatalf("select %d yaml = %q; an edit must never rebuild the first selection", index, unit.Selection.YAML)
		}
		if unit.Selection.ServiceID != original.Selection.ServiceID {
			t.Fatalf("select %d service = %q; want the first-selected service %q", index, unit.Selection.ServiceID, original.Selection.ServiceID)
		}
	}
	for index := 0; index < fixture.admission.callCount(); index++ {
		request := fixture.admission.call(index)
		if string(request.Definition.YAML) != firstYAML {
			t.Fatalf("admission %d yaml = %q; admission must use the persisted exact bytes", index, request.Definition.YAML)
		}
		if !reflect.DeepEqual(request.Inputs, original.Selection.Inputs) {
			t.Fatalf("admission %d inputs = %v; want the first-selection inputs %v", index, request.Inputs, original.Selection.Inputs)
		}
	}
	for index := 0; index < fixture.start.callCount(); index++ {
		unit := fixture.start.call(index)
		if !reflect.DeepEqual(unit.Selection, original.Selection) {
			t.Fatalf("start %d selection = %+v; want the first selection verbatim", index, unit.Selection)
		}
	}
	if !reflect.DeepEqual(firstResult.Selection, original.Selection) {
		t.Fatalf("stored selection changed to %+v; want the original %+v", firstResult.Selection, original.Selection)
	}
	if len(second.Refused) != 0 {
		t.Fatalf("refused = %v; an edit replay must not become a refusal", second.Refused)
	}
}

// TestPermanentRefusalIsNeverReplacedByALaterExecution keeps a durable refusal
// terminal even after the refusing dependency would succeed on a later pass.
func TestPermanentRefusalIsNeverReplacedByALaterExecution(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		return fixture.start.refuse(unit), nil
	})

	first := fixture.process(t)
	if !hasEvent(first.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the permanent refusal recorded", first.Refused)
	}
	key := keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	stored, ok := fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v); want a durable refusal", stored, ok)
	}

	// The dependency recovers, but the durable refusal still governs replay.
	fixture.start.setFn(nil)
	second := fixture.process(t)

	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; a durable refusal must never become an execution", fixture.start.executionCount())
	}
	if !hasEvent(second.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v on replay; the durable refusal must stay reported", second.Refused)
	}
	stored, ok = fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v) after replay; want the preserved refusal", stored, ok)
	}
}

// TestStartUnitCarriesCurrentAutomaticServiceAuthority asserts the processor
// supplies current authority reads for the automatic-start service on every
// start attempt, including service enablement and the event.read grant for the
// accepted event. A stored selection is never a permission cache.
func TestStartUnitCarriesCurrentAutomaticServiceAuthority(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 3, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 4),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	fixture.process(t)

	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want one start attempt", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.ExpectedPipelineVersion != 3 {
		t.Fatalf("expected pipeline version = %d; want the current 3", unit.ExpectedPipelineVersion)
	}
	if unit.ExpectedBindingVersion != 4 {
		t.Fatalf("expected binding version = %d; want the current 4", unit.ExpectedBindingVersion)
	}
	if len(unit.Authority) == 0 {
		t.Fatal("StartUnit carries no current authority reads; a selection must never be durable permission")
	}
	checks := make(map[authorize.AuthorityCheck]bool, len(unit.Authority))
	for _, read := range unit.Authority {
		if read.Scope != scope {
			t.Errorf("authority read %q scope = %+v; want the event scope", read.Check, read.Scope)
		}
		if read.PrincipalID != id.PrincipalID("svc-a") {
			t.Errorf("authority read %q principal = %q; want the automatic-start service svc-a", read.Check, read.PrincipalID)
		}
		checks[read.Check] = true
	}
	for _, want := range []authorize.AuthorityCheck{
		authorize.CheckMembership,
		authorize.CheckServiceEnabled,
		authorize.CheckPipelineVersion,
	} {
		if !checks[want] {
			t.Errorf("StartUnit authority is missing %q; the current automatic-service authority must be rechecked", want)
		}
	}

	// The automatic service must also still hold the event.read grant for the
	// accepted event in both commit units, with that registry operation's own
	// requires rather than the start operation's.
	eventRead, ok := authorize.LookupOperation(authorize.OperationName("event.read"))
	if !ok {
		t.Fatal("event.read is not registered")
	}
	requireEventRead := func(name string, reads []authorize.AuthorityRead) {
		t.Helper()
		for _, read := range reads {
			if read.Operation != eventRead.Name {
				continue
			}
			if read.Scope != scope {
				t.Errorf("%s event.read scope = %+v; want the event scope", name, read.Scope)
			}
			if read.PrincipalID != id.PrincipalID("svc-a") {
				t.Errorf("%s event.read principal = %q; want the automatic-start service svc-a", name, read.PrincipalID)
			}
			if read.Check != authorize.CheckMembership {
				t.Errorf("%s event.read check = %q; want membership", name, read.Check)
			}
			if !reflect.DeepEqual(read.Required, eventRead.Requires) {
				t.Errorf("%s event.read requires = %+v; want the registry %+v", name, read.Required, eventRead.Requires)
			}
			return
		}
		t.Errorf("%s carries no event.read authority read", name)
	}
	requireEventRead("SelectUnit", fixture.selections.selectCall(0).Authority)
	requireEventRead("StartUnit", unit.Authority)
}

// hasPostCommit returns the post-commit effect of one kind.
func hasPostCommit(effects []event.PostCommitEffect, kind event.PostCommitKind) (event.PostCommitEffect, bool) {
	for _, effect := range effects {
		if effect.Kind == kind {
			return effect, true
		}
	}
	return event.PostCommitEffect{}, false
}

// TestSelectedStartUnitCarriesQueuedEventAndPublishIntent proves a normal push
// start commits the canonical queued-execution event with publication and
// dispatch intent, not only a dispatch effect.
func TestSelectedStartUnitCarriesQueuedEventAndPublishIntent(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	fixture.process(t)

	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want one start attempt", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusSelected || unit.NewExecutionID == "" {
		t.Fatalf("unit status/execution = %q/%q; want a selected queued execution", unit.Status, unit.NewExecutionID)
	}
	if len(unit.Events) != 1 {
		t.Fatalf("start unit events = %d; want the one queued-execution event", len(unit.Events))
	}
	queued := unit.Events[0]
	if err := queued.Validate(); err != nil {
		t.Fatalf("queued-execution event is invalid: %v", err)
	}
	if queued.Type != event.TypeExecutionQueued {
		t.Errorf("queued event type = %q; want %q", queued.Type, event.TypeExecutionQueued)
	}
	if queued.SubjectID != string(unit.NewExecutionID) {
		t.Errorf("queued event subject = %q; want the new execution %q", queued.SubjectID, unit.NewExecutionID)
	}
	if queued.Source.Kind != event.SourceShell || queued.Source.PrincipalID != id.PrincipalID("svc-a") {
		t.Errorf("queued event source = %+v; want the shell automatic service svc-a", queued.Source)
	}
	if queued.Scope != scope {
		t.Errorf("queued event scope = %+v; want the event scope", queued.Scope)
	}
	if queued.CorrelationID != "event-1" {
		t.Errorf("queued event correlation = %q; want the accepted event id", queued.CorrelationID)
	}
	if _, ok := hasPostCommit(unit.PostCommit, event.PostPublishEvent); !ok {
		t.Error("start unit has no publish_event effect; the queued event would never be published")
	}
	dispatch, ok := hasPostCommit(unit.PostCommit, event.PostDispatch)
	if !ok || dispatch.Target != string(unit.NewExecutionID) {
		t.Errorf("dispatch effect = %+v (ok=%v); want the new execution target", dispatch, ok)
	}
}

// TestRefusedStartUnitCarriesRefusalEventAndPublishIntent proves a permanent
// refusal is published, not only recorded: the unit carries the canonical
// pipeline.start_refused decision event and a publish effect and never
// dispatches.
func TestRefusedStartUnitCarriesRefusalEventAndPublishIntent(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.admission.setFn(func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
		return pipeline.AdmissionResult{}, fault.New(fault.Forbidden, "automatic service is not permitted")
	})

	result := fixture.process(t)

	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the permanent refusal recorded", result.Refused)
	}
	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want one refusal commit", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusRefused {
		t.Fatalf("start status = %q; want a permanent refusal", unit.Status)
	}
	if len(unit.Events) != 1 {
		t.Fatalf("refusal unit events = %d; want the published refusal event", len(unit.Events))
	}
	refusal := unit.Events[0]
	if err := refusal.Validate(); err != nil {
		t.Fatalf("refusal event is invalid: %v", err)
	}
	if refusal.Type != event.TypePipelineStartRefused {
		t.Errorf("refusal event type = %q; want the canonical %q", refusal.Type, event.TypePipelineStartRefused)
	}
	if refusal.Type == event.TypeCustomObservation {
		t.Error("refusal event claims a custom observation; a Shell decision is never an execution observation")
	}
	if refusal.SubjectID != "pipeline-a" {
		t.Errorf("refusal event subject = %q; want the refused pipeline", refusal.SubjectID)
	}
	if refusal.SubjectVersion != 1 {
		t.Errorf("refusal event subject version = %d; want the current pipeline version 1", refusal.SubjectVersion)
	}
	if refusal.Source.Kind != event.SourceShell || refusal.Source.PrincipalID != id.PrincipalID("svc-a") {
		t.Errorf("refusal event source = %+v; want the shell automatic service svc-a", refusal.Source)
	}
	if refusal.Scope != scope {
		t.Errorf("refusal event scope = %+v; want the event scope", refusal.Scope)
	}
	if refusal.CorrelationID != "event-1" {
		t.Errorf("refusal event correlation = %q; want the accepted event id", refusal.CorrelationID)
	}
	assertRefusalPayload(t, refusal, "forbidden")
	if _, ok := hasPostCommit(unit.PostCommit, event.PostPublishEvent); !ok {
		t.Error("refusal unit has no publish_event effect; the permanent refusal would never be published")
	}
	if _, ok := hasPostCommit(unit.PostCommit, event.PostDispatch); ok {
		t.Error("refusal unit requests dispatch; a permanent refusal creates no execution")
	}
}

// TestSelectRefusalIsCommittedAndPublishedThroughStart proves a select-time
// refusal is not left durable-but-unpublished: the processor routes it through
// StartPort.Start exactly once with a canonical pipeline.start_refused event
// and a publish effect, and only that durable result completes the event.
func TestSelectRefusalIsCommittedAndPublishedThroughStart(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(unit trigger.SelectUnit) (trigger.Result, error) {
		return trigger.Result{
			Status:    trigger.StatusRefused,
			Selection: unit.Selection,
			Receipt:   unit.Receipt,
		}, nil
	})

	result := fixture.process(t)

	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the select-time refusal recorded", result.Refused)
	}
	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want exactly one refusal commit", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusRefused {
		t.Fatalf("start status = %q; want the refusal committed through StartPort", unit.Status)
	}
	if len(unit.Events) != 1 {
		t.Fatalf("refusal unit events = %d; want exactly one published refusal event", len(unit.Events))
	}
	refusal := unit.Events[0]
	if err := refusal.Validate(); err != nil {
		t.Fatalf("select refusal event is invalid: %v", err)
	}
	if refusal.Type != event.TypePipelineStartRefused {
		t.Fatalf("select refusal type = %q; want %q", refusal.Type, event.TypePipelineStartRefused)
	}
	if refusal.Source.Kind != event.SourceShell || refusal.Source.PrincipalID != id.PrincipalID("svc-a") {
		t.Fatalf("select refusal source = %+v; want the shell automatic service svc-a", refusal.Source)
	}
	assertRefusalPayload(t, refusal, "selection_refused")
	if _, ok := hasPostCommit(unit.PostCommit, event.PostPublishEvent); !ok {
		t.Fatal("select refusal unit has no publish_event effect; the refusal would never be published")
	}
	if _, ok := hasPostCommit(unit.PostCommit, event.PostDispatch); ok {
		t.Fatal("select refusal unit requests dispatch; a refusal creates no execution")
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; a select-time refusal must not queue work", fixture.start.executionCount())
	}
	stored, ok := fixture.selections.get(keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1")))
	if !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v); want the durable refusal committed by Start", stored, ok)
	}

	second := fixture.process(t)

	if fixture.start.callCount() != 1 || fixture.selections.selectCount() != 1 {
		t.Fatalf("replay called Select/Start %d/%d times; a durable refusal must not be committed twice", fixture.selections.selectCount(), fixture.start.callCount())
	}
	if !hasEvent(second.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v on replay; the durable refusal must stay reported", second.Refused)
	}
}

// TestSelectPermanentFaultIsPublishedThroughStart proves a permanent selection
// fault class is a refusal, not an infinite retry: it is committed through the
// same StartUnit refusal path and published with the stable fault class.
func TestSelectPermanentFaultIsPublishedThroughStart(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 2, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(trigger.SelectUnit) (trigger.Result, error) {
		return trigger.Result{}, fault.New(fault.VersionConflict, "binding changed before selection")
	})

	result := fixture.process(t)

	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the permanent selection fault recorded", result.Refused)
	}
	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want exactly one refusal commit", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusRefused || len(unit.Events) != 1 {
		t.Fatalf("select fault unit = %+v; want one StatusRefused event", unit)
	}
	if unit.Events[0].Type != event.TypePipelineStartRefused {
		t.Fatalf("select fault event type = %q; want %q", unit.Events[0].Type, event.TypePipelineStartRefused)
	}
	assertRefusalPayload(t, unit.Events[0], "version_conflict")
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; a selection fault must not queue work", fixture.start.executionCount())
	}
}

// TestSelectRefusalPublishesOnceAndCompletesAfterDurableStart simulates a lost
// Start response after the refusal committed: the first pass must not complete
// the event, the next pass must find the durable refusal and complete it, and
// the canonical refusal event must be published exactly once with the same ID.
func TestSelectRefusalPublishesOnceAndCompletesAfterDurableStart(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(unit trigger.SelectUnit) (trigger.Result, error) {
		return trigger.Result{
			Status:    trigger.StatusRefused,
			Selection: unit.Selection,
			Receipt:   unit.Receipt,
		}, nil
	})
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		result, err := fixture.start.defaultStart(unit)
		if err != nil {
			return trigger.Result{}, err
		}
		return result, fault.New(fault.DependencyUnavailable, "lost start response after commit")
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a lost response must retain the event", first.Deferred)
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; the event must not complete before the durable result is observed", fixture.completion.sequences())
	}
	published := fixture.start.publishedEvents()
	if len(published) != 1 || published[0].Type != event.TypePipelineStartRefused {
		t.Fatalf("published events = %+v; want one canonical refusal event", published)
	}
	committedID := published[0].ID

	second := fixture.process(t)

	if !hasEvent(second.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; the durable refusal must be observed on the next pass", second.Refused)
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
	if fixture.selections.selectCount() != 1 || fixture.start.callCount() != 1 {
		t.Fatalf("Select/Start called %d/%d times; the durable refusal must not be committed again", fixture.selections.selectCount(), fixture.start.callCount())
	}
	after := fixture.start.publishedEvents()
	if len(after) != 1 || after[0].ID != committedID {
		t.Fatalf("published events after replay = %+v; want the same single refusal event %q", after, committedID)
	}
}

// TestNewStartRefusedEventPublishesAuthorityRefusal proves the shared builder
// is the single canonical construction path for a current-authority or
// service-enablement refusal detected inside StartPort.Start.
func TestNewStartRefusedEventPublishesAuthorityRefusal(t *testing.T) {
	refusal, err := trigger.NewStartRefusedEvent(
		id.EventID("refusal-1"), testScope(),
		trigger.Selection{PipelineID: "pipeline-a", EventID: "event-1", ServiceID: "svc-a"},
		4, trigger.RefusalReasonAuthorityRefused, id.PrincipalID("svc-a"), fixedNow,
	)
	if err != nil {
		t.Fatalf("NewStartRefusedEvent: %v", err)
	}
	if err := refusal.Validate(); err != nil {
		t.Fatalf("authority refusal event is invalid: %v", err)
	}
	if refusal.Type != event.TypePipelineStartRefused {
		t.Fatalf("authority refusal type = %q; want %q", refusal.Type, event.TypePipelineStartRefused)
	}
	if refusal.Source.Kind != event.SourceShell || refusal.Source.PrincipalID != id.PrincipalID("svc-a") {
		t.Fatalf("authority refusal source = %+v; want the shell automatic service svc-a", refusal.Source)
	}
	if refusal.SubjectID != "pipeline-a" || refusal.SubjectVersion != 4 {
		t.Fatalf("authority refusal subject = %q@%d; want pipeline-a@4", refusal.SubjectID, refusal.SubjectVersion)
	}
	if refusal.CorrelationID != "event-1" {
		t.Fatalf("authority refusal correlation = %q; want event-1", refusal.CorrelationID)
	}
	assertRefusalPayload(t, refusal, "authority_refused")
}

// TestDisabledServiceRefusalIsDurable covers a service disabled between
// selection and the start commit: the refusal is recorded, the event is
// completed and no execution exists.
func TestDisabledServiceRefusalIsDurable(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		if unit.Status != trigger.StatusSelected {
			t.Errorf("start status = %q; the selection is committed before the authority check", unit.Status)
		}
		return fixture.start.refuse(unit), nil
	})

	result := fixture.process(t)

	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the disabled-service refusal", result.Refused)
	}
	if len(result.Started) != 0 {
		t.Fatalf("started = %v; a disabled service must not queue work", result.Started)
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; want zero", fixture.start.executionCount())
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
	key := keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	stored, ok := fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v); want a durable refusal", stored, ok)
	}
	if len(stored.Selection.YAML) == 0 {
		t.Fatal("the refusal dropped the first selection; it must be preserved")
	}
	published := fixture.start.publishedEvents()
	if len(published) != 1 {
		t.Fatalf("published events = %+v; want exactly one refusal event", published)
	}
	if published[0].Type != event.TypePipelineStartRefused {
		t.Fatalf("authority refusal type = %q; want %q", published[0].Type, event.TypePipelineStartRefused)
	}
	assertRefusalPayload(t, published[0], "authority_refused")
}

// TestAuthorityRevokedBetweenSelectionAndStartIsRefused proves the selection
// commits first and the later authority check records a refusal against the
// same common receipt instead of an execution.
func TestAuthorityRevokedBetweenSelectionAndStartIsRefused(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	selectionCommitted := false
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		key := keyFor(scope, unit.PipelineID, unit.EventID)
		stored, ok := fixture.selections.get(key)
		if ok && stored.Status == trigger.StatusSelected {
			selectionCommitted = true
		}
		return fixture.start.refuse(unit), nil
	})

	result := fixture.process(t)

	if !selectionCommitted {
		t.Fatal("the start attempt ran before the first selection was committed")
	}
	if fixture.selections.selectCount() != 1 {
		t.Fatalf("Select called %d times; want exactly one committed first selection", fixture.selections.selectCount())
	}
	selected := fixture.selections.selectCall(0)
	unit := fixture.start.call(0)
	if unit.Receipt.ID != selected.Receipt.ID {
		t.Fatalf("start receipt = %q; want the selection's common receipt %q", unit.Receipt.ID, selected.Receipt.ID)
	}
	if !reflect.DeepEqual(unit.Selection, selected.Selection) {
		t.Fatalf("start selection = %+v; want the committed first selection %+v", unit.Selection, selected.Selection)
	}
	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the revoked-authority refusal recorded", result.Refused)
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; revoked authority must never queue work", fixture.start.executionCount())
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
}

// TestPermanentAdmissionFaultRecordsRefusal checks every declared permanent
// admission class is recorded through StartPort.Start with StatusRefused and no
// admission result, never as a transient retry.
func TestPermanentAdmissionFaultRecordsRefusal(t *testing.T) {
	classes := []fault.Class{
		fault.Forbidden,
		fault.InvalidInput,
		fault.NotFound,
		fault.VersionConflict,
		fault.ChangedIdempotencyContent,
		fault.UnsupportedCapability,
	}
	for _, class := range classes {
		t.Run(string(class), func(t *testing.T) {
			fixture := newPushFixture(t)
			scope := testScope()
			fixture.candidates.set(testCandidate(
				testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
				testBinding(scope, "repo-1", "git-svc", 1),
			))
			fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
			fixture.admission.setFn(func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
				return pipeline.AdmissionResult{}, fault.New(class, "admission refused")
			})

			result := fixture.process(t)

			if fixture.admission.callCount() != 1 {
				t.Fatalf("Admit called %d times; want one attempt", fixture.admission.callCount())
			}
			if fixture.start.callCount() != 1 {
				t.Fatalf("Start called %d times; want the refusal recorded once", fixture.start.callCount())
			}
			unit := fixture.start.call(0)
			if unit.Status != trigger.StatusRefused {
				t.Fatalf("start status = %q; want refused", unit.Status)
			}
			if !reflect.DeepEqual(unit.Admission, pipeline.AdmissionResult{}) {
				t.Fatalf("refusal start carried admission %+v; a permanent admission fault has no admission result", unit.Admission)
			}
			if !hasEvent(result.Refused, id.EventID("event-1")) {
				t.Fatalf("refused = %v; want the event recorded as refused", result.Refused)
			}
			if hasEvent(result.Deferred, id.EventID("event-1")) {
				t.Fatalf("deferred = %v; a permanent fault must not be retried", result.Deferred)
			}
			if fixture.start.executionCount() != 0 {
				t.Fatalf("executions = %d; a refused admission must not queue work", fixture.start.executionCount())
			}
			if !fixture.completion.has(1) {
				t.Fatalf("completed = %v; the refusal must advance the cursor", fixture.completion.sequences())
			}
		})
	}
}

// TestLostAdmissionResponseRetriesWithoutRefusal models a lost admission
// response: the first attempt is uncertain, the retry succeeds and creates
// exactly one execution.
func TestLostAdmissionResponseRetriesWithoutRefusal(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	attempts := 0
	fixture.admission.setFn(func(request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
		attempts++
		if attempts == 1 {
			return pipeline.AdmissionResult{}, fault.New(fault.DependencyUnavailable, "admission response lost")
		}
		return defaultAdmit(request)
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a lost admission response must retain the work", first.Deferred)
	}
	if len(first.Refused) != 0 {
		t.Fatalf("refused = %v; an uncertain admission is not a permanent refusal", first.Refused)
	}
	if fixture.start.callCount() != 0 {
		t.Fatalf("Start called %d times; no start may run without an admission result", fixture.start.callCount())
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; unfinished work must not advance the cursor", fixture.completion.sequences())
	}
	if fixture.selections.selectCount() != 1 {
		t.Fatalf("Select called %d times; the first selection must be persisted before admission", fixture.selections.selectCount())
	}

	second := fixture.process(t)

	if len(second.Started) != 1 || second.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("started = %v; want the retried execution exec-1", second.Started)
	}
	if len(second.Refused) != 0 {
		t.Fatalf("refused = %v; a lost response must never become a refusal", second.Refused)
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; want exactly one", fixture.start.executionCount())
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled event must advance the cursor", fixture.completion.sequences())
	}
}

// TestLostStartResponseReturnsOriginalExecution models a start that committed
// but whose response was lost. The retry must return the original execution,
// never a second one and never a refusal.
func TestLostStartResponseReturnsOriginalExecution(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	attempts := 0
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		result, err := fixture.start.defaultStart(unit)
		if err != nil {
			return result, err
		}
		attempts++
		if attempts == 1 {
			return result, fault.New(fault.UncertainExternalEffect, "start response lost")
		}
		return result, nil
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a lost start response must retain the work", first.Deferred)
	}
	if len(first.Refused) != 0 {
		t.Fatalf("refused = %v; an uncertain start is not a permanent refusal", first.Refused)
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d after the lost response; the commit must have happened once", fixture.start.executionCount())
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; an unacknowledged result must not advance the cursor", fixture.completion.sequences())
	}

	second := fixture.process(t)

	if len(second.Started) != 1 || second.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("started = %v; the retry must return the original execution exec-1", second.Started)
	}
	if len(second.Refused) != 0 {
		t.Fatalf("refused = %v; the retry must never be replaced by a refusal", second.Refused)
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; a retry must never create a second execution", fixture.start.executionCount())
	}
	if ids := fixture.start.executionIDs(); len(ids) != 1 || ids[0] != id.ExecutionID("exec-1") {
		t.Fatalf("execution ids = %v; want only exec-1", ids)
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled event must advance the cursor", fixture.completion.sequences())
	}
	key := keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	stored, ok := fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusSelected || stored.ExecutionID != id.ExecutionID("exec-1") {
		t.Fatalf("stored result = %+v (ok=%v); want the original selected execution", stored, ok)
	}
}

// TestCursorStopsAtUnfinishedResult proves a later event is never completed
// while an earlier result is unfinished, and that recovery resumes in order.
func TestCursorStopsAtUnfinishedResult(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	refs := []string{"refs/heads/main", "refs/heads/next", "refs/heads/later"}
	candidates := make([]trigger.Candidate, 0, len(refs))
	for index, pipelineID := range []string{"pipeline-ok", "pipeline-transient", "pipeline-later"} {
		candidates = append(candidates, testCandidate(
			testDefinition(pipelineID, pushYAML("repo-1", "svc-"+pipelineID, []string{refs[index]}, []string{"ref"}, ""), 1, "repo-1", "svc-"+pipelineID, []string{refs[index]}),
			testBinding(scope, "repo-1", "git-svc", 1),
		))
	}
	fixture.candidates.set(candidates...)
	fixture.events.setRows(
		pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"),
		pushEvent(2, "event-2", "repo-1", "git-svc", "refs/heads/next", "base-2", "cand-2"),
		pushEvent(3, "event-3", "repo-1", "git-svc", "refs/heads/later", "base-3", "cand-3"),
	)
	fixture.admission.setFn(func(request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
		if request.Definition.ID == pipeline.PipelineID("pipeline-transient") {
			return pipeline.AdmissionResult{}, fault.New(fault.DependencyUnavailable, "registry unavailable")
		}
		return defaultAdmit(request)
	})

	first := fixture.process(t)

	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled first event must advance", fixture.completion.sequences())
	}
	if fixture.completion.has(2) || fixture.completion.has(3) {
		t.Fatalf("completed = %v; no later event may be completed while event-2 is unfinished", fixture.completion.sequences())
	}
	if !hasEvent(first.Deferred, id.EventID("event-2")) {
		t.Fatalf("deferred = %v; want event-2 retained", first.Deferred)
	}
	for index := 0; index < fixture.selections.selectCount(); index++ {
		if fixture.selections.selectCall(index).PipelineID == pipeline.PipelineID("pipeline-later") {
			t.Fatal("a later event was processed before the unfinished earlier result settled")
		}
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; only the first settled event may start", fixture.start.executionCount())
	}

	fixture.admission.setFn(nil)
	fixture.process(t)

	if !fixture.completion.has(2) || !fixture.completion.has(3) {
		t.Fatalf("completed = %v; recovery must resume in durable order", fixture.completion.sequences())
	}
	if fixture.start.executionCount() != 3 {
		t.Fatalf("executions = %d; each settled event must start exactly once", fixture.start.executionCount())
	}
	for _, cursor := range fixture.events.cursors() {
		if cursor != 0 {
			t.Fatalf("Pending cursor = %d; the processor must rescan from the zero sequence every pass", cursor)
		}
	}
}

// TestEventCompletesOnlyAfterEveryRelevantSelection proves one event matching
// two pipelines is not completed while one selection is unfinished, and that
// the finished pipeline is not restarted on recovery.
func TestEventCompletesOnlyAfterEveryRelevantSelection(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(
		testCandidate(
			testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
			testBinding(scope, "repo-1", "git-svc", 1),
		),
		testCandidate(
			testDefinition("pipeline-b", pushYAML("repo-1", "svc-b", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-b", []string{"refs/heads/main"}),
			testBinding(scope, "repo-1", "git-svc", 1),
		),
	)
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(unit trigger.SelectUnit) (trigger.Result, error) {
		if unit.PipelineID == pipeline.PipelineID("pipeline-b") {
			return trigger.Result{}, fault.New(fault.DependencyUnavailable, "selection store unavailable")
		}
		return fixture.selections.commit(unit), nil
	})

	first := fixture.process(t)

	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; the event has an unfinished relevant selection", fixture.completion.sequences())
	}
	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; want the event retained", first.Deferred)
	}
	if !hasExecution(first.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; the settled pipeline must still start", first.Started)
	}
	if _, ok := fixture.selections.get(keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))); !ok {
		t.Fatal("pipeline-a's durable selection was lost when pipeline-b failed")
	}

	fixture.selections.setSelectFn(nil)
	second := fixture.process(t)

	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; every relevant selection now has a result", fixture.completion.sequences())
	}
	if fixture.start.executionCount() != 2 {
		t.Fatalf("executions = %d; each pipeline must start exactly once", fixture.start.executionCount())
	}
	if len(second.Refused) != 0 {
		t.Fatalf("refused = %v; a transient selection failure is not a refusal", second.Refused)
	}
}

// TestDeletedPipelineSelectionIsRefusedNotSkipped covers a committed first
// selection whose start failed transiently and whose pipeline/binding was then
// deleted: the next pass must still find that persisted selection, keep the
// event retained while the refusal cannot be recorded, and finally record a
// permanent refusal before advancing the cursor.
func TestDeletedPipelineSelectionIsRefusedNotSkipped(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 2, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 3),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.start.setFn(func(trigger.StartUnit) (trigger.Result, error) {
		return trigger.Result{}, fault.New(fault.DependencyUnavailable, "runtime unavailable")
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a transient start failure must retain the event", first.Deferred)
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; an unfinished selection must not advance the cursor", fixture.completion.sequences())
	}
	key := keyFor(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	stored, ok := fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusSelected || stored.ExecutionID != "" {
		t.Fatalf("stored result = %+v (ok=%v); want the persisted unfinished selection", stored, ok)
	}

	// The definition and binding disappear before the retry, so the candidate
	// reader no longer returns the pipeline.
	fixture.candidates.set()
	fixture.start.setFn(func(trigger.StartUnit) (trigger.Result, error) {
		return trigger.Result{}, fault.New(fault.DependencyUnavailable, "store unavailable")
	})

	retained := fixture.process(t)

	if !hasEvent(retained.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; the deleted-pipeline selection is still unfinished", retained.Deferred)
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; the event must not complete while its selection is unfinished", fixture.completion.sequences())
	}

	fixture.start.setFn(nil)

	second := fixture.process(t)

	if !hasEvent(second.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; the orphaned selection must be permanently refused", second.Refused)
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the durable refusal must advance the cursor", fixture.completion.sequences())
	}
	stored, ok = fixture.selections.get(key)
	if !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v); want a durable refusal for the orphaned selection", stored, ok)
	}
	if len(stored.Selection.YAML) == 0 {
		t.Fatal("the orphaned refusal dropped the persisted first selection bytes")
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; a deleted pipeline must not queue work", fixture.start.executionCount())
	}
	unit := fixture.start.call(fixture.start.callCount() - 1)
	if unit.Status != trigger.StatusRefused {
		t.Fatalf("orphan start status = %q; want a permanent refusal", unit.Status)
	}
	if unit.PipelineID != pipeline.PipelineID("pipeline-a") || unit.EventID != id.EventID("event-1") {
		t.Fatalf("orphan unit pipeline/event = %q/%q; want the persisted selection identity", unit.PipelineID, unit.EventID)
	}
	if unit.ExpectedPipelineVersion != 0 || unit.ExpectedBindingVersion != 0 {
		t.Fatalf("orphan expected versions = %d/%d; a deleted definition/binding has no current version", unit.ExpectedPipelineVersion, unit.ExpectedBindingVersion)
	}
	if len(unit.Events) == 0 {
		t.Fatal("orphan refusal carries no event; the refusal must be published")
	}
	refusal := unit.Events[0]
	if err := refusal.Validate(); err != nil {
		t.Fatalf("orphan refusal event is invalid: %v", err)
	}
	if refusal.Type != event.TypePipelineStartRefused {
		t.Fatalf("orphan refusal type = %q; want %q", refusal.Type, event.TypePipelineStartRefused)
	}
	if refusal.SubjectVersion != 0 {
		t.Fatalf("orphan refusal subject version = %d; want 0 for a deleted definition", refusal.SubjectVersion)
	}
	assertRefusalPayload(t, refusal, "definition_missing")
	if _, ok := hasPostCommit(unit.PostCommit, event.PostPublishEvent); !ok {
		t.Fatal("orphan refusal carries no publish_event effect")
	}
}

// TestDuplicateEventDeliveryStartsOnce replays the same event ID in two rows:
// one durable selection and one execution must exist.
func TestDuplicateEventDeliveryStartsOnce(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(
		pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"),
		pushEvent(2, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"),
	)

	fixture.process(t)

	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; duplicate delivery must not create a second execution", fixture.start.executionCount())
	}
	if ids := fixture.start.executionIDs(); len(ids) != 1 {
		t.Fatalf("execution ids = %v; want one durable execution", ids)
	}
	if fixture.selections.selectCount() == 0 {
		t.Fatal("no first selection was persisted")
	}
	first := fixture.selections.selectCall(0).Selection
	for index := 1; index < fixture.selections.selectCount(); index++ {
		if !reflect.DeepEqual(fixture.selections.selectCall(index).Selection, first) {
			t.Fatalf("duplicate delivery rebuilt a different selection at index %d", index)
		}
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; want the first row settled", fixture.completion.sequences())
	}
}

// TestConcurrentProcessCallsProduceOneExecution runs two Process passes with
// overlapping selections and asserts one durable execution per (pipeline,
// event).
func TestConcurrentProcessCallsProduceOneExecution(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	fixture.selections.setSelectFn(func(unit trigger.SelectUnit) (trigger.Result, error) {
		entered <- struct{}{}
		<-release
		return fixture.selections.commit(unit), nil
	})

	results := make([]trigger.ProcessResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for index := range 2 {
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = fixture.processor.Process(context.Background())
		}(index)
	}

	overlapped := 0
	deadline := time.After(time.Second)
wait:
	for overlapped < 2 {
		select {
		case <-entered:
			overlapped++
		case <-deadline:
			break wait
		}
	}
	close(release)
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Process %d: %v", index, err)
		}
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; concurrent retries must produce one execution", fixture.start.executionCount())
	}
	for _, result := range results {
		if len(result.Refused) != 0 {
			t.Fatalf("refused = %v; concurrent retries must not refuse", result.Refused)
		}
		if len(result.Started) > 0 && !hasExecution(result.Started, id.ExecutionID("exec-1")) {
			t.Fatalf("started = %v; want the single execution exec-1", result.Started)
		}
	}
}

// TestSelectionFailureDefersWithoutCompletion keeps work when the selection
// commit is transiently unavailable and retries it later.
func TestSelectionFailureDefersWithoutCompletion(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.selections.setSelectFn(func(trigger.SelectUnit) (trigger.Result, error) {
		return trigger.Result{}, fault.New(fault.DependencyUnavailable, "selection store unavailable")
	})

	first := fixture.process(t)

	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; want the event retained", first.Deferred)
	}
	if fixture.admission.callCount() != 0 || fixture.start.callCount() != 0 {
		t.Fatalf("admission/start ran %d/%d times without a committed selection", fixture.admission.callCount(), fixture.start.callCount())
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; no result was durable", fixture.completion.sequences())
	}

	fixture.selections.setSelectFn(nil)
	fixture.process(t)

	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; the retried selection must start once", fixture.start.executionCount())
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled event must advance", fixture.completion.sequences())
	}
}

// TestEventReadFailureDoesNotAdvanceTheCursor covers an unreadable event table:
// no selection is invented and no completion is recorded.
func TestEventReadFailureDoesNotAdvanceTheCursor(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	fixture.events.setError(fault.New(fault.DependencyUnavailable, "event table unavailable"))

	result, err := fixture.processor.Process(context.Background())

	if err == nil && len(result.Completed) != 0 {
		t.Fatalf("Process returned completed = %v with no error after a read failure", result.Completed)
	}
	if fixture.completion.count() != 0 {
		t.Fatalf("completed = %v; an unreadable table must not advance the cursor", fixture.completion.sequences())
	}
	if fixture.selections.selectCount() != 0 {
		t.Fatalf("Select called %d times; no event was read", fixture.selections.selectCount())
	}
}

// TestRunReturnsOnContextCancellation keeps the long-running loop joinable.
func TestRunReturnsOnContextCancellation(t *testing.T) {
	fixture := newPushFixture(t)
	scope := testScope()
	fixture.candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	idle := make(chan struct{}, 1)
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         fixture.events,
		Candidates:     fixture.candidates,
		Selections:     fixture.selections,
		Select:         fixture.selections,
		Admission:      fixture.admission,
		Start:          fixture.start,
		Completion:     fixture.completion,
		Clock:          fakeClock{},
		IDs:            &fakeIDs{},
		BatchSize:      16,
		DefaultTimeout: 5 * time.Minute,
		Idle: func(ctx context.Context) error {
			select {
			case idle <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.Run(ctx) }()

	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never reached the idle wait")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v; want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if fixture.start.executionCount() != 1 {
		t.Fatalf("executions = %d; Run must process committed events before idling", fixture.start.executionCount())
	}
}
