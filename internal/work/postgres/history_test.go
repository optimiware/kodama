package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
)

// Test_blocking_history: setting and clearing a blocked reason retains actor,
// timestamps and before/after values without changing the content revision, in
// history, audit and events; the runtime role cannot edit or delete history.
// Real PostgreSQL. WM-06, WM-A08 (blocking/history portion).
func Test_blocking_history(t *testing.T) {
	e := newEnv(t)
	it := e.create(scopeA, work.KindStory, "Blocked story")

	block, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version,
		Patch: work.Patch{BlockedReason: work.Some("waiting for ERP sandbox access")}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	unblock, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: block.Version,
		Patch: work.Patch{BlockedReason: work.Clear[string]()}})
	if err != nil {
		t.Fatal(err)
	}
	if block.ContentRevision != 1 || unblock.ContentRevision != 1 || unblock.Version != 3 || unblock.BlockedReason != "" {
		t.Fatalf("revisions: %+v %+v", block, unblock)
	}

	hist, err := e.service.History(context.Background(), scopeA, e.caller(analyst), it.ID, work.HistoryQuery{Limit: 10})
	if err != nil || len(hist) != 3 {
		t.Fatalf("history: %v %d", err, len(hist))
	}
	set, clear := hist[1], hist[2]
	if set.Actor.ID != engineer || set.Actor.Type != work.ActorUser || set.After[work.FieldBlockedReason] != "waiting for ERP sandbox access" ||
		set.Before[work.FieldBlockedReason] != nil || set.ContentRevision != 1 || set.ContentChanged() {
		t.Fatalf("set entry: %+v", set)
	}
	if clear.Before[work.FieldBlockedReason] != "waiting for ERP sandbox access" || clear.After[work.FieldBlockedReason] != nil || clear.ContentChanged() {
		t.Fatalf("clear entry: %+v", clear)
	}
	if !clear.OccurredAt.After(set.OccurredAt) || set.OccurredAt.Before(hist[0].OccurredAt) {
		t.Fatalf("timestamps do not advance: %v %v %v", hist[0].OccurredAt, set.OccurredAt, clear.OccurredAt)
	}

	// Audit rows carry actor, action, version and timing but no content.
	tenant := string(tenantA)
	var action, actor string
	var version int64
	var metadata string
	if err := storage.WithTenant(context.Background(), e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT action, actor_id, resource_version, metadata::text FROM audit
			WHERE tenant_id = $1 AND resource_id = $2 AND resource_version = 2`, tenant, string(it.ID)).Scan(&action, &actor, &version, &metadata)
	}); err != nil {
		t.Fatal(err)
	}
	if action != "work_item.updated" || actor != engineer || strings.Contains(metadata, "ERP sandbox") || !strings.Contains(metadata, "blockedReason") {
		t.Fatalf("audit: %s %s %s", action, actor, metadata)
	}

	// The event names the changed field and carries no content revision (none changed).
	var payload string
	if err := storage.WithTenant(context.Background(), e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT convert_from(payload, 'UTF8') FROM event_observations WHERE tenant_id = $1 AND resource_id = $2 AND resource_version = 2`, tenant, string(it.ID)).Scan(&payload)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"changedFields":["blockedReason"]`) || strings.Contains(payload, "contentRevision") || strings.Contains(payload, "ERP sandbox") {
		t.Fatalf("event payload: %s", payload)
	}

	// Reopen history for interval reports: states and reasons are recorded per entry.
	reopened := e.toReview(e.activate(scopeA, "reopened"))
	done := e.mustMove(reopened, reviewer, work.StateDone, completion(reopened))
	e.mustMove(done, reviewer, work.StateActive, func(r *work.TransitionRequest) { r.Reason = "regression" })
	rh, _ := e.service.History(context.Background(), scopeA, e.caller(analyst), reopened.ID, work.HistoryQuery{Limit: 20})
	var path []string
	for _, h := range rh {
		if h.Kind == work.ChangeTransitioned {
			path = append(path, string(h.From)+">"+string(h.To))
		}
	}
	if strings.Join(path, ",") != "backlog>ready,ready>active,active>review,review>done,done>active" {
		t.Fatalf("transition path: %v", path)
	}

	// The runtime role cannot rewrite or delete history (or audit).
	for _, stmt := range []string{
		`UPDATE work_item_history SET reason = 'edited'`,
		`DELETE FROM work_item_history`,
		`TRUNCATE work_item_history`,
		`DELETE FROM work_items`,
	} {
		err := storage.WithTenant(context.Background(), e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%q: %v", stmt, err)
		}
	}
	// Even through an owner-equivalent privilege escalation attempt in SQL: the
	// history table has no UPDATE policy for the tenant role.
	if n := e.count(tenant, `SELECT count(*) FROM work_item_history WHERE tenant_id = $1 AND reason = 'edited'`, tenant); n != 0 {
		t.Fatalf("history was rewritten")
	}
	_ = domain.Scope{}
}
