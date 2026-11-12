package platform_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestEventReadReturnsScopedCommittedPage asserts the read adapter returns only
// the caller's committed events in durable sequence order.
func TestEventReadReturnsScopedCommittedPage(t *testing.T) {
	s := newStore()
	seedEvent(t, s, testScope(), "event-1", event.TypeTaskCreated)
	seedEvent(t, s, testScope(), "event-2", event.TypeTaskQuestion)
	seedEvent(t, s, testScope(), "event-3", event.TypeTaskReply)
	seedEvent(t, s, otherScope(), "event-4", event.TypeTaskCreated)
	seedEvent(t, s, testTenantOnlyScope(), "event-5", event.TypeTaskCreated)

	reader, err := platform.NewEventReader(s)
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	page, err := reader.Read(context.Background(), event.ReadQuery{Scope: testScope(), Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"event-1", "event-2", "event-3"}
	if len(page.Events) != len(want) {
		t.Fatalf("events = %d, want %d", len(page.Events), len(want))
	}
	for index, eventID := range want {
		if string(page.Events[index].ID) != eventID {
			t.Errorf("event[%d] = %q, want %q", index, page.Events[index].ID, eventID)
		}
	}
	if page.HasMore {
		t.Error("HasMore = true, want false")
	}
	if page.Next != 3 {
		t.Errorf("Next = %d, want the last sequence 3", page.Next)
	}
}

// TestEventReadPaginatesStably asserts the cursor returns every remaining
// event exactly once and never repeats a prior page.
func TestEventReadPaginatesStably(t *testing.T) {
	s := newStore()
	seedEvent(t, s, testScope(), "event-1", event.TypeTaskCreated)
	seedEvent(t, s, testScope(), "event-2", event.TypeTaskCreated)
	seedEvent(t, s, testScope(), "event-3", event.TypeTaskCreated)

	reader, err := platform.NewEventReader(s)
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	first, err := reader.Read(context.Background(), event.ReadQuery{Scope: testScope(), Limit: 2})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Events) != 2 || !first.HasMore {
		t.Fatalf("first page = %d events, HasMore=%v; want 2 and true", len(first.Events), first.HasMore)
	}
	second, err := reader.Read(context.Background(), event.ReadQuery{Scope: testScope(), AfterSequence: first.Next, Limit: 2})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Events) != 1 || second.HasMore {
		t.Fatalf("second page = %d events, HasMore=%v; want 1 and false", len(second.Events), second.HasMore)
	}
	if string(second.Events[0].ID) != "event-3" {
		t.Errorf("second page event = %q, want event-3", second.Events[0].ID)
	}
}

// TestEventReadTypeFilterFillsPageAcrossExcludedRows asserts a narrowed read
// keeps scanning until it fills the page instead of reporting a false end.
func TestEventReadTypeFilterFillsPageAcrossExcludedRows(t *testing.T) {
	s := newStore()
	seedEvent(t, s, testScope(), "event-1", event.TypeTaskQuestion)
	seedEvent(t, s, testScope(), "event-2", event.TypeTaskCreated)
	seedEvent(t, s, testScope(), "event-3", event.TypeTaskQuestion)
	seedEvent(t, s, testScope(), "event-4", event.TypeTaskCreated)

	reader, err := platform.NewEventReader(s)
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	page, err := reader.Read(context.Background(), event.ReadQuery{
		Scope: testScope(),
		Types: []event.Type{event.TypeTaskCreated},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("filtered events = %d, want 2", len(page.Events))
	}
	for _, envelope := range page.Events {
		if envelope.Type != event.TypeTaskCreated {
			t.Errorf("filtered event type = %q, want task.created", envelope.Type)
		}
	}
	if page.HasMore {
		t.Error("HasMore = true after exhausting matching rows")
	}
}

// TestEventReadRefusesUnboundedLimit asserts the adapter refuses a limit the
// read contract rejects before running a scan.
func TestEventReadRefusesUnboundedLimit(t *testing.T) {
	s := newStore()
	reader, err := platform.NewEventReader(s)
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	_, err = reader.Read(context.Background(), event.ReadQuery{Scope: testScope(), Limit: event.MaxReadLimit + 1})
	requireClass(t, err, fault.InvalidInput)
}

func testTenantOnlyScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant}
}
