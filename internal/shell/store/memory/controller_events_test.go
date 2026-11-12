// Controller event/audit commit conformance for the memory adapter. The store
// package declares the execution-bound event and audit write path, the
// PostgreSQL adapter implements it alongside this one, and the shared
// store.ControllerEventCommitImplemented declaration is true. Most tests here
// flip the adapter-local gate explicitly to state their intent; the
// default-gate test proves the declared default and the forced-off refusal.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// enableControllerEventCommit flips the adapter-local gate for one test and
// restores it afterwards. Production follows the store declaration.
func enableControllerEventCommit(t *testing.T) {
	t.Helper()
	previous := controllerEventCommit
	controllerEventCommit = true
	t.Cleanup(func() { controllerEventCommit = previous })
}

// controllerLifecycleEvent builds one deterministic lifecycle event row for a
// test. The ids are supplied by the test (the real projection derives them from
// the execution/version/type/subject tuple).
func controllerLifecycleEvent(scope tenancy.Scope, identity string, eventType event.Type, subjectID string, subjectVersion uint64, data string) event.Envelope {
	return event.Envelope{
		ID:             id.EventID(identity),
		Type:           eventType,
		Source:         event.SystemSource,
		At:             fixedTime(),
		Scope:          scope,
		SubjectID:      subjectID,
		SubjectVersion: subjectVersion,
		Data:           []byte(data),
	}
}

// engineCommitUnit is the engine commit shape: a guarded execution lock and
// state write plus lifecycle events and audit rows committed with it.
func engineCommitUnit(events []event.Envelope, audit []event.Audit) store.Unit {
	return store.Unit{
		Locks: []store.Lock{
			{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1},
		},
		Writes: []store.Write{
			{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}},
		},
		Events: events,
		Audit:  audit,
	}
}

// TestControllerEngineCommitPublishesEventsAndAuditAtomically proves the engine
// unit commits its state writes, three ordered lifecycle events and one audit
// row in one atomic step, and that the events are readable through the scoped
// executor afterwards while audit stays write-only for the controller.
func TestControllerEngineCommitPublishesEventsAndAuditAtomically(t *testing.T) {
	enableControllerEventCommit(t)
	s := NewWithClock(func() time.Time { return fixedTime() })
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	seedRow(t, s, scope, "actions", testActionID, actionValues(1, "pending"))

	events := []event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{"execution_id":"execution-1","state":"running","version":2}`),
		controllerLifecycleEvent(scope, "event-2", event.TypeCleanup, testExecutionID, 2, `{"execution_id":"execution-1","cleanup_state":"pending"}`),
		controllerLifecycleEvent(scope, "event-3", event.TypeActionTransition, testActionID, 2, `{"action_id":"action-1","state":"running","version":2}`),
	}
	audit := event.Audit{
		ID:        id.EventID("audit-1"),
		Scope:     scope,
		Actor:     id.PrincipalID("system"),
		Operation: "execution.engine.commit",
		At:        fixedTime(),
		Detail:    "engine commit",
	}

	unit := engineCommitUnit(events, []event.Audit{audit})
	unit.Locks = append(unit.Locks, store.Lock{Table: "actions", IDs: []string{testActionID}, ExpectVersion: 1})
	unit.Writes = append(unit.Writes, store.Write{Table: "actions", ID: testActionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}})
	if _, err := s.Controller().Execute(context.Background(), testExecutionID, unit); err != nil {
		t.Fatalf("controller engine commit: %v", err)
	}

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("execution state = %q; want running", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	action := mustSnapshot(t, s, scope, "actions", testActionID)
	if got := rowString(t, action, "state"); got != "running" {
		t.Errorf("action state = %q; want running", got)
	}
	if got := rowVersion(t, action, "version"); got != 2 {
		t.Errorf("action version = %d; want 2", got)
	}

	// Publication sequence and ids are preserved in the unit's order.
	committed := s.Events(scope)
	if got := eventIDs(committed); !equalStrings(got, []string{"event-1", "event-2", "event-3"}) {
		t.Fatalf("committed event ids = %v; want event-1,event-2,event-3", got)
	}
	for index, identity := range []string{"event-1", "event-2", "event-3"} {
		row, err := s.Read(context.Background(), scope, "events", identity)
		if err != nil {
			t.Fatalf("scoped executor read of %s: %v", identity, err)
		}
		if got := rowVersion(t, row, "publish_sequence"); got != uint64(index+1) {
			t.Errorf("%s publish_sequence = %d; want %d", identity, got, index+1)
		}
		if got := rowString(t, row, "publish_state"); got != "pending" {
			t.Errorf("%s publish_state = %q; want pending", identity, got)
		}
	}

	if got := auditOperations(s.Audit(scope)); !equalStrings(got, []string{"execution.engine.commit"}) {
		t.Fatalf("committed audit operations = %v; want execution.engine.commit", got)
	}

	// audit remains write-only for the controller.
	if _, err := s.Controller().Read(context.Background(), "audit", "audit-1"); err == nil {
		t.Fatal("controller Read of the committed audit row succeeded; audit is write-only")
	} else {
		assertFault(t, err, fault.InvalidInput)
	}
}

// TestControllerEngineCommitRefusesForeignScopeEventAtomically proves a single
// foreign-scope event refuses the whole unit: the state writes are not applied
// and no event row is committed.
func TestControllerEngineCommitRefusesForeignScopeEventAtomically(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	foreign := scopeFor(otherTenantID, otherProjectID)
	unit := engineCommitUnit([]event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{"execution_id":"execution-1"}`),
		controllerLifecycleEvent(foreign, "event-2", event.TypeActionTransition, testActionID, 2, `{"action_id":"action-1"}`),
	}, nil)
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.Forbidden)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued (the unit must roll back)", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("execution version = %d; want 1 (the unit must roll back)", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")
	assertAbsent(t, s, foreign, "events", "event-2")
}

// TestControllerEngineCommitRefusesForeignScopeAuditAtomically proves the same
// all-or-nothing rule for an audit row whose scope is not the derived one.
func TestControllerEngineCommitRefusesForeignScopeAuditAtomically(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	foreign := scopeFor(otherTenantID, otherProjectID)
	unit := engineCommitUnit(nil, []event.Audit{{
		ID:        id.EventID("audit-1"),
		Scope:     foreign,
		Actor:     id.PrincipalID("system"),
		Operation: "execution.engine.commit",
		At:        fixedTime(),
	}})
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.Forbidden)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued (the unit must roll back)", got)
	}
	assertAbsent(t, s, foreign, "audit", "audit-1")
}

// TestControllerEngineCommitRefusesReceipt keeps the rule that a receipt
// belongs to a scoped capability: a controller unit carrying one is refused and
// nothing is staged.
func TestControllerEngineCommitRefusesReceipt(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	record := defaultReceipt(t)
	unit := engineCommitUnit([]event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{"execution_id":"execution-1"}`),
	}, nil)
	unit.Receipt = &record
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.Forbidden)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued (the unit must roll back)", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")
}

// TestControllerEngineCommitRefusesEventAndAuditMutation proves the
// INSERT-only authority: an update or delete of an event or audit row is
// forbidden and the committed rows are unchanged.
func TestControllerEngineCommitRefusesEventAndAuditMutation(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "running"))
	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 1, `{"execution_id":"execution-1"}`),
	}})
	mustExecute(t, s, scope, store.Unit{Audit: []event.Audit{{
		ID:        id.EventID("audit-1"),
		Scope:     scope,
		Actor:     id.PrincipalID("system"),
		Operation: "execution.engine.commit",
		At:        fixedTime(),
	}}})

	lock := []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1}}
	cases := []struct {
		name  string
		write store.Write
	}{
		{"event update", store.Write{Table: "events", ID: "event-1", Expect: map[string]any{"publish_state": "pending"}, Values: map[string]any{"publish_state": "published"}}},
		{"event delete", store.Write{Table: "events", ID: "event-1", Delete: true, Expect: map[string]any{"publish_state": "pending"}}},
		{"audit delete", store.Write{Table: "audit", ID: "audit-1", Delete: true, Expect: map[string]any{"operation": "execution.engine.commit"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
				Locks:  lock,
				Writes: []store.Write{testCase.write},
			})
			assertFault(t, err, fault.Forbidden)
		})
	}

	committed, err := s.Read(context.Background(), scope, "events", "event-1")
	if err != nil {
		t.Fatalf("read event-1: %v", err)
	}
	if got := rowString(t, committed, "publish_state"); got != "pending" {
		t.Errorf("event publish_state = %q; want pending (mutation must be refused)", got)
	}
	if got := auditOperations(s.Audit(scope)); !equalStrings(got, []string{"execution.engine.commit"}) {
		t.Errorf("audit rows = %v; want the original audit row", got)
	}
}

// TestControllerEngineCommitStaleExecutionVersion proves a stale expected
// execution version is version_conflict and commits no event.
func TestControllerEngineCommitStaleExecutionVersion(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(2, "queued"))

	unit := store.Unit{
		Locks:  []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1}},
		Writes: []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Events: []event.Envelope{
			controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 3, `{"execution_id":"execution-1"}`),
		},
	}
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.VersionConflict)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")
}

// TestControllerEngineCommitRequiresExecutionLock proves an event or audit unit
// with no execution row lock is forbidden, because the scope and the atomic
// guard are not established.
func TestControllerEngineCommitRequiresExecutionLock(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	write := store.Write{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}
	cases := []struct {
		name string
		unit store.Unit
	}{
		{"event without a lock", store.Unit{
			Writes: []store.Write{write},
			Events: []event.Envelope{controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`)},
		}},
		{"audit without a lock", store.Unit{
			Writes: []store.Write{write},
			Audit:  []event.Audit{{ID: id.EventID("audit-1"), Scope: scope, Actor: "system", Operation: "execution.engine.commit", At: fixedTime()}},
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := s.Controller().Execute(context.Background(), testExecutionID, testCase.unit)
			assertFault(t, err, fault.Forbidden)
		})
	}

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")
	assertAbsent(t, s, scope, "audit", "audit-1")
}

// TestControllerEngineCommitConcurrentExactlyOneWinner proves two engine units
// on the same execution serialize: exactly one wins and the loser commits no
// event and no state change.
func TestControllerEngineCommitConcurrentExactlyOneWinner(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	states := []string{"running", "succeeded"}

	errs := runConcurrently(t, 2, func(index int) error {
		_, err := s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
			Locks:  []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1}},
			Writes: []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": states[index]}}},
			Events: []event.Envelope{
				controllerLifecycleEvent(scope, fmt.Sprintf("event-%d", index), event.TypeExecutionStarted, testExecutionID, 2, `{}`),
			},
		})
		return err
	})
	if got := countNil(errs); got != 1 {
		t.Fatalf("successful engine units = %d; want exactly 1 (%v)", got, describeErrors(errs))
	}
	if got := countFault(t, errs, fault.VersionConflict); got != 1 {
		t.Fatalf("conflicting engine units = %d; want exactly 1 (%v)", got, describeErrors(errs))
	}

	winner := -1
	for index, err := range errs {
		if err == nil {
			winner = index
		}
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != states[winner] {
		t.Errorf("execution state = %q; want the winner state %q", got, states[winner])
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	committed := s.Events(scope)
	if len(committed) != 1 {
		t.Fatalf("committed events = %d; want exactly the winner's 1 (%v)", len(committed), eventIDs(committed))
	}
	if got := string(committed[0].Envelope.ID); got != fmt.Sprintf("event-%d", winner) {
		t.Errorf("committed event = %q; want the winner's event-%d", got, winner)
	}
}

// TestControllerEngineCommitRollsBackWhenSecondEventIsInvalid proves a unit is
// all-or-nothing: the first valid event cannot commit when the second event row
// is invalid.
func TestControllerEngineCommitRollsBackWhenSecondEventIsInvalid(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	invalid := controllerLifecycleEvent(scope, "event-2", event.TypeExecutionStarted, testExecutionID, 2, `{}`)
	invalid.Type = ""
	unit := engineCommitUnit([]event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`),
		invalid,
	}, nil)
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.InvalidInput)

	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued (the whole unit must roll back)", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")
	assertAbsent(t, s, scope, "events", "event-2")
}

// TestControllerEngineCommitDuplicateEventIDRefusedWithoutPartialState proves
// controller idempotence comes from the deterministic event ids, not a receipt:
// a duplicate id is a version_conflict and applies no state.
func TestControllerEngineCommitDuplicateEventIDRefusedWithoutPartialState(t *testing.T) {
	enableControllerEventCommit(t)
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	duplicate := controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`)
	unit := engineCommitUnit([]event.Envelope{duplicate, duplicate}, nil)
	_, err := s.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.VersionConflict)
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued (the duplicate must roll back)", got)
	}
	assertAbsent(t, s, scope, "events", "event-1")

	// The first commit succeeds; replaying the same deterministic event id with
	// the current execution version is refused and leaves the committed state.
	if _, err := s.Controller().Execute(context.Background(), testExecutionID, engineCommitUnit([]event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`),
	}, nil)); err != nil {
		t.Fatalf("first engine commit: %v", err)
	}
	replay := store.Unit{
		Locks:  []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 2}},
		Writes: []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 2, Values: map[string]any{"state": "succeeded"}}},
		Events: []event.Envelope{
			controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`),
		},
	}
	_, err = s.Controller().Execute(context.Background(), testExecutionID, replay)
	assertFault(t, err, fault.VersionConflict)
	execution = mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("execution state = %q; want running (the replay must not apply)", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	if got := eventIDs(s.Events(scope)); !equalStrings(got, []string{"event-1"}) {
		t.Errorf("committed event ids = %v; want only event-1", got)
	}
}

// TestControllerEventCommitDefaultFollowsTheContractGate pins the default
// adapter behavior against the declaration.
// store.ControllerEventCommitImplemented is true, so the default gate commits
// an execution-bound event/audit unit; forcing the adapter-local gate off must
// still refuse that same unit atomically.
func TestControllerEventCommitDefaultFollowsTheContractGate(t *testing.T) {
	if controllerEventCommit != store.ControllerEventCommitImplemented {
		t.Fatalf("the memory controller event gate = %v; want the declaration %v", controllerEventCommit, store.ControllerEventCommitImplemented)
	}
	if !controllerEventCommit {
		t.Fatal("the declaration must declare the event/audit write path implemented")
	}

	scope := projectScope()
	unit := engineCommitUnit([]event.Envelope{
		controllerLifecycleEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID, 2, `{}`),
	}, []event.Audit{{
		ID:        id.EventID("audit-1"),
		Scope:     scope,
		Actor:     id.PrincipalID("system"),
		Operation: "execution.engine.commit",
		At:        fixedTime(),
		Detail:    "engine commit",
	}})

	// With the default gate the unit commits its state write, event and audit
	// row together.
	s := New()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	if _, err := s.Controller().Execute(context.Background(), testExecutionID, unit); err != nil {
		t.Fatalf("controller event commit with the default gate: %v", err)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "running" {
		t.Errorf("execution state = %q; want running", got)
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want 2", got)
	}
	if got := eventIDs(s.Events(scope)); !equalStrings(got, []string{"event-1"}) {
		t.Errorf("committed event ids = %v; want event-1", got)
	}
	if got := auditOperations(s.Audit(scope)); !equalStrings(got, []string{"execution.engine.commit"}) {
		t.Errorf("committed audit operations = %v; want execution.engine.commit", got)
	}

	// With the gate forced off the same unit is refused and nothing persists.
	previous := controllerEventCommit
	controllerEventCommit = false
	t.Cleanup(func() { controllerEventCommit = previous })

	refused := New()
	seedRow(t, refused, scope, "executions", testExecutionID, executionValues(1, "queued"))
	_, err := refused.Controller().Execute(context.Background(), testExecutionID, unit)
	assertFault(t, err, fault.Forbidden)

	execution = mustSnapshot(t, refused, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != "queued" {
		t.Errorf("execution state = %q; want queued", got)
	}
	if got := rowVersion(t, execution, "version"); got != 1 {
		t.Errorf("execution version = %d; want 1 (the unit must roll back)", got)
	}
	assertAbsent(t, refused, scope, "events", "event-1")
	if got := auditOperations(refused.Audit(scope)); len(got) != 0 {
		t.Errorf("committed audit operations = %v; want none", got)
	}
}

// controllerVectorDocument is the adapter controller vector set. The contract
// package's reference interpreter is test-only code in another package, so
// this file copies its runner, exactly as the seam vectors are re-run here.
type controllerVectorDocument struct {
	Version int                `json:"version"`
	Vectors []controllerVector `json:"vectors"`
}

type controllerVector struct {
	Name  string                 `json:"name"`
	Clock string                 `json:"clock"`
	Seed  seamSeed               `json:"seed"`
	Steps []controllerVectorStep `json:"steps"`
}

type controllerVectorStep struct {
	Op            string                      `json:"op"`
	Table         string                      `json:"table"`
	ID            string                      `json:"id"`
	ExecutionID   string                      `json:"execution_id"`
	Scope         *seamScope                  `json:"scope"`
	Locks         []seamLock                  `json:"locks"`
	Writes        []seamWrite                 `json:"writes"`
	Events        []controllerVectorEventSpec `json:"events"`
	Audit         []controllerVectorAuditSpec `json:"audit"`
	Receipt       *seamSeedReceipt            `json:"receipt"`
	ReceiptMode   string                      `json:"receipt_mode"`
	ReceiptExpect *seamReceiptExpect          `json:"receipt_expect"`
	Equals        map[string]any              `json:"equals"`
	Range         []scanVectorRange           `json:"range"`
	OrderBy       []scanVectorOrder           `json:"order_by"`
	Limit         int                         `json:"limit"`
	WantValues    map[string]any              `json:"want_values"`
	WantVersion   *uint64                     `json:"want_version"`
	WantAbsent    bool                        `json:"want_absent"`
	WantIDs       []string                    `json:"want_ids"`
	WantError     string                      `json:"want_error"`
}

type controllerVectorEventSpec struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	ReportType      string     `json:"report_type"`
	SourceKind      string     `json:"source_kind"`
	SourcePrincipal string     `json:"source_principal"`
	At              string     `json:"at"`
	Scope           *seamScope `json:"scope"`
	SubjectID       string     `json:"subject_id"`
	SubjectVersion  uint64     `json:"subject_version"`
	CorrelationID   string     `json:"correlation_id"`
	CausationID     string     `json:"causation_id"`
	Data            string     `json:"data"`
}

type controllerVectorAuditSpec struct {
	ID        string     `json:"id"`
	Scope     *seamScope `json:"scope"`
	Actor     string     `json:"actor"`
	Operation string     `json:"operation"`
	At        string     `json:"at"`
	Detail    string     `json:"detail"`
}

// TestControllerVectorsAgainstMemory runs every declared controller vector
// against the memory adapter with the event/audit write path enabled.
func TestControllerVectorsAgainstMemory(t *testing.T) {
	enableControllerEventCommit(t)
	document := loadControllerVectors(t)
	if document.Version != 1 {
		t.Fatalf("controller vector version = %d; want 1", document.Version)
	}
	if len(document.Vectors) == 0 {
		t.Fatal("controller vector set is empty")
	}
	for _, vector := range document.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			s := NewWithClock(func() time.Time { return seamClock(vector.Clock) })
			seedControllerVectorStore(t, s, vector.Seed)
			if err := runControllerVector(t, s, vector); err != nil {
				t.Fatalf("vector %q: %v", vector.Name, err)
			}
		})
	}
}

func loadControllerVectors(t *testing.T) controllerVectorDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "controller_events.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read controller vectors: %v", err)
	}
	var document controllerVectorDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode controller vectors: %v", err)
	}
	return document
}

// seedControllerVectorStore seeds the vector rows through the adapter's real
// write path. Events and audit rows are committed publication records, never
// generic business writes.
func seedControllerVectorStore(t *testing.T, s *Store, seed seamSeed) {
	t.Helper()
	for _, row := range seed.Rows {
		if row.Table == "audit" {
			scope := scopeFor(row.TenantID, row.ProjectID)
			audit := event.Audit{
				ID:        id.EventID(row.ID),
				Scope:     scope,
				Actor:     id.PrincipalID(scanVectorString(row.Values["actor"])),
				Operation: scanVectorString(row.Values["operation"]),
				At:        fixedTime(),
				Detail:    scanVectorString(row.Values["detail"]),
			}
			mustExecute(t, s, scope, store.Unit{Audit: []event.Audit{audit}})
			continue
		}
		seedSeamStore(t, s, seamSeed{Rows: []seamSeedRow{row}})
	}
	if len(seed.Receipts) > 0 {
		seedSeamStore(t, s, seamSeed{Receipts: seed.Receipts})
	}
}

func runControllerVector(t *testing.T, s *Store, vector controllerVector) error {
	t.Helper()
	controller := s.Controller()
	for index, step := range vector.Steps {
		switch step.Op {
		case "execute":
			unit, err := controllerVectorUnit(step)
			if err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
			_, err = controller.Execute(context.Background(), step.ExecutionID, unit)
			if err := checkSeamError(index, seamStep{WantError: step.WantError}, err); err != nil {
				return err
			}
		case "scan":
			scan := store.ControllerScan{
				Table:   step.Table,
				Equals:  step.Equals,
				Range:   scanVectorRangeFilters(step.Range),
				OrderBy: scanVectorOrderTerms(step.OrderBy),
				Limit:   step.Limit}
			page, err := controller.Scan(context.Background(), scan)
			if err := checkSeamError(index, seamStep{WantError: step.WantError}, err); err != nil {
				return err
			}
			if step.WantError != "" {
				continue
			}
			got := make([]string, 0, len(page.Rows))
			for _, row := range page.Rows {
				identity, _ := row.Values["id"].(string)
				got = append(got, identity)
			}
			if step.WantIDs != nil && !equalStrings(got, step.WantIDs) {
				return fmt.Errorf("step %d ids = %v; want %v", index, got, step.WantIDs)
			}
		case "inspect":
			if step.Scope == nil {
				return fmt.Errorf("step %d inspects without a scope", index)
			}
			row, found := s.Snapshot(seamScopeOf(*step.Scope), step.Table, step.ID)
			expected := seamStep{
				Table:       step.Table,
				ID:          step.ID,
				WantValues:  step.WantValues,
				WantVersion: step.WantVersion,
				WantAbsent:  step.WantAbsent,
			}
			if err := checkSeamRow(expected, row, found); err != nil {
				return fmt.Errorf("step %d: %w", index, err)
			}
		default:
			return fmt.Errorf("step %d has unknown controller op %q", index, step.Op)
		}
	}
	return nil
}

func controllerVectorUnit(step controllerVectorStep) (store.Unit, error) {
	unit := store.Unit{ReceiptMode: store.ReceiptMode(step.ReceiptMode)}
	locks, err := seamLocks(step.Locks)
	if err != nil {
		return store.Unit{}, err
	}
	unit.Locks = locks
	writes, err := seamWrites(step.Writes)
	if err != nil {
		return store.Unit{}, err
	}
	unit.Writes = writes
	for _, spec := range step.Events {
		unit.Events = append(unit.Events, controllerVectorEnvelope(spec))
	}
	for _, spec := range step.Audit {
		unit.Audit = append(unit.Audit, controllerVectorAudit(spec))
	}
	if step.Receipt != nil {
		key := seamKeyOf(*step.Receipt)
		record := receipt.Record{
			ID:          seamReceiptIDOf(key),
			Key:         key,
			ContentHash: step.Receipt.ContentHash,
			Result:      []byte(step.Receipt.Result),
			ExecutionID: id.ExecutionID(step.Receipt.ExecutionID),
			At:          fixedTime(),
		}
		unit.Receipt = &record
	}
	if step.ReceiptExpect != nil {
		unit.ReceiptExpect = &store.ReceiptExpect{
			Completed:   step.ReceiptExpect.Completed,
			ContentHash: step.ReceiptExpect.ContentHash,
		}
	}
	return unit, nil
}

func controllerVectorEnvelope(spec controllerVectorEventSpec) event.Envelope {
	source := event.SystemSource
	if spec.SourceKind != "" {
		source.Kind = event.SourceKind(spec.SourceKind)
	}
	if spec.SourcePrincipal != "" {
		source.PrincipalID = id.PrincipalID(spec.SourcePrincipal)
	}
	scope := seamScope{}
	if spec.Scope != nil {
		scope = *spec.Scope
	}
	return event.Envelope{
		ID:             id.EventID(spec.ID),
		Type:           event.Type(spec.Type),
		ReportType:     event.ReportType(spec.ReportType),
		Source:         source,
		At:             seamClock(spec.At),
		Scope:          seamScopeOf(scope),
		SubjectID:      spec.SubjectID,
		SubjectVersion: spec.SubjectVersion,
		CorrelationID:  spec.CorrelationID,
		CausationID:    spec.CausationID,
		Data:           []byte(spec.Data),
	}
}

func controllerVectorAudit(spec controllerVectorAuditSpec) event.Audit {
	scope := seamScope{}
	if spec.Scope != nil {
		scope = *spec.Scope
	}
	return event.Audit{
		ID:        id.EventID(spec.ID),
		Scope:     seamScopeOf(scope),
		Actor:     id.PrincipalID(spec.Actor),
		Operation: spec.Operation,
		At:        seamClock(spec.At),
		Detail:    spec.Detail,
	}
}

// TestControllerEngineCommitRefusesRawPublicationInsert proves a committed
// event or audit row reaches its table only through the unit's publication
// records: a raw business insert into either is refused by the shared write
// rules even under a guarded execution row, exactly as the PostgreSQL
// controller refuses it.
func TestControllerEngineCommitRefusesRawPublicationInsert(t *testing.T) {
	enableControllerEventCommit(t)
	cases := []struct {
		name  string
		write store.Write
	}{
		{"event insert", store.Write{Table: "events", ID: "event-9", Insert: true, Values: map[string]any{
			"type": "execution.started", "subject_id": testExecutionID, "source_kind": "shell", "source_principal": "system",
		}}},
		{"audit insert", store.Write{Table: "audit", ID: "audit-9", Insert: true, Values: map[string]any{
			"actor": "system", "operation": "execution.engine.commit",
		}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := New()
			scope := projectScope()
			seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "running"))
			_, err := s.Controller().Execute(context.Background(), testExecutionID, store.Unit{
				Locks:  []store.Lock{{Table: "executions", IDs: []string{testExecutionID}, ExpectVersion: 1}},
				Writes: []store.Write{testCase.write},
			})
			assertFault(t, err, fault.InvalidInput)
			if got := len(s.Events(scope)); got != 0 {
				t.Errorf("committed events = %d; want 0", got)
			}
			if got := len(s.Audit(scope)); got != 0 {
				t.Errorf("committed audit rows = %d; want 0", got)
			}
		})
	}
}
