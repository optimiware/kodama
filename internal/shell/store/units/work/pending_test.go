package work

import (
	"context"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// TestTriggerPendingSkipsCompletedEvents proves Pending consults the durable
// completion marker instead of returning the same completed prefix forever.
func TestTriggerPendingSkipsCompletedEvents(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	sequences := make([]event.Sequence, 0, 5)
	for index := range 5 {
		sequences = append(sequences, fixture.seedEvent(t, scope, id.EventID("event-"+strconv.Itoa(index)), event.TypeExecutionQueued, "repo-1"))
	}
	for index := range 3 {
		if err := fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: sequences[index], EventID: id.EventID("event-" + strconv.Itoa(index))}); err != nil {
			t.Fatalf("complete %d: %v", index, err)
		}
	}
	pending, err := fixture.triggers.Pending(context.Background(), 0, 2)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 || pending[0].Sequence != sequences[3] || pending[1].Sequence != sequences[4] {
		t.Fatalf("pending after completed prefix = %+v; want sequences %d,%d", pending, sequences[3], sequences[4])
	}
	// Completing everything leaves nothing pending.
	for index := 3; index < 5; index++ {
		if err := fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: sequences[index], EventID: id.EventID("event-" + strconv.Itoa(index))}); err != nil {
			t.Fatalf("complete %d: %v", index, err)
		}
	}
	empty, err := fixture.triggers.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending after all completed: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("pending after every event completed = %+v", empty)
	}
}

// TestTriggerPendingReturnsLateLowerSequence proves the scan keeps sequence
// order so an unconsumed lower-sequence row is still returned after a later
// event was completed.
func TestTriggerPendingReturnsLateLowerSequence(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	first := fixture.seedEvent(t, scope, "event-a", event.TypeExecutionQueued, "repo-1")
	second := fixture.seedEvent(t, scope, "event-b", event.TypeExecutionQueued, "repo-2")
	third := fixture.seedEvent(t, scope, "event-c", event.TypeExecutionQueued, "repo-3")
	if err := fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: first, EventID: "event-a"}); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	if err := fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: scope, Sequence: third, EventID: "event-c"}); err != nil {
		t.Fatalf("complete third: %v", err)
	}
	pending, err := fixture.triggers.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Sequence != second || pending[0].Event.ID != "event-b" {
		t.Fatalf("pending = %+v; want only the unconsumed lower-sequence event-b", pending)
	}
}

// TestTriggerPendingHandlesTenantWideEvents proves a tenant-wide event marker is
// read in its own scope and skipped, while tenant-wide unconsumed rows remain
// visible.
func TestTriggerPendingHandlesTenantWideEvents(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	tenant := tenancy.Scope{TenantID: testTenant}
	tenantSequence := fixture.seedEvent(t, tenant, "event-tenant", event.TypeExecutionQueued, "repo-tenant")
	projectSequence := fixture.seedEvent(t, scope, "event-project", event.TypeExecutionQueued, "repo-project")
	pending, err := fixture.triggers.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 || pending[0].Sequence != tenantSequence || pending[1].Sequence != projectSequence {
		t.Fatalf("pending before completion = %+v", pending)
	}
	if err := fixture.triggers.Complete(context.Background(), trigger.CompletionUnit{Owner: tenant, Sequence: tenantSequence, EventID: "event-tenant"}); err != nil {
		t.Fatalf("complete tenant event: %v", err)
	}
	pending, err = fixture.triggers.Pending(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("Pending after tenant completion: %v", err)
	}
	if len(pending) != 1 || pending[0].Sequence != projectSequence || pending[0].Event.ID != "event-project" {
		t.Fatalf("pending after tenant completion = %+v", pending)
	}
}

// TestTriggerPendingHonoursLimit proves the scan stops once limit unconsumed
// rows are collected, keeping one pass bounded.
func TestTriggerPendingHonoursLimit(t *testing.T) {
	fixture := newFixture(t)
	scope := testScope()
	for index := range 6 {
		fixture.seedEvent(t, scope, id.EventID("event-"+strconv.Itoa(index)), event.TypeExecutionQueued, "repo-1")
	}
	pending, err := fixture.triggers.Pending(context.Background(), 0, 3)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("bounded pending rows = %d; want 3", len(pending))
	}
}
