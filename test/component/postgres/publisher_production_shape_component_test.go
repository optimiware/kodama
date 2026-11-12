//go:build component

package postgrescomponent

import (
	"context"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestPublisherProductionShapeOnPostgreSQL proves the publisher's declared
// policies in the shape production actually uses: the outbox adapter runs
// UNSCOPED (no kodama.tenant_id/project_id), so the PUBLIC events_scope policy
// yields NULL and only the publisher's own SELECT/UPDATE policies admit rows.
// It also pins the two effective out-of-set protections: RLS WITH CHECK in the
// unscoped production shape, and the events_publish_state CHECK constraint when
// a caller happens to set the row's scope.
func TestPublisherProductionShapeOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope1 := pgtest.Scope("tenant-1", "project-1")
	scope2 := pgtest.Scope("tenant-2", "project-2")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-2", "project-2")
	shard.SeedEvent(ctx, scope1, "event-1", "execution.queued", "execution-1", 1)
	shard.SeedEvent(ctx, scope2, "event-2", "execution.queued", "execution-2", 1)

	publisher := shard.Role(store.PublisherRole, 2)
	defer publisher.Close()
	pgtest.RequireNonOwnerSession(t, ctx, publisher, store.PublisherRole)

	// (1) Raw unscoped read: the declared any SELECT policy returns BOTH
	// tenants' pending rows even though no transaction-local scope is set.
	rows, err := pgtest.QueryStringsN(ctx, publisher, 3,
		`SELECT id, tenant_id, publish_sequence::text FROM events WHERE publish_state = 'pending' ORDER BY publish_sequence`)
	if err != nil {
		t.Fatalf("publisher unscoped read: %v%s", err, pgtest.CauseText(err))
	}
	if len(rows) != 2 || rows[0][1] != "tenant-1" || rows[1][1] != "tenant-2" {
		t.Fatalf("publisher unscoped pending rows = %v; want both tenants in sequence order", rows)
	}

	// (1b) The PRODUCT outbox adapter in its real unscoped shape: claim pending
	// events across tenants, mark one published, and read the remainder.
	outbox := storepg.NewOutbox(publisher)
	pending, err := outbox.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("outbox Pending: %v%s", err, pgtest.CauseText(err))
	}
	if len(pending) != 2 {
		t.Fatalf("outbox pending events = %d; want 2 across tenants", len(pending))
	}
	if pending[0].Event.ID != "event-1" || pending[1].Event.ID != "event-2" {
		t.Fatalf("outbox pending order = %v; want event-1,event-2", pendingIDs(pending))
	}
	if pending[0].Event.Scope != scope1 || pending[1].Event.Scope != scope2 {
		t.Fatalf("outbox pending scopes = %v/%v; want both tenants", pending[0].Event.Scope, pending[1].Event.Scope)
	}
	// A stale sequence for a still-pending event is not_found and leaves the
	// row pending: the acknowledgement must match the exact sequence the
	// publisher read, not merely the event identity.
	err = outbox.MarkPublished(ctx, id.EventID("event-2"), pending[0].Sequence)
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("outbox MarkPublished at the wrong sequence error = %v; want not_found", err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-2' AND publish_state = 'pending'`, 1, "event-2 pending after wrong-sequence ack")
	publishObserver := shard.ObserveUnit(t, "outbox", "mark_published")
	if err := outbox.MarkPublished(ctx, id.EventID("event-1"), pending[0].Sequence); err != nil {
		t.Fatalf("outbox MarkPublished: %v%s", err, pgtest.CauseText(err))
	}
	state, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT publish_state FROM events WHERE id = 'event-1'`)
	if err != nil || state != "published" {
		t.Fatalf("event-1 state = %q, err=%v; want published", state, err)
	}
	shard.RequireGoldenRow(t, ctx, scopedDB(t, shard), scope1, "events", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "execution.queued", "report_type": pgtest.NullValue,
		"subject_id": "execution-1", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "published",
		"publish_sequence": int64(pending[0].Sequence),
	})
	publishObserver.Finish(map[string]int{"events": 1})
	// Re-acknowledging the exact sequence is idempotent; acknowledging an
	// existing event at a different sequence is not_found, never a silent
	// success that would acknowledge a row the publisher did not read.
	if err := outbox.MarkPublished(ctx, id.EventID("event-1"), pending[0].Sequence); err != nil {
		t.Fatalf("outbox MarkPublished replay: %v%s", err, pgtest.CauseText(err))
	}
	err = outbox.MarkPublished(ctx, id.EventID("event-1"), pending[1].Sequence)
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("outbox MarkPublished at the wrong sequence error = %v; want not_found", err)
	}
	remaining, err := outbox.Pending(ctx, 0, 10)
	if err != nil {
		t.Fatalf("outbox Pending after mark: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Event.ID != "event-2" {
		t.Fatalf("outbox remaining = %v; want event-2 only", pendingIDs(remaining))
	}

	// (2) An in-set unscoped UPDATE affects exactly the targeted row.
	shard.SeedEvent(ctx, scope1, "event-3", "execution.queued", "execution-3", 1)
	affected, err := pgtest.ExecSQL(ctx, publisher,
		`UPDATE events SET publish_state = 'published' WHERE id = 'event-3'`)
	if err != nil || affected != 1 {
		t.Fatalf("publisher unscoped in-set update = %d rows, err=%v; want 1", affected, err)
	}
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-3' AND publish_state = 'published'`, 1, "event-3 published")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-2' AND publish_state = 'pending'`, 1, "event-2 untouched")

	// (3) An out-of-set value is refused by RLS in the UNSCOPED production
	// shape on the unmodified schema (the events_scope policy yields NULL and
	// the publisher WITH CHECK is the only admitting predicate).
	_, err = pgtest.ExecSQL(ctx, publisher,
		`UPDATE events SET publish_state = 'bogus' WHERE id = 'event-2'`)
	pgtest.RequireRLSViolation(t, err, "publisher unscoped out-of-set publish_state")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-2' AND publish_state = 'pending'`, 1, "event-2 unchanged after RLS refusal")

	// (4) The same out-of-set update with the row's scope SET is admitted by
	// the permissive events_scope policy but still refused by the explicit
	// events_publish_state CHECK constraint (SQLSTATE 23514).
	_, err = pgtest.ScopedExec(ctx, publisher, scope2,
		`UPDATE events SET publish_state = 'bogus' WHERE id = 'event-2'`)
	pgtest.RequireSQLState(t, err, "23514", "publisher scoped out-of-set publish_state check")
	pgtest.RequireRowCount(t, ctx, shard.Admin(),
		`SELECT count(*)::text FROM events WHERE id = 'event-2' AND publish_state = 'pending'`, 1, "event-2 unchanged after check refusal")

	// The publisher still cannot read a domain table unscoped.
	_, err = pgtest.ExecSQL(ctx, publisher, `SELECT count(*) FROM tasks`)
	pgtest.RequirePermissionDenied(t, err, "publisher unscoped task read")
}

func pendingIDs(pending []event.PendingEvent) []string {
	out := make([]string, len(pending))
	for index, row := range pending {
		out[index] = string(row.Event.ID) + "@" + strconv.FormatUint(uint64(row.Sequence), 10)
	}
	return out
}

// TestOutboxPaginationOnPostgreSQL proves Pending respects its limit exactly and
// continues after the last returned sequence without skipping or repeating.
func TestOutboxPaginationOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	for index, id := range []string{"event-1", "event-2", "event-3"} {
		shard.SeedEvent(ctx, scope, id, "execution.queued", "subject-"+id, int64(index+1))
	}
	publisher := shard.Role(store.PublisherRole, 2)
	defer publisher.Close()
	outbox := storepg.NewOutbox(publisher)
	first, err := outbox.Pending(ctx, 0, 2)
	if err != nil {
		t.Fatalf("Pending page 1: %v%s", err, pgtest.CauseText(err))
	}
	if len(first) != 2 || first[0].Event.ID != "event-1" || first[1].Event.ID != "event-2" {
		t.Fatalf("pending page 1 = %v; want exactly event-1,event-2", pendingIDs(first))
	}
	second, err := outbox.Pending(ctx, first[len(first)-1].Sequence, 10)
	if err != nil {
		t.Fatalf("Pending page 2: %v%s", err, pgtest.CauseText(err))
	}
	if len(second) != 1 || second[0].Event.ID != "event-3" {
		t.Fatalf("pending page 2 = %v; want exactly event-3", pendingIDs(second))
	}
}
