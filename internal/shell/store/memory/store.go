package memory

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var _ store.Executor = (*Store)(nil)
var _ event.Outbox = (*Store)(nil)

// Store is one independent, ephemeral in-memory persistence adapter. One Store
// owns one shared committed state guarded by one mutex: there are no
// per-capability maps with independent locks, so no caller can observe or
// publish part of a commit unit.
type Store struct {
	mu       sync.Mutex
	state    *state
	failNext error
	clock    func() time.Time
}

// New returns an independent, empty store. Dropping the *Store drops its state,
// and two stores never share rows.
func New() *Store {
	return NewWithClock(time.Now)
}

// NewWithClock returns an independent, empty store whose transaction clock is
// the injected clock. DeadlineGuard compares a stored timestamp against the
// value this clock reports inside the same atomic section, so a test can pin the
// transaction time deterministically. A nil clock means time.Now.
func NewWithClock(clock func() time.Time) *Store {
	if clock == nil {
		clock = time.Now
	}
	return &Store{state: newState(), clock: clock}
}

// now reads the store's transaction clock. The clock is immutable after
// construction, so no lock is required.
func (s *Store) now() time.Time {
	return s.clock()
}

// FailNextCommit arms exactly one deterministic failure. The armed error is
// returned unchanged by the next Execute call once its unit has been fully
// staged and before anything is published, so no staged effect becomes visible.
// It is a one-shot hook for tests.
func (s *Store) FailNextCommit(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// Read returns a defensive deep copy of one committed row inside the caller's
// scope. A cross-scope row never leaks; a missing row is not_found.
func (s *Store) Read(ctx context.Context, scope tenancy.Scope, table, identity string) (store.Row, error) {
	if err := ctx.Err(); err != nil {
		return store.Row{}, err
	}
	meta, ok := lookupTable(table)
	if !ok {
		return store.Row{}, faultInvalid(fmt.Sprintf("unknown table %q", table))
	}
	if err := scope.Validate(); err != nil {
		return store.Row{}, err
	}
	if !validIdentity(identity) {
		return store.Row{}, faultInvalid(fmt.Sprintf("row id %q is not a valid identity", identity))
	}
	if meta.projectRequired && scope.TenantScoped() {
		return store.Row{}, faultInvalid(fmt.Sprintf("table %q is project scoped; a tenant-scope read is not allowed", table))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch table {
	case "events":
		if row, ok := s.snapshotEvent(scope, identity); ok {
			return row, nil
		}
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found in scope", table, identity))
	case "audit":
		if row, ok := s.snapshotAudit(scope, identity); ok {
			return row, nil
		}
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found in scope", table, identity))
	}
	values, found := s.state.visible(meta, scope, table, identity, false)
	if !found {
		return store.Row{}, faultNotFound(fmt.Sprintf("%s %q was not found in scope", table, identity))
	}
	return store.Row{Table: table, Values: materializeValues(meta, values)}, nil
}

// Snapshot returns a defensive copy of one committed row by table/id inside the
// scope. It reports false when the row is absent or out of scope. Committed
// event and audit rows are snapshotted from their publication records so the
// contract vectors can inspect them like any other row.
func (s *Store) Snapshot(scope tenancy.Scope, table, identity string) (store.Row, bool) {
	meta, ok := lookupTable(table)
	if !ok {
		return store.Row{}, false
	}
	if err := scope.Validate(); err != nil {
		return store.Row{}, false
	}
	if !validIdentity(identity) {
		return store.Row{}, false
	}
	if meta.projectRequired && scope.TenantScoped() {
		return store.Row{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch table {
	case "events":
		return s.snapshotEvent(scope, identity)
	case "audit":
		return s.snapshotAudit(scope, identity)
	}
	values, found := s.state.visible(meta, scope, table, identity, false)
	if !found {
		return store.Row{}, false
	}
	return store.Row{Table: table, Values: materializeValues(meta, values)}, true
}

// snapshotEvent rebuilds one committed event row from its publication record.
func (s *Store) snapshotEvent(scope tenancy.Scope, identity string) (store.Row, bool) {
	for _, committed := range s.state.events {
		envelope := committed.Envelope
		if string(envelope.ID) != identity {
			continue
		}
		if !scopeVisible(scope, envelope.Scope) {
			return store.Row{}, false
		}
		return store.Row{Table: "events", Values: eventRowValues(committed)}, true
	}
	return store.Row{}, false
}

// snapshotAudit rebuilds one committed audit row from its commit record.
func (s *Store) snapshotAudit(scope tenancy.Scope, identity string) (store.Row, bool) {
	for _, audit := range s.state.audits {
		if string(audit.ID) != identity {
			continue
		}
		if !scopeVisible(scope, audit.Scope) {
			return store.Row{}, false
		}
		return store.Row{Table: "audit", Values: auditRowValues(audit)}, true
	}
	return store.Row{}, false
}

// eventRowValues renders one committed event as scoped column values.
func eventRowValues(committed CommittedEvent) map[string]any {
	envelope := committed.Envelope
	values := map[string]any{
		"id":               string(envelope.ID),
		"tenant_id":        string(envelope.Scope.TenantID),
		"project_id":       projectValue(envelope.Scope),
		"type":             string(envelope.Type),
		"report_type":      nullableString(string(envelope.ReportType)),
		"subject_id":       envelope.SubjectID,
		"subject_version":  nullableInt64(envelope.SubjectVersion),
		"source_kind":      string(envelope.Source.Kind),
		"source_principal": string(envelope.Source.PrincipalID),
		"correlation_id":   nullableString(envelope.CorrelationID),
		"causation_id":     nullableString(envelope.CausationID),
		"at":               envelope.At,
		"data":             copyBytes(envelope.Data),
		"publish_state":    publishStateValue(committed.Published),
		"publish_sequence": int64(committed.Sequence),
	}
	return values
}

// publishStateValue renders the events.publish_state column: a committed row
// starts as publication intent (pending) and a publisher acknowledgement moves
// it to published. No other value is declared by the schema.
func publishStateValue(published bool) string {
	if published {
		return "published"
	}
	return "pending"
}

// auditRowValues renders one committed audit row as scoped column values.
func auditRowValues(audit event.Audit) map[string]any {
	return map[string]any{
		"id":         string(audit.ID),
		"tenant_id":  string(audit.Scope.TenantID),
		"project_id": projectValue(audit.Scope),
		"actor":      string(audit.Actor),
		"operation":  audit.Operation,
		"at":         audit.At,
		"detail":     nullableString(audit.Detail),
	}
}

// projectValue is the project column value for one committed row: the project
// identity, or nil for a tenant-wide row so it reads as SQL NULL.
func projectValue(scope tenancy.Scope) any {
	if scope.TenantScoped() {
		return nil
	}
	return string(scope.ProjectID)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt64(value uint64) any {
	if value == 0 {
		return nil
	}
	return int64(value)
}

// Receipt returns a defensive copy of the durable receipt for one idempotency
// identity, if one was committed.
func (s *Store) Receipt(key receipt.Key) (receipt.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, found := s.state.receipts[key]
	if !found {
		return receipt.Record{}, false
	}
	return copyReceipt(record), true
}

// Events returns committed event rows visible in the scope in ascending
// publication sequence order: a tenant scope sees tenant-wide rows and a project
// scope sees exactly its project. Every returned envelope is a defensive copy.
func (s *Store) Events(scope tenancy.Scope) []CommittedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := make([]CommittedEvent, 0, len(s.state.events))
	for _, committed := range s.state.events {
		if !scopeVisible(scope, committed.Envelope.Scope) {
			continue
		}
		events = append(events, CommittedEvent{
			Sequence:  committed.Sequence,
			Envelope:  copyEnvelope(committed.Envelope),
			Published: committed.Published,
		})
	}
	return events
}

// Audit returns committed audit rows visible in the scope in commit order. Every
// returned row is a defensive copy.
func (s *Store) Audit(scope tenancy.Scope) []event.Audit {
	s.mu.Lock()
	defer s.mu.Unlock()
	audits := make([]event.Audit, 0, len(s.state.audits))
	for _, audit := range s.state.audits {
		if !scopeVisible(scope, audit.Scope) {
			continue
		}
		audits = append(audits, audit)
	}
	return audits
}
