package trigger_test

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// fixedNow is the injected clock value used by every test.
var fixedNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	now time.Time
}

func (clock fakeClock) Now() time.Time {
	if clock.now.IsZero() {
		return fixedNow
	}
	return clock.now
}

// fakeIDs supplies deterministic identities without reading process state.
type fakeIDs struct {
	mu        sync.Mutex
	event     int
	receipt   int
	execution int
}

func (ids *fakeIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.event++
	return id.EventID("event-" + strconv.Itoa(ids.event)), nil
}

func (ids *fakeIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.receipt++
	return id.ReceiptID("receipt-" + strconv.Itoa(ids.receipt)), nil
}

func (ids *fakeIDs) NewExecutionID() (id.ExecutionID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.execution++
	return id.ExecutionID("exec-" + strconv.Itoa(ids.execution)), nil
}

// fakeEvents is the committed event table. It returns rows in durable sequence
// order and honors the exclusive lower-bound cursor.
type fakeEvents struct {
	mu    sync.Mutex
	rows  []event.PendingEvent
	err   error
	after []event.Sequence
}

func (events *fakeEvents) Pending(_ context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	events.mu.Lock()
	defer events.mu.Unlock()
	events.after = append(events.after, after)
	if events.err != nil {
		return nil, events.err
	}
	if limit <= 0 {
		limit = len(events.rows)
	}
	var pending []event.PendingEvent
	for _, row := range events.rows {
		if row.Sequence <= after {
			continue
		}
		pending = append(pending, row)
		if len(pending) == limit {
			break
		}
	}
	return pending, nil
}

func (events *fakeEvents) setRows(rows ...event.PendingEvent) {
	events.mu.Lock()
	defer events.mu.Unlock()
	events.rows = append([]event.PendingEvent(nil), rows...)
}

func (events *fakeEvents) setError(err error) {
	events.mu.Lock()
	defer events.mu.Unlock()
	events.err = err
}

func (events *fakeEvents) cursors() []event.Sequence {
	events.mu.Lock()
	defer events.mu.Unlock()
	return append([]event.Sequence(nil), events.after...)
}

// fakeCandidates returns the current definitions and bindings for one scope.
// Tests may override fn to simulate scope filtering or read failures.
type fakeCandidates struct {
	mu         sync.Mutex
	candidates []trigger.Candidate
	fn         func(tenancy.Scope) ([]trigger.Candidate, error)
}

func (candidates *fakeCandidates) Candidates(_ context.Context, scope tenancy.Scope) ([]trigger.Candidate, error) {
	candidates.mu.Lock()
	fn := candidates.fn
	current := append([]trigger.Candidate(nil), candidates.candidates...)
	candidates.mu.Unlock()
	if fn != nil {
		return fn(scope)
	}
	return current, nil
}

func (candidates *fakeCandidates) set(values ...trigger.Candidate) {
	candidates.mu.Lock()
	defer candidates.mu.Unlock()
	candidates.candidates = append([]trigger.Candidate(nil), values...)
}

type selectionKey struct {
	tenant   id.TenantID
	project  id.ProjectID
	pipeline pipeline.PipelineID
	event    id.EventID
}

func keyFor(scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) selectionKey {
	return selectionKey{
		tenant:   scope.TenantID,
		project:  scope.ProjectID,
		pipeline: pipelineID,
		event:    eventID,
	}
}

// fakeSelections is both the first-selection reader and the SelectPort. Select
// is idempotent for one (pipeline, event) pair: an existing durable result is
// returned with Replayed set and is never overwritten.
type fakeSelections struct {
	mu          sync.Mutex
	records     map[selectionKey]trigger.Result
	selectCalls []trigger.SelectUnit
	selectFn    func(trigger.SelectUnit) (trigger.Result, error)
}

func (selections *fakeSelections) Lookup(_ context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) (trigger.Result, bool, error) {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	result, ok := selections.records[keyFor(scope, pipelineID, eventID)]
	if !ok {
		return trigger.Result{}, false, nil
	}
	result.Replayed = true
	return result, true, nil
}

// Unfinished lists the persisted selections of one event that still have no
// durable result, including a selection whose pipeline is no longer current.
func (selections *fakeSelections) Unfinished(_ context.Context, scope tenancy.Scope, eventID id.EventID) ([]trigger.Result, error) {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	var out []trigger.Result
	for key, result := range selections.records {
		if key.tenant != scope.TenantID || key.project != scope.ProjectID || key.event != eventID {
			continue
		}
		if result.Status != trigger.StatusSelected || result.ExecutionID != "" {
			continue
		}
		result.Replayed = true
		out = append(out, result)
	}
	sort.Slice(out, func(left, right int) bool {
		return out[left].Selection.PipelineID < out[right].Selection.PipelineID
	})
	return out, nil
}

func (selections *fakeSelections) Select(_ context.Context, unit trigger.SelectUnit) (trigger.Result, error) {
	selections.mu.Lock()
	selections.selectCalls = append(selections.selectCalls, unit)
	fn := selections.selectFn
	selections.mu.Unlock()
	if fn != nil {
		return fn(unit)
	}
	return selections.commit(unit), nil
}

// commit applies the idempotent first-selection write for one (pipeline,
// event) pair. An existing durable result is returned replayed and is never
// overwritten, mirroring the SelectPort contract.
func (selections *fakeSelections) commit(unit trigger.SelectUnit) trigger.Result {
	key := keyFor(unit.Owner, unit.PipelineID, unit.EventID)
	selections.mu.Lock()
	defer selections.mu.Unlock()
	if result, ok := selections.records[key]; ok {
		result.Replayed = true
		return result
	}
	result := trigger.Result{
		ReceiptID: unit.Receipt.ID,
		Status:    trigger.StatusSelected,
		Selection: unit.Selection,
		Receipt:   unit.Receipt,
	}
	selections.records[key] = result
	return result
}

func (selections *fakeSelections) setSelectFn(fn func(trigger.SelectUnit) (trigger.Result, error)) {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	selections.selectFn = fn
}

func (selections *fakeSelections) store(key selectionKey, result trigger.Result) {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	if selections.records == nil {
		selections.records = make(map[selectionKey]trigger.Result)
	}
	selections.records[key] = result
}

func (selections *fakeSelections) get(key selectionKey) (trigger.Result, bool) {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	result, ok := selections.records[key]
	return result, ok
}

func (selections *fakeSelections) selectCount() int {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	return len(selections.selectCalls)
}

func (selections *fakeSelections) selectCall(index int) trigger.SelectUnit {
	selections.mu.Lock()
	defer selections.mu.Unlock()
	return selections.selectCalls[index]
}

func newFakeSelections() *fakeSelections {
	return &fakeSelections{records: make(map[selectionKey]trigger.Result)}
}

// fakeAdmission records each admission request and returns a snapshot built
// from the exact inline YAML bytes the processor supplied.
type fakeAdmission struct {
	mu    sync.Mutex
	calls []pipeline.AdmitRequest
	fn    func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
}

func (admission *fakeAdmission) Admit(_ context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	admission.mu.Lock()
	admission.calls = append(admission.calls, request)
	fn := admission.fn
	admission.mu.Unlock()
	if fn != nil {
		return fn(request)
	}
	return defaultAdmit(request)
}

func defaultAdmit(request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   request.Definition.YAML,
		Inputs: request.Inputs,
	})
	if err != nil {
		return pipeline.AdmissionResult{}, err
	}
	return pipeline.AdmissionResult{
		PipelineID:      request.Definition.ID,
		PipelineVersion: request.Definition.Version,
		Snapshot:        snapshot,
	}, nil
}

func (admission *fakeAdmission) callCount() int {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return len(admission.calls)
}

func (admission *fakeAdmission) setFn(fn func(pipeline.AdmitRequest) (pipeline.AdmissionResult, error)) {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	admission.fn = fn
}

func (admission *fakeAdmission) call(index int) pipeline.AdmitRequest {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return admission.calls[index]
}

// fakeStart records each start unit and commits exactly one execution per
// (pipeline, event). A committed result is written back to the shared selection
// store before an injected lost-response error is returned.
type fakeStart struct {
	mu         sync.Mutex
	calls      []trigger.StartUnit
	results    map[selectionKey]trigger.Result
	selections *fakeSelections
	fn         func(trigger.StartUnit) (trigger.Result, error)
	executions int
	published  []event.Envelope
}

func (start *fakeStart) Start(_ context.Context, unit trigger.StartUnit) (trigger.Result, error) {
	start.mu.Lock()
	start.calls = append(start.calls, unit)
	fn := start.fn
	start.mu.Unlock()
	if fn != nil {
		return fn(unit)
	}
	return start.defaultStart(unit)
}

func (start *fakeStart) defaultStart(unit trigger.StartUnit) (trigger.Result, error) {
	key := keyFor(unit.Owner, unit.PipelineID, unit.EventID)
	start.mu.Lock()
	if result, ok := start.results[key]; ok {
		result.Replayed = true
		start.mu.Unlock()
		return result, nil
	}
	if unit.Status == trigger.StatusRefused {
		result := trigger.Result{
			Status:    trigger.StatusRefused,
			Selection: unit.Selection,
			Receipt:   unit.Receipt,
		}
		start.results[key] = result
		if len(unit.Events) == 1 {
			start.published = append(start.published, unit.Events[0])
		}
		start.mu.Unlock()
		start.persist(key, result)
		return result, nil
	}
	result := trigger.Result{
		ExecutionID: unit.NewExecutionID,
		ReceiptID:   unit.Receipt.ID,
		Status:      trigger.StatusSelected,
		Selection:   unit.Selection,
		Receipt:     unit.Receipt,
	}
	start.results[key] = result
	start.executions++
	if len(unit.Events) == 1 {
		start.published = append(start.published, unit.Events[0])
	}
	start.mu.Unlock()
	start.persist(key, result)
	return result, nil
}

// refuse commits a durable refusal. A StatusSelected unit is refused by the
// authority check inside Start, so the fake mirrors the StartPort contract: it
// never publishes the queued event and instead publishes the canonical
// pipeline.start_refused event built with the authority-refused reason.
func (start *fakeStart) refuse(unit trigger.StartUnit) trigger.Result {
	key := keyFor(unit.Owner, unit.PipelineID, unit.EventID)
	result := trigger.Result{
		Status:    trigger.StatusRefused,
		Selection: unit.Selection,
		Receipt:   unit.Receipt,
	}
	published := event.Envelope{}
	switch {
	case unit.Status == trigger.StatusRefused && len(unit.Events) == 1:
		published = unit.Events[0]
	case unit.Status == trigger.StatusSelected:
		refusal, err := trigger.NewStartRefusedEvent(
			id.EventID("refusal-"+string(unit.EventID)+"-"+string(unit.PipelineID)),
			unit.Owner, unit.Selection, unit.ExpectedPipelineVersion,
			trigger.RefusalReasonAuthorityRefused, unit.Initiator, unit.At,
		)
		if err == nil {
			published = refusal
		}
	}
	start.mu.Lock()
	start.results[key] = result
	if published.ID != "" {
		start.published = append(start.published, published)
	}
	start.mu.Unlock()
	start.persist(key, result)
	return result
}

func (start *fakeStart) persist(key selectionKey, result trigger.Result) {
	if start.selections != nil {
		start.selections.store(key, result)
	}
}

func (start *fakeStart) callCount() int {
	start.mu.Lock()
	defer start.mu.Unlock()
	return len(start.calls)
}

func (start *fakeStart) setFn(fn func(trigger.StartUnit) (trigger.Result, error)) {
	start.mu.Lock()
	defer start.mu.Unlock()
	start.fn = fn
}

func (start *fakeStart) executionIDs() []id.ExecutionID {
	start.mu.Lock()
	defer start.mu.Unlock()
	seen := make(map[id.ExecutionID]struct{}, len(start.results))
	ids := make([]id.ExecutionID, 0, len(start.results))
	for _, result := range start.results {
		if result.ExecutionID == "" {
			continue
		}
		if _, ok := seen[result.ExecutionID]; ok {
			continue
		}
		seen[result.ExecutionID] = struct{}{}
		ids = append(ids, result.ExecutionID)
	}
	return ids
}

func (start *fakeStart) call(index int) trigger.StartUnit {
	start.mu.Lock()
	defer start.mu.Unlock()
	return start.calls[index]
}

func (start *fakeStart) publishedEvents() []event.Envelope {
	start.mu.Lock()
	defer start.mu.Unlock()
	return append([]event.Envelope(nil), start.published...)
}

func (start *fakeStart) executionCount() int {
	start.mu.Lock()
	defer start.mu.Unlock()
	return start.executions
}

func newFakeStart(selections *fakeSelections) *fakeStart {
	return &fakeStart{results: make(map[selectionKey]trigger.Result), selections: selections}
}

// fakeCompletion records each durable completion cursor advance.
type fakeCompletion struct {
	mu    sync.Mutex
	calls []trigger.CompletionUnit
	err   error
}

func (completion *fakeCompletion) Complete(_ context.Context, unit trigger.CompletionUnit) error {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	if completion.err != nil {
		return completion.err
	}
	completion.calls = append(completion.calls, unit)
	return nil
}

func (completion *fakeCompletion) sequences() []event.Sequence {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	out := make([]event.Sequence, 0, len(completion.calls))
	for _, call := range completion.calls {
		out = append(out, call.Sequence)
	}
	return out
}

func (completion *fakeCompletion) has(sequence event.Sequence) bool {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	for _, call := range completion.calls {
		if call.Sequence == sequence {
			return true
		}
	}
	return false
}

func (completion *fakeCompletion) count() int {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	return len(completion.calls)
}

// reportData is the exact durable payload of one accepted git.ref.updated
// report: the verified Git subject fields (operation, resource, ref,
// expected_base, candidate) as one JSON object with sorted keys, so retries are
// byte-identical. The deleted flag is only present on a deletion report.
func reportData(resourceID, ref, base, candidate string, deleted bool) []byte {
	fields := map[string]any{
		"operation":     "git.ref.update",
		"resource":      resourceID,
		"ref":           ref,
		"expected_base": base,
		"candidate":     candidate,
	}
	if deleted {
		fields["deleted"] = true
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return payload
}

// pushEvent is one accepted git.ref.updated report in the exact shape the
// verified-report ingress commits for a human push: the reporting service is
// the source, the causation is the cited verification and there is no
// execution correlation.
func pushEvent(sequence event.Sequence, eventID, resourceID, reportingService, ref, base, candidate string) event.PendingEvent {
	return event.PendingEvent{
		Sequence: sequence,
		Event: event.Envelope{
			ID:         id.EventID(eventID),
			Type:       event.TypeVerifiedReport,
			ReportType: event.ReportType("git.ref.updated"),
			Source: event.Source{
				Kind:        event.SourceService,
				PrincipalID: id.PrincipalID(reportingService),
			},
			At:          fixedNow,
			Scope:       testScope(),
			SubjectID:   resourceID,
			CausationID: "verification-" + eventID,
			Data:        reportData(resourceID, ref, base, candidate, false),
		},
	}
}

func deletionEvent(sequence event.Sequence, eventID, resourceID, reportingService, ref string) event.PendingEvent {
	row := pushEvent(sequence, eventID, resourceID, reportingService, ref, "", "")
	row.Event.Data = reportData(resourceID, ref, "", "", true)
	return row
}

// executionOriginatedEvent is the same report for a push made with an
// execution credential: the ingress correlates it with the verified pusher's
// execution.
func executionOriginatedEvent(sequence event.Sequence, eventID, resourceID, reportingService, ref string) event.PendingEvent {
	row := pushEvent(sequence, eventID, resourceID, reportingService, ref, "base-1", "candidate-1")
	row.Event.CorrelationID = "execution-1"
	return row
}

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
}

func testBinding(scope tenancy.Scope, resourceID, reportingService string, version uint64) resource.Binding {
	return resource.Binding{
		ID:               resource.BindingID("binding-" + resourceID),
		ResourceID:       resource.ResourceID(resourceID),
		Scope:            scope,
		EnforcingService: serviceID(reportingService),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          version,
	}
}

func testCandidate(def pipeline.Definition, binding resource.Binding) trigger.Candidate {
	return trigger.Candidate{Definition: def, Binding: binding}
}

// testDefinition builds a current definition whose Push value is the exact
// parsed trigger and whose YAML is the exact authored bytes.
func testDefinition(pipelineID, yaml string, version uint64, repository, service string, refs []string) pipeline.Definition {
	return pipeline.Definition{
		ID:      pipeline.PipelineID(pipelineID),
		Name:    pipelineID,
		YAML:    []byte(yaml),
		Version: version,
		Push: &pipeline.PushTrigger{
			Repository: resource.ResourceID(repository),
			Service:    serviceID(service),
			Refs:       append([]string(nil), refs...),
		},
	}
}

func serviceID(value string) service.ServiceID {
	return service.ServiceID(value)
}

// pushYAML renders one valid API-posted pipeline. extraEnv changes the exact
// bytes without changing the push trigger so edit tests can compare bytes.
func pushYAML(repository, service string, refs, inputs []string, extraEnv string) string {
	yaml := "repository: " + repository + "\n" +
		"service: " + service + "\n" +
		"on:\n" +
		"  push: [" + join(refs) + "]\n"
	if len(inputs) > 0 {
		yaml += "inputs: [" + join(inputs) + "]\n"
	}
	if extraEnv != "" {
		yaml += "env:\n  - name: MARKER\n    value: " + extraEnv + "\n"
	}
	yaml += "actions:\n" +
		"  - name: test\n" +
		"    image: registry.example.com/tools:1\n" +
		"    commands:\n" +
		"      - echo ok\n"
	return yaml
}

func join(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += ", "
		}
		out += value
	}
	return out
}

func newProcessor(t *testing.T, events *fakeEvents, candidates *fakeCandidates, selections *fakeSelections, admission *fakeAdmission, start *fakeStart, completion *fakeCompletion) *trigger.Processor {
	t.Helper()
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         events,
		Candidates:     candidates,
		Selections:     selections,
		Select:         selections,
		Admission:      admission,
		Start:          start,
		Completion:     completion,
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
