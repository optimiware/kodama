package trigger

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// defaultBatchSize bounds one processor pass when the caller does not set one.
const defaultBatchSize = 100

// startOperation is the shared operation a push start commits against.
const startOperation = authorize.OperationName("pipeline.start")

// eventReadOperation is the registry operation the automatic service must hold
// to read the accepted push event it acts on.
const eventReadOperation = authorize.OperationName("event.read")

// ProcessorConfig is the complete push-processor composition. It owns the
// committed event-table reader, the current candidate reader, the first
// selection store, the shared admission/start seams, the installation default
// action timeout and the durable completion cursor. DefaultTimeout is required
// and positive: it is the fallback every admitted action without an explicit
// timeout resolves to, so a push can never reach admission with a zero
// installation default and be permanently refused. It deliberately has no
// broker, subscription or dispatch queue: the event table is truth and NATS is
// never required.
type ProcessorConfig struct {
	Events         EventReader
	Candidates     CandidateReader
	Selections     SelectionReader
	Select         SelectPort
	Admission      AdmissionPort
	Start          StartPort
	Completion     CompletionPort
	Clock          Clock
	IDs            IDSource
	BatchSize      int
	DefaultTimeout time.Duration
	Idle           func(context.Context) error
}

// Processor scans committed accepted events and drives the exact push
// selection/start commit units. It keeps no in-memory work cursor: every pass
// rescans from the zero sequence, so a committed row is never skipped and a
// retry resumes from the persisted first selection and its receipt.
type Processor struct {
	events         EventReader
	candidates     CandidateReader
	selections     SelectionReader
	selectPort     SelectPort
	admission      AdmissionPort
	startPort      StartPort
	completion     CompletionPort
	clock          Clock
	ids            IDSource
	batchSize      int
	defaultTimeout time.Duration
	idle           func(context.Context) error

	// startMu orders execution-id allocation with the idempotent start commit.
	// Two concurrent passes may select and admit at the same time, but only one
	// can allocate the first execution identity before its start is persisted.
	startMu sync.Mutex
}

// NewProcessor validates the required dependencies and returns a processor. A
// non-positive batch size uses the default bound and an absent Idle simply ends
// Run after a pass with no more work.
func NewProcessor(config ProcessorConfig) (*Processor, error) {
	switch {
	case config.Events == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires an event reader")
	case config.Candidates == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a candidate reader")
	case config.Selections == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a selection reader")
	case config.Select == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a selection port")
	case config.Admission == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires an admission port")
	case config.Start == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a start port")
	case config.Completion == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a completion port")
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires an id source")
	case config.DefaultTimeout <= 0:
		return nil, fault.New(fault.InternalFailure, "trigger processor requires a positive default action timeout")
	}
	batchSize := config.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	return &Processor{
		events:         config.Events,
		candidates:     config.Candidates,
		selections:     config.Selections,
		selectPort:     config.Select,
		admission:      config.Admission,
		startPort:      config.Start,
		completion:     config.Completion,
		clock:          config.Clock,
		ids:            config.IDs,
		batchSize:      batchSize,
		defaultTimeout: config.DefaultTimeout,
		idle:           config.Idle,
	}, nil
}

// Run processes committed events until the context is canceled. A pass with
// more work continues immediately; otherwise the injected idle wait runs. Run
// never owns a goroutine, so canceling the context always joins it.
func (processor *Processor) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := processor.Process(ctx)
		if err != nil {
			return err
		}
		if result.More && len(result.Deferred) == 0 {
			continue
		}
		if processor.idle == nil {
			return nil
		}
		if err := processor.idle(ctx); err != nil {
			return err
		}
	}
}

// Process runs one bounded pass over the committed event table. Rows are read
// from the zero sequence in durable order and processing stops at the first
// event whose relevant selections are not all durable, so a later event can
// never be completed ahead of an unfinished earlier one.
func (processor *Processor) Process(ctx context.Context) (ProcessResult, error) {
	rows, err := processor.events.Pending(ctx, 0, processor.batchSize)
	if err != nil {
		return ProcessResult{}, err
	}
	result := ProcessResult{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome, err := processor.processEvent(ctx, row.Event)
		if err != nil {
			return result, err
		}
		result.Started = append(result.Started, outcome.started...)
		result.Refused = append(result.Refused, outcome.refused...)
		if outcome.deferred {
			result.Deferred = append(result.Deferred, row.Event.ID)
			break
		}
		if err := processor.completion.Complete(ctx, CompletionUnit{
			Owner:    row.Event.Scope,
			Sequence: row.Sequence,
			EventID:  row.Event.ID,
		}); err != nil {
			return result, err
		}
		result.Completed = append(result.Completed, row.Sequence)
	}
	if len(result.Deferred) == 0 && len(rows) >= processor.batchSize {
		result.More = true
	}
	return result, nil
}

// eventOutcome is the durable outcome of one event row.
type eventOutcome struct {
	started  []id.ExecutionID
	refused  []id.EventID
	deferred bool
}

// processEvent handles one committed row. A row that is not an accepted
// verified push report settles without work; an accepted report settles only
// after every relevant candidate has a durable result.
func (processor *Processor) processEvent(ctx context.Context, envelope event.Envelope) (eventOutcome, error) {
	report, accepted := acceptedReport(envelope)
	if !accepted {
		return eventOutcome{}, nil
	}
	candidates, err := processor.candidates.Candidates(ctx, envelope.Scope)
	if err != nil {
		return eventOutcome{deferred: true}, nil
	}
	outcome := eventOutcome{}
	handled := make(map[pipeline.PipelineID]struct{}, len(candidates))
	for _, candidate := range candidates {
		// Only the binding the push was reported through is a candidate for
		// this event. Filtering before the stored-selection lookup keeps
		// another binding of the same repository from resuming, authorizing
		// or locking the start; a selection whose reporting binding is gone
		// stays unhandled and is refused as an orphan below.
		if !reportingBinding(candidate.Binding, envelope, report) {
			continue
		}
		handled[candidate.Definition.ID] = struct{}{}
		deferred, err := processor.processCandidate(ctx, envelope, report, candidate, &outcome)
		if err != nil {
			return outcome, err
		}
		if deferred {
			outcome.deferred = true
		}
	}
	// A selection committed for a pipeline that has since been deleted is not
	// returned by CandidateReader, so it would otherwise never be looked up
	// again. List every persisted selection of the event that still has no
	// durable result and permanently refuse the ones no current candidate
	// covers; the event cannot complete while any of them is unfinished.
	unfinished, err := processor.selections.Unfinished(ctx, envelope.Scope, envelope.ID)
	if err != nil {
		outcome.deferred = true
		return outcome, nil
	}
	for _, stored := range unfinished {
		if _, ok := handled[stored.Selection.PipelineID]; ok {
			continue
		}
		deferred, err := processor.refuseOrphan(ctx, envelope, stored, &outcome)
		if err != nil {
			return outcome, err
		}
		if deferred {
			outcome.deferred = true
		}
	}
	return outcome, nil
}

// processCandidate consults the persisted first selection before any current
// definition, binding or service is used. Only a candidate with no persisted
// selection is matched against the current configuration.
func (processor *Processor) processCandidate(ctx context.Context, envelope event.Envelope, report Report, candidate Candidate, outcome *eventOutcome) (bool, error) {
	stored, found, err := processor.selections.Lookup(ctx, envelope.Scope, candidate.Definition.ID, envelope.ID)
	if err != nil {
		return true, nil
	}
	if found {
		return processor.resume(ctx, envelope, candidate, stored, outcome)
	}
	pushTrigger, matched := matchingTrigger(candidate, envelope, report)
	if !matched {
		return false, nil
	}
	unit, selection, record, err := processor.newSelection(envelope, report, candidate, pushTrigger.Service)
	if err != nil {
		return true, nil
	}
	selected, err := processor.selectPort.Select(ctx, unit)
	if err != nil {
		// Select never commits a refusal. A permanent selection fault is
		// recorded and published exactly once through the shared start commit,
		// like every other permanent push refusal; only a transient or
		// uncertain failure retains the event for a later pass.
		if reason, permanent := permanentRefusalReason(err); permanent {
			return processor.refuse(ctx, envelope, candidate, selection, record, reason, outcome)
		}
		return true, nil
	}
	selection, record = adopt(selected, selection, record)
	switch {
	case selected.Status == StatusRefused:
		// A refusal is committed only through StartPort, so a select-time
		// refusal is routed through the same start commit that publishes it.
		return processor.refuse(ctx, envelope, candidate, selection, record, RefusalReasonSelectionRefused, outcome)
	case selected.Status == StatusDeferred:
		// The selection commit reported a transient dependency failure, so the
		// first selection is not durable yet. Retain the event instead of
		// admitting or starting work without a persisted intent.
		return true, nil
	case selected.ExecutionID != "":
		outcome.started = append(outcome.started, selected.ExecutionID)
		return false, nil
	}
	return processor.start(ctx, envelope, candidate, selection, record, outcome)
}

// resume continues a persisted first selection. A durable selected execution
// or refusal is reported as-is; an unfinished selection is resumed from its
// exact persisted YAML, service, inputs and common receipt.
func (processor *Processor) resume(ctx context.Context, envelope event.Envelope, candidate Candidate, stored Result, outcome *eventOutcome) (bool, error) {
	switch {
	case stored.Status == StatusRefused:
		outcome.refused = append(outcome.refused, envelope.ID)
		return false, nil
	case stored.ExecutionID != "":
		outcome.started = append(outcome.started, stored.ExecutionID)
		return false, nil
	}
	selection := stored.Selection
	record := stored.Receipt
	if record.ID == "" {
		record.ID = stored.ReceiptID
	}
	if len(selection.YAML) == 0 {
		// An unfinished selection without persisted bytes cannot be resumed
		// safely; retain the event rather than inventing a definition.
		return true, nil
	}
	return processor.start(ctx, envelope, candidate, selection, record, outcome)
}

// refuseOrphan records the permanent refusal for a persisted selection whose
// current pipeline definition or binding no longer exists. The candidate is
// zero because there is no current version or versioned authority target, but
// the automatic-service membership, enablement and event-read reads still
// apply. The persisted first selection and its common receipt are preserved.
func (processor *Processor) refuseOrphan(ctx context.Context, envelope event.Envelope, stored Result, outcome *eventOutcome) (bool, error) {
	selection := stored.Selection
	record := stored.Receipt
	if record.ID == "" {
		record.ID = stored.ReceiptID
	}
	unit, err := processor.startUnit(envelope, Candidate{}, selection, record, pipeline.AdmissionResult{}, StatusRefused, "", RefusalReasonDefinitionMissing)
	if err != nil {
		return true, nil
	}
	started, err := processor.startPort.Start(ctx, unit)
	if err != nil {
		return true, nil
	}
	if started.Status == StatusRefused {
		outcome.refused = append(outcome.refused, envelope.ID)
		return false, nil
	}
	if started.ExecutionID != "" {
		outcome.started = append(outcome.started, started.ExecutionID)
		return false, nil
	}
	return true, nil
}

// start admits the persisted exact bytes and invokes the shared start commit.
// A permanent admission fault is recorded as a durable refusal; every other
// failure retains the persisted selection for a later pass.
func (processor *Processor) start(ctx context.Context, envelope event.Envelope, candidate Candidate, selection Selection, record receipt.Record, outcome *eventOutcome) (bool, error) {
	// The request carries both the identity and the persisted exact bytes: the
	// admission port must treat the supplied YAML as authoritative and never
	// re-read a definition that may have been edited since the first selection.
	admitted, err := processor.admission.Admit(ctx, pipeline.AdmitRequest{
		Scope: envelope.Scope,
		Definition: pipeline.Definition{
			ID:           selection.PipelineID,
			Name:         candidate.Definition.Name,
			YAML:         append([]byte(nil), selection.YAML...),
			Version:      candidate.Definition.Version,
			Push:         candidate.Definition.Push,
			PushTriggers: append([]pipeline.PushTrigger(nil), candidate.Definition.PushTriggers...),
		},
		Inputs: cloneSelectionInputs(selection.Inputs),
		// The installation default resolves every action that declares no
		// explicit timeout. Admission refuses an out-of-range fallback, so a
		// misconfigured processor fails closed instead of queuing a zero
		// deadline.
		DefaultTimeout: processor.defaultTimeout,
	})
	if err != nil {
		if reason, permanent := permanentRefusalReason(err); permanent {
			return processor.refuse(ctx, envelope, candidate, selection, record, reason, outcome)
		}
		return true, nil
	}
	processor.startMu.Lock()
	defer processor.startMu.Unlock()
	// The shared start completes the selection's common receipt with a
	// compare-and-set. A concurrent pass (or a retry whose earlier read raced
	// the commit) may already have completed it, so re-read the persisted
	// result before allocating an execution identity: a retry that now finds a
	// durable result applies nothing, returns the original execution and never
	// consumes a fresh identity. A read failure falls through to the start
	// commit, whose receipt compare-and-set remains the durable guard.
	if stored, found, err := processor.selections.Lookup(ctx, envelope.Scope, selection.PipelineID, selection.EventID); err == nil && found {
		switch {
		case stored.Status == StatusRefused:
			outcome.refused = append(outcome.refused, envelope.ID)
			return false, nil
		case stored.ExecutionID != "":
			outcome.started = append(outcome.started, stored.ExecutionID)
			return false, nil
		}
	}
	executionID, err := processor.ids.NewExecutionID()
	if err != nil {
		return true, nil
	}
	unit, err := processor.startUnit(envelope, candidate, selection, record, admitted, StatusSelected, executionID, "")
	if err != nil {
		return true, nil
	}
	started, err := processor.startPort.Start(ctx, unit)
	if err != nil {
		return true, nil
	}
	if started.Status == StatusRefused {
		outcome.refused = append(outcome.refused, envelope.ID)
		return false, nil
	}
	if started.ExecutionID != "" {
		outcome.started = append(outcome.started, started.ExecutionID)
		return false, nil
	}
	return true, nil
}

// refuse records a permanent refusal through the same shared start commit. A
// refused unit carries no admission result and no new execution identity, and
// its published event carries only the stable reason class and the selection
// identity.
func (processor *Processor) refuse(ctx context.Context, envelope event.Envelope, candidate Candidate, selection Selection, record receipt.Record, reason RefusalReason, outcome *eventOutcome) (bool, error) {
	unit, err := processor.startUnit(envelope, candidate, selection, record, pipeline.AdmissionResult{}, StatusRefused, "", reason)
	if err != nil {
		return true, nil
	}
	started, err := processor.startPort.Start(ctx, unit)
	if err != nil {
		return true, nil
	}
	if started.Status == StatusRefused {
		outcome.refused = append(outcome.refused, envelope.ID)
		return false, nil
	}
	if started.ExecutionID != "" {
		outcome.started = append(outcome.started, started.ExecutionID)
		return false, nil
	}
	return true, nil
}

// newSelection builds the immutable first selection and its common receipt
// from the exact current bytes. The selection is persisted before admission.
func (processor *Processor) newSelection(envelope event.Envelope, report Report, candidate Candidate, serviceID service.ServiceID) (SelectUnit, Selection, receipt.Record, error) {
	keys, err := declaredInputs(candidate.Definition.YAML)
	if err != nil {
		return SelectUnit{}, Selection{}, receipt.Record{}, err
	}
	fingerprint := eventFingerprint(envelope)
	selection := Selection{
		PipelineID:       candidate.Definition.ID,
		EventID:          envelope.ID,
		YAML:             append([]byte(nil), candidate.Definition.YAML...),
		ServiceID:        serviceID,
		Inputs:           selectionInputs(keys, report),
		EventContentHash: fingerprint,
	}
	receiptID, err := processor.ids.NewReceiptID()
	if err != nil {
		return SelectUnit{}, Selection{}, receipt.Record{}, err
	}
	auditID, err := processor.ids.NewEventID()
	if err != nil {
		return SelectUnit{}, Selection{}, receipt.Record{}, err
	}
	now := processor.clock.Now()
	principal := id.PrincipalID(selection.ServiceID)
	// The select unit's declared receipt mode is ReceiptInsert: the adapter
	// inserts this common receipt incomplete (completed_at NULL, no result).
	// The shared start completes the exact same key and content hash under
	// ReceiptComplete, so one (pipeline, event) pair never inserts two receipts.
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    envelope.Scope.TenantID,
			ProjectID:   envelope.Scope.ProjectID,
			PrincipalID: principal,
			Operation:   string(startOperation),
			CallerKey:   string(selection.PipelineID) + "/" + string(selection.EventID),
		},
		ContentHash: contentHash(fingerprint),
		At:          now,
	}
	unit := SelectUnit{
		Owner:                   envelope.Scope,
		PipelineID:              selection.PipelineID,
		EventID:                 selection.EventID,
		ExpectedPipelineVersion: candidate.Definition.Version,
		ExpectedBindingVersion:  candidate.Binding.Version,
		Selection:               selection,
		Receipt:                 record,
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     envelope.Scope,
			Actor:     principal,
			Operation: string(startOperation),
			At:        now,
		}},
		Authority: authorityReads(envelope.Scope, principal, candidate),
	}
	return unit, selection, record, nil
}

// startUnit builds the complete shared-start commit unit with the current
// automatic-service authority reads.
func (processor *Processor) startUnit(envelope event.Envelope, candidate Candidate, selection Selection, record receipt.Record, admitted pipeline.AdmissionResult, status Status, executionID id.ExecutionID, reason RefusalReason) (StartUnit, error) {
	auditID, err := processor.ids.NewEventID()
	if err != nil {
		return StartUnit{}, err
	}
	eventID, err := processor.ids.NewEventID()
	if err != nil {
		return StartUnit{}, err
	}
	now := processor.clock.Now()
	principal := id.PrincipalID(selection.ServiceID)
	unitEvent, err := startEvent(eventID, envelope, selection, candidate, status, executionID, reason, principal, now)
	if err != nil {
		return StartUnit{}, err
	}
	unit := StartUnit{
		Owner:                   envelope.Scope,
		PipelineID:              selection.PipelineID,
		EventID:                 selection.EventID,
		Selection:               selection,
		Admission:               admitted,
		ExpectedPipelineVersion: candidate.Definition.Version,
		ExpectedBindingVersion:  candidate.Binding.Version,
		Initiator:               principal,
		ExecutionPrincipal:      executionPrincipal(executionID, principal),
		NewExecutionID:          executionID,
		Status:                  status,
		At:                      now,
		Receipt:                 record,
		Audit: []event.Audit{{
			ID:        auditID,
			Scope:     envelope.Scope,
			Actor:     principal,
			Operation: string(startOperation),
			At:        now,
		}},
		Events:    []event.Envelope{unitEvent},
		Authority: authorityReads(envelope.Scope, principal, candidate),
	}
	switch {
	case status == StatusSelected && executionID != "":
		unit.PostCommit = []event.PostCommitEffect{
			{Kind: event.PostPublishEvent},
			{Kind: event.PostDispatch, Target: string(executionID)},
		}
	case status == StatusRefused:
		unit.PostCommit = []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	}
	return unit, nil
}

// startEvent builds the publication intent committed with one push start or
// refusal. A queued start publishes the canonical execution.queued fact for the
// new execution. A permanent refusal publishes the canonical Shell decision
// fact pipeline.start_refused through NewStartRefusedEvent.
func startEvent(eventID id.EventID, envelope event.Envelope, selection Selection, candidate Candidate, status Status, executionID id.ExecutionID, reason RefusalReason, principal id.PrincipalID, at time.Time) (event.Envelope, error) {
	switch {
	case status == StatusSelected && executionID != "":
		unitEvent := event.Envelope{
			ID:             eventID,
			Type:           event.TypeExecutionQueued,
			Source:         event.Source{Kind: event.SourceShell, PrincipalID: principal},
			At:             at,
			Scope:          envelope.Scope,
			SubjectID:      string(executionID),
			SubjectVersion: 1,
			CorrelationID:  string(envelope.ID),
		}
		if err := unitEvent.Validate(); err != nil {
			return event.Envelope{}, err
		}
		return unitEvent, nil
	case status == StatusRefused:
		return NewStartRefusedEvent(eventID, envelope.Scope, selection, candidate.Definition.Version, reason, principal, at)
	default:
		return event.Envelope{}, fault.New(fault.InternalFailure, "push start unit has no event for its status")
	}
}

// refusalPayload is the exact durable JSON shape of one pipeline.start_refused
// event. It carries only the stable reason class and the selection identity;
// error messages, credential material and free-form text are never published.
type refusalPayload struct {
	Reason     string `json:"reason"`
	PipelineID string `json:"pipeline_id"`
	EventID    string `json:"event_id"`
}

// NewStartRefusedEvent builds the canonical pipeline.start_refused envelope for
// one permanent push-start refusal. It is the single construction path for
// every refusal kind: an orphaned persisted selection, a permanent admission
// fault, a select-time refusal and a current-authority/service-enablement
// refusal detected inside StartPort.Start. The event is Shell-sourced and its
// subject is the pipeline with its current version (0 only for an orphan). The
// data holds only the stable reason class and the pipeline/event selection
// identity.
func NewStartRefusedEvent(eventID id.EventID, scope tenancy.Scope, selection Selection, pipelineVersion uint64, reason RefusalReason, principal id.PrincipalID, at time.Time) (event.Envelope, error) {
	if !reason.Valid() {
		return event.Envelope{}, fault.New(fault.InternalFailure, "push start refusal reason is not a stable permanent class")
	}
	data, err := json.Marshal(refusalPayload{
		Reason:     string(reason),
		PipelineID: string(selection.PipelineID),
		EventID:    string(selection.EventID),
	})
	if err != nil {
		return event.Envelope{}, fault.Wrap(fault.InternalFailure, "encode push start refusal data", err)
	}
	unitEvent := event.Envelope{
		ID:             eventID,
		Type:           event.TypePipelineStartRefused,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: principal},
		At:             at,
		Scope:          scope,
		SubjectID:      string(selection.PipelineID),
		SubjectVersion: pipelineVersion,
		CorrelationID:  string(selection.EventID),
		Data:           data,
	}
	if err := unitEvent.Validate(); err != nil {
		return event.Envelope{}, err
	}
	return unitEvent, nil
}

// adopt prefers the persisted selection and receipt returned by the selection
// commit; a store that echoes only the receipt identity still keeps the exact
// bytes the processor persisted.
func adopt(selected Result, selection Selection, record receipt.Record) (Selection, receipt.Record) {
	if len(selected.Selection.YAML) > 0 {
		selection = selected.Selection
	}
	if selected.Receipt.ID != "" {
		record = selected.Receipt
	} else if selected.ReceiptID != "" {
		record.ID = selected.ReceiptID
	}
	return selection, record
}

// permanentFaultClass reports whether a fault class is a permanent refusal.
func permanentFaultClass(class fault.Class) bool {
	switch class {
	case fault.Forbidden,
		fault.InvalidInput,
		fault.NotFound,
		fault.VersionConflict,
		fault.ChangedIdempotencyContent,
		fault.UnsupportedCapability:
		return true
	default:
		return false
	}
}

// permanentRefusalReason maps a permanent admission/selection fault to the
// stable reason class published on pipeline.start_refused. Any other class is
// transient or uncertain and must be retried, never published as a refusal.
func permanentRefusalReason(err error) (RefusalReason, bool) {
	class, ok := fault.ClassOf(err)
	if !ok || !permanentFaultClass(class) {
		return "", false
	}
	return RefusalReason(class), true
}

// authorityReads is the current automatic-service authority for one start. A
// persisted selection is never permission: membership, service enablement and
// the current pipeline/binding versions are read again at the commit boundary,
// and the service must still hold the event.read grant for the accepted event
// it is acting on. A zero candidate means the current definition/binding no
// longer exists, so only the non-versioned reads apply.
func authorityReads(scope tenancy.Scope, principal id.PrincipalID, candidate Candidate) []authorize.AuthorityRead {
	start, _ := authorize.LookupOperation(startOperation)
	read := func(operation authorize.Operation, check authorize.AuthorityCheck) authorize.AuthorityRead {
		return authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
	}
	reads := []authorize.AuthorityRead{
		read(start, authorize.CheckMembership),
		read(start, authorize.CheckServiceEnabled),
	}
	if candidate.Definition.ID != "" {
		pipelineVersion := read(start, authorize.CheckPipelineVersion)
		pipelineVersion.TargetID = string(candidate.Definition.ID)
		pipelineVersion.ExpectedVersion = candidate.Definition.Version
		bindingVersion := read(start, authorize.CheckBindingVersion)
		bindingVersion.TargetID = string(candidate.Binding.ID)
		bindingVersion.ExpectedVersion = candidate.Binding.Version
		reads = append(reads, pipelineVersion, bindingVersion)
	}
	eventRead, _ := authorize.LookupOperation(eventReadOperation)
	for _, check := range eventRead.Authority {
		reads = append(reads, read(eventRead, check))
	}
	return reads
}

// cloneSelectionInputs copies the persisted input map so a caller cannot
// mutate it after admission.
func cloneSelectionInputs(values map[id.InputKey]string) map[id.InputKey]string {
	if values == nil {
		return nil
	}
	out := make(map[id.InputKey]string, len(values))
	maps.Copy(out, values)
	return out
}

// executionPrincipal returns the capability principal of one push-started
// execution. Like a pipeline.start through the edges, the automatic service is
// only the initiator: the execution runs as its own principal, which resolves to
// that execution during preparation. It is derived from the execution identity
// every attempt allocates immediately before the start commit, and IDSource
// needs no new method. A retry therefore proposes a principal for its own new
// execution identity, never a reused one: the receipt compare-and-set completes
// only the first attempt, and an equal-key replay returns that attempt's stored
// execution, so the committed principal always names the committed execution. A
// refusal starts no execution and keeps the service.
func executionPrincipal(executionID id.ExecutionID, service id.PrincipalID) id.PrincipalID {
	if executionID == "" {
		return service
	}
	return id.PrincipalID("principal-" + string(executionID))
}
