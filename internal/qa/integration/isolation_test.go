package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/qa/qatest"
	"github.com/optimiware/kodama/internal/work"
)

// Test_qa_tenant_isolation proves WM-01 for the mapping: defects and their pins
// belong to the tenant and project of the QA attempt; another tenant or project
// can neither read them (row-level security and scoped lookups) nor map the
// attempt's report.
func Test_qa_tenant_isolation(t *testing.T) {
	s := newStack(t, qatest.Options{})
	s.complete(s.finding("Order form shows a blank page", "/orders/new", 0), s.finding("Totals are wrong", "/orders", 1))
	res := s.mustRecord(t)
	ctx := context.Background()
	id := res.Defects[0].ID

	// The defects exist in the attempt's project only.
	if got := s.DefectRows(scopeA); got != 2 {
		t.Fatalf("defects: %d", got)
	}
	if got := s.X.CountTenant(scopeA.TenantID, `SELECT count(*) FROM work_items WHERE kind = 'defect'`); got != 2 {
		t.Fatalf("tenant A sees %d defects through RLS", got)
	}
	for name, q := range map[string]string{
		"defects":        `SELECT count(*) FROM work_items WHERE kind = 'defect'`,
		"pins":           `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'qa_defect'`,
		"defect history": `SELECT count(*) FROM work_item_history WHERE actor_type = 'workload'`,
		"defect audit":   `SELECT count(*) FROM audit WHERE actor_type = 'workload' AND resource_type = 'work_item'`,
	} {
		if got := s.X.CountTenant(qatest.ScopeB.TenantID, q); got != 0 {
			t.Fatalf("tenant B sees %d of tenant A's %s", got, name)
		}
	}

	foreign := map[string]domain.Scope{
		"another tenant":  qatest.ScopeB,
		"another project": qatest.ScopeA2,
	}
	for name, sc := range foreign {
		if _, err := s.WorkStore.Get(ctx, sc, id); !errors.Is(err, work.ErrNotFound) {
			t.Fatalf("%s reads the defect: %v", name, err)
		}
		cmd := s.Run.Command()
		cmd.Scope = sc
		if _, err := s.Service.Record(ctx, cmd); !errors.Is(err, qa.ErrNotFound) {
			t.Fatalf("%s maps the attempt's report: %v", name, err)
		}
	}
	if got := s.DefectRows(qatest.ScopeB) + s.DefectRows(qatest.ScopeA2); got != 0 {
		t.Fatalf("a foreign mapping attempt filed %d defects", got)
	}
	// The attempt's own workload identity may not map at all (mapping is a
	// control-plane operation), in its own tenant or another one.
	cmd := s.Run.Command()
	cmd.Scope = qatest.ScopeB
	cmd.Caller = qa.Caller{Actor: qa.Actor{Type: qa.ActorWorkload, ID: string(s.Run.ID())}}
	if _, err := s.Service.Record(ctx, cmd); !errors.Is(err, qa.ErrDenied) {
		t.Fatalf("the attempt's identity maps through another tenant: %v", err)
	}
}
