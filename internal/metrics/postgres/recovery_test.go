package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// spyDB wraps the runtime pool: it records transaction options and can make the
// summary statement fail after the tenant context was installed.
type spyDB struct {
	inner    storage.TxBeginner
	opts     []pgx.TxOptions
	failRead bool
}

func (s *spyDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	s.opts = append(s.opts, o)
	tx, err := s.inner.BeginTx(ctx, o)
	if err != nil || !s.failRead {
		return tx, err
	}
	return failingTx{tx}, nil
}

type failingTx struct{ pgx.Tx }

func (failingTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("injected statement failure")
}

// rawItem inserts a work item row and its history rows as the owner, simulating
// stored history that no T-011 mutation could have produced.
type rawRow struct {
	version int64
	kind    string
	at      time.Time
	from    *string
	to      *string
	after   string
}

func (e *env) rawItem(scope domain.Scope, id string, rows ...rawRow) {
	e.t.Helper()
	e.stack.OwnerExec(`INSERT INTO work_items (tenant_id, project_id, id, kind, title, created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'task', 't', 'user', 'u', now(), now())`, string(scope.TenantID), string(scope.ProjectID), id)
	for _, r := range rows {
		after := r.after
		if after == "" {
			after = "{}"
		}
		e.stack.OwnerExec(`INSERT INTO work_item_history
			(tenant_id, project_id, id, work_item_id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, from_state, to_state, after_values)
			VALUES ($1, $2, $3, $4, $5, 1, $6, 'user', 'u', $7, $8, $9, $10::jsonb)`,
			string(scope.TenantID), string(scope.ProjectID), "wih_"+id+"_"+string(rune('a'+r.version)), id, r.version, r.kind, r.at, r.from, r.to, after)
	}
}

func s(v string) *string { return &v }

// Test_summary_failure_recovery: a failed or cancelled read returns nothing, the
// statement is a read-only snapshot with no write effects so a retry is safe,
// malformed stored history is reported explicitly per item (never as negative or
// invented durations), and a pooled connection is clean after success and
// failure. WM-12.
func Test_summary_failure_recovery(t *testing.T) {
	e := newEnv(t)
	good := e.toActive(e.create(scopeA, at(0), "healthy"), at(mins(1)), at(mins(2)))
	w := window(at(0), at(mins(60)))
	ids := []domain.WorkItemID{good.ID}
	ctx := context.Background()

	t.Run("cancellation_returns_nothing", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		got, err := e.store.ReadSummaries(cctx, scopeA, ids, w)
		if got != nil || !errors.Is(err, metrics.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v err %v", got, err)
		}
		dctx, dcancel := context.WithTimeout(ctx, time.Nanosecond)
		defer dcancel()
		time.Sleep(time.Millisecond)
		got, err = e.store.ReadSummaries(dctx, scopeA, ids, w)
		if got != nil || !errors.Is(err, metrics.ErrUnavailable) {
			t.Fatalf("deadline: got %v err %v", got, err)
		}
		if ok, err := e.store.ReadSummaries(ctx, scopeA, ids, w); err != nil || len(ok) != 1 {
			t.Fatalf("retry after cancellation: %v %v", ok, err)
		}
	})

	t.Run("a_database_outage_returns_nothing", func(t *testing.T) {
		pool := e.stack.DB.RuntimePool(ctx, 2)
		st, err := metricspg.NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		pool.Close()
		if got, err := st.ReadSummaries(ctx, scopeA, ids, w); got != nil || !errors.Is(err, metrics.ErrUnavailable) {
			t.Fatalf("closed pool: got %v err %v", got, err)
		}
		if _, err := metricspg.NewStore(nil); err == nil {
			t.Fatal("a store without a database was built")
		}
	})

	t.Run("reads_are_snapshots_without_effects", func(t *testing.T) {
		spy := &spyDB{inner: e.rt}
		st, _ := metricspg.NewStore(spy)
		count := func() [2]int {
			return [2]int{
				e.count(tenantA, `SELECT count(*) FROM work_item_history`),
				e.count(tenantA, `SELECT count(*) FROM audit`),
			}
		}
		before := count()
		var first []metrics.WorkSummary
		for i := 0; i < 3; i++ {
			got, err := st.ReadSummaries(ctx, scopeA, ids, w)
			if err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = got
			} else if len(got) != 1 || got[0] != first[0] {
				t.Fatalf("retry %d differs: %+v vs %+v", i, got, first)
			}
		}
		if after := count(); after != before {
			t.Fatalf("reads changed stored rows: %v -> %v", before, after)
		}
		for _, o := range spy.opts {
			if o.AccessMode != pgx.ReadOnly || o.IsoLevel != pgx.RepeatableRead {
				t.Fatalf("transaction options = %+v, want read-only repeatable read", o)
			}
		}
		// The options do what they say: a write in such a transaction is refused.
		err := storage.WithTenantOptions(ctx, e.rt, tenantA, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
			func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE work_items SET title = 'x'`)
				return err
			})
		if err == nil {
			t.Fatal("a write succeeded in a read-only transaction")
		}
	})

	t.Run("a_pooled_connection_is_clean_after_success_and_failure", func(t *testing.T) {
		one := e.stack.DB.RuntimePool(ctx, 1)
		spy := &spyDB{inner: one}
		st, _ := metricspg.NewStore(spy)
		setting := func() *string {
			var v *string
			if err := one.QueryRow(ctx, `SELECT nullif(current_setting('kodama.tenant_id', true), '')`).Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if _, err := st.ReadSummaries(ctx, scopeA, ids, w); err != nil || setting() != nil {
			t.Fatalf("after success: %v / %v", err, setting())
		}
		spy.failRead = true
		if got, err := st.ReadSummaries(ctx, scopeA, ids, w); got != nil || !errors.Is(err, metrics.ErrUnavailable) {
			t.Fatalf("injected failure: %v %v", got, err)
		}
		if setting() != nil {
			t.Fatal("tenant context survived a failed read on the pooled connection")
		}
		spy.failRead = false
		if got, err := st.ReadSummaries(ctx, scopeA, ids, w); err != nil || len(got) != 1 {
			t.Fatalf("after failure: %v %v", got, err)
		}
		if n := countOn(t, one); n != 0 {
			t.Fatalf("the reused connection reads %d rows without tenant context", n)
		}
	})

	t.Run("malformed_history_fails_explicitly_per_item", func(t *testing.T) {
		start := at(mins(10))
		created := rawRow{version: 1, kind: "created", at: start, to: s("backlog")}
		cases := []struct {
			id      string
			anomaly string
			rows    []rawRow
		}{
			{"wi_t042_gap", "version_gap", []rawRow{created, {version: 3, kind: "updated", at: at(mins(11)), from: s("backlog"), to: s("backlog")}}},
			{"wi_t042_backwards", "time_backwards", []rawRow{created, {version: 2, kind: "updated", at: at(mins(5)), from: s("backlog"), to: s("backlog")}}},
			{"wi_t042_chain", "state_chain", []rawRow{created, {version: 2, kind: "transitioned", at: at(mins(11)), from: s("review"), to: s("done")}}},
			{"wi_t042_no_change", "state_chain", []rawRow{created, {version: 2, kind: "transitioned", at: at(mins(11)), from: s("backlog"), to: s("backlog")}}},
			{"wi_t042_after_cancel", "state_chain", []rawRow{created,
				{version: 2, kind: "transitioned", at: at(mins(11)), from: s("backlog"), to: s("cancelled")},
				{version: 3, kind: "updated", at: at(mins(12)), from: s("cancelled"), to: s("cancelled")}}},
			{"wi_t042_first", "first_not_created", []rawRow{{version: 1, kind: "updated", at: start, from: s("backlog"), to: s("backlog")}}},
			{"wi_t042_twice", "duplicate_created", []rawRow{created, {version: 2, kind: "created", at: at(mins(11)), to: s("backlog")}}},
			{"wi_t042_blocked", "blocked_value", []rawRow{created, {version: 2, kind: "updated", at: at(mins(11)), from: s("backlog"), to: s("backlog"), after: `{"blockedReason": 5}`}}},
			{"wi_t042_nostate", "state_unknown", []rawRow{{version: 1, kind: "created", at: start}}},
		}
		var all []domain.WorkItemID
		for _, c := range cases {
			e.rawItem(scopeA, c.id, c.rows...)
			all = append(all, domain.WorkItemID(c.id))
		}
		all = append(all, good.ID)
		got, err := e.store.ReadSummaries(ctx, scopeA, all, window(at(0), at(mins(60))))
		if err != nil || len(got) != len(all) {
			t.Fatalf("got %d summaries, err %v", len(got), err)
		}
		byID := map[domain.WorkItemID]metrics.WorkSummary{}
		for _, g := range got {
			byID[g.ItemID] = g
		}
		for _, c := range cases {
			g := byID[domain.WorkItemID(c.id)]
			want := metrics.WorkSummary{Scope: scopeA, ItemID: g.ItemID, Window: g.Window, Status: metrics.StatusInvalidHistory, Anomaly: c.anomaly}
			if g != want {
				t.Errorf("%s: %+v, want anomaly %s without figures", c.id, g, c.anomaly)
			}
			if err := g.Validate(); err != nil {
				t.Errorf("%s: %v", c.id, err)
			}
		}
		if g := byID[good.ID]; g.Status != metrics.StatusOK || g.Lead != mins(60) {
			t.Errorf("a healthy item beside corrupt ones: %+v", g)
		}
	})
}

func countOn(t *testing.T, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM work_flow_segments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
