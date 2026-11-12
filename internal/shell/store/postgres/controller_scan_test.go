// Regression tests: an unfiltered controller scan must render without a WHERE
// clause, and every scoped scan builder must avoid an empty WHERE.
package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func assertNoEmptyWhere(t *testing.T, statement string) {
	t.Helper()
	upper := strings.ToUpper(statement)
	if strings.Contains(upper, "WHERE  ") || strings.HasSuffix(strings.TrimSpace(upper), "WHERE") {
		t.Fatalf("statement has an empty WHERE clause: %s", statement)
	}
	for _, clause := range []string{"WHERE ORDER BY", "WHERE LIMIT", "WHERE FOR UPDATE"} {
		if strings.Contains(upper, clause) {
			t.Fatalf("statement has an empty predicate before %q: %s", clause, statement)
		}
	}
}

// An unfiltered controller scan (no Equals/Range/cursor) must render without a
// WHERE clause; `... WHERE  ORDER BY ...` is a syntax error in PostgreSQL that
// the lenient emulator accepts.
func TestControllerScanWithoutFiltersRendersNoEmptyWhere(t *testing.T) {
	meta, ok := lookupTable("events")
	if !ok {
		t.Fatal("schema has no events table")
	}
	rendered, err := buildControllerScanStatement(meta, store.ScanQuery{Table: "events", Limit: 10})
	if err != nil {
		t.Fatalf("render unfiltered controller scan: %v", err)
	}
	assertNoEmptyWhere(t, rendered.query.SQL)
	if strings.Contains(strings.ToUpper(rendered.query.SQL), " WHERE ") {
		t.Fatalf("unfiltered controller scan rendered a WHERE: %s", rendered.query.SQL)
	}

	controller := newControllerRegressionHarness(t, []seamSeedRow{{
		Table: "events", TenantID: "tenant-1", ProjectID: "project-1", ID: "event-1", Version: 1,
		Values: map[string]any{"type": "execution.succeeded", "publish_sequence": 1},
	}})
	page, err := controller.Scan(context.Background(), store.ControllerScan{
		Table: "events", Limit: 10})
	if err != nil {
		t.Fatalf("execute unfiltered controller scan: %v", err)
	}
	if len(page.Rows) != 1 || emuString(page.Rows[0].Values["id"]) != "event-1" {
		t.Fatalf("unfiltered controller scan rows = %+v; want event-1", page.Rows)
	}
}

// Every scoped scan builder must also omit an empty WHERE. The scope predicate
// currently guarantees at least one term, and this test pins that a future
// scope-free table cannot reintroduce the syntax error.
func TestScopedScanBuildersNeverRenderEmptyWhere(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	for _, table := range store.SchemaDesign().Tables {
		meta, ok := lookupTable(table.Name)
		if !ok {
			t.Fatalf("schema has no %q table", table.Name)
		}
		rendered, err := buildScanStatement(meta, scope, store.ScanQuery{Table: table.Name, Limit: 1})
		if err != nil {
			t.Fatalf("render scan for %q: %v", table.Name, err)
		}
		assertNoEmptyWhere(t, rendered.query.SQL)
	}
}
