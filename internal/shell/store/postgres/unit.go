package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// absenceGuard is the single declaration-derived switch for the absence
// guard. It mirrors ControllerEventCommitImplemented: the adapters consult the
// declaration, each keeps an overridable unexported gate for tests, and
// while the gate is off the guard refuses the unit before any statement.
var absenceGuard = store.AbsenceGuardImplemented

// ExecuteUnit applies one complete commit unit inside exactly one scoped
// transaction. The unit, its locks and its writes are the neutral store types,
// so this SQL-level helper and the Store.Executor seam share one definition.
//
// Everything is validated before the transaction starts, so a refused unit
// leaves no statement and no open transaction behind. Inside the transaction
// the adapter reads any existing receipt first (an equal receipt replays the
// committed result and a changed one is refused), acquires every declared row
// lock in the declared lock order, applies the business writes in that same
// order, and only then commits the receipt, audit rows and event publication
// intent. Any failure rolls the whole unit back; a failed commit is never
// reported as success and never retried.
func ExecuteUnit(ctx context.Context, connector store.ScopedConnection, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if connector == nil {
		return store.UnitResult{}, faultInvalid("commit unit has no scoped connection")
	}
	if err := scope.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	// store.Unit.Validate is the single authority on receipt-mode shape: an
	// incomplete insert may carry an in-progress result, while a completed
	// insert requires one and an insert-mode unit carries no completion
	// expectation. Validate before any statement so a refused unit leaves no
	// transaction behind.
	if err := unit.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	// The absence guard is declared and implemented by this adapter. While
	// the single declaration-derived gate is off the adapter refuses such a
	// unit before it opens a transaction, so a declared-but-unimplemented guard
	// can never be silently ignored.
	if !absenceGuard && unit.HasAbsenceGuard() {
		return store.UnitResult{}, faultInvalid("the PostgreSQL adapter does not implement the absence guard")
	}

	locks, err := planLocks(scope, unit.Locks)
	if err != nil {
		return store.UnitResult{}, err
	}
	writes, err := planUnitWrites(scope, unit.Writes)
	if err != nil {
		return store.UnitResult{}, err
	}
	// An insert into an absence-guardable table must take its advisory lock in
	// the declared lock order, like the guard itself, so fold those locks into
	// the lock phase before any statement runs.
	locks = addInsertAdvisories(locks, writes)
	for _, audit := range unit.Audit {
		if err := validateAudit(scope, audit); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, envelope := range unit.Events {
		if err := validateEvent(scope, envelope); err != nil {
			return store.UnitResult{}, err
		}
	}

	var record receipt.Record
	hasReceipt := unit.Receipt != nil
	if hasReceipt {
		record = *unit.Receipt
		if err := validateReceipt(scope, record); err != nil {
			return store.UnitResult{}, err
		}
	}
	if err := validateReceiptMode(unit); err != nil {
		return store.UnitResult{}, err
	}

	tx, err := connector.Begin(ctx, scope)
	if err != nil {
		return store.UnitResult{}, err
	}
	// Rollback is a no-op once the transaction is closed, so this defer never
	// issues a second terminal call after an explicit commit or rollback.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var existing receipt.Record
	existingCompleted := false
	found := false
	if hasReceipt {
		existing, existingCompleted, found, err = readReceipt(ctx, tx, record.Key)
		if err != nil {
			return store.UnitResult{}, err
		}
	}
	switch unit.ReceiptMode {
	case store.ReceiptInsert, store.ReceiptInsertIncomplete:
		if found {
			if !sameContentHash(existing.ContentHash, record.ContentHash) {
				return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent,
					"a receipt with the same caller key already holds different request content")
			}
			if err := tx.Commit(ctx); err != nil {
				return store.UnitResult{}, err
			}
			return store.UnitResult{Replayed: true, Receipt: existing}, nil
		}
	case store.ReceiptComplete:
		// Compare-and-set first: only the winner applies the unit's business
		// writes. A concurrent completer blocks on the receipt row, then
		// reclassifies its zero-row outcome as a replay or a conflict, so it can
		// never overwrite the first committed result.
		applied, replayed, replayRecord, err := completeReceipt(ctx, tx, record, *unit.ReceiptExpect, existing, existingCompleted, found)
		if err != nil {
			return store.UnitResult{}, err
		}
		if replayed {
			if err := tx.Commit(ctx); err != nil {
				return store.UnitResult{}, err
			}
			return store.UnitResult{Replayed: true, Receipt: replayRecord}, nil
		}
		if !applied {
			return store.UnitResult{}, faultConflict("the receipt to complete is missing")
		}
	}

	if err := applyLocks(ctx, tx, locks); err != nil {
		return store.UnitResult{}, err
	}
	for _, write := range writes {
		if err := applyWrite(ctx, tx, write); err != nil {
			return store.UnitResult{}, err
		}
	}
	if hasReceipt && unit.ReceiptMode != store.ReceiptComplete {
		statement, err := receiptInsertStatement(record, unit.ReceiptMode)
		if err != nil {
			return store.UnitResult{}, err
		}
		if _, err := tx.Exec(ctx, statement); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, audit := range unit.Audit {
		if _, err := tx.Exec(ctx, auditInsertStatement(audit)); err != nil {
			return store.UnitResult{}, err
		}
	}
	for _, envelope := range unit.Events {
		if _, err := tx.Exec(ctx, eventInsertStatement(envelope)); err != nil {
			return store.UnitResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if hasAbsenceInsert(writes) {
			return store.UnitResult{}, mapAbsenceInsertError(err)
		}
		return store.UnitResult{}, err
	}
	return store.UnitResult{Replayed: false, Receipt: record}, nil
}

// hasAbsenceInsert reports whether the unit inserted into a table with a
// registered absence-guardable unique key. A unique violation surfaced by the
// final commit of such a unit is the same version_conflict a guard conflict is.
func hasAbsenceInsert(writes []plannedWrite) bool {
	for _, write := range writes {
		if write.advisory != nil {
			return true
		}
	}
	return false
}

// validateReceiptMode checks the adapter-level receipt shape the neutral seam
// leaves open: a non-insert mode must carry a receipt. store.Unit.Validate
// is enforced separately and owns the receipt result rules.
func validateReceiptMode(unit store.Unit) error {
	if !unit.ReceiptMode.Valid() {
		return faultInvalid("receipt mode is not frozen")
	}
	if unit.Receipt == nil {
		if unit.ReceiptMode != store.ReceiptInsert {
			return faultInvalid("a receipt mode requires a receipt")
		}
		if unit.ReceiptExpect != nil {
			return faultInvalid("a receipt expectation requires a receipt")
		}
		return nil
	}
	switch unit.ReceiptMode {
	case store.ReceiptInsert, store.ReceiptInsertIncomplete:
		if unit.ReceiptExpect != nil {
			return faultInvalid("an insert-mode unit cannot carry a receipt expectation")
		}
		if unit.Receipt == nil {
			return faultInvalid("an insert-mode unit requires a receipt")
		}
	case store.ReceiptComplete:
		if unit.Receipt == nil || unit.ReceiptExpect == nil {
			return faultInvalid("a completion-mode unit requires a receipt and its expectation")
		}
		if err := unit.ReceiptExpect.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// completeReceipt runs the single compare-and-set that advances an incomplete
// receipt. It returns applied=true when the update matched, replayed=true with
// the original record when the receipt was already completed with equal content,
// and an error for changed content or a stale expectation.
func completeReceipt(
	ctx context.Context,
	tx store.Transaction,
	record receipt.Record,
	expect store.ReceiptExpect,
	existing receipt.Record,
	completed bool,
	found bool,
) (applied, replayed bool, replayRecord receipt.Record, err error) {
	if !found {
		return false, false, receipt.Record{}, nil
	}
	if !sameContentHash(existing.ContentHash, expect.ContentHash) || !sameContentHash(existing.ContentHash, record.ContentHash) {
		return false, false, receipt.Record{}, fault.New(fault.ChangedIdempotencyContent,
			"a receipt with the same caller key already holds different request content")
	}
	if completed {
		return false, true, existing, nil
	}
	if expect.Completed {
		return false, false, receipt.Record{}, faultConflict("the receipt is not at the expected completion state")
	}
	statement, err := receiptCompleteStatement(record, expect)
	if err != nil {
		return false, false, receipt.Record{}, err
	}
	result, err := tx.Exec(ctx, statement)
	if err != nil {
		return false, false, receipt.Record{}, err
	}
	if result.RowsAffected > 0 {
		return true, false, receipt.Record{}, nil
	}
	// Zero-row compare-and-set: re-read inside the same transaction and
	// reclassify, never overwrite.
	current, currentCompleted, currentFound, err := readReceipt(ctx, tx, record.Key)
	if err != nil {
		return false, false, receipt.Record{}, err
	}
	if !currentFound {
		return false, false, receipt.Record{}, nil
	}
	if !sameContentHash(current.ContentHash, record.ContentHash) {
		return false, false, receipt.Record{}, fault.New(fault.ChangedIdempotencyContent,
			"a receipt with the same caller key already holds different request content")
	}
	if currentCompleted {
		return false, true, current, nil
	}
	return false, false, receipt.Record{}, faultConflict("the receipt is not at the expected completion state")
}

// planUnitWrites validates and renders every write, then orders them by the
// deterministic write rank so a unit applies its tables in the declared order.
// Equal ranks keep the caller's relative order.
func planUnitWrites(scope tenancy.Scope, writes []store.Write) ([]plannedWrite, error) {
	planned := make([]plannedWrite, 0, len(writes))
	for _, write := range writes {
		rendered, err := planWrite(scope, write)
		if err != nil {
			return nil, err
		}
		planned = append(planned, rendered)
	}
	sort.SliceStable(planned, func(left, right int) bool {
		return planned[left].rank < planned[right].rank
	})
	return planned, nil
}

// validateAudit refuses an audit row outside the unit scope or with an unusable
// identity, actor, operation or time.
func validateAudit(scope tenancy.Scope, audit event.Audit) error {
	if audit.Scope != scope {
		return faultForbidden("audit record scope does not match the commit unit scope")
	}
	if _, err := id.ParseEventID(string(audit.ID)); err != nil {
		return faultInvalid("audit record id is invalid")
	}
	if audit.Actor == "" {
		return faultInvalid("audit record actor is empty")
	}
	if audit.Operation == "" {
		return faultInvalid("audit record operation is empty")
	}
	if audit.At.IsZero() {
		return faultInvalid("audit record time is zero")
	}
	return nil
}

// validateEvent refuses an event envelope outside the unit scope or with an
// unusable type, source, time or payload. The envelope rules themselves are the
// event contract's.
func validateEvent(scope tenancy.Scope, envelope event.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if envelope.Scope != scope {
		return faultForbidden("event scope does not match the commit unit scope")
	}
	return nil
}

// validateReceipt refuses a receipt whose identity, content hash or scope cannot
// protect the unit. Receipt identity is (tenant, project, principal, operation,
// caller key), and the project must be exactly the unit scope's project.
func validateReceipt(scope tenancy.Scope, record receipt.Record) error {
	if err := record.Key.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseReceiptID(string(record.ID)); err != nil {
		return faultInvalid("receipt id is invalid")
	}
	if _, err := contentHashBytes(record.ContentHash); err != nil {
		return faultInvalid("receipt content hash is not a sha256 fingerprint")
	}
	if record.At.IsZero() {
		return faultInvalid("receipt time is zero")
	}
	if string(record.Key.TenantID) != string(scope.TenantID) || string(record.Key.ProjectID) != string(scope.ProjectID) {
		return faultForbidden("receipt identity does not match the commit unit scope")
	}
	return nil
}

// receiptLookupSQL reads the durable receipt for one idempotency identity.
// project_id uses IS NOT DISTINCT FROM because a tenant-scoped receipt stores
// SQL NULL and ordinary equality would never match it.
const receiptLookupSQL = `SELECT id, tenant_id, project_id, principal_id, operation, caller_key, content_sha256, result, completed_at, execution_id, snapshot_sha256, created_at FROM receipts WHERE tenant_id = $1 AND project_id IS NOT DISTINCT FROM $2 AND principal_id = $3 AND operation = $4 AND caller_key = $5`

// receiptColumns is the receipt lookup's SELECT list in column order.
var receiptColumns = []string{
	"id", "tenant_id", "project_id", "principal_id", "operation",
	"caller_key", "content_sha256", "result", "completed_at", "execution_id", "snapshot_sha256", "created_at",
}

// readReceipt returns the durable receipt for a key, if one was committed. It
// runs inside the unit transaction so the replay decision and any business write
// cannot be separated. completed reports whether the completion marker is set.
func readReceipt(ctx context.Context, tx store.Transaction, key receipt.Key) (receipt.Record, bool, bool, error) {
	var project any
	if key.ProjectID != "" {
		project = string(key.ProjectID)
	}
	rows, err := tx.Query(ctx, store.Query{
		SQL:  receiptLookupSQL,
		Args: []any{string(key.TenantID), project, string(key.PrincipalID), key.Operation, key.CallerKey},
	})
	if err != nil {
		return receipt.Record{}, false, false, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return receipt.Record{}, false, false, rows.Err()
	}
	meta, ok := lookupTable("receipts")
	if !ok {
		return receipt.Record{}, false, false, faultInternal("the frozen schema has no receipts table")
	}
	values, err := scanColumns(meta, receiptColumns, rows)
	if err != nil {
		return receipt.Record{}, false, false, err
	}
	if err := rows.Err(); err != nil {
		return receipt.Record{}, false, false, err
	}
	record, completed, err := receiptFromValues(values)
	if err != nil {
		return receipt.Record{}, false, false, err
	}
	return record, completed, true, nil
}

// receiptFromValues rebuilds one durable receipt from its row values.
func receiptFromValues(values map[string]any) (receipt.Record, bool, error) {
	identity, _ := values["id"].(string)
	tenant, _ := values["tenant_id"].(string)
	principal, _ := values["principal_id"].(string)
	operation, _ := values["operation"].(string)
	callerKey, _ := values["caller_key"].(string)
	project, _ := values["project_id"].(string)
	execution, _ := values["execution_id"].(string)
	created, _ := values["created_at"].(time.Time)
	contentHash, ok := values["content_sha256"].([]byte)
	if !ok || len(contentHash) == 0 {
		return receipt.Record{}, false, faultInternal("durable receipt has no content hash")
	}
	_, completed := values["completed_at"].(time.Time)
	record := receipt.Record{
		ID:          id.ReceiptID(identity),
		Key:         receipt.Key{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project), PrincipalID: id.PrincipalID(principal), Operation: operation, CallerKey: callerKey},
		ContentHash: "sha256:" + hex.EncodeToString(contentHash),
		ExecutionID: id.ExecutionID(execution),
		At:          created,
	}
	if result, ok := values["result"].([]byte); ok {
		record.Result = result
	}
	if snapshot, ok := values["snapshot_sha256"].([]byte); ok && len(snapshot) == 32 {
		var digest [32]byte
		copy(digest[:], snapshot)
		record.SnapshotHash = &digest
	}
	return record, completed, nil
}

// contentHashBytes decodes a receipt content fingerprint into its bytea form.
func contentHashBytes(value string) ([]byte, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return nil, fmt.Errorf("receipt content hash has no sha256 prefix")
	}
	decoded, err := hex.DecodeString(value[len(prefix):])
	if err != nil {
		return nil, err
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("receipt content hash is %d bytes; want 32", len(decoded))
	}
	return decoded, nil
}

// sameContentHash compares two receipt fingerprints byte-exactly, falling back
// to text equality only if a stored value is not a well-formed fingerprint.
func sameContentHash(left, right string) bool {
	leftBytes, leftErr := contentHashBytes(left)
	rightBytes, rightErr := contentHashBytes(right)
	if leftErr != nil || rightErr != nil {
		return left == right
	}
	return bytes.Equal(leftBytes, rightBytes)
}

const receiptInsertSQL = `INSERT INTO receipts (id, tenant_id, project_id, principal_id, operation, caller_key, content_sha256, result, completed_at, execution_id, snapshot_sha256, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

// receiptInsertStatement renders the receipt row committed with the unit. A
// plain insert is stored completed at insert; an incomplete insert stores a
// NULL completion marker and, when the unit carries one, its in-progress
// result (the trigger Select's persisted selection).
func receiptInsertStatement(record receipt.Record, mode store.ReceiptMode) (store.Statement, error) {
	contentHash, err := contentHashBytes(record.ContentHash)
	if err != nil {
		return store.Statement{}, faultInvalid("receipt content hash is not a sha256 fingerprint")
	}
	var project any
	if record.Key.ProjectID != "" {
		project = string(record.Key.ProjectID)
	}
	var execution any
	if record.ExecutionID != "" {
		execution = string(record.ExecutionID)
	}
	var result any
	var completed any
	if mode == store.ReceiptInsertIncomplete {
		// An incomplete receipt has a NULL completion marker. It MAY carry an
		// in-progress result, which a later completion overwrites.
		if record.Result != nil {
			result = record.Result
		}
	} else {
		if record.Result != nil {
			result = record.Result
		} else {
			result = []byte{}
		}
		completed = record.At
	}
	var snapshot any
	if record.SnapshotHash != nil {
		snapshot = record.SnapshotHash[:]
	}
	return store.Statement{
		SQL: receiptInsertSQL,
		Args: []any{
			string(record.ID), string(record.Key.TenantID), project, string(record.Key.PrincipalID),
			record.Key.Operation, record.Key.CallerKey, contentHash, result, completed, execution, snapshot, record.At,
		},
	}, nil
}

const receiptCompleteSQL = `UPDATE receipts SET result = $1, completed_at = $2, execution_id = $3, snapshot_sha256 = COALESCE($4, snapshot_sha256) WHERE tenant_id = $5 AND project_id IS NOT DISTINCT FROM $6 AND principal_id = $7 AND operation = $8 AND caller_key = $9 AND completed_at IS NULL AND content_sha256 = $10`

// receiptCompleteStatement renders the single compare-and-set that completes an
// incomplete receipt under its existing key, its NULL completion marker and its
// expected content hash. A zero-row outcome is reclassified by the caller.
func receiptCompleteStatement(record receipt.Record, expect store.ReceiptExpect) (store.Statement, error) {
	contentHash, err := contentHashBytes(expect.ContentHash)
	if err != nil {
		return store.Statement{}, faultInvalid("receipt expectation content hash is not a sha256 fingerprint")
	}
	var project any
	if record.Key.ProjectID != "" {
		project = string(record.Key.ProjectID)
	}
	var execution any
	if record.ExecutionID != "" {
		execution = string(record.ExecutionID)
	}
	var result any
	if record.Result != nil {
		result = record.Result
	} else {
		result = []byte{}
	}
	// A completion that observed a snapshot digest stores it; one that carries
	// none keeps the digest the incomplete insert stored instead of clearing
	// it, which is why the statement coalesces onto the stored column.
	var snapshot any
	if record.SnapshotHash != nil {
		snapshot = record.SnapshotHash[:]
	}
	return store.Statement{
		SQL: receiptCompleteSQL,
		Args: []any{
			result, record.At, execution, snapshot,
			string(record.Key.TenantID), project, string(record.Key.PrincipalID),
			record.Key.Operation, record.Key.CallerKey, contentHash,
		},
	}, nil
}

const auditInsertSQL = `INSERT INTO audit (id, tenant_id, project_id, actor, operation, at, detail) VALUES ($1, $2, $3, $4, $5, $6, $7)`

// auditInsertStatement renders one immutable audit row committed with the unit.
func auditInsertStatement(audit event.Audit) store.Statement {
	var project any
	if audit.Scope.ProjectID != "" {
		project = string(audit.Scope.ProjectID)
	}
	var detail any
	if audit.Detail != "" {
		detail = audit.Detail
	}
	return store.Statement{
		SQL: auditInsertSQL,
		Args: []any{
			string(audit.ID), string(audit.Scope.TenantID), project, string(audit.Actor),
			audit.Operation, audit.At, detail,
		},
	}
}

// eventInsertSQL writes the event row and its committed publication intent. The
// pending publication state is adapter-fixed SQL, not a caller value, and the
// publication sequence comes from the migration's sequence default so the
// publisher has a durable total order.
const eventInsertSQL = `INSERT INTO events (id, tenant_id, project_id, type, report_type, subject_id, subject_version, source_kind, source_principal, correlation_id, causation_id, at, data, publish_state) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'pending')`

// eventInsertStatement renders one event row committed with the unit.
func eventInsertStatement(envelope event.Envelope) store.Statement {
	var project any
	if envelope.Scope.ProjectID != "" {
		project = string(envelope.Scope.ProjectID)
	}
	var reportType any
	if envelope.ReportType != "" {
		reportType = string(envelope.ReportType)
	}
	var subjectVersion any
	if envelope.SubjectVersion != 0 {
		subjectVersion = envelope.SubjectVersion
	}
	var correlation any
	if envelope.CorrelationID != "" {
		correlation = envelope.CorrelationID
	}
	var causation any
	if envelope.CausationID != "" {
		causation = envelope.CausationID
	}
	var data any
	if envelope.Data != nil {
		data = envelope.Data
	}
	return store.Statement{
		SQL: eventInsertSQL,
		Args: []any{
			string(envelope.ID), string(envelope.Scope.TenantID), project, string(envelope.Type),
			reportType, envelope.SubjectID, subjectVersion, string(envelope.Source.Kind),
			string(envelope.Source.PrincipalID), correlation, causation, envelope.At, data,
		},
	}
}
