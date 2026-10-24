package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// The named workload t042-flow-v1 (also described in internal/metrics/README.md):
//
//	3 tenants x 2 projects x 200 work items x 21 history rows = 25,200 rows
//	(created, ready, active, then alternating review/active, one hour apart);
//	one list page of 50 items of one project; window of the first ten hours.
const (
	workloadTenants  = 3
	workloadProjects = 2
	workloadItems    = 200
	workloadRows     = 21
	workloadPage     = 50
)

func (e *env) loadWorkload() {
	e.t.Helper()
	x := e.stack.OwnerExec
	x(`INSERT INTO tenants (id, key) SELECT 'tw_' || t, 'tw_' || t FROM generate_series(1, $1) t`, workloadTenants)
	x(`INSERT INTO projects (tenant_id, id, key, active_wip_limit)
		SELECT 'tw_' || t, 'pw_' || p, 'pw_' || p, 1000 FROM generate_series(1, $1) t, generate_series(1, $2) p`, workloadTenants, workloadProjects)
	x(`INSERT INTO work_items (tenant_id, project_id, id, kind, title, state, version, review_subject, created_by_type, created_by_id, created_at, updated_at)
		SELECT 'tw_' || t, 'pw_' || p, 'wi_' || t || '_' || p || '_' || lpad(n::text, 4, '0'), 'task', 'workload', 'active', $4, NULL,
		       'user', 'u', timestamptz '2026-10-01 00:00:00+00' + n * interval '1 minute', timestamptz '2026-10-01 00:00:00+00' + n * interval '1 minute'
		FROM generate_series(1, $1) t, generate_series(1, $2) p, generate_series(1, $3) n`, workloadTenants, workloadProjects, workloadItems, workloadRows)
	x(`INSERT INTO work_item_history (tenant_id, project_id, id, work_item_id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, from_state, to_state)
		SELECT w.tenant_id, w.project_id, 'wih_' || w.id || '_' || v, w.id, v, 1,
		       CASE v WHEN 1 THEN 'created' ELSE 'transitioned' END, 'user', 'u',
		       w.created_at + (v - 1) * interval '1 hour',
		       CASE WHEN v = 1 THEN NULL WHEN v = 2 THEN 'backlog' WHEN v = 3 THEN 'ready' WHEN v % 2 = 0 THEN 'active' ELSE 'review' END,
		       CASE WHEN v = 1 THEN 'backlog' WHEN v = 2 THEN 'ready' WHEN v = 3 THEN 'active' WHEN v % 2 = 0 THEN 'review' ELSE 'active' END
		FROM work_items w, generate_series(1, $1) v`, workloadRows)
	x(`ANALYZE work_items`)
	x(`ANALYZE work_item_history`)
}

type planNode struct {
	NodeType    string     `json:"Node Type"`
	Relation    string     `json:"Relation Name"`
	Index       string     `json:"Index Name"`
	ActualRows  float64    `json:"Actual Rows"`
	ActualLoops float64    `json:"Actual Loops"`
	SharedHit   float64    `json:"Shared Hit Blocks"`
	SharedRead  float64    `json:"Shared Read Blocks"`
	Plans       []planNode `json:"Plans"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// Test_flow_query_plan records the EXPLAIN (ANALYZE, BUFFERS) plan of the batch
// summary over the named workload and asserts the properties that justify the
// access path without pinning planner output: the history of only the selected
// items is read, through the existing (tenant_id, work_item_id, version) unique
// index, and no additional index is needed. A retained result is in
// internal/metrics/PLAN.md.
func Test_flow_query_plan(t *testing.T) {
	e := newEnv(t)
	e.loadWorkload()
	ctx := context.Background()

	var ids []string
	for n := 1; n <= workloadPage; n++ {
		ids = append(ids, fmt.Sprintf("wi_2_1_%04d", n))
	}
	from, to := at(0), at(mins(600))
	var raw []byte
	err := storage.WithTenantOptions(ctx, e.rt, domain.TenantID("tw_2"), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+metricspg.SummarySQL,
				"tw_2", "pw_1", ids, from, to).Scan(&raw)
		})
	if err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan          planNode `json:"Plan"`
		ExecutionTime float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan: %v\n%s", err, raw)
	}

	var historyRows, historyScans, hits, reads float64
	indexes := map[string]bool{}
	plans[0].Plan.walk(func(n planNode) {
		if n.Relation == "work_item_history" {
			historyScans++
			historyRows += n.ActualRows * n.ActualLoops
			hits += n.SharedHit
			reads += n.SharedRead
			if strings.Contains(n.NodeType, "Seq Scan") {
				t.Errorf("work_item_history is read by a %s", n.NodeType)
			}
		}
		// A bitmap heap scan names the relation, its child bitmap index scan the index.
		if n.Index != "" {
			indexes[n.Index] = true
		}
	})
	selected := float64(workloadPage * workloadRows)
	t.Logf("workload t042-flow-v1: %d history rows in table; page of %d items = %.0f rows; execution %.2f ms", workloadTenants*workloadProjects*workloadItems*workloadRows, workloadPage, selected, plans[0].ExecutionTime)
	t.Logf("history scans: %.0f, rows read %.0f, indexes %v", historyScans, historyRows, indexes)
	if historyScans == 0 {
		t.Fatal("the plan does not read work_item_history")
	}
	// The statement derives from the same view three times (summary, blocked runs,
	// episodes); each must touch only the selected items.
	if limit := 3 * selected; historyRows > limit {
		t.Errorf("read %.0f history rows, more than %.0f for %d selected items", historyRows, limit, workloadPage)
	}
	if !indexes["work_item_history_version_key"] {
		t.Errorf("the existing unique index is not used: %v", indexes)
	}
	if os.Getenv("KODAMA_PRINT_PLAN") != "" {
		t.Logf("%s", raw)
	}

	// An item-only page of a different tenant context must find none of these rows.
	var n int
	err = storage.WithTenant(ctx, e.rt, "tw_3", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM work_flow_segments WHERE work_item_id = $1`, ids[0]).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Fatalf("another tenant sees %d rows: %v", n, err)
	}
}
