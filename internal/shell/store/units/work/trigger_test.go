package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"crypto/sha256"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// seedSelection commits one common receipt carrying a trigger selection result.
func (f *fixture) seedSelection(t *testing.T, scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID, status trigger.Status, executionID id.ExecutionID, at time.Time) receipt.Record {
	t.Helper()
	record := f.receiptFor(scope, startOperationName, selectionCallerKey(pipelineID, eventID), contentHashA)
	record.At = at
	result := trigger.Result{
		ExecutionID: executionID,
		ReceiptID:   record.ID,
		Status:      status,
		Selection: trigger.Selection{
			PipelineID: pipelineID,
			EventID:    eventID,
			YAML:       []byte("kind: Pipeline\nversion: 1\n"),
			ServiceID:  "service-1",
			Inputs:     map[id.InputKey]string{"ref": "main"},
		},
		Receipt: record,
	}
	encoded, err := encodeSelectionResult(result)
	if err != nil {
		t.Fatalf("encode selection: %v", err)
	}
	record.Result = encoded
	// A selected result with no execution is an unfinished selection: it stays
	// incomplete until Start completes the same receipt. A durable execution or
	// refusal has already completed the receipt.
	mode := store.ReceiptInsert
	if status == trigger.StatusSelected && executionID == "" {
		mode = store.ReceiptInsertIncomplete
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{Receipt: &record, ReceiptMode: mode}); err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	return record
}

// seedManualStart persists one ordinary completed pipeline.start receipt exactly
// as the production execution-start adapter writes it: the bare
// execution.StartResult JSON, with no trigger selection envelope. An inline
// start carries no stored pipeline identity or version; a stored start pins
// one.
func (f *fixture) seedManualStart(t *testing.T, scope tenancy.Scope, caller string, pipelineID pipeline.PipelineID, pipelineVersion uint64) id.ExecutionID {
	t.Helper()
	executionID := id.ExecutionID("execution-" + caller)
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("kind: Pipeline\nversion: 1\n"),
		Inputs: map[id.InputKey]string{"ref": "main"},
	})
	if err != nil {
		t.Fatalf("seed manual start snapshot: %v", err)
	}
	record := f.receiptFor(scope, startOperationName, caller, contentHashA)
	record.ID = id.ReceiptID("receipt-" + caller)
	unit := execution.StartUnit{
		Owner:                   scope,
		PipelineID:              pipelineID,
		ExpectedPipelineVersion: pipelineVersion,
		Snapshot:                snapshot,
		Actions:                 []pipeline.AdmittedAction{testAction(0)},
		Initiator:               testPrincipal,
		ExecutionPrincipal:      testPrincipal,
		NewExecutionID:          executionID,
		At:                      testNow,
		Receipt:                 record,
		Audit:                   []event.Audit{f.audit(scope, "pipeline.start")},
		Events:                  []event.Envelope{f.event(scope, string(executionID), 1)},
	}
	if _, err := executionstore.New(f.store, f.store).Start(context.Background(), unit); err != nil {
		t.Fatalf("seed manual start %q: %v", caller, err)
	}
	return executionID
}

func TestTriggerSelectionLookupFirstSelection(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	first := fixture.seedSelection(t, scope, testPipeline, testEvent, trigger.StatusSelected, "", testNow)
	fixture.seedSelection(t, scope, testPipeline, testEvent, trigger.StatusSelected, "", testNow.Add(time.Minute))
	result, found, err := fixture.triggers.Lookup(context.Background(), scope, testPipeline, testEvent)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("the persisted first selection was not found")
	}
	if !result.Replayed || result.Status != trigger.StatusSelected {
		t.Fatalf("lookup result = %+v", result)
	}
	if result.Selection.PipelineID != testPipeline || result.Selection.EventID != testEvent {
		t.Fatalf("selection identity = %+v", result.Selection)
	}
	if result.ReceiptID != first.ID {
		t.Fatalf("lookup returned receipt %q; want the first %q", result.ReceiptID, first.ID)
	}
	if string(result.Selection.YAML) != "kind: Pipeline\nversion: 1\n" || result.Selection.ServiceID != "service-1" {
		t.Fatalf("selection content = %+v", result.Selection)
	}
	if _, found, err := fixture.triggers.Lookup(context.Background(), scope, testPipeline, "event-other"); err != nil || found {
		t.Fatalf("lookup of an unselected event = found %v, err %v", found, err)
	}
}

func TestTriggerSelectionUnfinishedListsSelectedWithoutExecution(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedSelection(t, scope, "pipeline-b", testEvent, trigger.StatusSelected, "", testNow)
	fixture.seedSelection(t, scope, "pipeline-a", testEvent, trigger.StatusSelected, "", testNow.Add(time.Minute))
	fixture.seedSelection(t, scope, "pipeline-c", testEvent, trigger.StatusSelected, "execution-1", testNow.Add(2*time.Minute))
	fixture.seedSelection(t, scope, "pipeline-d", testEvent, trigger.StatusRefused, "", testNow.Add(3*time.Minute))
	fixture.seedSelection(t, scope, "pipeline-e", "event-other", trigger.StatusSelected, "", testNow.Add(4*time.Minute))
	unfinished, err := fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 2 {
		t.Fatalf("unfinished selections = %d; want 2: %+v", len(unfinished), unfinished)
	}
	if unfinished[0].Selection.PipelineID != "pipeline-a" || unfinished[1].Selection.PipelineID != "pipeline-b" {
		t.Fatalf("unfinished order = %+v", unfinished)
	}
	for _, result := range unfinished {
		if !result.Replayed || result.Status != trigger.StatusSelected || result.ExecutionID != "" {
			t.Fatalf("unfinished result = %+v", result)
		}
	}
}

// TestTriggerUnfinishedIgnoresCompletedManualStarts proves that a completed
// ordinary pipeline.start receipt (the bare execution.StartResult JSON) is not
// mistaken for an unfinished push selection. Only an incomplete common receipt
// can be an unfinished selection, so completed manual starts, completed
// successful selections and completed refusals are skipped, while a genuine
// incomplete orphan is still discovered and an unreadable incomplete selection
// still fails closed.
func TestTriggerUnfinishedIgnoresCompletedManualStarts(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()

	// One inline start and one stored-pipeline start, both ordinary completed
	// execution receipts with no trigger envelope.
	fixture.seedManualStart(t, scope, "manual-inline-start", "", 0)
	fixture.seedPipeline(t, scope, "pipeline-manual", "manual", []byte("kind: Pipeline\nversion: 1\n"), 1)
	fixture.seedManualStart(t, scope, "manual-stored-start", "pipeline-manual", 1)

	unfinished, err := fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished with completed manual starts: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("completed manual starts were reported as unfinished: %+v", unfinished)
	}

	// Completed trigger selections -- a successful start and a permanent
	// refusal -- are equally not unfinished work.
	fixture.seedSelection(t, scope, "pipeline-done", testEvent, trigger.StatusSelected, "execution-done", testNow.Add(time.Minute))
	fixture.seedSelection(t, scope, "pipeline-refused", testEvent, trigger.StatusRefused, "", testNow.Add(2*time.Minute))
	unfinished, err = fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished with completed selections: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("completed selections were reported as unfinished: %+v", unfinished)
	}

	// A genuine incomplete selection whose definition is gone is still
	// returned exactly.
	fixture.seedSelection(t, scope, "pipeline-gone", testEvent, trigger.StatusSelected, "", testNow.Add(3*time.Minute))
	unfinished, err = fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished with an incomplete orphan: %v", err)
	}
	if len(unfinished) != 1 {
		t.Fatalf("unfinished = %d; want the one incomplete orphan: %+v", len(unfinished), unfinished)
	}
	stored := unfinished[0]
	if stored.Selection.PipelineID != "pipeline-gone" || stored.Selection.EventID != testEvent {
		t.Fatalf("unfinished orphan = %+v", stored)
	}
	if !stored.Replayed || stored.Status != trigger.StatusSelected || stored.ExecutionID != "" {
		t.Fatalf("unfinished orphan result = %+v", stored)
	}
	if string(stored.Selection.YAML) != "kind: Pipeline\nversion: 1\n" || stored.Selection.ServiceID != "service-1" {
		t.Fatalf("unfinished orphan payload = %+v", stored.Selection)
	}

	t.Run("unreadable incomplete selection is still an error", func(t *testing.T) {
		fixture := newFixture(t)
		scope := testScope()
		record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
		record.Result = []byte("not-a-selection-document")
		if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete}); err != nil {
			t.Fatalf("seed unreadable incomplete receipt: %v", err)
		}
		_, err := fixture.triggers.Unfinished(context.Background(), scope, testEvent)
		mustClass(t, err, fault.InternalFailure)
	})
}

func TestTriggerSelectionUnreadableResultIsRefused(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	record.Result = []byte("not-a-selection-document")
	// An unreadable selection is only unfinished work while the receipt is
	// incomplete; a completed receipt is not a trigger selection at all.
	if _, err := fixture.store.Execute(context.Background(), scope, store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete}); err != nil {
		t.Fatalf("seed unreadable incomplete receipt: %v", err)
	}
	_, _, err := fixture.triggers.Lookup(context.Background(), scope, testPipeline, testEvent)
	mustClass(t, err, fault.InternalFailure)
	_, err = fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	mustClass(t, err, fault.InternalFailure)
}

func TestTriggerSelectionScopeIsolation(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedSelection(t, scope, testPipeline, testEvent, trigger.StatusSelected, "", testNow)
	if _, found, err := fixture.triggers.Lookup(context.Background(), otherScope(), testPipeline, testEvent); err != nil || found {
		t.Fatalf("cross-scope lookup = found %v, err %v", found, err)
	}
	unfinished, err := fixture.triggers.Unfinished(context.Background(), otherScope(), testEvent)
	if err != nil {
		t.Fatalf("cross-scope Unfinished: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("a cross-scope scan leaked selections: %+v", unfinished)
	}
}

func TestTriggerAdmissionPortHasARealImplementation(t *testing.T) {
	var _ trigger.AdmissionPort = (*pipeline.AdmissionService)(nil)
}

// seedEvent commits one event row and returns its durable sequence.
func (f *fixture) seedEvent(t *testing.T, scope tenancy.Scope, identity id.EventID, eventType event.Type, subject string) event.Sequence {
	t.Helper()
	envelope := event.Envelope{
		ID:        identity,
		Type:      eventType,
		Source:    event.Source{Kind: event.SourceService, PrincipalID: "git-svc"},
		At:        testNow,
		Scope:     scope,
		SubjectID: subject,
		Data:      []byte(`{"ref":"refs/heads/main"}`),
	}
	if _, err := f.store.Execute(context.Background(), scope, store.Unit{Events: []event.Envelope{envelope}}); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	committed := f.store.Events(scope)
	if len(committed) == 0 {
		t.Fatalf("seed event: no committed event")
	}
	return event.Sequence(committed[len(committed)-1].Sequence)
}

func TestTriggerEventReaderScansEveryScope(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	other := otherScope()
	first := fixture.seedEvent(t, scope, "event-a", event.TypeExecutionQueued, "repo-1")
	second := fixture.seedEvent(t, other, "event-b", event.TypeExecutionQueued, "repo-2")
	third := fixture.seedEvent(t, scope, "event-c", event.TypeExecutionQueued, "repo-3")
	all, err := fixture.triggers.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("pending rows = %d; want 3", len(all))
	}
	want := []event.Sequence{first, second, third}
	for index, entry := range all {
		if entry.Sequence != want[index] {
			t.Fatalf("pending[%d] sequence = %d; want %d", index, entry.Sequence, want[index])
		}
	}
	if all[0].Event.Scope != scope || all[1].Event.Scope != other || all[2].Event.Scope != scope {
		t.Fatalf("pending scopes = %+v", []tenancy.Scope{all[0].Event.Scope, all[1].Event.Scope, all[2].Event.Scope})
	}
	if all[1].Event.ID != "event-b" || string(all[1].Event.Data) != `{"ref":"refs/heads/main"}` {
		t.Fatalf("pending envelope = %+v", all[1].Event)
	}
	resumed, err := fixture.triggers.Pending(context.Background(), second, 10)
	if err != nil {
		t.Fatalf("Pending after: %v", err)
	}
	if len(resumed) != 1 || resumed[0].Sequence != third {
		t.Fatalf("resumed rows = %+v; want only %d", resumed, third)
	}
	bounded, err := fixture.triggers.Pending(context.Background(), 0, 1)
	if err != nil {
		t.Fatalf("bounded Pending: %v", err)
	}
	if len(bounded) != 1 || bounded[0].Sequence != first {
		t.Fatalf("bounded rows = %+v", bounded)
	}
}

func TestTriggerCompletionWritesOneIdempotentMarker(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	sequence := fixture.seedEvent(t, scope, "event-1", event.TypeExecutionQueued, "repo-1")
	unit := trigger.CompletionUnit{Owner: scope, Sequence: sequence, EventID: "event-1"}
	if err := fixture.triggers.Complete(context.Background(), unit); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	marker, ok := fixture.storedRow(t, scope, store.EventConsumptionsTable, "event-1")
	if !ok {
		t.Fatal("the completion marker was not committed")
	}
	if textValue(marker, "consumer") != trigger.CompletionConsumer {
		t.Fatalf("marker consumer = %q", textValue(marker, "consumer"))
	}
	if versionValue(marker, "version") != uint64(sequence) {
		t.Fatalf("marker version = %d; want %d", versionValue(marker, "version"), sequence)
	}
	if !timeValue(marker, "completed_at").Equal(testNow) {
		t.Fatalf("marker completed_at = %v; want the injected clock %v", timeValue(marker, "completed_at"), testNow)
	}
	// Completing the same event twice applies nothing and is not an error.
	if err := fixture.triggers.Complete(context.Background(), unit); err != nil {
		t.Fatalf("idempotent Complete: %v", err)
	}
	again, _ := fixture.storedRow(t, scope, store.EventConsumptionsTable, "event-1")
	if !timeValue(again, "completed_at").Equal(testNow) {
		t.Fatal("a replayed completion rewrote the marker")
	}
	// A different sequence for the same event is refused.
	stale := unit
	stale.Sequence = sequence + 1
	mustClass(t, fixture.triggers.Complete(context.Background(), stale), fault.VersionConflict)
}

func TestTriggerCompletionScopeAndEventGuard(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	sequence := fixture.seedEvent(t, scope, "event-1", event.TypeExecutionQueued, "repo-1")
	// A wrong scope never writes a marker.
	mustClass(t, fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: otherScope(), Sequence: sequence, EventID: "event-1"}), fault.Forbidden)
	// An unknown event is refused.
	mustClass(t, fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: sequence, EventID: "event-missing"}), fault.NotFound)
	if _, ok := fixture.storedRow(t, scope, store.EventConsumptionsTable, "event-1"); ok {
		t.Fatal("a refused completion wrote a marker")
	}
	// A completion failure rolls back the marker.
	fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
	mustClass(t, fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: sequence, EventID: "event-1"}), fault.DependencyUnavailable)
	if _, ok := fixture.storedRow(t, scope, store.EventConsumptionsTable, "event-1"); ok {
		t.Fatal("a failed completion left a marker")
	}
}

func TestTriggerCompletionConcurrentCompletersConverge(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	sequence := fixture.seedEvent(t, scope, "event-1", event.TypeExecutionQueued, "repo-1")
	unit := trigger.CompletionUnit{Owner: scope, Sequence: sequence, EventID: "event-1"}
	start := make(chan struct{})
	results := make([]error, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index] = fixture.triggers.Complete(context.Background(), unit)
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range results {
		if err != nil {
			t.Fatalf("completer %d: %v", index, err)
		}
	}
	if count := fixture.countRows(t, scope, store.EventConsumptionsTable, nil); count != 1 {
		t.Fatalf("completion markers = %d; want 1", count)
	}
}

// triggerSelection builds one immutable first selection.
func triggerSelection(scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) trigger.Selection {
	return trigger.Selection{
		PipelineID:       pipelineID,
		EventID:          eventID,
		YAML:             []byte("kind: Pipeline\nversion: 1\n"),
		ServiceID:        "svc-a",
		Inputs:           map[id.InputKey]string{"ref": "refs/heads/main"},
		EventContentHash: sha256.Sum256([]byte("event-1")),
	}
}

// triggerAuthority is the current pipeline/binding authority of one selection.
func triggerAuthority(scope tenancy.Scope, pipelineID pipeline.PipelineID, pipelineVersion uint64, bindingID string, bindingVersion uint64) []authorize.AuthorityRead {
	return []authorize.AuthorityRead{
		{Operation: "pipeline.start", Check: authorize.CheckPipelineVersion, Scope: scope, PrincipalID: "svc-a", TargetID: string(pipelineID), ExpectedVersion: pipelineVersion},
		{Operation: "pipeline.start", Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "svc-a", TargetID: bindingID, ExpectedVersion: bindingVersion},
	}
}

// selectUnit builds one complete push-selection unit.
func (f *fixture) selectUnit(scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID, bindingID string, record receipt.Record) trigger.SelectUnit {
	return trigger.SelectUnit{
		Owner:                   scope,
		PipelineID:              pipelineID,
		EventID:                 eventID,
		ExpectedPipelineVersion: 1,
		ExpectedBindingVersion:  1,
		Selection:               triggerSelection(scope, pipelineID, eventID),
		Receipt:                 record,
		Audit:                   []event.Audit{f.audit(scope, "pipeline.start")},
		Authority:               triggerAuthority(scope, pipelineID, 1, bindingID, 1),
	}
}

// triggerStartUnit builds one complete push-start unit.
func (f *fixture) triggerStartUnit(scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID, bindingID string, record receipt.Record, executionID id.ExecutionID, status trigger.Status, pipelineVersion, bindingVersion uint64) trigger.StartUnit {
	selection := triggerSelection(scope, pipelineID, eventID)
	unit := trigger.StartUnit{
		Owner:                   scope,
		PipelineID:              pipelineID,
		EventID:                 eventID,
		Selection:               selection,
		ExpectedPipelineVersion: pipelineVersion,
		ExpectedBindingVersion:  bindingVersion,
		Initiator:               "svc-a",
		ExecutionPrincipal:      "svc-a",
		Status:                  status,
		At:                      testNow,
		Receipt:                 record,
		Audit:                   []event.Audit{f.audit(scope, "pipeline.start")},
		Authority:               triggerAuthority(scope, pipelineID, pipelineVersion, bindingID, bindingVersion),
	}
	if status == trigger.StatusSelected {
		unit.NewExecutionID = executionID
		unit.Admission = pipeline.AdmissionResult{
			PipelineID:      pipelineID,
			PipelineVersion: pipelineVersion,
			Snapshot:        testSnapshot([]byte("kind: Pipeline\nversion: 1\n")),
			Actions:         []pipeline.AdmittedAction{testAction(0)},
		}
		unit.Events = []event.Envelope{f.event(scope, string(executionID), 1)}
	} else {
		refusal, err := trigger.NewStartRefusedEvent("event-refusal", scope, selection, pipelineVersion, trigger.RefusalReasonDefinitionMissing, "svc-a", testNow)
		if err != nil {
			panic(err)
		}
		unit.Events = []event.Envelope{refusal}
	}
	return unit
}

// seedPushEnvironment seeds the pipeline, binding and service a selection needs.
func (f *fixture) seedPushEnvironment(t *testing.T, scope tenancy.Scope) {
	t.Helper()
	f.seedPipeline(t, scope, testPipeline, "build", []byte("kind: Pipeline\n"), 1)
	f.seedBinding(t, scope, "binding-1", "repo-1", "svc-a", 1)
	f.seedService(t, scope, "svc-a", "enabled", 1)
}

func TestTriggerSelectInsertsIncompleteReceipt(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	unit := fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)
	result, err := fixture.triggers.Select(context.Background(), unit)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if result.Status != trigger.StatusSelected || result.ReceiptID != record.ID || result.ExecutionID != "" || result.Replayed {
		t.Fatalf("select result = %+v", result)
	}
	if string(result.Selection.YAML) != string(unit.Selection.YAML) || result.Selection.ServiceID != "svc-a" {
		t.Fatalf("select selection = %+v", result.Selection)
	}
	row, ok := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	if !ok {
		t.Fatal("the selection receipt was not committed")
	}
	if row.Values["completed_at"] != nil {
		t.Fatalf("selection receipt is already completed: %+v", row.Values)
	}
	inProgress, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil {
		t.Fatalf("decode in-progress selection: %v", err)
	}
	if string(inProgress.Selection.YAML) != string(unit.Selection.YAML) || inProgress.Selection.ServiceID != "svc-a" {
		t.Fatalf("in-progress selection = %+v", inProgress.Selection)
	}
	if len(inProgress.Selection.Inputs) != 1 || inProgress.Selection.EventContentHash != unit.Selection.EventContentHash {
		t.Fatalf("in-progress selection payload = %+v", inProgress.Selection)
	}
	if count := fixture.countRows(t, scope, "audit", nil); count != 1 {
		t.Fatalf("selection audit rows = %d; want 1", count)
	}
	// An equal retry applies nothing and returns the STORED first selection,
	// even if the retrying unit carries edited bytes.
	edited := unit
	edited.Selection.YAML = []byte("kind: Pipeline\nversion: 99\n")
	replayed, err := fixture.triggers.Select(context.Background(), edited)
	if err != nil {
		t.Fatalf("replayed Select: %v", err)
	}
	if !replayed.Replayed || replayed.ReceiptID != result.ReceiptID {
		t.Fatalf("replayed select = %+v", replayed)
	}
	if string(replayed.Selection.YAML) != string(unit.Selection.YAML) {
		t.Fatalf("replayed select returned edited bytes %q; want the first %q", replayed.Selection.YAML, unit.Selection.YAML)
	}
	if count := fixture.countRows(t, scope, "audit", nil); count != 1 {
		t.Fatalf("a replayed select committed %d audit rows; want 1", count)
	}
	// Changed content is refused.
	changed := unit
	changed.Receipt = fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashB)
	mustClass(t, func() error { _, err := fixture.triggers.Select(context.Background(), changed); return err }(), fault.ChangedIdempotencyContent)
	// A stale pipeline version leaves no receipt and no audit.
	stale := unit
	stale.ExpectedPipelineVersion = 9
	stale.Authority = triggerAuthority(scope, testPipeline, 9, "binding-1", 1)
	stale.Receipt = fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, "event-2"), contentHashA)
	stale.EventID = "event-2"
	stale.Selection.EventID = "event-2"
	mustClass(t, func() error { _, err := fixture.triggers.Select(context.Background(), stale); return err }(), fault.VersionConflict)
	if _, ok := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(stale.Receipt.ID)}); ok {
		t.Fatal("a stale select committed a receipt")
	}
}

func TestTriggerStartCompletesReceiptAndReplays(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)); err != nil {
		t.Fatalf("Select: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	result, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Status != trigger.StatusSelected || result.ExecutionID != "execution-1" || result.ReceiptID != record.ID || result.Replayed {
		t.Fatalf("start result = %+v", result)
	}
	row, ok := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	if !ok {
		t.Fatal("the completed receipt is missing")
	}
	if row.Values["completed_at"] == nil || row.Values["result"] == nil {
		t.Fatalf("receipt is not completed: %+v", row.Values)
	}
	completed, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil {
		t.Fatalf("decode completed receipt: %v", err)
	}
	if completed.ExecutionID != "execution-1" || completed.Status != trigger.StatusSelected {
		t.Fatalf("completed receipt result = %+v", completed)
	}
	if string(completed.Selection.YAML) != string(start.Selection.YAML) {
		t.Fatalf("completed receipt lost the exact selection YAML: %q", completed.Selection.YAML)
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-1"); !ok {
		t.Fatal("the queued execution was not committed")
	}
	if count := fixture.countRows(t, scope, "actions", map[string]any{"execution_id": "execution-1"}); count != 1 {
		t.Fatalf("committed actions = %d; want 1", count)
	}
	if events := fixture.store.Events(scope); len(events) != 1 || events[0].Envelope.Type != event.TypeExecutionQueued {
		t.Fatalf("committed events = %+v", events)
	}
	// Lookup now returns the durable completed result.
	looked, found, err := fixture.triggers.Lookup(context.Background(), scope, testPipeline, testEvent)
	if err != nil || !found {
		t.Fatalf("Lookup = found %v, err %v", found, err)
	}
	if looked.ExecutionID != "execution-1" || !looked.Replayed {
		t.Fatalf("lookup result = %+v", looked)
	}
	// An equal retry replays the original result and commits nothing.
	replayed, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("replayed Start: %v", err)
	}
	if !replayed.Replayed || replayed.ExecutionID != result.ExecutionID {
		t.Fatalf("replayed start = %+v", replayed)
	}
	if count := fixture.countRows(t, scope, "executions", nil); count != 1 {
		t.Fatalf("a replayed start committed %d executions; want 1", count)
	}
	// Changed content is refused and leaves the completed receipt intact.
	changed := start
	changed.Receipt = fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashB)
	mustClass(t, func() error { _, err := fixture.triggers.Start(context.Background(), changed); return err }(), fault.ChangedIdempotencyContent)
}

func TestTriggerStartAuthorityRefusalCommitsRefusal(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)); err != nil {
		t.Fatalf("Select: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 9, 1)
	result, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("Start with stale authority: %v", err)
	}
	if result.Status != trigger.StatusRefused || result.ExecutionID != "" || result.Replayed {
		t.Fatalf("authority refusal result = %+v", result)
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-1"); ok {
		t.Fatal("an authority refusal committed an execution")
	}
	row, _ := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	completed, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil {
		t.Fatalf("decode refusal receipt: %v", err)
	}
	if completed.Status != trigger.StatusRefused {
		t.Fatalf("refusal receipt result = %+v", completed)
	}
	events := fixture.store.Events(scope)
	if len(events) != 1 || events[0].Envelope.Type != event.TypePipelineStartRefused {
		t.Fatalf("refusal events = %+v", events)
	}
}

func TestTriggerStartRefusedUnitRecordsOrphanWithoutVersions(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	// The pipeline and binding are gone; the selection's service still exists.
	fixture.seedService(t, scope, "svc-a", "enabled", 1)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey("pipeline-gone", testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), trigger.SelectUnit{
		Owner:                   scope,
		PipelineID:              "pipeline-gone",
		EventID:                 testEvent,
		ExpectedPipelineVersion: 0,
		ExpectedBindingVersion:  0,
		Selection:               triggerSelection(scope, "pipeline-gone", testEvent),
		Receipt:                 record,
		Audit:                   []event.Audit{fixture.audit(scope, "pipeline.start")},
	}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	start := fixture.triggerStartUnit(scope, "pipeline-gone", testEvent, "", record, "", trigger.StatusRefused, 0, 0)
	start.Selection = triggerSelection(scope, "pipeline-gone", testEvent)
	start.Authority = nil
	start.Events = nil
	refusal, err := trigger.NewStartRefusedEvent("event-refusal", scope, start.Selection, 0, trigger.RefusalReasonDefinitionMissing, "svc-a", testNow)
	if err != nil {
		t.Fatalf("NewStartRefusedEvent: %v", err)
	}
	start.Events = []event.Envelope{refusal}
	result, err := fixture.triggers.Start(context.Background(), start)
	if err != nil {
		t.Fatalf("orphan Start: %v", err)
	}
	if result.Status != trigger.StatusRefused || result.Replayed {
		t.Fatalf("orphan result = %+v", result)
	}
	if count := fixture.countRows(t, scope, "executions", nil); count != 0 {
		t.Fatalf("an orphan refusal committed %d executions", count)
	}
	if events := fixture.store.Events(scope); len(events) != 1 || events[0].Envelope.Type != event.TypePipelineStartRefused {
		t.Fatalf("orphan events = %+v", events)
	}
}

func TestTriggerStartRollbackLeavesReceiptIncomplete(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)); err != nil {
		t.Fatalf("Select: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-1", trigger.StatusSelected, 1, 1)
	fixture.store.FailNextCommit(fault.New(fault.DependencyUnavailable, "commit did not start"))
	mustClass(t, func() error { _, err := fixture.triggers.Start(context.Background(), start); return err }(), fault.DependencyUnavailable)
	row, _ := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	if row.Values["completed_at"] != nil {
		t.Fatal("a failed start completed the receipt")
	}
	inProgress, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil || string(inProgress.Selection.YAML) != string(start.Selection.YAML) {
		t.Fatalf("a failed start lost the in-progress selection: %+v / %v", inProgress.Selection, err)
	}
	if _, ok := fixture.storedRow(t, scope, "executions", "execution-1"); ok {
		t.Fatal("a failed start committed an execution")
	}
}

func TestTriggerStartConcurrentCompletersOneWinner(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)); err != nil {
		t.Fatalf("Select: %v", err)
	}
	units := []trigger.StartUnit{
		fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-a", trigger.StatusSelected, 1, 1),
		fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", record, "execution-b", trigger.StatusSelected, 1, 1),
	}
	start := make(chan struct{})
	results := make([]trigger.Result, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for index := range units {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = fixture.triggers.Start(context.Background(), units[index])
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("completer %d: %v", index, err)
		}
	}
	if results[0].ExecutionID == "" || results[0].ExecutionID != results[1].ExecutionID {
		t.Fatalf("concurrent results = %+v", results)
	}
	replayed := 0
	for _, result := range results {
		if result.Replayed {
			replayed++
		}
	}
	if replayed != 1 {
		t.Fatalf("replayed results = %d; want exactly 1", replayed)
	}
	if count := fixture.countRows(t, scope, "executions", nil); count != 1 {
		t.Fatalf("concurrent completers committed %d executions; want 1", count)
	}
}

func TestTriggerSelectionUnfinishedListsIncompleteOrphan(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedService(t, scope, "svc-a", "enabled", 1)
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey("pipeline-gone", testEvent), contentHashA)
	if _, err := fixture.triggers.Select(context.Background(), trigger.SelectUnit{
		Owner:                   scope,
		PipelineID:              "pipeline-gone",
		EventID:                 testEvent,
		ExpectedPipelineVersion: 0,
		ExpectedBindingVersion:  0,
		Selection:               triggerSelection(scope, "pipeline-gone", testEvent),
		Receipt:                 record,
		Audit:                   []event.Audit{fixture.audit(scope, "pipeline.start")},
	}); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Lookup returns the stored in-progress selection even before completion.
	looked, found, err := fixture.triggers.Lookup(context.Background(), scope, "pipeline-gone", testEvent)
	if err != nil || !found {
		t.Fatalf("Lookup of an incomplete receipt = found %v, err %v", found, err)
	}
	if string(looked.Selection.YAML) != "kind: Pipeline\nversion: 1\n" || looked.Receipt.ID != record.ID {
		t.Fatalf("looked-up in-progress selection = %+v", looked)
	}
	unfinished, err := fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 1 {
		t.Fatalf("unfinished = %d; want the incomplete orphan", len(unfinished))
	}
	entry := unfinished[0]
	if entry.Selection.PipelineID != "pipeline-gone" || entry.Selection.EventID != testEvent || entry.Receipt.ID != record.ID {
		t.Fatalf("recovered incomplete selection = %+v", entry)
	}
	if string(entry.Selection.YAML) != "kind: Pipeline\nversion: 1\n" || entry.Selection.ServiceID != "svc-a" {
		t.Fatalf("recovered incomplete selection payload = %+v", entry.Selection)
	}
	// The recovered receipt lets Start record the orphan refusal and complete
	// the same key.
	refusal, err := trigger.NewStartRefusedEvent("event-refusal", scope, entry.Selection, 0, trigger.RefusalReasonDefinitionMissing, "svc-a", testNow)
	if err != nil {
		t.Fatalf("NewStartRefusedEvent: %v", err)
	}
	result, err := fixture.triggers.Start(context.Background(), trigger.StartUnit{
		Owner:      scope,
		PipelineID: "pipeline-gone",
		EventID:    testEvent,
		Selection:  entry.Selection,
		Status:     trigger.StatusRefused,
		At:         testNow,
		Receipt:    entry.Receipt,
		Audit:      []event.Audit{fixture.audit(scope, "pipeline.start")},
		Events:     []event.Envelope{refusal},
	})
	if err != nil {
		t.Fatalf("orphan Start: %v", err)
	}
	if result.Status != trigger.StatusRefused {
		t.Fatalf("orphan result = %+v", result)
	}
	unfinished, err = fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished after refusal: %v", err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("unfinished after refusal = %+v", unfinished)
	}
}

func TestTriggerSelectFirstSelectionSurvivesDefinitionEdit(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	fixture.seedPushEnvironment(t, scope)
	firstYAML := []byte("kind: Pipeline\nversion: 1\n")
	record := fixture.receiptFor(scope, startOperationName, selectionCallerKey(testPipeline, testEvent), contentHashA)
	selectUnit := fixture.selectUnit(scope, testPipeline, testEvent, "binding-1", record)
	selectUnit.Selection.YAML = firstYAML
	if _, err := fixture.triggers.Select(context.Background(), selectUnit); err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Edit the pipeline definition after the first selection.
	update := fixture.changeUnit(scope, pipeline.ChangeUpdate, testPipeline, "build", []byte("kind: Pipeline\nversion: 2\n"), 1, fixture.receipt("pipeline.update", "edit-after-select", contentHashA))
	update.PushTriggers = testPushTriggers()
	if _, err := fixture.pipelines.Change(context.Background(), update); err != nil {
		t.Fatalf("pipeline edit: %v", err)
	}
	// Lookup and Unfinished both return the FIRST exact bytes.
	looked, found, err := fixture.triggers.Lookup(context.Background(), scope, testPipeline, testEvent)
	if err != nil || !found {
		t.Fatalf("Lookup = found %v, err %v", found, err)
	}
	if string(looked.Selection.YAML) != string(firstYAML) {
		t.Fatalf("Lookup returned edited bytes %q; want the first %q", looked.Selection.YAML, firstYAML)
	}
	unfinished, err := fixture.triggers.Unfinished(context.Background(), scope, testEvent)
	if err != nil {
		t.Fatalf("Unfinished: %v", err)
	}
	if len(unfinished) != 1 || string(unfinished[0].Selection.YAML) != string(firstYAML) {
		t.Fatalf("unfinished = %+v; want the first selection", unfinished)
	}
	// Start resumes the persisted first bytes under the current version guards.
	admitted, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   looked.Selection.YAML,
		Inputs: looked.Selection.Inputs,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	start := fixture.triggerStartUnit(scope, testPipeline, testEvent, "binding-1", looked.Receipt, "execution-1", trigger.StatusSelected, 2, 1)
	start.Selection = looked.Selection
	start.Receipt = looked.Receipt
	start.Admission = pipeline.AdmissionResult{
		PipelineID:      testPipeline,
		PipelineVersion: 2,
		Snapshot:        admitted,
		Actions:         []pipeline.AdmittedAction{testAction(0)},
	}
	start.Events = []event.Envelope{fixture.event(scope, "execution-1", 1)}
	if _, err := fixture.triggers.Start(context.Background(), start); err != nil {
		t.Fatalf("Start: %v", err)
	}
	row, ok := fixture.scanRow(t, scope, "receipts", map[string]any{"id": string(record.ID)})
	if !ok {
		t.Fatal("the completed receipt is missing")
	}
	completed, err := decodeSelectionResult(bytesValue(row, "result"))
	if err != nil {
		t.Fatalf("decode completed receipt: %v", err)
	}
	if string(completed.Selection.YAML) != string(firstYAML) {
		t.Fatalf("completion stored edited bytes %q; want the first %q", completed.Selection.YAML, firstYAML)
	}
	if completed.ExecutionID != "execution-1" {
		t.Fatalf("completed receipt = %+v", completed)
	}
}
