package trigger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// receiptEntry is one common receipt of a (pipeline, event) pair under the
// receipt lifecycle: an incomplete insert carries no result, and the shared
// start completes the same key with a compare-and-set guarded on the prior
// state and content hash.
type receiptEntry struct {
	contentHash string
	completed   bool
	result      trigger.Result
}

// completionMarker is the durable event_consumptions row written by the
// completion port.
type completionMarker struct {
	consumer    string
	scope       tenancy.Scope
	sequence    event.Sequence
	completedAt time.Time
}

// receiptPorts is a faithful in-memory model of the declared receipt lifecycle
// and the completion marker. Select inserts the common receipt incomplete (or
// replays an equal one), Start completes the same key with a compare-and-set,
// and Complete writes one idempotent marker per event. It implements
// SelectionReader, SelectPort, StartPort and CompletionPort so the processor is
// exercised against the declared contract rather than a result cache.
type receiptPorts struct {
	mu sync.Mutex

	entries map[selectionKey]*receiptEntry
	markers map[id.EventID]completionMarker

	selectCalls   []trigger.SelectUnit
	startCalls    []trigger.StartUnit
	completeCalls []trigger.CompletionUnit
	executionIDs  []id.ExecutionID
	markerWrites  int

	// staleLookups makes that many completed lookups report an unfinished
	// result, modelling a read that races the durable start commit.
	staleLookups int
	// selectGate lets a test force concurrent callers to overlap before the
	// receipt compare-and-set.
	selectGate    chan struct{}
	selectArrived chan struct{}
}

func newReceiptPorts() *receiptPorts {
	return &receiptPorts{
		entries: make(map[selectionKey]*receiptEntry),
		markers: make(map[id.EventID]completionMarker),
	}
}

func (ports *receiptPorts) Lookup(_ context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) (trigger.Result, bool, error) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	entry, ok := ports.entries[keyFor(scope, pipelineID, eventID)]
	if !ok {
		return trigger.Result{}, false, nil
	}
	result := entry.result
	result.Replayed = true
	if ports.staleLookups > 0 && entry.completed {
		ports.staleLookups--
		result.ExecutionID = ""
	}
	return result, true, nil
}

func (ports *receiptPorts) Unfinished(_ context.Context, scope tenancy.Scope, eventID id.EventID) ([]trigger.Result, error) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	var out []trigger.Result
	for key, entry := range ports.entries {
		if key.tenant != scope.TenantID || key.project != scope.ProjectID || key.event != eventID || entry.completed {
			continue
		}
		result := entry.result
		result.Replayed = true
		out = append(out, result)
	}
	return out, nil
}

func (ports *receiptPorts) Select(_ context.Context, unit trigger.SelectUnit) (trigger.Result, error) {
	ports.mu.Lock()
	ports.selectCalls = append(ports.selectCalls, unit)
	gate, arrived := ports.selectGate, ports.selectArrived
	ports.mu.Unlock()

	if gate != nil {
		arrived <- struct{}{}
		<-gate
	}

	ports.mu.Lock()
	defer ports.mu.Unlock()
	key := keyFor(unit.Owner, unit.PipelineID, unit.EventID)
	entry, found := ports.entries[key]
	if found {
		if entry.contentHash != unit.Receipt.ContentHash {
			return trigger.Result{}, fault.New(fault.ChangedIdempotencyContent, "trigger receipt content changed")
		}
		result := entry.result
		result.Replayed = true
		return result, nil
	}
	// The selection inserts the common receipt incomplete: no result and no
	// execution yet. The shared start completes this exact key.
	result := trigger.Result{
		ReceiptID: unit.Receipt.ID,
		Status:    trigger.StatusSelected,
		Selection: unit.Selection,
		Receipt:   unit.Receipt,
	}
	ports.entries[key] = &receiptEntry{
		contentHash: unit.Receipt.ContentHash,
		completed:   false,
		result:      result,
	}
	return result, nil
}

func (ports *receiptPorts) Start(_ context.Context, unit trigger.StartUnit) (trigger.Result, error) {
	ports.mu.Lock()
	ports.startCalls = append(ports.startCalls, unit)
	key := keyFor(unit.Owner, unit.PipelineID, unit.EventID)
	entry, found := ports.entries[key]
	ports.mu.Unlock()

	ports.mu.Lock()
	defer ports.mu.Unlock()
	if !found {
		return trigger.Result{}, fault.New(fault.VersionConflict, "trigger receipt is missing")
	}
	if entry.contentHash != unit.Receipt.ContentHash {
		return trigger.Result{}, fault.New(fault.ChangedIdempotencyContent, "trigger receipt content changed")
	}
	if entry.completed {
		// Equal-key replay of a completed receipt applies nothing and returns
		// the original result.
		result := entry.result
		result.Replayed = true
		return result, nil
	}
	var result trigger.Result
	if unit.Status == trigger.StatusRefused {
		result = trigger.Result{
			Status:    trigger.StatusRefused,
			Selection: unit.Selection,
			Receipt:   unit.Receipt,
		}
	} else {
		result = trigger.Result{
			ExecutionID: unit.NewExecutionID,
			ReceiptID:   unit.Receipt.ID,
			Status:      trigger.StatusSelected,
			Selection:   unit.Selection,
			Receipt:     unit.Receipt,
		}
	}
	entry.completed = true
	entry.result = result
	if result.ExecutionID != "" {
		ports.executionIDs = append(ports.executionIDs, result.ExecutionID)
	}
	return result, nil
}

func (ports *receiptPorts) Complete(_ context.Context, unit trigger.CompletionUnit) error {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.completeCalls = append(ports.completeCalls, unit)
	if marker, ok := ports.markers[unit.EventID]; ok {
		if marker.sequence != unit.Sequence || marker.scope != unit.Owner {
			return fault.New(fault.VersionConflict, "event completion marker changed")
		}
		// Completing the same event twice applies nothing and is not an error.
		return nil
	}
	ports.markers[unit.EventID] = completionMarker{
		consumer:    trigger.CompletionConsumer,
		scope:       unit.Owner,
		sequence:    unit.Sequence,
		completedAt: fixedNow,
	}
	ports.markerWrites++
	return nil
}

// seedIncomplete persists an unfinished first selection and its incomplete
// common receipt, as a crashed earlier pass would leave behind.
func (ports *receiptPorts) seedIncomplete(scope tenancy.Scope, selection trigger.Selection, record receipt.Record) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.entries[keyFor(scope, selection.PipelineID, selection.EventID)] = &receiptEntry{
		contentHash: record.ContentHash,
		completed:   false,
		result: trigger.Result{
			ReceiptID: record.ID,
			Status:    trigger.StatusSelected,
			Selection: selection,
			Receipt:   record,
		},
	}
}

func (ports *receiptPorts) entry(scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) (receiptEntry, bool) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	entry, ok := ports.entries[keyFor(scope, pipelineID, eventID)]
	if !ok {
		return receiptEntry{}, false
	}
	return *entry, true
}

func (ports *receiptPorts) marker(eventID id.EventID) (completionMarker, bool) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	marker, ok := ports.markers[eventID]
	return marker, ok
}

func (ports *receiptPorts) markerExists(eventID id.EventID, sequence event.Sequence) bool {
	marker, ok := ports.marker(eventID)
	return ok && marker.sequence == sequence
}

func (ports *receiptPorts) selectCall(index int) trigger.SelectUnit {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return ports.selectCalls[index]
}

func (ports *receiptPorts) startCall(index int) trigger.StartUnit {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return ports.startCalls[index]
}

func (ports *receiptPorts) selectCount() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.selectCalls)
}

func (ports *receiptPorts) startCount() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.startCalls)
}

func (ports *receiptPorts) completeCount() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.completeCalls)
}

func (ports *receiptPorts) executionCount() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.executionIDs)
}

func (ports *receiptPorts) markerWriteCount() int {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return ports.markerWrites
}

func (ports *receiptPorts) winner() (id.ExecutionID, bool) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	if len(ports.executionIDs) != 1 {
		return "", false
	}
	return ports.executionIDs[0], true
}

// newReceiptFixture wires one processor against the receipt model.
func newReceiptFixture(t *testing.T) (*fakeEvents, *fakeCandidates, *fakeAdmission, *receiptPorts, *trigger.Processor) {
	t.Helper()
	events := &fakeEvents{}
	candidates := &fakeCandidates{}
	admission := &fakeAdmission{}
	ports := newReceiptPorts()
	return events, candidates, admission, ports, newReceiptProcessor(t, events, candidates, admission, ports)
}

func newReceiptProcessor(t *testing.T, events *fakeEvents, candidates *fakeCandidates, admission *fakeAdmission, ports *receiptPorts) *trigger.Processor {
	t.Helper()
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         events,
		Candidates:     candidates,
		Selections:     ports,
		Select:         ports,
		Admission:      admission,
		Start:          ports,
		Completion:     ports,
		Clock:          fakeClock{},
		IDs:            &fakeIDs{},
		BatchSize:      16,
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return processor
}

// TestSelectionInsertsIncompleteReceiptAndStartCompletesIt pins the receipt
// lifecycle: the selection unit declares the insert mode and persists an
// incomplete receipt, and the shared start declares the completion mode on the
// exact same key and content hash.
func TestSelectionInsertsIncompleteReceiptAndStartCompletesIt(t *testing.T) {
	events, candidates, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasExecution(result.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; want exec-1", result.Started)
	}

	selectUnit := ports.selectCall(0)
	if got := selectUnit.ReceiptMode(); got != trigger.ReceiptInsert {
		t.Fatalf("select receipt mode = %q; want %q", got, trigger.ReceiptInsert)
	}
	if len(selectUnit.Receipt.Result) != 0 || selectUnit.Receipt.ExecutionID != "" {
		t.Fatalf("selection receipt is not incomplete: %+v", selectUnit.Receipt)
	}
	if selectUnit.Receipt.ContentHash == "" {
		t.Fatal("selection receipt has no content hash")
	}

	startUnit := ports.startCall(0)
	if got := startUnit.ReceiptMode(); got != trigger.ReceiptComplete {
		t.Fatalf("start receipt mode = %q; want %q", got, trigger.ReceiptComplete)
	}
	if startUnit.Receipt.Key != selectUnit.Receipt.Key {
		t.Fatalf("start receipt key = %+v; want the selection key %+v", startUnit.Receipt.Key, selectUnit.Receipt.Key)
	}
	if startUnit.Receipt.ContentHash != selectUnit.Receipt.ContentHash {
		t.Fatalf("start content hash = %q; want the selection hash %q", startUnit.Receipt.ContentHash, selectUnit.Receipt.ContentHash)
	}

	entry, ok := ports.entry(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	if !ok || !entry.completed {
		t.Fatalf("common receipt = %+v (ok=%v); want it completed", entry, ok)
	}
	if entry.result.ExecutionID != id.ExecutionID("exec-1") {
		t.Fatalf("completed receipt execution = %q; want exec-1", entry.result.ExecutionID)
	}
	if !ports.markerExists(id.EventID("event-1"), 1) {
		t.Fatal("the settled event has no completion marker")
	}
}

// TestCompletedSelectionReplayAppliesNothing proves a second pass returns the
// original execution, never applies the units again and never consumes a fresh
// execution identity.
func TestCompletedSelectionReplayAppliesNothing(t *testing.T) {
	events, candidates, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	first, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if !hasExecution(first.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("first started = %v; want exec-1", first.Started)
	}

	second, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if len(second.Started) != 1 || second.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("second started = %v; want the original execution exec-1", second.Started)
	}
	if len(second.Refused) != 0 {
		t.Fatalf("second refused = %v; a replay must not refuse", second.Refused)
	}
	if ports.selectCount() != 1 || ports.startCount() != 1 {
		t.Fatalf("select/start calls = %d/%d; a completed selection must apply nothing", ports.selectCount(), ports.startCount())
	}
	if ports.executionCount() != 1 {
		t.Fatalf("executions = %d; a replay must not consume a fresh execution identity", ports.executionCount())
	}
}

// TestStaleLookupDoesNotConsumeExecutionIdentity covers a read that still
// reports a started selection as unfinished: the retry must re-read the durable
// result before allocating an identity, then return the original execution
// without invoking Start again.
func TestStaleLookupDoesNotConsumeExecutionIdentity(t *testing.T) {
	events, candidates, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	if _, err := processor.Process(context.Background()); err != nil {
		t.Fatalf("first Process: %v", err)
	}
	ports.mu.Lock()
	ports.staleLookups = 1
	ports.mu.Unlock()

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if len(result.Started) != 1 || result.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("started = %v; want the original execution exec-1", result.Started)
	}
	if ports.startCount() != 1 {
		t.Fatalf("Start called %d times; a stale read must not re-run the completed start", ports.startCount())
	}
	if ports.executionCount() != 1 {
		t.Fatalf("executions = %d; a stale read must not consume a fresh identity", ports.executionCount())
	}
}

// TestConcurrentCompletersHaveOneWinner runs two processors over the same
// durable receipt store with overlapping selections: the first completion wins,
// the second replays it, and the event is completed once.
func TestConcurrentCompletersHaveOneWinner(t *testing.T) {
	events, candidates, admission, ports, first := newReceiptFixture(t)
	second := newReceiptProcessor(t, events, candidates, admission, ports)
	scope := testScope()
	candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	gate := make(chan struct{})
	ports.mu.Lock()
	ports.selectGate = gate
	ports.selectArrived = make(chan struct{}, 2)
	ports.mu.Unlock()

	processors := []*trigger.Processor{first, second}
	results := make([]trigger.ProcessResult, len(processors))
	errs := make([]error, len(processors))
	var wait sync.WaitGroup
	wait.Add(len(processors))
	for index := range processors {
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = processors[index].Process(context.Background())
		}(index)
	}

	deadline := time.After(2 * time.Second)
	for arrived := range processors {
		select {
		case <-ports.selectArrived:
		case <-deadline:
			close(gate)
			wait.Wait()
			t.Fatalf("only %d of %d processors reached Select", arrived, len(processors))
		}
	}
	close(gate)
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Process %d: %v", index, err)
		}
	}
	if ports.executionCount() != 1 {
		t.Fatalf("executions = %d; concurrent completers must produce one execution", ports.executionCount())
	}
	winner, ok := ports.winner()
	if !ok {
		t.Fatalf("winner = %q (ok=%v); want exactly one completed execution", winner, ok)
	}
	for index, result := range results {
		if len(result.Refused) != 0 {
			t.Fatalf("result %d refused = %v; concurrent completers must not refuse", index, result.Refused)
		}
		if len(result.Started) != 1 || result.Started[0] != winner {
			t.Fatalf("result %d started = %v; want the single winner %q", index, result.Started, winner)
		}
	}
	if ports.markerWriteCount() != 1 {
		t.Fatalf("completion marker writes = %d; want one idempotent marker", ports.markerWriteCount())
	}
}

// TestChangedReceiptContentIsRefused pins the compare-and-set content guard: a
// completion whose content hash differs from the persisted receipt is refused
// with changed_idempotency_content and leaves the receipt incomplete.
func TestChangedReceiptContentIsRefused(t *testing.T) {
	_, _, _, ports, _ := newReceiptFixture(t)
	scope := testScope()
	selection := trigger.Selection{
		PipelineID: "pipeline-a",
		EventID:    "event-1",
		YAML:       []byte(pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, "")),
		ServiceID:  "svc-a",
	}
	record := receipt.Record{
		ID:          id.ReceiptID("receipt-1"),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "svc-a", Operation: "pipeline.start", CallerKey: "pipeline-a/event-1"},
		ContentHash: "sha256:persisted",
		At:          fixedNow,
	}
	ports.seedIncomplete(scope, selection, record)

	_, err := ports.Start(context.Background(), trigger.StartUnit{
		Owner:          scope,
		PipelineID:     "pipeline-a",
		EventID:        "event-1",
		Selection:      selection,
		Status:         trigger.StatusSelected,
		NewExecutionID: id.ExecutionID("exec-9"),
		Receipt: receipt.Record{
			ID:          id.ReceiptID("receipt-2"),
			Key:         record.Key,
			ContentHash: "sha256:changed",
			At:          fixedNow,
		},
	})
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("completion error = %v; want class %q", err, fault.ChangedIdempotencyContent)
	}
	entry, found := ports.entry(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	if !found || entry.completed {
		t.Fatalf("receipt = %+v (found=%v); changed content must not complete it", entry, found)
	}
	if ports.executionCount() != 0 {
		t.Fatalf("executions = %d; changed content must never start work", ports.executionCount())
	}
}

// TestCompletionMarkerIsIdempotent proves the event_consumptions marker is
// keyed by the event identity under the stable trigger consumer, carries the
// durable sequence, and is written once across repeated passes.
func TestCompletionMarkerIsIdempotent(t *testing.T) {
	events, candidates, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	candidates.set(testCandidate(
		testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	for pass := range 2 {
		if _, err := processor.Process(context.Background()); err != nil {
			t.Fatalf("Process pass %d: %v", pass, err)
		}
	}
	if ports.completeCount() != 2 {
		t.Fatalf("Complete calls = %d; the rescanned row must be completed again", ports.completeCount())
	}
	if ports.markerWriteCount() != 1 {
		t.Fatalf("completion marker writes = %d; want one idempotent marker", ports.markerWriteCount())
	}
	marker, ok := ports.marker(id.EventID("event-1"))
	if !ok {
		t.Fatal("event-1 has no completion marker")
	}
	if marker.consumer != trigger.CompletionConsumer {
		t.Fatalf("marker consumer = %q; want %q", marker.consumer, trigger.CompletionConsumer)
	}
	if marker.sequence != 1 {
		t.Fatalf("marker sequence = %d; want the durable sequence 1", marker.sequence)
	}
	if !marker.completedAt.Equal(fixedNow) {
		t.Fatalf("marker completed_at = %v; want the injected clock %v", marker.completedAt, fixedNow)
	}
	if marker.scope != scope {
		t.Fatalf("marker scope = %+v; want the event scope %+v", marker.scope, scope)
	}
}

// TestUnfinishedSelectionIsResumed proves a persisted incomplete selection is
// resumed from its exact bytes and its common receipt is completed.
func TestUnfinishedSelectionIsResumed(t *testing.T) {
	events, candidates, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	yaml := pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, "")
	candidates.set(testCandidate(
		testDefinition("pipeline-a", yaml, 1, "repo-1", "svc-a", []string{"refs/heads/main"}),
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	ports.seedIncomplete(scope, trigger.Selection{
		PipelineID: "pipeline-a",
		EventID:    "event-1",
		YAML:       []byte(yaml),
		ServiceID:  "svc-a",
	}, receipt.Record{
		ID:          id.ReceiptID("receipt-1"),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "svc-a", Operation: "pipeline.start", CallerKey: "pipeline-a/event-1"},
		ContentHash: "sha256:persisted",
		At:          fixedNow,
	})

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasExecution(result.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; an unfinished selection must be resumed", result.Started)
	}
	if ports.startCount() != 1 {
		t.Fatalf("Start calls = %d; want one resume", ports.startCount())
	}
	if got := string(ports.startCall(0).Selection.YAML); got != yaml {
		t.Fatalf("resumed YAML = %q; want the persisted exact bytes", got)
	}
	entry, ok := ports.entry(scope, pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	if !ok || !entry.completed || entry.result.ExecutionID != id.ExecutionID("exec-1") {
		t.Fatalf("resumed receipt = %+v (ok=%v); want a completed exec-1", entry, ok)
	}
	if !ports.markerExists(id.EventID("event-1"), 1) {
		t.Fatal("the resumed event has no completion marker")
	}
}

// TestUnfinishedOrphanSelectionIsRefused proves a persisted selection whose
// current pipeline definition is gone is refused through Start with zero
// expected versions instead of being skipped, and the event settles.
func TestUnfinishedOrphanSelectionIsRefused(t *testing.T) {
	events, _, _, ports, processor := newReceiptFixture(t)
	scope := testScope()
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	ports.seedIncomplete(scope, trigger.Selection{
		PipelineID: "pipeline-gone",
		EventID:    "event-1",
		YAML:       []byte(pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, "")),
		ServiceID:  "svc-a",
	}, receipt.Record{
		ID:          id.ReceiptID("receipt-1"),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: "svc-a", Operation: "pipeline.start", CallerKey: "pipeline-gone/event-1"},
		ContentHash: "sha256:persisted",
		At:          fixedNow,
	})

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; an orphaned selection must be refused, not skipped", result.Refused)
	}
	if ports.startCount() != 1 {
		t.Fatalf("Start calls = %d; want one orphan refusal", ports.startCount())
	}
	unit := ports.startCall(0)
	if unit.Status != trigger.StatusRefused {
		t.Fatalf("orphan start status = %q; want refused", unit.Status)
	}
	if unit.ExpectedPipelineVersion != 0 || unit.ExpectedBindingVersion != 0 {
		t.Fatalf("orphan expected versions = %d/%d; want zero", unit.ExpectedPipelineVersion, unit.ExpectedBindingVersion)
	}
	if len(unit.Events) != 1 || unit.Events[0].Type != event.TypePipelineStartRefused {
		t.Fatalf("orphan events = %+v; want one pipeline.start_refused", unit.Events)
	}
	if ports.executionCount() != 0 {
		t.Fatalf("executions = %d; an orphan must not queue work", ports.executionCount())
	}
	entry, ok := ports.entry(scope, pipeline.PipelineID("pipeline-gone"), id.EventID("event-1"))
	if !ok || !entry.completed || entry.result.Status != trigger.StatusRefused {
		t.Fatalf("orphan receipt = %+v (ok=%v); want a completed refusal", entry, ok)
	}
	if !ports.markerExists(id.EventID("event-1"), 1) {
		t.Fatal("the refused event has no completion marker")
	}
}

// TestPushProjectionMatchesWithoutParsedTrigger proves the processor matches
// the persisted on.push projection supplied by CandidateReader instead of
// requiring the parsed single trigger, and carries it into admission.
func TestPushProjectionMatchesWithoutParsedTrigger(t *testing.T) {
	events, candidates, admission, ports, processor := newReceiptFixture(t)
	scope := testScope()
	yaml := pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, "")
	candidates.set(testCandidate(
		pipeline.Definition{
			ID:      pipeline.PipelineID("pipeline-a"),
			Name:    "pipeline-a",
			YAML:    []byte(yaml),
			Version: 1,
			PushTriggers: []pipeline.PushTrigger{
				{Repository: "repo-2", Service: "svc-a", Refs: []string{"refs/heads/main"}},
				{Repository: "repo-1", Service: "svc-a", Refs: []string{"refs/heads/other", "refs/heads/main"}},
			},
		},
		testBinding(scope, "repo-1", "git-svc", 1),
	))
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !hasExecution(result.Started, id.ExecutionID("exec-1")) {
		t.Fatalf("started = %v; the persisted on.push projection must match", result.Started)
	}
	if ports.selectCount() != 1 {
		t.Fatalf("Select calls = %d; want the projected trigger to select", ports.selectCount())
	}
	projection := admission.call(0).Definition.PushTriggers
	if len(projection) != 2 || projection[1].Repository != "repo-1" {
		t.Fatalf("admission push projection = %+v; want the persisted projection", projection)
	}
}

// TestPushProjectionRejectsNonMatchingTriggers proves the projection match is
// still exact: a different repository, a missing service or an unlisted ref
// selects nothing.
func TestPushProjectionRejectsNonMatchingTriggers(t *testing.T) {
	cases := []struct {
		name     string
		triggers []pipeline.PushTrigger
	}{
		{name: "different repository", triggers: []pipeline.PushTrigger{{Repository: "repo-2", Service: "svc-a", Refs: []string{"refs/heads/main"}}}},
		{name: "missing service", triggers: []pipeline.PushTrigger{{Repository: "repo-1", Refs: []string{"refs/heads/main"}}}},
		{name: "unlisted ref", triggers: []pipeline.PushTrigger{{Repository: "repo-1", Service: "svc-a", Refs: []string{"refs/heads/other"}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events, candidates, _, ports, processor := newReceiptFixture(t)
			scope := testScope()
			candidates.set(testCandidate(
				pipeline.Definition{
					ID:           pipeline.PipelineID("pipeline-a"),
					Name:         "pipeline-a",
					YAML:         []byte(pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, "")),
					Version:      1,
					PushTriggers: testCase.triggers,
				},
				testBinding(scope, "repo-1", "git-svc", 1),
			))
			events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))

			result, err := processor.Process(context.Background())
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if len(result.Started) != 0 || ports.selectCount() != 0 {
				t.Fatalf("started/select = %v/%d; a non-matching projection must select nothing", result.Started, ports.selectCount())
			}
		})
	}
}
