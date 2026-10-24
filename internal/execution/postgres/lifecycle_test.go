package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_run_lifecycle is the end-to-end happy path through the real PostgreSQL
// store: admission pins a run, dispatch persists an attempt as intent, the
// observed start and a validated result complete it, settlement releases the
// capacity, and the run finishes with its history, audit and outbox recorded.
func Test_run_lifecycle(t *testing.T) {
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(
		pipelinetest.Container("build", readRun),
		pipelinetest.Container("test", "needs: [build]", readRun),
	))
	run := e.MustAdmit(p, def, nil)
	if run.Status != execution.RunQueued || run.SourceRetention != execution.RetentionConfirmed {
		t.Fatalf("run %s retention %s", run.Status, run.SourceRetention)
	}
	in := e.MustClaim(p.Scope.TenantID)
	if in.StepID != "build" || in.Ordinal != 1 {
		t.Fatalf("intent %+v", in)
	}
	agg := e.Get(run)
	at := agg.Attempt(in.AttemptID)
	if at.Status != execution.AttemptRunning || at.Reason != execution.ReasonStarting {
		t.Fatalf("attempt %s/%s", at.Status, at.Reason)
	}
	e.Start(in)
	if _, err := e.Succeed(in, nil, ""); err != nil {
		t.Fatal(err)
	}
	in2 := e.MustClaim(p.Scope.TenantID)
	if in2.StepID != "test" {
		t.Fatalf("second claim %+v", in2)
	}
	e.Start(in2)
	if _, err := e.Succeed(in2, nil, ""); err != nil {
		t.Fatal(err)
	}
	final := e.Get(run)
	if final.Run.Status != execution.RunSucceeded || final.Run.FinishedAt == nil {
		t.Fatalf("run %s", final.Run.Status)
	}
	if held := e.Count(`SELECT count(*) FROM execution_reservations WHERE released_at IS NULL`); held != 0 {
		t.Fatalf("%d reservations still held", held)
	}
	if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1`, string(run.ID)); n < 8 {
		t.Fatalf("%d history rows", n)
	}
	_ = context.Background()
}
