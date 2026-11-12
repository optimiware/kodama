// Event outbox conformance for the memory adapter. The committed event table is
// publication truth: Pending returns every committed unacknowledged row in
// durable sequence order without a publication high-water mark, and
// MarkPublished acknowledges exactly one row by identity and sequence. The
// publisher's publish_state transition is also available through the scoped
// commit unit, so a store-backed Outbox adapter never needs an unscoped write.
package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func commitEvents(t *testing.T, s *Store, scope tenancy.Scope, ids ...string) {
	t.Helper()
	envelopes := make([]event.Envelope, 0, len(ids))
	for _, identity := range ids {
		envelopes = append(envelopes, defaultEvent(scope, identity, event.TypeTaskCreated, "task-1"))
	}
	mustExecute(t, s, scope, store.Unit{Events: envelopes})
}

func pendingIDs(t *testing.T, s *Store, after event.Sequence, limit int) []string {
	t.Helper()
	pending, err := s.Pending(context.Background(), after, limit)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	identities := make([]string, len(pending))
	for index, row := range pending {
		identities[index] = string(row.Event.ID)
	}
	return identities
}

func TestOutboxPendingIsSequenceOrderedAndBounded(t *testing.T) {
	s := New()
	commitEvents(t, s, projectScope(), "event-1", "event-2", "event-3")

	if got := pendingIDs(t, s, 0, 10); !reflect.DeepEqual(got, []string{"event-1", "event-2", "event-3"}) {
		t.Fatalf("pending = %v; want sequence order", got)
	}
	if got := pendingIDs(t, s, 0, 2); !reflect.DeepEqual(got, []string{"event-1", "event-2"}) {
		t.Fatalf("bounded pending = %v; want the first two rows", got)
	}
	if got := pendingIDs(t, s, 2, 10); !reflect.DeepEqual(got, []string{"event-3"}) {
		t.Fatalf("pending after 2 = %v; want only event-3", got)
	}
	if _, err := s.Pending(context.Background(), 0, 0); err == nil {
		t.Fatal("a non-positive outbox limit was accepted")
	}
}

func TestOutboxReturnsLateLowerSequenceRow(t *testing.T) {
	s := New()
	s.mu.Lock()
	s.state.sequence = 6
	s.state.events = append(s.state.events, CommittedEvent{
		Sequence: 6,
		Envelope: defaultEvent(projectScope(), "event-6", event.TypeTaskCreated, "task-1"),
	})
	s.mu.Unlock()

	if got := pendingIDs(t, s, 0, 10); !reflect.DeepEqual(got, []string{"event-6"}) {
		t.Fatalf("first pending = %v; want event-6", got)
	}
	if err := s.MarkPublished(context.Background(), id.EventID("event-6"), event.Sequence(6)); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	// The transaction that reserved sequence 5 commits after row 6 was
	// acknowledged. It is still unacknowledged and must be returned.
	s.mu.Lock()
	s.state.events = append(s.state.events, CommittedEvent{
		Sequence: 5,
		Envelope: defaultEvent(projectScope(), "event-5", event.TypeTaskCreated, "task-1"),
	})
	s.mu.Unlock()

	if got := pendingIDs(t, s, 0, 10); !reflect.DeepEqual(got, []string{"event-5"}) {
		t.Fatalf("late pending = %v; want the late event-5", got)
	}
	if err := s.MarkPublished(context.Background(), id.EventID("event-5"), event.Sequence(5)); err != nil {
		t.Fatalf("MarkPublished late row: %v", err)
	}
	if got := pendingIDs(t, s, 0, 10); len(got) != 0 {
		t.Fatalf("drained pending = %v; want none", got)
	}
}

func TestOutboxMarkPublishedIsExactAndIdempotent(t *testing.T) {
	s := New()
	commitEvents(t, s, projectScope(), "event-1")

	err := s.MarkPublished(context.Background(), id.EventID("event-1"), event.Sequence(2))
	assertFault(t, err, fault.VersionConflict)
	err = s.MarkPublished(context.Background(), id.EventID("missing"), event.Sequence(1))
	assertFault(t, err, fault.NotFound)

	if err := s.MarkPublished(context.Background(), id.EventID("event-1"), event.Sequence(1)); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	// A retried acknowledgement after a lost response is a safe no-op.
	if err := s.MarkPublished(context.Background(), id.EventID("event-1"), event.Sequence(1)); err != nil {
		t.Fatalf("repeated MarkPublished: %v", err)
	}
	if got := pendingIDs(t, s, 0, 10); len(got) != 0 {
		t.Fatalf("pending after acknowledgement = %v; want none", got)
	}
}

func TestOutboxPublishStateTransitionThroughTheScopedUnit(t *testing.T) {
	s := New()
	scope := projectScope()
	commitEvents(t, s, scope, "event-1")

	row := mustSnapshot(t, s, scope, "events", "event-1")
	if got := rowString(t, row, "publish_state"); got != "pending" {
		t.Fatalf("committed publish_state = %q; want pending", got)
	}

	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{{
		Table:  "events",
		ID:     "event-1",
		Expect: map[string]any{"publish_state": "pending"},
		Values: map[string]any{"publish_state": "published"},
	}}})
	row = mustSnapshot(t, s, scope, "events", "event-1")
	if got := rowString(t, row, "publish_state"); got != "published" {
		t.Fatalf("acknowledged publish_state = %q; want published", got)
	}
	if got := pendingIDs(t, s, 0, 10); len(got) != 0 {
		t.Fatalf("pending after scoped acknowledgement = %v; want none", got)
	}

	// A stale expectation is refused and does not rewrite the acknowledged row.
	_, err := s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "events",
		ID:     "event-1",
		Expect: map[string]any{"publish_state": "pending"},
		Values: map[string]any{"publish_state": "published"},
	}}})
	assertFault(t, err, fault.VersionConflict)

	// Only publish_state may move, and only with an explicit guard.
	_, err = s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "events",
		ID:     "event-1",
		Expect: map[string]any{"publish_state": "published"},
		Values: map[string]any{"type": "execution.failed"},
	}}})
	assertFault(t, err, fault.InvalidInput)
	_, err = s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:  "events",
		ID:     "event-1",
		Values: map[string]any{"publish_state": "published"},
	}}})
	assertFault(t, err, fault.InvalidInput)
	_, err = s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table: "events", ID: "event-2", Insert: true,
		Values: map[string]any{"publish_state": "pending"},
	}}})
	assertFault(t, err, fault.InvalidInput)

	// A publication write cannot be used as a deadline-guarded settlement, and a
	// tenant-scope unit cannot acknowledge a project-scoped event.
	_, err = s.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table:    "events",
		ID:       "event-1",
		Expect:   map[string]any{"publish_state": "published"},
		Values:   map[string]any{"publish_state": "published"},
		Deadline: &store.DeadlineGuard{Column: "at"},
	}}})
	assertFault(t, err, fault.InvalidInput)
	_, err = s.Execute(context.Background(), tenantScope(), store.Unit{Writes: []store.Write{{
		Table:  "events",
		ID:     "event-1",
		Expect: map[string]any{"publish_state": "published"},
		Values: map[string]any{"publish_state": "published"},
	}}})
	assertFault(t, err, fault.VersionConflict)
}

// testPublisher records deliveries and can fail a scripted number of times.
type testPublisher struct {
	delivered []id.EventID
	failures  map[id.EventID]int
}

func (publisher *testPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	if remaining := publisher.failures[envelope.ID]; remaining > 0 {
		publisher.failures[envelope.ID] = remaining - 1
		return errors.New("broker unavailable")
	}
	publisher.delivered = append(publisher.delivered, envelope.ID)
	return nil
}

// TestOutboxDrivesTheDispatcher asserts the memory outbox composes with the real
// retrying dispatcher: a failed delivery is retained and retried with the same
// identity, and an acknowledged row is never republished.
func TestOutboxDrivesTheDispatcher(t *testing.T) {
	s := New()
	commitEvents(t, s, projectScope(), "event-1", "event-2")
	publisher := &testPublisher{failures: map[id.EventID]int{"event-2": 1}}
	dispatcher, err := event.NewDispatcher(event.DispatcherConfig{
		Outbox:    s,
		Publisher: publisher,
		BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-1"}) {
		t.Fatalf("first pass published = %v; want only event-1", result.Published)
	}
	if result.Failed == nil || *result.Failed != id.EventID("event-2") {
		t.Fatalf("failed = %v; want event-2", result.Failed)
	}

	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-2"}) {
		t.Fatalf("retry published = %v; want event-2", result.Published)
	}
	if !reflect.DeepEqual(publisher.delivered, []id.EventID{"event-1", "event-2"}) {
		t.Fatalf("delivered = %v; want each identity once", publisher.delivered)
	}
	if got := pendingIDs(t, s, 0, 10); len(got) != 0 {
		t.Fatalf("pending after dispatch = %v; want none", got)
	}
}
