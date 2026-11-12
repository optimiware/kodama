package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// reportOperation is the declared registry operation a verified report commits
// under. It is also the receipt key operation of every report.
const reportOperation = "resource.report"

// RecordAdapter is the store-backed verify.RecordPort.
//
// Record maps one verification record to a single verifications row and commits
// it together with the unit's receipt, audit rows and event publication intent.
// The current-authority reads the capability attached become read-only locks, so
// a service, binding, actor or execution fact that changed after the capability
// observed it refuses the commit instead of persisting a stale fact.
type RecordAdapter struct {
	executor store.Executor
	scanner  store.Scanner
}

// NewRecordAdapter builds the verification record adapter.
func NewRecordAdapter(executor store.Executor, scanner store.Scanner) (*RecordAdapter, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "verification record adapter requires an executor")
	}
	return &RecordAdapter{executor: executor, scanner: scanner}, nil
}

var _ verify.RecordPort = (*RecordAdapter)(nil)

// Record commits one durable verification record.
func (adapter *RecordAdapter) Record(ctx context.Context, unit verify.RecordUnit) (verify.RecordResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return verify.RecordResult{}, err
	}
	if unit.Record.Scope != unit.Owner {
		return verify.RecordResult{}, fault.New(fault.Forbidden, "verification record scope does not match the unit owner")
	}
	if _, err := verify.ParseVerificationID(string(unit.Record.ID)); err != nil {
		return verify.RecordResult{}, err
	}
	values, err := verificationRowValues(unit.Record)
	if err != nil {
		return verify.RecordResult{}, err
	}
	locks, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Owner, unit.Authority, authorityFacts{
		ExecutionID: unit.Record.ExecutionID,
		ActionID:    unit.Record.ActionID,
		ActorKind:   unit.Record.Actor.Kind,
	})
	if err != nil {
		return verify.RecordResult{}, err
	}
	result, err := adapter.executor.Execute(ctx, unit.Owner, store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:  verificationsTable,
			ID:     string(unit.Record.ID),
			Insert: true,
			Values: values,
		}},
		Receipt: receiptPointer(unit.Receipt),
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return verify.RecordResult{}, err
	}
	if result.Replayed {
		var replayed verify.RecordResult
		if err := decodeJSON(result.Receipt.Result, &replayed, "result"); err != nil {
			return verify.RecordResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return verify.RecordResult{ID: unit.Record.ID, Replayed: false}, nil
}

// verificationRowValues maps every Record field onto its declared column. A
// verification precedes the reported effect, so the table carries no
// source-event identity.
func verificationRowValues(record verify.Record) (map[string]any, error) {
	subject, err := jsonValue(record.Subject)
	if err != nil {
		return nil, err
	}
	approval, err := nullableJSON(optionalAssignment(record.RequestedApproval))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		serviceIDColumn:      string(record.CallingService),
		actorIDColumn:        string(record.Actor.ID),
		executionIDColumn:    nullableIdentity(string(record.ExecutionID)),
		actionIDColumn:       nullableIdentity(string(record.ActionID)),
		bindingIDColumn:      string(record.BindingID),
		resourceIDColumn:     string(record.ResourceID),
		operationColumn:      record.Operation,
		subjectColumn:        subject,
		requestedApprovalCol: approval,
		resultColumn:         string(record.Result),
		approvedColumn:       record.Approved,
		createdAtColumn:      record.At,
	}, nil
}

// ReportAdapter is the store-backed verify.ReportPort.
//
// Report guards the cited verification row inside the commit and applies the
// complete receipt, audit rows and accepted resource.verified event as one
// store.Unit. Report deduplication is the receipts partial unique identity
// (resource.report keyed by the reporting service principal and the
// source-event caller key); an equal retry replays the original ReportResult
// before any lock or write, and changed content is refused by the executor.
type ReportAdapter struct {
	executor store.Executor
	scanner  store.Scanner
}

// NewReportAdapter builds the verified report adapter.
func NewReportAdapter(executor store.Executor, scanner store.Scanner) (*ReportAdapter, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "verified report adapter requires an executor")
	}
	return &ReportAdapter{executor: executor, scanner: scanner}, nil
}

var _ verify.ReportPort = (*ReportAdapter)(nil)

// Report commits one verified resource report.
func (adapter *ReportAdapter) Report(ctx context.Context, unit verify.ReportUnit) (verify.ReportResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return verify.ReportResult{}, err
	}
	if _, err := verify.ParseVerificationID(string(unit.VerificationID)); err != nil {
		return verify.ReportResult{}, err
	}
	if unit.SourceEventID == "" || len(unit.SourceEventID) > id.MaxIDLength {
		return verify.ReportResult{}, fault.New(fault.InvalidInput, "report source event id is invalid")
	}
	accepted, err := acceptedReportEvent(unit.Events)
	if err != nil {
		return verify.ReportResult{}, err
	}
	if err := validateReportUnit(unit, accepted); err != nil {
		return verify.ReportResult{}, err
	}
	locks, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Owner, unit.Authority, authorityFacts{
		// A report is committed by an authenticated service; resource.report
		// carries no execution/action authority read.
		ActorKind: identity.KindService,
	})
	if err != nil {
		return verify.ReportResult{}, err
	}
	// The cited verification record is the report's durable causation. Locking
	// it proves the referenced fact still exists in the report scope inside the
	// same transaction; the typed linkage check belongs to the capability.
	locks = append(locks, store.Lock{Table: verificationsTable, IDs: []string{string(unit.VerificationID)}})
	receiptRecord := unit.Receipt
	result, err := adapter.executor.Execute(ctx, unit.Owner, store.Unit{
		Locks:   locks,
		Receipt: &receiptRecord,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return verify.ReportResult{}, err
	}
	if result.Replayed {
		var replayed verify.ReportResult
		if err := decodeJSON(result.Receipt.Result, &replayed, "result"); err != nil {
			return verify.ReportResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return verify.ReportResult{
		VerificationID: unit.VerificationID,
		EventID:        accepted.ID,
		Replayed:       false,
	}, nil
}

// validateReportUnit checks the durable consistency facts the report adapter
// owns. These are not policy decisions: the capability already authorized the
// report and built the receipt. They prove the unit it built can only commit
// under the exact deduplication identity and causation the report contract
// names, so a missing receipt or a receipt for another principal, operation or
// source event can never publish an event without deduplication.
func validateReportUnit(unit verify.ReportUnit, accepted event.Envelope) error {
	if unit.Receipt.ID == "" {
		return fault.New(fault.InvalidInput, "report unit requires a deduplication receipt")
	}
	if err := unit.Receipt.Key.Validate(); err != nil {
		return err
	}
	key := unit.Receipt.Key
	if key.TenantID != unit.Owner.TenantID || key.ProjectID != unit.Owner.ProjectID {
		return fault.New(fault.InvalidInput, "report receipt key is outside the unit scope")
	}
	if key.PrincipalID != id.PrincipalID(unit.ReportingService) {
		return fault.New(fault.InvalidInput, "report receipt principal is not the reporting service")
	}
	if key.Operation != reportOperation {
		return fault.New(fault.InvalidInput, fmt.Sprintf("report receipt operation %q is not %q", key.Operation, reportOperation))
	}
	if key.CallerKey != unit.SourceEventID {
		return fault.New(fault.InvalidInput, "report receipt caller key is not the source event id")
	}
	if accepted.Source.PrincipalID != id.PrincipalID(unit.ReportingService) {
		return fault.New(fault.InvalidInput, "accepted report event source is not the reporting service")
	}
	if accepted.CausationID != string(unit.VerificationID) {
		return fault.New(fault.InvalidInput, "accepted report event causation is not the verification id")
	}
	return nil
}

// acceptedReportEvent returns the exactly-one accepted resource.verified
// envelope of a report unit. The binding-declared type check belongs to the
// capability that holds the binding; the adapter proves the committed event
// shape is the accepted one.
func acceptedReportEvent(envelopes []event.Envelope) (event.Envelope, error) {
	var accepted event.Envelope
	count := 0
	for _, envelope := range envelopes {
		if envelope.Type != event.TypeVerifiedReport {
			continue
		}
		accepted = envelope
		count++
	}
	if count != 1 {
		return event.Envelope{}, fault.New(fault.InvalidInput, "verified report unit must carry exactly one resource.verified event")
	}
	return accepted, nil
}

// EvidenceReader is the store-backed verify.EvidenceReader.
//
// ReadEvidence returns the durable prior request facts of one execution: actions
// that are request actions, succeeded, and resolved with a pass decision. The
// typed approval match itself stays in the verifier; the adapter returns the
// stored assignment, subject and resolution time unchanged.
//
// The action row's request_subject column carries the immutable
// definition.Subject the request-activation adapter persisted (an object keyed
// by field name with literal/reference values). A request used as approval
// evidence carries its `operation` and `resource` in that subject alongside the
// effect fields (KODAMA.md, S05 §4), so the reader derives both from the stored
// literals instead of echoing the query. This reader decodes that exact shape
// strictly and converts it to verify.Subject; a reference that was never
// resolved, an unknown value shape or an empty recorded subject is an
// internal_failure, never a silently empty subject.
//
// A subject that omits either approval field is still returned as evidence, but
// with that field empty. The verifier's exact operation/resource comparison
// then fails closed: a missing stored fact can never match a validated query,
// and one malformed historical row does not fail the whole read.
type EvidenceReader struct {
	scanner store.Scanner
}

// NewEvidenceReader builds the evidence reader.
func NewEvidenceReader(scanner store.Scanner) (*EvidenceReader, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "evidence reader requires a scanner")
	}
	return &EvidenceReader{scanner: scanner}, nil
}

var _ verify.EvidenceReader = (*EvidenceReader)(nil)

// ReadEvidence returns every matching prior-request fact in one execution.
func (reader *EvidenceReader) ReadEvidence(ctx context.Context, query verify.EvidenceQuery) ([]verify.Evidence, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	evidence := make([]verify.Evidence, 0)
	cursor := ""
	for {
		page, err := reader.scanner.Query(ctx, query.Scope, store.ScanQuery{
			Table: actionsTable,
			Equals: map[string]any{
				executionIDColumn:     string(query.ExecutionID),
				"kind":                requestActionKind,
				stateColumn:           succeededActionState,
				requestDecisionColumn: passRequestDecision,
			},
			OrderBy: []store.Order{{Column: idColumn}},
			After:   cursor,
			Limit:   store.MaxQueryLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Rows {
			fact, err := decodeEvidenceRow(row, query)
			if err != nil {
				return nil, err
			}
			evidence = append(evidence, fact)
		}
		if !page.HasMore {
			return evidence, nil
		}
		cursor = page.Next
	}
}

func decodeEvidenceRow(row store.Row, query verify.EvidenceQuery) (verify.Evidence, error) {
	actionID, err := textValue(row, idColumn)
	if err != nil {
		return verify.Evidence{}, err
	}
	decision, err := textValue(row, requestDecisionColumn)
	if err != nil {
		return verify.Evidence{}, err
	}
	assignmentBytes, err := bytesValue(row, assignmentColumn)
	if err != nil {
		return verify.Evidence{}, err
	}
	subjectBytes, err := bytesValue(row, requestSubjectColumn)
	if err != nil {
		return verify.Evidence{}, err
	}
	resolvedAt, err := nullableTimeValue(row, requestResolvedAtCol)
	if err != nil {
		return verify.Evidence{}, err
	}
	var assignment request.Assignment
	if err := decodeJSON(assignmentBytes, &assignment, assignmentColumn); err != nil {
		return verify.Evidence{}, err
	}
	stored, subject, err := decodeStoredSubject(subjectBytes)
	if err != nil {
		return verify.Evidence{}, err
	}
	// The approval operation and resource are stored literals of the immutable
	// request subject. A missing field stays empty so the verifier's exact
	// comparison refuses the evidence instead of this read failing.
	operation, _ := storedLiteral(stored, approvalOperationField)
	resourceID, _ := storedLiteral(stored, approvalResourceField)
	return verify.Evidence{
		ExecutionID: query.ExecutionID,
		ActionID:    id.ActionID(actionID),
		Assignment:  assignment,
		Operation:   operation,
		ResourceID:  resource.ResourceID(resourceID),
		Subject:     subject,
		Decision:    request.Decision(decision),
		ResolvedAt:  resolvedAt,
	}, nil
}

// The stored request subject field names that carry the approval's operation
// and resource (KODAMA.md, S05 §4).
const (
	approvalOperationField = "operation"
	approvalResourceField  = "resource"
)

// storedLiteral returns one stored subject literal by name. A missing field, an
// unresolved reference or any other shape is reported as absent; the strict
// decoder has already refused malformed subjects before this is called.
func storedLiteral(subject definition.Subject, name string) (string, bool) {
	value, present := subject.Fields()[name]
	if !present || value.Kind != definition.SubjectLiteral {
		return "", false
	}
	return value.Literal, true
}

// decodeStoredSubject strictly decodes the persisted definition.Subject shape
// and converts it to the typed verify.Subject. Every field must be a resolved
// literal; a reference, an unknown kind, an empty name or an empty recorded
// subject refuses as internal_failure because a committed request action can
// never carry one.
func decodeStoredSubject(data []byte) (definition.Subject, verify.Subject, error) {
	if len(data) == 0 {
		return definition.Subject{}, verify.Subject{}, missingColumn(actionsTable, requestSubjectColumn)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var stored definition.Subject
	if err := decoder.Decode(&stored); err != nil {
		return definition.Subject{}, verify.Subject{}, fault.Wrap(fault.InternalFailure,
			fmt.Sprintf("%s row column %q does not decode as a definition subject", actionsTable, requestSubjectColumn), err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return definition.Subject{}, verify.Subject{}, fault.New(fault.InternalFailure,
			fmt.Sprintf("%s row column %q carries trailing data", actionsTable, requestSubjectColumn))
	}
	fields := stored.Fields()
	if len(fields) == 0 {
		return definition.Subject{}, verify.Subject{}, fault.New(fault.InternalFailure,
			fmt.Sprintf("%s row column %q carries an empty request subject", actionsTable, requestSubjectColumn))
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	subject := verify.Subject{Fields: make([]verify.SubjectField, 0, len(names))}
	for _, name := range names {
		if name == "" {
			return definition.Subject{}, verify.Subject{}, fault.New(fault.InternalFailure,
				fmt.Sprintf("%s row column %q carries an unnamed subject field", actionsTable, requestSubjectColumn))
		}
		value := fields[name]
		switch value.Kind {
		case definition.SubjectLiteral:
			// The approval operation and resource are immutable fields of the
			// recorded request subject. They stay in the matched subject so the
			// enforcing service's canonical five-field subject compares field by
			// field; the dedicated Evidence fields are derived from them below.
			subject.Fields = append(subject.Fields, verify.SubjectField{
				Name:  name,
				Kind:  verify.KindString,
				Value: value.Literal,
			})
		case definition.SubjectReference:
			return definition.Subject{}, verify.Subject{}, fault.New(fault.InternalFailure,
				fmt.Sprintf("%s row column %q carries an unresolved subject reference %q", actionsTable, requestSubjectColumn, name))
		default:
			return definition.Subject{}, verify.Subject{}, fault.New(fault.InternalFailure,
				fmt.Sprintf("%s row column %q carries an unmappable subject kind %q", actionsTable, requestSubjectColumn, value.Kind))
		}
	}
	return stored, subject, nil
}

// receiptPointer returns the unit's receipt as the store pointer shape, or nil
// when the capability attached no receipt.
func receiptPointer(record receipt.Record) *receipt.Record {
	if record.ID == "" {
		return nil
	}
	copied := record
	return &copied
}

// nullableIdentity renders an optional identity as a SQL NULL or its text.
func nullableIdentity(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// optionalAssignment renders a zero assignment as SQL NULL.
func optionalAssignment(assignment request.Assignment) any {
	if assignment == (request.Assignment{}) {
		return nil
	}
	return assignment
}
