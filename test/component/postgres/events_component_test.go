//go:build component

package postgrescomponent

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestEventReadAndCursorOnPostgreSQL proves the real event read port and cursor
// over PostgreSQL: durable sequence order, keyset pagination that stays stable
// across concurrent inserts, and strict scope isolation.
func TestEventReadAndCursorOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	other := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	for index, id := range []string{"event-1", "event-2", "event-3"} {
		shard.SeedEvent(ctx, scope, id, "execution.queued", "execution-"+id, int64(index+1))
	}
	shard.SeedEvent(ctx, other, "event-other", "execution.queued", "execution-other", 1)

	runtime := shard.Runtime(4)
	defer runtime.Close()
	scanner := storepg.New(runtime)

	cursorReader, err := readers.NewCursorReader(scanner)
	if err != nil {
		t.Fatalf("NewCursorReader: %v", err)
	}
	cursor, err := cursorReader.Cursor(ctx, scope)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	latest, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT max(publish_sequence)::text FROM events WHERE tenant_id = 'tenant-1'`)
	if err != nil || int64(cursor) != latest {
		t.Fatalf("cursor = %d, want %d (err=%v)", cursor, latest, err)
	}

	reader, err := platform.NewEventReader(scanner)
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	first, err := reader.Read(ctx, event.ReadQuery{Scope: scope, Limit: 2})
	if err != nil {
		t.Fatalf("read page 1: %v", err)
	}
	if len(first.Events) != 2 || !first.HasMore || first.Next == 0 {
		t.Fatalf("page 1 = %d events hasMore=%v next=%d; want 2 and more", len(first.Events), first.HasMore, first.Next)
	}
	if first.Events[0].ID != "event-1" || first.Events[1].ID != "event-2" {
		t.Fatalf("page 1 ids = %v; want event-1,event-2", []string{string(first.Events[0].ID), string(first.Events[1].ID)})
	}

	// Insert a new event between pages. The keyset cursor must not duplicate or
	// skip an existing row, and the new row must appear after the cursor.
	shard.SeedEvent(ctx, scope, "event-4", "execution.queued", "execution-4", 4)
	second, err := reader.Read(ctx, event.ReadQuery{Scope: scope, AfterSequence: first.Next, Limit: 10})
	if err != nil {
		t.Fatalf("read page 2: %v", err)
	}
	if len(second.Events) != 2 || second.Events[0].ID != "event-3" || second.Events[1].ID != "event-4" {
		t.Fatalf("page 2 ids = %v; want event-3,event-4", eventIDs(second.Events))
	}
	for _, envelope := range second.Events {
		if envelope.ID == first.Events[0].ID || envelope.ID == first.Events[1].ID {
			t.Fatalf("page 2 repeated %q", envelope.ID)
		}
	}

	// The other scope never appears in a scoped read.
	all, err := reader.Read(ctx, event.ReadQuery{Scope: scope, Limit: event.MaxReadLimit})
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	for _, envelope := range all.Events {
		if envelope.Scope != scope {
			t.Fatalf("scoped read returned %v from another scope", envelope.Scope)
		}
	}
}

func eventIDs(events []event.Envelope) []string {
	out := make([]string, len(events))
	for index, envelope := range events {
		out[index] = string(envelope.ID)
	}
	return out
}

// TestEventReadTypeFilterOnPostgreSQL proves the type filter is applied to real
// committed rows: only matching types are returned, and keyset pagination steps
// over interleaved non-matching rows without skipping or duplicating a match.
func TestEventReadTypeFilterOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	// Interleave two types so every matching row is separated by a skipped one.
	seeds := []struct {
		id        string
		eventType string
	}{
		{"event-1", "execution.queued"},
		{"event-2", "task.created"},
		{"event-3", "execution.queued"},
		{"event-4", "task.created"},
		{"event-5", "execution.queued"},
		{"event-6", "task.created"},
	}
	for index, seed := range seeds {
		shard.SeedEvent(ctx, scope, seed.id, seed.eventType, "subject-"+seed.id, int64(index+1))
	}

	runtime := shard.Runtime(4)
	defer runtime.Close()
	reader, err := platform.NewEventReader(storepg.New(runtime))
	if err != nil {
		t.Fatalf("NewEventReader: %v", err)
	}
	first, err := reader.Read(ctx, event.ReadQuery{
		Scope: scope, Types: []event.Type{event.TypeTaskCreated}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("read filtered page 1: %v%s", err, pgtest.CauseText(err))
	}
	if got := eventIDs(first.Events); len(got) != 2 || got[0] != "event-2" || got[1] != "event-4" {
		t.Fatalf("filtered page 1 ids = %v; want event-2,event-4", got)
	}
	if !first.HasMore || first.Next != 4 {
		t.Fatalf("filtered page 1 hasMore=%v next=%d; want true and 4", first.HasMore, first.Next)
	}
	second, err := reader.Read(ctx, event.ReadQuery{
		Scope: scope, AfterSequence: first.Next, Types: []event.Type{event.TypeTaskCreated}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("read filtered page 2: %v%s", err, pgtest.CauseText(err))
	}
	if got := eventIDs(second.Events); len(got) != 1 || got[0] != "event-6" {
		t.Fatalf("filtered page 2 ids = %v; want event-6", got)
	}
	if second.HasMore || second.Next != 6 {
		t.Fatalf("filtered page 2 hasMore=%v next=%d; want false and 6", second.HasMore, second.Next)
	}
}

// TestScannerKeysetCursorOnPostgreSQL proves the scanner's opaque keyset cursor
// is a strict continuation: a second page starts after the last returned row
// and never repeats it.
func TestScannerKeysetCursorOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	for index, id := range []string{"event-1", "event-2", "event-3", "event-4"} {
		shard.SeedEvent(ctx, scope, id, "execution.queued", "subject-"+id, int64(index+1))
	}
	runtime := shard.Runtime(4)
	defer runtime.Close()
	scanner := storepg.New(runtime)

	for name, order := range map[string][]store.Order{
		"sequence order": {{Column: "publish_sequence"}},
		"identity order": {{Column: "id"}},
	} {
		t.Run(name, func(t *testing.T) {
			first, err := scanner.Query(ctx, scope, store.ScanQuery{Table: "events", OrderBy: order, Limit: 2})
			if err != nil {
				t.Fatalf("scan page 1: %v%s", err, pgtest.CauseText(err))
			}
			if got := scanRowIDs(first.Rows); len(got) != 2 || got[0] != "event-1" || got[1] != "event-2" {
				t.Fatalf("scan page 1 ids = %v; want event-1,event-2", got)
			}
			if !first.HasMore || first.Next == "" {
				t.Fatalf("scan page 1 hasMore=%v next=%q; want true and a cursor", first.HasMore, first.Next)
			}
			second, err := scanner.Query(ctx, scope, store.ScanQuery{
				Table: "events", OrderBy: order, Limit: 2, After: first.Next,
			})
			if err != nil {
				t.Fatalf("scan page 2: %v%s", err, pgtest.CauseText(err))
			}
			if got := scanRowIDs(second.Rows); len(got) != 2 || got[0] != "event-3" || got[1] != "event-4" {
				t.Fatalf("scan page 2 ids = %v; want event-3,event-4", got)
			}
			if second.HasMore {
				t.Fatalf("scan page 2 hasMore=true; want false")
			}
		})
	}
}

// scanRowIDs extracts the id column of a scanned page in order.
func scanRowIDs(rows []store.Row) []string {
	out := make([]string, len(rows))
	for index, row := range rows {
		out[index], _ = row.Values["id"].(string)
	}
	return out
}

// TestReceiptLookupOnPostgreSQL proves the scoped receipt lookup returns the
// committed record inside its scope and nothing outside it.
func TestReceiptLookupOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedReceipt(ctx, scope, "receipt-1", "principal-1", "task.create", "caller-1", `{"a":1}`, `{"task":"task-1"}`)
	shard.SeedReceipt(ctx, scope, "receipt-3", "principal-1", "task.create", "caller-3", `{"c":3}`, `{"task":"task-3"}`)
	shard.SeedReceipt(ctx, scope, "receipt-op-1", "principal-1", "task.create", "shared-caller", `{"op":1}`, `{"op":"create"}`)
	shard.SeedReceipt(ctx, scope, "receipt-op-2", "principal-1", "task.reply", "shared-caller", `{"op":2}`, `{"op":"reply"}`)
	shard.SeedReceipt(ctx, pgtest.Scope("tenant-2", "project-2"), "receipt-2", "principal-1", "task.create", "caller-2", `{"b":2}`, `{"task":"task-2"}`)

	runtime := shard.Runtime(4)
	defer runtime.Close()
	lookup, err := readers.NewReceiptLookup(storepg.New(runtime))
	if err != nil {
		t.Fatalf("NewReceiptLookup: %v", err)
	}
	key := receipt.Key{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID,
		PrincipalID: "principal-1", Operation: "task.create", CallerKey: "caller-1",
	}
	record, found, err := lookup.Lookup(ctx, key)
	if err != nil || !found {
		t.Fatalf("lookup in scope: found=%v err=%v", found, err)
	}
	if string(record.Result) != `{"task":"task-1"}` {
		t.Fatalf("receipt result = %s; want task-1", record.Result)
	}
	// The caller key is part of the identity: the same principal and operation
	// under another caller key returns its own receipt, never the first one.
	other := key
	other.CallerKey = "caller-3"
	otherRecord, found, err := lookup.Lookup(ctx, other)
	if err != nil || !found {
		t.Fatalf("lookup caller-3: found=%v err=%v", found, err)
	}
	if otherRecord.ID != "receipt-3" || string(otherRecord.Result) != `{"task":"task-3"}` {
		t.Fatalf("caller-3 receipt = %+v; want receipt-3/task-3", otherRecord)
	}
	unknown := key
	unknown.CallerKey = "caller-unknown"
	if _, found, err := lookup.Lookup(ctx, unknown); err != nil || found {
		t.Fatalf("unknown caller-key lookup found=%v err=%v; want not found", found, err)
	}
	// The operation is part of the identity: two receipts that differ only by
	// operation resolve independently, and an unknown operation is not found.
	for operation, wantID := range map[string]string{"task.create": "receipt-op-1", "task.reply": "receipt-op-2"} {
		opKey := key
		opKey.Operation = operation
		opKey.CallerKey = "shared-caller"
		record, found, err := lookup.Lookup(ctx, opKey)
		if err != nil || !found || string(record.ID) != wantID {
			t.Fatalf("lookup operation %s = %+v found=%v err=%v; want %s", operation, record, found, err, wantID)
		}
	}
	unknownOp := key
	unknownOp.Operation = "task.delete"
	unknownOp.CallerKey = "shared-caller"
	if _, found, err := lookup.Lookup(ctx, unknownOp); err != nil || found {
		t.Fatalf("unknown-operation lookup found=%v err=%v; want not found", found, err)
	}
	foreign := key
	foreign.ProjectID = "project-2"
	if _, found, err := lookup.Lookup(ctx, foreign); err != nil || found {
		t.Fatalf("cross-scope lookup found=%v err=%v; want not found", found, err)
	}
}
