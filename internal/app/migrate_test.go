package app

import (
	"context"
	"strings"
	"testing"
)

func TestMigrationOrderIsDeclared(t *testing.T) {
	m, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var modules []string
	for _, c := range Contributions() {
		modules = append(modules, c.Module)
	}
	got := strings.Join(modules, ",")
	want := "storage,events,httpapi,identity,work,secrets,pipeline,workspace,artifacts,execution,execution-workspace-links,execution-links,runtimeauth,execution-cleanup,logs,agents,delivery,gates,gateway,mcpproxy,work_dependencies,metrics,chatops,gitwebhook"
	if got != want {
		t.Fatalf("contribution order\n got %s\nwant %s", got, want)
	}
	ids := map[string]int{}
	for i, mig := range m.Migrations() {
		ids[mig.ID] = i
	}
	// Foreign keys of later modules refer to earlier ones: the order is a dependency order.
	before := func(a, b string) {
		t.Helper()
		ia, ib := -1, -1
		for id, i := range ids {
			if strings.HasPrefix(id, a) {
				ia = i
			}
			if strings.HasPrefix(id, b) {
				ib = i
			}
		}
		if ia < 0 || ib < 0 || ia >= ib {
			t.Fatalf("%s must be applied before %s", a, b)
		}
	}
	before("T002_", "T010_")
	before("T010_", "T011_")
	before("T011_", "T020_002")
	before("T013_", "T020_001")
	before("T015_", "T020_003")
	before("T020_002", "T019_")
	before("T086_", "T023_")
	before("T019_", "T023_")
	// T-072: delivery and gates reference the execution tables (and delivery the
	// artifact and runtime-grant ones), so they are applied after them.
	before("T020_001", "T035_")
	before("T015_", "T035_")
	before("T019_", "T035_")
	before("T020_001", "T034_")
	// T-039: the model gateway references runs and step attempts and the secrets;
	// the MCP proxy references projects.
	before("T020_001", "T032_")
	before("T019_", "T032_")
	before("T010_", "T033_")
	// T-100/T-101: the Git triggers reference runs, work items and definitions.
	before("T020_001", "T100_")
	before("T011_", "T100_")
}

func TestMigrateRefusesUnsafeIdentities(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]MigrateConfig{
		"no owner":           {RuntimeURL: "postgres://rt:p@h/db"},
		"no runtime":         {MigrationURL: "postgres://own:p@h/db"},
		"same role":          {MigrationURL: "postgres://same:p@h/db", RuntimeURL: "postgres://same:q@h/db"},
		"anonymous owner":    {MigrationURL: "postgres://h/db", RuntimeURL: "postgres://rt:p@h/db"},
		"same role by name":  {MigrationURL: "postgres://same:p@h/db", RuntimeRole: "same"},
		"both runtime forms": {MigrationURL: "postgres://own:p@h/db", RuntimeURL: "postgres://rt:p@h/db", RuntimeRole: "rt"},
	} {
		if _, err := Migrate(ctx, tc); err == nil {
			t.Errorf("%s: migration was attempted", name)
		}
	}
}
