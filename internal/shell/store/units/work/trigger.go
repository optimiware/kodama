package work

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// startOperationName is the declared shared operation of a push selection and
// its eventual start. The common receipt is keyed by it and by
// "<pipeline>/<event>".
const startOperationName = "pipeline.start"

// Pending reads committed event rows across every scope in durable sequence
// order under the controller's read authority. The zero cursor means "from the
// beginning"; the processor always rescans from zero so a committed row can
// never be skipped. The returned scope travels with each row.
//
// Pending returns only events that have no durable trigger completion marker:
// the trigger processor always asks from sequence zero and relies on the reader
// to skip events it already settled, so returning the completed prefix would
// stall every event past the first batch forever. The event table is scanned in
// sequence order and the marker of each row is read in that row's own scope;
// scanning continues until limit unconsumed rows are collected or the table is
// exhausted, keeping the pass bounded while still returning a late
// lower-sequence unconsumed row.
func (adapter *Triggers) Pending(ctx context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	if limit <= 0 || limit > store.MaxQueryLimit {
		limit = store.MaxQueryLimit
	}
	pending := make([]event.PendingEvent, 0, limit)
	cursor := int64(after)
	for len(pending) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := adapter.controller.Scan(ctx, store.ControllerScan{
			Table:   "events",
			Range:   []store.RangeFilter{{Column: "publish_sequence", After: cursor}},
			OrderBy: []store.Order{{Column: "publish_sequence"}},
			Limit:   limit})
		if err != nil {
			return nil, err
		}
		if len(page.Rows) == 0 {
			return pending, nil
		}
		for _, row := range page.Rows {
			sequence := event.Sequence(versionValue(row, "publish_sequence"))
			cursor = int64(sequence)
			consumed, err := adapter.eventConsumed(ctx, row, sequence)
			if err != nil {
				return nil, err
			}
			if consumed {
				continue
			}
			entry, err := pendingEvent(row)
			if err != nil {
				return nil, err
			}
			pending = append(pending, entry)
			if len(pending) == limit {
				return pending, nil
			}
		}
		if !page.HasMore {
			return pending, nil
		}
	}
	return pending, nil
}

// eventConsumed reports whether one event row already has the trigger
// completion marker. The marker lives in the event's own scope, so it is read
// through the scoped executor rather than the controller seam (which may read
// only the event table). A missing marker means the event is unconsumed.
func (adapter *Triggers) eventConsumed(ctx context.Context, row store.Row, sequence event.Sequence) (bool, error) {
	scope := tenancy.Scope{
		TenantID:  id.TenantID(textValue(row, "tenant_id")),
		ProjectID: id.ProjectID(textValue(row, "project_id")),
	}
	marker, err := adapter.executor.Read(ctx, scope, store.EventConsumptionsTable, textValue(row, "id"))
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return false, nil
		}
		return false, err
	}
	return textValue(marker, "consumer") == trigger.CompletionConsumer && versionValue(marker, "version") == uint64(sequence), nil
}

// pendingEvent decodes one committed events row into its durable envelope.
func pendingEvent(row store.Row) (event.PendingEvent, error) {
	envelope := event.Envelope{
		ID:             id.EventID(textValue(row, "id")),
		Type:           event.Type(textValue(row, "type")),
		ReportType:     event.ReportType(textValue(row, "report_type")),
		Source:         event.Source{Kind: event.SourceKind(textValue(row, "source_kind")), PrincipalID: id.PrincipalID(textValue(row, "source_principal"))},
		At:             timeValue(row, "at"),
		Scope:          tenancy.Scope{TenantID: id.TenantID(textValue(row, "tenant_id")), ProjectID: id.ProjectID(textValue(row, "project_id"))},
		SubjectID:      textValue(row, "subject_id"),
		SubjectVersion: versionValue(row, "subject_version"),
		CorrelationID:  textValue(row, "correlation_id"),
		CausationID:    textValue(row, "causation_id"),
		Data:           bytesValue(row, "data"),
	}
	if err := envelope.Validate(); err != nil {
		return event.PendingEvent{}, fault.Wrap(fault.InternalFailure, "a stored event row is not a valid envelope", err)
	}
	return event.PendingEvent{Sequence: event.Sequence(versionValue(row, "publish_sequence")), Event: envelope}, nil
}

// Complete writes the durable event_consumptions marker for one fully settled
// event: consumer "trigger", id = event id, version = the event's durable
// sequence and completed_at from the adapter clock. The event row is read under
// the controller authority first, so the marker always lands in the event's own
// scope and a wrong scope or sequence is refused. Completing the same event
// twice applies nothing and is not an error.
func (adapter *Triggers) Complete(ctx context.Context, unit trigger.CompletionUnit) error {
	if err := unit.Owner.Validate(); err != nil {
		return err
	}
	if err := requireNonEmpty("event id", string(unit.EventID)); err != nil {
		return err
	}
	if unit.Sequence == 0 {
		return fault.New(fault.InvalidInput, "event completion sequence is zero")
	}
	row, err := adapter.controller.Read(ctx, "events", string(unit.EventID))
	if err != nil {
		return err
	}
	owner := tenancy.Scope{TenantID: id.TenantID(textValue(row, "tenant_id")), ProjectID: id.ProjectID(textValue(row, "project_id"))}
	if owner != unit.Owner {
		return fault.New(fault.Forbidden, "event completion scope does not match the event row")
	}
	if sequence := event.Sequence(versionValue(row, "publish_sequence")); sequence != unit.Sequence {
		return fault.New(fault.VersionConflict, "event completion sequence does not match the event row")
	}
	if err := adapter.completionMatches(ctx, unit); err == nil {
		return nil
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return err
	}
	_, err = execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Writes: []store.Write{{
			Table:  store.EventConsumptionsTable,
			ID:     string(unit.EventID),
			Insert: true,
			Values: map[string]any{
				"consumer":     trigger.CompletionConsumer,
				"completed_at": adapter.clock(),
				"version":      int64(unit.Sequence),
			},
		}},
	})
	if err == nil {
		return nil
	}
	// A concurrent completer may have inserted the same marker between the read
	// and the insert. Re-read and converge on an equal marker.
	if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
		return adapter.completionMatches(ctx, unit)
	}
	return err
}

// completionMatches reads the marker and reports whether it already records this
// exact completion. A missing marker is not_found; a different marker is
// version_conflict.
func (adapter *Triggers) completionMatches(ctx context.Context, unit trigger.CompletionUnit) error {
	row, err := readRow(ctx, adapter.executor, unit.Owner, store.EventConsumptionsTable, string(unit.EventID))
	if err != nil {
		return err
	}
	if textValue(row, "consumer") != trigger.CompletionConsumer || versionValue(row, "version") != uint64(unit.Sequence) {
		return fault.New(fault.VersionConflict, "the event completion marker changed")
	}
	return nil
}

// Select inserts the common receipt of one (pipeline, event) pair in the
// incomplete mode and commits the selection's audit and publication intent. The
// shared Start completes that exact key by compare-and-set. An equal-key retry
// applies nothing and returns the first selection carried by the unit.
//
// The receipt rules let the incomplete insert carry an in-progress result,
// so the exact first selection (YAML, service, inputs, event fingerprint and
// pipeline identity) is durable before admission. Start later overwrites it with
// the final result under the compare-and-set; an equal-key replay returns the
// stored first selection and applies nothing.
func (adapter *Triggers) Select(ctx context.Context, unit trigger.SelectUnit) (trigger.Result, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return trigger.Result{}, err
	}
	if err := requireNonEmpty("pipeline id", string(unit.PipelineID)); err != nil {
		return trigger.Result{}, err
	}
	if err := requireNonEmpty("event id", string(unit.EventID)); err != nil {
		return trigger.Result{}, err
	}
	if unit.Selection.PipelineID != unit.PipelineID || unit.Selection.EventID != unit.EventID {
		return trigger.Result{}, fault.New(fault.InvalidInput, "selection identity does not match the select unit")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return trigger.Result{}, err
	}
	result := trigger.Result{
		ReceiptID: unit.Receipt.ID,
		Status:    trigger.StatusSelected,
		Selection: unit.Selection,
		Receipt:   unit.Receipt,
	}
	record, err := withSelectionResult(unit.Receipt, result)
	if err != nil {
		return trigger.Result{}, err
	}
	locks, err := adapter.versionedLocks(ctx, unit.PipelineID, unit.ExpectedPipelineVersion, unit.ExpectedBindingVersion, unit.Authority)
	if err != nil {
		return trigger.Result{}, err
	}
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:       locks,
		Receipt:     &record,
		ReceiptMode: store.ReceiptInsertIncomplete,
		Audit:       unit.Audit,
		Events:      unit.Events,
	})
	if err != nil {
		return trigger.Result{}, err
	}
	if committed.Replayed {
		// The stored in-progress payload is the FIRST selection; a retry after a
		// definition edit must never replace it with the retrying unit's bytes.
		stored, err := decodeSelectionResult(committed.Receipt.Result)
		if err != nil {
			return trigger.Result{}, err
		}
		stored.Replayed = true
		return stored, nil
	}
	return result, nil
}

// versionedLocks renders the current pipeline/binding version guards and every
// current-authority guard of one selection or start. A stale or revoked
// authority guard is a permanent selection refusal. A refusal for an orphaned
// selection carries zero expected versions and no versioned authority read; the
// non-versioned authority guards still apply.
func (adapter *Triggers) versionedLocks(
	ctx context.Context,
	pipelineID pipeline.PipelineID,
	pipelineVersion, bindingVersion uint64,
	reads []authorize.AuthorityRead,
) ([]store.Lock, error) {
	var locks []store.Lock
	if pipelineVersion != 0 {
		locks = append(locks, pipelineLock(pipelineID, pipelineVersion))
	}
	bindingID := authorityTarget(reads, "binding_version")
	if bindingVersion != 0 {
		if bindingID == "" {
			return nil, fault.New(fault.InvalidInput, "a versioned selection requires its binding authority target")
		}
		locks = append(locks, store.Lock{Table: "resource_bindings", IDs: []string{bindingID}, ExpectVersion: bindingVersion})
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, reads, authorityContext{})
	if err != nil {
		return nil, err
	}
	locks = append(locks, authority...)
	return locks, nil
}

// Start completes the selection's common receipt with the execution/actions and
// queued event, or with a permanent refusal. A completed-receipt replay applies
// nothing and returns the original result; a changed-content completion is
// refused. When a StatusSelected unit's current authority no longer holds, the
// canonical pipeline.start_refused event is committed instead of the queued
// execution and its dispatch effect.
func (adapter *Triggers) Start(ctx context.Context, unit trigger.StartUnit) (trigger.Result, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return trigger.Result{}, err
	}
	if err := requireNonEmpty("pipeline id", string(unit.PipelineID)); err != nil {
		return trigger.Result{}, err
	}
	if err := requireNonEmpty("event id", string(unit.EventID)); err != nil {
		return trigger.Result{}, err
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return trigger.Result{}, err
	}
	if unit.ReceiptMode() != trigger.ReceiptComplete {
		return trigger.Result{}, fault.New(fault.InvalidInput, "a start unit must complete the common receipt")
	}
	if unit.Selection.PipelineID != unit.PipelineID || unit.Selection.EventID != unit.EventID {
		return trigger.Result{}, fault.New(fault.InvalidInput, "selection identity does not match the start unit")
	}
	switch unit.Status {
	case trigger.StatusSelected:
		if len(unit.Events) != 1 || unit.Events[0].Type != event.TypeExecutionQueued {
			return trigger.Result{}, fault.New(fault.InvalidInput, "a selected start must commit exactly one execution.queued event")
		}
	case trigger.StatusRefused:
		if len(unit.Events) != 1 || unit.Events[0].Type != event.TypePipelineStartRefused {
			return trigger.Result{}, fault.New(fault.InvalidInput, "a refused start must commit exactly one pipeline.start_refused event")
		}
	case trigger.StatusDeferred:
		return trigger.Result{}, fault.New(fault.InvalidInput, "a deferred selection is not a start unit")
	default:
		return trigger.Result{}, fault.New(fault.InvalidInput, "unknown start status")
	}
	selected, err := adapter.startSelected(ctx, unit)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && unit.Status == trigger.StatusSelected && permanentAuthorityClass(class) {
			// A colliding execution identity is an internal fault, not a stale
			// authority refusal.
			if class == fault.VersionConflict {
				if _, readErr := readRow(ctx, adapter.executor, unit.Owner, "executions", string(unit.NewExecutionID)); readErr == nil {
					return trigger.Result{}, fault.New(fault.InternalFailure, "the start execution identity already exists")
				}
			}
			return adapter.startRefused(ctx, unit, trigger.RefusalReasonAuthorityRefused)
		}
		return trigger.Result{}, err
	}
	return selected, nil
}

// startSelected commits the queued execution and its admitted actions with the
// completed receipt and the unit's queued event.
func (adapter *Triggers) startSelected(ctx context.Context, unit trigger.StartUnit) (trigger.Result, error) {
	if unit.Status == trigger.StatusRefused {
		return adapter.startRefused(ctx, unit, "")
	}
	if err := requireNonEmpty("execution id", string(unit.NewExecutionID)); err != nil {
		return trigger.Result{}, err
	}
	result := trigger.Result{
		ExecutionID: unit.NewExecutionID,
		ReceiptID:   unit.Receipt.ID,
		Status:      trigger.StatusSelected,
		Selection:   unit.Selection,
		Receipt:     unit.Receipt,
	}
	record, err := withSelectionResult(unit.Receipt, result)
	if err != nil {
		return trigger.Result{}, err
	}
	pipelineVersion := unit.Admission.PipelineVersion
	if pipelineVersion == 0 {
		pipelineVersion = unit.ExpectedPipelineVersion
	}
	plan := executionPlan{
		executionID:     unit.NewExecutionID,
		pipelineID:      unit.PipelineID,
		pipelineVersion: pipelineVersion,
		principal:       unit.ExecutionPrincipal,
		snapshot:        unit.Admission.Snapshot,
		actions:         unit.Admission.Actions,
		at:              unit.At,
	}
	writes, err := plan.writes()
	if err != nil {
		return trigger.Result{}, err
	}
	locks, err := adapter.versionedLocks(ctx, unit.PipelineID, unit.ExpectedPipelineVersion, unit.ExpectedBindingVersion, unit.Authority)
	if err != nil {
		return trigger.Result{}, err
	}
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:         locks,
		Writes:        writes,
		Receipt:       &record,
		ReceiptMode:   store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: unit.Receipt.ContentHash},
		Audit:         unit.Audit,
		Events:        unit.Events,
	})
	if err != nil {
		return trigger.Result{}, err
	}
	return adapter.completedResult(committed, result)
}

// startRefused completes the receipt with a permanent refusal and the canonical
// pipeline.start_refused event. No execution, action or queued event is
// committed.
func (adapter *Triggers) startRefused(ctx context.Context, unit trigger.StartUnit, reason trigger.RefusalReason) (trigger.Result, error) {
	result := trigger.Result{
		Status:    trigger.StatusRefused,
		Selection: unit.Selection,
		Receipt:   unit.Receipt,
	}
	record, err := withSelectionResult(unit.Receipt, result)
	if err != nil {
		return trigger.Result{}, err
	}
	events := unit.Events
	if reason != "" {
		eventID, err := adapter.newEventID()
		if err != nil {
			return trigger.Result{}, err
		}
		refusal, err := trigger.NewStartRefusedEvent(eventID, unit.Owner, unit.Selection, unit.ExpectedPipelineVersion, reason, unit.Initiator, unit.At)
		if err != nil {
			return trigger.Result{}, err
		}
		events = []event.Envelope{refusal}
	}
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Receipt:       &record,
		ReceiptMode:   store.ReceiptComplete,
		ReceiptExpect: &store.ReceiptExpect{ContentHash: unit.Receipt.ContentHash},
		Audit:         unit.Audit,
		Events:        events,
	})
	if err != nil {
		return trigger.Result{}, err
	}
	return adapter.completedResult(committed, result)
}

// withSelectionResult stores the common-receipt result envelope, which carries
// the exact first-selected YAML bytes beside the trigger result.
func withSelectionResult(record receipt.Record, result trigger.Result) (receipt.Record, error) {
	encoded, err := encodeSelectionResult(result)
	if err != nil {
		return receipt.Record{}, err
	}
	record.Result = encoded
	return record, nil
}

// completedResult returns the committed or replayed start result.
func (adapter *Triggers) completedResult(committed store.UnitResult, result trigger.Result) (trigger.Result, error) {
	if !committed.Replayed {
		return result, nil
	}
	replayed, err := decodeSelectionResult(committed.Receipt.Result)
	if err != nil {
		return trigger.Result{}, err
	}
	replayed.Replayed = true
	return replayed, nil
}

// permanentAuthorityClass reports whether one authority-lock fault is a
// permanent current-authority refusal that must be recorded as a start refusal
// instead of retried forever. An unmappable check (invalid_input) is a
// programming error, not a recorded refusal.
func permanentAuthorityClass(class fault.Class) bool {
	switch class {
	case fault.Forbidden, fault.NotFound, fault.VersionConflict:
		return true
	default:
		return false
	}
}

// authorityTarget returns the target identity of one authority check, if the
// unit carries it.
func authorityTarget(reads []authorize.AuthorityRead, check string) string {
	for _, read := range reads {
		if string(read.Check) == check && read.TargetID != "" {
			return read.TargetID
		}
	}
	return ""
}

// Lookup reads the persisted first selection for one (pipeline, event) pair.
// The selection is the common receipt: Select inserted it and Start completes
// the same identity. Lookup never consults a current definition or binding, so
// an edited definition cannot rebuild an existing selection. The receipt
// identity also carries the selecting principal; if more than one principal
// committed a receipt for the same caller key, the earliest committed row is
// the first selection.
func (adapter *Triggers) Lookup(ctx context.Context, scope tenancy.Scope, pipelineID pipeline.PipelineID, eventID id.EventID) (trigger.Result, bool, error) {
	if err := validProjectScope(scope); err != nil {
		return trigger.Result{}, false, err
	}
	if err := requireNonEmpty("pipeline id", string(pipelineID)); err != nil {
		return trigger.Result{}, false, err
	}
	if err := requireNonEmpty("event id", string(eventID)); err != nil {
		return trigger.Result{}, false, err
	}
	page, err := adapter.scanner.Query(ctx, scope, store.ScanQuery{
		Table: "receipts",
		Equals: map[string]any{
			"operation":  startOperationName,
			"caller_key": selectionCallerKey(pipelineID, eventID),
		},
		OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
		Limit:   1,
	})
	if err != nil {
		return trigger.Result{}, false, err
	}
	if len(page.Rows) == 0 {
		return trigger.Result{}, false, nil
	}
	if len(bytesValue(page.Rows[0], "result")) == 0 {
		// An incomplete receipt may carry no payload; then there is no durable
		// first selection to return.
		return trigger.Result{}, false, nil
	}
	result, err := decodeSelection(page.Rows[0])
	if err != nil {
		return trigger.Result{}, false, err
	}
	if result.Selection.PipelineID != pipelineID || result.Selection.EventID != eventID {
		return trigger.Result{}, false, fault.New(fault.InternalFailure, "a stored selection receipt does not match its caller key")
	}
	result.Replayed = true
	return result, true, nil
}

// Unfinished lists every persisted selection of one event that still has no
// durable result, including a selection whose current definition or binding no
// longer exists. The processor resumes or refuses each one before it advances
// the event cursor.
func (adapter *Triggers) Unfinished(ctx context.Context, scope tenancy.Scope, eventID id.EventID) ([]trigger.Result, error) {
	if err := validProjectScope(scope); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("event id", string(eventID)); err != nil {
		return nil, err
	}
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "receipts",
		Equals:  map[string]any{"operation": startOperationName},
		OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	unfinished := make([]trigger.Result, 0, len(rows))
	for _, row := range rows {
		// Only an incomplete receipt can be an unfinished selection. An
		// ordinary pipeline.start receipt (the bare execution.StartResult JSON)
		// is completed at insert and shares this operation name, so it must be
		// skipped before its result is decoded as a trigger selection.
		if row.Values["completed_at"] != nil {
			continue
		}
		if len(bytesValue(row, "result")) == 0 {
			continue
		}
		result, err := decodeSelection(row)
		if err != nil {
			return nil, err
		}
		if result.Selection.EventID != eventID {
			continue
		}
		if result.Status != trigger.StatusSelected || result.ExecutionID != "" {
			continue
		}
		result.Replayed = true
		unfinished = append(unfinished, result)
	}
	sort.SliceStable(unfinished, func(left, right int) bool {
		return unfinished[left].Selection.PipelineID < unfinished[right].Selection.PipelineID
	})
	return unfinished, nil
}

// selectionCallerKey is the declared receipt caller key of one selection.
func selectionCallerKey(pipelineID pipeline.PipelineID, eventID id.EventID) string {
	return string(pipelineID) + "/" + string(eventID)
}

// selectionRecord is the durable common-receipt result shape. trigger.Selection
// keeps its exact admitted YAML bytes out of JSON (json:"-"), so the adapter
// stores them beside the result and rebuilds the selection on read. Without this
// envelope an edited or deleted definition could not be replayed exactly.
type selectionRecord struct {
	Result trigger.Result `json:"result"`
	YAML   []byte         `json:"yaml"`
}

// encodeSelectionResult renders one common selection receipt result.
func encodeSelectionResult(result trigger.Result) ([]byte, error) {
	encoded, err := json.Marshal(selectionRecord{Result: result, YAML: result.Selection.YAML})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "selection result cannot be encoded", err)
	}
	return encoded, nil
}

// decodeSelectionResult reads one common selection receipt result and restores
// the exact first-selected YAML bytes.
func decodeSelectionResult(raw []byte) (trigger.Result, error) {
	if len(raw) == 0 {
		return trigger.Result{}, fault.New(fault.InternalFailure, "a stored selection receipt has no durable result")
	}
	var record selectionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return trigger.Result{}, fault.Wrap(fault.InternalFailure, "a stored selection receipt result is not readable", err)
	}
	if !record.Result.Status.Valid() {
		return trigger.Result{}, fault.New(fault.InternalFailure, "a stored selection receipt has an unknown status")
	}
	record.Result.Selection.YAML = record.YAML
	return record.Result, nil
}

// decodeSelection decodes one pipeline.start receipt result. An unreadable or
// empty durable decision is an internal failure, never an absent selection.
func decodeSelection(row store.Row) (trigger.Result, error) {
	return decodeSelectionResult(bytesValue(row, "result"))
}

// compile-time proof that the pipeline admission service already satisfies the
// trigger admission seam; it is not a store adapter and needs no duplicate.
var _ trigger.AdmissionPort = (*pipeline.AdmissionService)(nil)
