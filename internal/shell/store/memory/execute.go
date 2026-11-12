package memory

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// absenceGuard is the single declaration-derived switch for the absence
// guard. It mirrors ControllerEventCommitImplemented: the adapters consult the
// declaration, each keeps an overridable unexported gate for tests, and
// while the gate is off the guard refuses the unit before anything is staged.
var absenceGuard = store.AbsenceGuardImplemented

// Execute applies one complete commit unit against a transactional copy of the
// shared committed state and publishes it all-or-nothing.
//
// The whole unit is validated before any effect, then an existing receipt is
// consulted: equal content replays the original durable result and applies
// nothing, while changed content is refused. Locks are validated in the
// declared lock order, writes are applied in the declared write order, and the
// receipt, audit rows and event publication intent are published together with
// them. A refused, failed or canceled unit leaves no partial effect behind.
func (s *Store) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if err := ctx.Err(); err != nil {
		return store.UnitResult{}, err
	}
	if err := scope.Validate(); err != nil {
		return store.UnitResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executeLocked(scope, unit, false)
}

// executeLocked applies one unit while the store lock is held. controller
// selects the cross-scope controller seam's rules: the caller already derived
// the owning scope from the execution row and the unit may contain only state
// rows inside that scope.
func (s *Store) executeLocked(scope tenancy.Scope, unit store.Unit, controller bool) (store.UnitResult, error) {
	// The absence guard is declared and implemented by this adapter. While
	// the single declaration-derived gate is off the adapter refuses such a
	// unit before it stages anything, so a declared-but-unimplemented guard can
	// never be silently ignored.
	if !absenceGuard && unit.HasAbsenceGuard() {
		return store.UnitResult{}, faultInvalid("the memory adapter does not implement the absence guard")
	}
	// The whole unit is validated before it is planned, so a malformed absence
	// guard (or any other malformed shape) is invalid_input before any effect.
	if err := unit.Validate(); err != nil {
		return store.UnitResult{}, err
	}
	now := s.now()
	locks, err := planLocks(scope, unit.Locks)
	if err != nil {
		return store.UnitResult{}, err
	}
	writes, err := planUnitWrites(scope, unit.Writes)
	if err != nil {
		return store.UnitResult{}, err
	}
	if controller {
		// The controller holds execution-bound event/audit authority. While
		// the declaration-derived gate is off the adapter refuses those rows;
		// the single declaration is the only place the refusal is lifted.
		if unit.Receipt != nil {
			return store.UnitResult{}, faultForbidden("the controller cannot commit a receipt")
		}
		if !controllerEventCommit && (len(unit.Audit) > 0 || len(unit.Events) > 0) {
			return store.UnitResult{}, faultForbidden("the controller cannot commit an audit row or event until the controller event write path is implemented")
		}
	}

	plan := receiptPlan{}
	if unit.Receipt != nil {
		plan, err = planReceipt(scope, *unit.Receipt, unit.ReceiptMode)
		if err != nil {
			return store.UnitResult{}, err
		}
		plan.expect = unit.ReceiptExpect
	}
	// Every event and audit row is validated against the same scope the unit is
	// applied under. For the controller that scope was derived from the
	// guarded execution row inside this atomic section, so a row in another
	// tenant or project refuses the whole unit before anything is staged.
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

	// Resolve the receipt before staging anything. An equal-key insert replays
	// the original durable result and applies no business write, audit row or
	// event. A completion unit compare-and-sets on the stored completion marker
	// and content hash, so a concurrent second completer replays the first
	// result instead of overwriting it.
	var receiptRecord receipt.Record
	completing := false
	if plan.has {
		switch plan.mode {
		case store.ReceiptInsert, store.ReceiptInsertIncomplete:
			if existing, found := s.state.receipts[plan.record.Key]; found {
				if !sameContentHash(existing.ContentHash, plan.record.ContentHash) {
					return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent,
						"a receipt with the same caller key already holds different request content")
				}
				return store.UnitResult{Replayed: true, Receipt: copyReceipt(existing)}, nil
			}
		case store.ReceiptComplete:
			existing, found := s.state.receipts[plan.record.Key]
			if !found {
				return store.UnitResult{}, faultConflict("the receipt to complete is missing")
			}
			if !sameContentHash(existing.ContentHash, plan.expect.ContentHash) ||
				!sameContentHash(existing.ContentHash, plan.record.ContentHash) {
				return store.UnitResult{}, fault.New(fault.ChangedIdempotencyContent,
					"a receipt with the same caller key already holds different request content")
			}
			if s.state.receiptCompleted[plan.record.Key] {
				return store.UnitResult{Replayed: true, Receipt: copyReceipt(existing)}, nil
			}
			if plan.expect.Completed {
				return store.UnitResult{}, faultConflict("the receipt is not at the expected completion state")
			}
			completing = true
			receiptRecord = existing
		}
	}

	var recordPointer *receipt.Record
	if plan.has && !completing {
		recordPointer = &plan.record
	}
	if err := checkPublicationIdentities(s.state, recordPointer, unit.Audit, unit.Events); err != nil {
		return store.UnitResult{}, err
	}

	staged := s.state.clone()
	if err := applyLocks(staged, scope, locks, now, controller); err != nil {
		return store.UnitResult{}, err
	}
	for _, write := range writes {
		if err := applyWrite(staged, scope, write, now, controller); err != nil {
			return store.UnitResult{}, err
		}
	}
	if plan.has {
		switch plan.mode {
		case store.ReceiptInsert:
			stored := copyReceipt(plan.record)
			if stored.Result == nil {
				stored.Result = []byte{}
			}
			staged.receipts[stored.Key] = stored
			staged.receiptCompleted[stored.Key] = true
			receiptRecord = stored
		case store.ReceiptInsertIncomplete:
			// The receipt completion check lets an incomplete receipt carry
			// the in-progress result (the trigger Select's persisted
			// selection). The NULL completion marker is what keeps it
			// incomplete; a later completion overwrites the result.
			stored := copyReceipt(plan.record)
			staged.receipts[stored.Key] = stored
			staged.receiptCompleted[stored.Key] = false
			receiptRecord = stored
		case store.ReceiptComplete:
			stored := copyReceipt(receiptRecord)
			stored.Result = copyBytes(plan.record.Result)
			if stored.Result == nil {
				stored.Result = []byte{}
			}
			stored.ExecutionID = plan.record.ExecutionID
			if plan.record.SnapshotHash != nil {
				digest := *plan.record.SnapshotHash
				stored.SnapshotHash = &digest
			}
			staged.receipts[stored.Key] = stored
			staged.receiptCompleted[stored.Key] = true
			receiptRecord = stored
		}
	}
	staged.audits = append(staged.audits, unit.Audit...)
	for _, envelope := range unit.Events {
		staged.sequence++
		staged.events = append(staged.events, CommittedEvent{
			Sequence: staged.sequence,
			Envelope: copyEnvelope(envelope),
		})
	}

	// FailNextCommit simulates a failure after the unit is fully staged and
	// before anything is published. The armed error is returned unchanged and
	// the staged copy is discarded, so no effect becomes visible and no
	// publication sequence is consumed.
	if s.failNext != nil {
		armed := s.failNext
		s.failNext = nil
		return store.UnitResult{}, armed
	}

	s.state = staged
	result := store.UnitResult{Replayed: false}
	if plan.has {
		result.Receipt = copyReceipt(receiptRecord)
	}
	return result, nil
}

// receiptPlan is one validated receipt operation: the record, its mode and, for
// a completion unit, the required prior state.
type receiptPlan struct {
	has    bool
	mode   store.ReceiptMode
	record receipt.Record
	expect *store.ReceiptExpect
}

// planReceipt validates one receipt record against the unit scope. The
// receipt-mode shape is already checked by store.Unit.Validate; the durable
// completion marker is resolved later, inside the same atomic section.
func planReceipt(scope tenancy.Scope, record receipt.Record, mode store.ReceiptMode) (receiptPlan, error) {
	if err := validateReceipt(scope, record); err != nil {
		return receiptPlan{}, err
	}
	return receiptPlan{has: true, mode: mode, record: record}, nil
}

// applyLocks verifies every locked row exists inside the unit scope, then
// asserts every read-only guard. A zero-row lock or guard is a version_conflict:
// the row is missing, out of scope, or no longer at the expected version, so the
// unit must not continue. Under the controller seam a row that exists outside
// the derived owning scope is forbidden rather than merely missing.
//
// An absence guard is the inverse: it scans the staged table for a row whose
// registered unique-key columns equal the guard key and is a version_conflict
// when one exists. NULL matches only NULL, so a tenant-wide key and a project
// key are different identities. The check runs inside the store's single atomic
// section before any write, so a concurrently committed first grant and this
// unit cannot both succeed.
func applyLocks(staged *state, scope tenancy.Scope, locks []plannedLock, now time.Time, controller bool) error {
	for _, lock := range locks {
		if lock.absence {
			if _, present := absenceMatch(staged, lock); present {
				return faultConflict("an absence guard matched an existing row")
			}
			continue
		}
		values, ok := staged.visible(lock.meta, scope, lock.table, lock.identity, lock.tenantWide)
		if !ok {
			if controller && staged.hasRow(lock.table, lock.identity) {
				return faultForbidden("a locked row is outside the owning scope")
			}
			return faultConflict("a locked row is missing or out of scope")
		}
		if lock.guarded {
			if lock.expectVersion != 0 {
				current, ok := asUint64(values["version"])
				if !ok || current != lock.expectVersion {
					return faultConflict("a guarded row was not at the expected version")
				}
			}
			for name, expected := range lock.expect {
				if !valuesEqual(values[name], expected) {
					return faultConflict("a guarded row no longer matches its expected column values")
				}
			}
		}
		if lock.deadline != nil {
			if err := checkDeadline(values, *lock.deadline, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// absenceMatch returns the identity of a staged row whose registered unique-key
// columns equal the guard key. Every registered column must match exactly, and a
// NULL key column matches only a NULL stored value, so a tenant-wide row never
// satisfies a project-scoped guard and vice versa.
func absenceMatch(staged *state, lock plannedLock) (string, bool) {
	for identity, values := range staged.rows[lock.table] {
		matched := true
		for _, name := range lock.keyColumns {
			if !valuesEqual(values[name], lock.key[name]) {
				matched = false
				break
			}
		}
		if matched {
			return identity, true
		}
	}
	return "", false
}

// checkDeadline asserts that the guarded timestamp column of the row is
// strictly after the transaction clock. A NULL value or a value at or before
// the clock is deadline_expired.
func checkDeadline(values map[string]any, guard store.DeadlineGuard, now time.Time) error {
	stamp, ok := values[guard.Column].(time.Time)
	if !ok || !stamp.After(now) {
		return fault.New(fault.DeadlineExpired, "the guarded timestamp is not strictly after the transaction clock")
	}
	return nil
}

// applyWrite applies one validated write to the staged state. A duplicate insert
// and a zero-row update or delete are version_conflict, never a silent success,
// and so is an insert or update whose row violates a declared uniqueness rule.
// Under the controller seam a row that exists outside the derived owning scope
// is forbidden. An event write is the publisher's publish_state transition, not
// a generic business-row write.
func applyWrite(staged *state, scope tenancy.Scope, write plannedWrite, now time.Time, controller bool) error {
	if write.table == "events" {
		return applyEventPublishWrite(staged, scope, write)
	}
	switch {
	case write.insert:
		if _, exists := staged.row(write.table, write.identity); exists {
			return faultConflict("an inserted row already exists")
		}
		values := copyValues(write.values)
		values["id"] = write.identity
		if write.meta.hasTenant {
			values["tenant_id"] = string(scope.TenantID)
		}
		if write.meta.hasProject {
			if scope.TenantScoped() {
				values["project_id"] = nil
			} else {
				values["project_id"] = string(scope.ProjectID)
			}
		}
		if staged.rows[write.table] == nil {
			staged.rows[write.table] = make(map[string]map[string]any)
		}
		staged.rows[write.table][write.identity] = values
		return checkUnique(staged, write)
	case write.delete:
		values, ok := staged.visible(write.meta, scope, write.table, write.identity, false)
		if !ok {
			if controller && staged.hasRow(write.table, write.identity) {
				return faultForbidden("a deleted row is outside the owning scope")
			}
			return faultConflict("a deleted row is missing or out of scope")
		}
		if err := checkWriteGuards(values, write); err != nil {
			return err
		}
		if write.deadline != nil {
			if err := checkDeadline(values, *write.deadline, now); err != nil {
				return err
			}
		}
		delete(staged.rows[write.table], write.identity)
		return nil
	default:
		values, ok := staged.visible(write.meta, scope, write.table, write.identity, false)
		if !ok {
			if controller && staged.hasRow(write.table, write.identity) {
				return faultForbidden("an updated row is outside the owning scope")
			}
			return faultConflict("an updated row is missing or out of scope")
		}
		if err := checkWriteGuards(values, write); err != nil {
			return err
		}
		if write.deadline != nil {
			if err := checkDeadline(values, *write.deadline, now); err != nil {
				return err
			}
		}
		for name, value := range write.values {
			values[name] = copyValue(value)
		}
		if write.meta.hasVersion {
			current, ok := asUint64(values["version"])
			if !ok {
				return faultInternal("the stored row has no usable version")
			}
			values["version"] = int64(current + 1)
		}
		return checkUnique(staged, write)
	}
}

// applyEventPublishWrite applies the publisher's acknowledgement to one
// committed event row. It is the only generic write events accepts: the row
// identity and scope must already match, the explicit expectation is checked,
// and only publish_state may move (pending -> published).
func applyEventPublishWrite(staged *state, scope tenancy.Scope, write plannedWrite) error {
	for index := range staged.events {
		committed := &staged.events[index]
		if string(committed.Envelope.ID) != write.identity {
			continue
		}
		if !scopeVisible(scope, committed.Envelope.Scope) {
			return faultConflict("an updated event is missing or out of scope")
		}
		values := eventRowValues(*committed)
		if err := checkWriteGuards(values, write); err != nil {
			return err
		}
		published, ok := write.values["publish_state"].(string)
		if !ok {
			return faultInvalid("an event publication write must set publish_state")
		}
		committed.Published = published == "published"
		return nil
	}
	return faultConflict("an updated event is missing or out of scope")
}

// checkPublicationIdentities enforces the primary-key uniqueness the PostgreSQL
// reference gets from its receipts, events and audit primary keys. A receipt,
// event or audit id that already exists in committed state, or appears twice in
// the same unit, is a version_conflict before anything is staged or published,
// so the whole unit rolls back and no publication sequence is consumed. A
// completion unit passes no record: it advances the row already stored under
// the same receipt key.
func checkPublicationIdentities(committed *state, record *receipt.Record, audits []event.Audit, events []event.Envelope) error {
	if record != nil {
		for _, existing := range committed.receipts {
			if existing.ID == record.ID {
				return faultConflict("a receipt with the same id already exists")
			}
		}
	}
	auditIDs := make(map[string]struct{}, len(audits))
	for _, audit := range audits {
		identity := string(audit.ID)
		if _, duplicate := auditIDs[identity]; duplicate {
			return faultConflict("an audit row with the same id appears twice in one unit")
		}
		auditIDs[identity] = struct{}{}
		for _, existing := range committed.audits {
			if existing.ID == audit.ID {
				return faultConflict("an audit row with the same id already exists")
			}
		}
	}
	eventIDs := make(map[string]struct{}, len(events))
	for _, envelope := range events {
		identity := string(envelope.ID)
		if _, duplicate := eventIDs[identity]; duplicate {
			return faultConflict("an event with the same id appears twice in one unit")
		}
		eventIDs[identity] = struct{}{}
		for _, existing := range committed.events {
			if existing.Envelope.ID == envelope.ID {
				return faultConflict("an event with the same id already exists")
			}
		}
	}
	return nil
}

// checkWriteGuards asserts the optimistic version and explicit column
// expectations of one update or delete against the staged row.
func checkWriteGuards(values map[string]any, write plannedWrite) error {
	if write.meta.hasVersion && write.expectVersion != 0 {
		current, ok := asUint64(values["version"])
		if !ok || current != write.expectVersion {
			return faultConflict("a written row was not at the expected version")
		}
	}
	for name, expected := range write.expect {
		if !valuesEqual(values[name], expected) {
			return faultConflict("a written row no longer matches its expected column values")
		}
	}
	return nil
}

// CommittedEvent is one committed event row and its global publication sequence.
type CommittedEvent struct {
	Sequence uint64
	Envelope event.Envelope
	// Published reports the events.publish_state acknowledgement. A committed
	// row is publication intent until the outbox marks it published.
	Published bool
}
