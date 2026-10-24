package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_quota_double_claim: PX-11, PX-06. Tenant-wide and project-wide
// contention cannot exceed the limits; reservation and release are idempotent.
func Test_quota_double_claim(t *testing.T) {
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 3, 2
	e := executiontest.New(t, executiontest.Options{Limits: &lim})
	e.GrantAll()
	var defs = map[domain.Scope]pipeline.Definition{}
	var projects []executiontest.Project
	for _, s := range []domain.Scope{executiontest.ScopeA, executiontest.ScopeA2} {
		defs[s] = e.Define(s, pipelinetest.Doc(pipelinetest.Container("job", readRun)))
		for range 3 {
			p := e.NewProject(s)
			e.MustAdmit(p, defs[s], nil)
			projects = append(projects, p)
		}
	}
	tenant := executiontest.ScopeA.TenantID

	won := claimAll(t, e, tenant, 30)
	if len(won) != 3 {
		t.Fatalf("%d claims won, want exactly the tenant limit of 3", len(won))
	}
	perProject := map[domain.ProjectID]int{}
	for _, in := range won {
		perProject[in.Scope.ProjectID]++
	}
	for pid, n := range perProject {
		if n > 2 {
			t.Fatalf("project %s holds %d attempts over its limit of 2", pid, n)
		}
	}
	if held := reservationsHeld(e); held != 3 {
		t.Fatalf("%d reservations held, want 3", held)
	}
	for _, in := range won {
		if in.Ordinal != 1 {
			t.Fatalf("contention consumed an attempt ordinal: %d", in.Ordinal)
		}
	}

	// Releasing one reservation by settlement admits exactly one more claim, and
	// repeating the settlement releases nothing again.
	first := won[0]
	e.Start(first)
	if _, err := e.Succeed(first, nil, ""); err != nil {
		t.Fatal(err)
	}
	if held := reservationsHeld(e); held != 2 {
		t.Fatalf("%d held after one settlement", held)
	}
	ctx := context.Background()
	if err := e.Terminate(first, execution.LaunchStarted); err != nil { // duplicate settlement signal
		t.Fatal(err)
	}
	if _, err := e.Svc.ReconcileTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if held := reservationsHeld(e); held != 2 {
		t.Fatalf("a duplicate settlement released again: %d held", held)
	}
	if n := e.Count(`SELECT count(*) FROM execution_reservations WHERE released_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d released reservations, want 1", n)
	}
	more := claimAll(t, e, tenant, 20)
	if len(more) != 1 {
		t.Fatalf("%d claims after one release, want exactly 1", len(more))
	}
	if held := reservationsHeld(e); held != 3 {
		t.Fatalf("%d held after refilling, want 3", held)
	}
	_ = projects
}

// The project limit binds before the tenant limit when only one project has
// work, and limits that are missing deny dispatch.
func Test_quota_project_limit_and_missing_limits(t *testing.T) {
	lim := executiontest.DefaultLimit()
	lim.TenantConcurrency, lim.ProjectConcurrency = 10, 2
	e := executiontest.New(t, executiontest.Options{Limits: &lim})
	e.GrantAll()
	def := e.Define(executiontest.ScopeA, pipelinetest.Doc(pipelinetest.Container("job", readRun)))
	for range 4 {
		e.MustAdmit(e.NewProject(executiontest.ScopeA), def, nil)
	}
	if won := claimAll(t, e, executiontest.ScopeA.TenantID, 12); len(won) != 2 {
		t.Fatalf("%d claims won, want the project limit of 2", len(won))
	}
	e.Limits.Fail(execution.ErrNotConfigured)
	if _, err := e.Claim(executiontest.ScopeA.TenantID); err == nil || reservationsHeld(e) != 2 {
		t.Fatalf("missing limits did not deny dispatch: %v", err)
	}
}
