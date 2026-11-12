package identity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// testNow is the injected authoritative time for every adapter test.
var testNow = time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)

// fixedClock is a deterministic Clock.
type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

// sequenceIDs is a deterministic, concurrency-safe row-identity source. A
// concurrent first-grant test allocates row identities through one adapter, so
// the sequence itself is guarded.
type sequenceIDs struct {
	mu   sync.Mutex
	next int
}

func (source *sequenceIDs) NewRowID() (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.next++
	return fmt.Sprintf("membership-%d", source.next), nil
}

// harness binds one memory store to one adapter and allocates deterministic
// receipts, audits and events.
type harness struct {
	t       *testing.T
	store   *memory.Store
	adapter *identityunits.Adapter
	clock   fixedClock
	ids     *sequenceIDs
	counter int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	instance := memory.NewWithClock(func() time.Time { return testNow })
	rowIDs := &sequenceIDs{}
	adapter, err := identityunits.New(identityunits.Config{
		Executor: instance,
		Scanner:  instance,
		Clock:    fixedClock{now: testNow},
		IDs:      rowIDs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, store: instance, adapter: adapter, clock: fixedClock{now: testNow}, ids: rowIDs}
}

// bumpExecutor advances one committed row through the underlying store on the
// first commit unit, simulating a concurrent change after the adapter evaluated
// an authority fact but before the unit's guard runs.
type bumpExecutor struct {
	*memory.Store
	table    string
	identity string
	version  uint64
	values   map[string]any
	bumped   bool
}

func (bump *bumpExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	if !bump.bumped && unit.Receipt != nil {
		bump.bumped = true
		if _, err := bump.Store.Execute(ctx, scope, store.Unit{Writes: []store.Write{{
			Table:         bump.table,
			ID:            bump.identity,
			ExpectVersion: bump.version,
			Values:        bump.values,
		}}}); err != nil {
			return store.UnitResult{}, err
		}
	}
	return bump.Store.Execute(ctx, scope, unit)
}

// newBumpingHarness builds a harness whose adapter commits through a bumping
// executor so a test can change an authority row between evaluation and commit.
func newBumpingHarness(t *testing.T, table, identity string, version uint64, values map[string]any) *harness {
	t.Helper()
	instance := memory.NewWithClock(func() time.Time { return testNow })
	bump := &bumpExecutor{Store: instance, table: table, identity: identity, version: version, values: values}
	rowIDs := &sequenceIDs{}
	adapter, err := identityunits.New(identityunits.Config{
		Executor: bump,
		Scanner:  instance,
		Clock:    fixedClock{now: testNow},
		IDs:      rowIDs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, store: instance, adapter: adapter, clock: fixedClock{now: testNow}, ids: rowIDs}
}

// observingExecutor records every commit unit it is asked to apply and can run
// one hook immediately before the first receipt-bearing unit, simulating a
// concurrent committed change after the adapter confirmed authority and before
// the guarded transaction runs.
type observingExecutor struct {
	*memory.Store
	hook   func(context.Context) error
	fired  bool
	units  []store.Unit
	scopes []tenancy.Scope
}

func (observer *observingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	observer.units = append(observer.units, unit)
	observer.scopes = append(observer.scopes, scope)
	if !observer.fired && unit.Receipt != nil && observer.hook != nil {
		observer.fired = true
		if err := observer.hook(ctx); err != nil {
			return store.UnitResult{}, err
		}
	}
	return observer.Store.Execute(ctx, scope, unit)
}

// newObservingHarness builds a harness whose adapter commits through an
// observing executor. The returned observer exposes the recorded units so a
// test can inspect the guards the adapter built, and runs hook before the first
// commit unit so a test can interpose a concurrent change.
func newObservingHarness(t *testing.T, hook func(context.Context) error) (*harness, *observingExecutor) {
	t.Helper()
	instance := memory.NewWithClock(func() time.Time { return testNow })
	observer := &observingExecutor{Store: instance, hook: hook}
	rowIDs := &sequenceIDs{}
	adapter, err := identityunits.New(identityunits.Config{
		Executor: observer,
		Scanner:  instance,
		Clock:    fixedClock{now: testNow},
		IDs:      rowIDs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{t: t, store: instance, adapter: adapter, clock: fixedClock{now: testNow}, ids: rowIDs}, observer
}

func (h *harness) nextID(prefix string) string {
	h.counter++
	return fmt.Sprintf("%s-%d", prefix, h.counter)
}

// receipt builds one valid durable receipt for a commit unit.
func (h *harness) receipt(scope tenancy.Scope, principal id.PrincipalID, operation, callerKey string, result any) receipt.Record {
	h.t.Helper()
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"caller":"` + callerKey + `"}`)}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		h.t.Fatalf("Fingerprint: %v", err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		h.t.Fatalf("marshal receipt result: %v", err)
	}
	return receipt.Record{
		ID:          id.ReceiptID(h.nextID("receipt")),
		Key:         receipt.Key{TenantID: scope.TenantID, ProjectID: scope.ProjectID, PrincipalID: principal, Operation: operation, CallerKey: callerKey},
		ContentHash: fingerprint,
		Result:      body,
		At:          testNow,
	}
}

// audit builds one valid immutable audit row.
func (h *harness) audit(scope tenancy.Scope, actor id.PrincipalID, operation string) event.Audit {
	return event.Audit{
		ID:        id.EventID(h.nextID("audit")),
		Scope:     scope,
		Actor:     actor,
		Operation: operation,
		At:        testNow,
	}
}

// envelope builds one valid config.changed publication intent.
func (h *harness) envelope(scope tenancy.Scope, actor id.PrincipalID, subject string, version uint64) event.Envelope {
	envelope := event.Envelope{
		ID:             id.EventID(h.nextID("event")),
		Type:           event.TypeConfigChanged,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: actor},
		At:             testNow,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: version,
	}
	if err := envelope.Validate(); err != nil {
		h.t.Fatalf("event envelope: %v", err)
	}
	return envelope
}

// seed commits one raw row through the executor so a test can establish a
// pre-existing committed state without going through a capability.
func (h *harness) seed(scope tenancy.Scope, table, identity string, values map[string]any) {
	h.t.Helper()
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: table, ID: identity, Insert: true, Values: values}},
	}); err != nil {
		h.t.Fatalf("seed %s/%s: %v", table, identity, err)
	}
}

// snapshot returns one committed row.
func (h *harness) snapshot(scope tenancy.Scope, table, identity string) (store.Row, bool) {
	return h.store.Snapshot(scope, table, identity)
}

// requireFaultClass asserts the fault class of a refusal.
func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("fault class = %q (%v); want %q", class, err, want)
	}
}

// requireNoRows asserts a table has no committed row in scope.
func (h *harness) requireNoRows(scope tenancy.Scope, table, identity string) {
	h.t.Helper()
	if _, found := h.snapshot(scope, table, identity); found {
		h.t.Fatalf("%s/%s was committed; want no row", table, identity)
	}
}

// requireNoPublications asserts the commit published no audit row or event.
func (h *harness) requireNoPublications(scope tenancy.Scope) {
	h.t.Helper()
	if audits := h.store.Audit(scope); len(audits) != 0 {
		h.t.Fatalf("audit rows = %d; want 0", len(audits))
	}
	if events := h.store.Events(scope); len(events) != 0 {
		h.t.Fatalf("event rows = %d; want 0", len(events))
	}
}

// tenantScope builds a tenant-wide scope.
func tenantScope(tenant string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant)}
}

// projectScope builds a project scope.
func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}
