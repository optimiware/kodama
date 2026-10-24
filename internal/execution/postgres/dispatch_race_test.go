package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_dispatch_race: PX-06, PX-14, PX-05, PX-11. Concurrent dispatch of one
// runnable step creates exactly one durable attempt, one capacity reservation,
// one writer reservation and one atomic audit/outbox record; dependencies and
// the exact source binding are honored.
func Test_dispatch_race(t *testing.T) {
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(
		pipelinetest.Container("build", writeRun),
		pipelinetest.Container("verify", "needs: [build]", readRun),
		pipelinetest.Container("lint", readRun),
	))
	run := e.MustAdmit(p, def, nil)

	// build (a writer) and lint are both runnable; verify waits for build.
	won := claimAll(t, e, p.Scope.TenantID, 24)
	if len(won) != 2 {
		t.Fatalf("%d claims won, want the 2 runnable steps (build, lint)", len(won))
	}
	steps := map[string]*execution.AttemptSpec{}
	for _, in := range won {
		steps[in.StepID] = in
	}
	if steps["build"] == nil || steps["lint"] == nil || steps["verify"] != nil {
		t.Fatalf("claimed steps %v: dependencies were not honored", steps)
	}
	if n := attemptCount(e, run.ID); n != 2 {
		t.Fatalf("%d attempts, want 2", n)
	}
	if held := reservationsHeld(e); held != 2 {
		t.Fatalf("%d capacity reservations, want 2", held)
	}
	// One writer reservation, owned by the build attempt, with the exact input
	// commit and the expected tip observed outside the transaction.
	var writer, input, tip string
	e.Scalar(&writer, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
	e.Scalar(&input, `SELECT writer_input_commit FROM workspaces WHERE id = $1`, string(p.Workspace))
	e.Scalar(&tip, `SELECT writer_expected_tip FROM workspaces WHERE id = $1`, string(p.Workspace))
	if writer != string(steps["build"].AttemptID) || input != executiontest.CommitA || tip != executiontest.CommitA {
		t.Fatalf("writer reservation %s/%s/%s", writer, input, tip)
	}
	// Source binding: the reader is pinned to the original run commit.
	var readerInput string
	var readerTip *string
	e.Scalar(&readerInput, `SELECT input_commit FROM step_attempts WHERE id = $1`, string(steps["lint"].AttemptID))
	e.Scalar(&readerTip, `SELECT expected_tip FROM step_attempts WHERE id = $1`, string(steps["lint"].AttemptID))
	if readerInput != executiontest.CommitA || readerTip != nil {
		t.Fatalf("reader source %s tip %v", readerInput, readerTip)
	}
	// Atomic audit/outbox: one attempt event and audit row per attempt.
	if n := e.CountTenant(p.Scope.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'step_attempt.state_changed'`); n != 2 {
		t.Fatalf("%d attempt events", n)
	}
	if n := e.Count(`SELECT count(*) FROM audit WHERE action = 'step_attempt.state_changed'`); n != 2 {
		t.Fatalf("%d attempt audit rows", n)
	}
	// Ordinals are contiguous: contention consumed none.
	if n := e.Count(`SELECT count(*) FROM step_attempts WHERE ordinal <> 1`); n != 0 {
		t.Fatalf("%d attempts with a skipped ordinal", n)
	}

	// The dependent step becomes runnable only after build succeeded; a second
	// writer cannot be admitted while build holds the slot.
	if _, err := e.Claim(p.Scope.TenantID); err == nil {
		t.Fatal("a third step was claimed while build was unfinished")
	}
	e.Start(steps["build"])
	if _, err := e.Succeed(steps["build"], nil, executiontest.CommitA); err != nil {
		t.Fatal(err)
	}
	next := e.MustClaim(p.Scope.TenantID)
	if next.StepID != "verify" {
		t.Fatalf("claimed %s after build", next.StepID)
	}
	if n := e.Count(`SELECT count(*) FROM workspaces WHERE id = $1 AND writer_attempt_id IS NOT NULL`, string(p.Workspace)); n != 0 {
		t.Fatal("the writer slot was not released by settlement")
	}
}

// A stale writer tip observed outside the transaction is rechecked under the
// workspace lock and refused without consuming an ordinal or a reservation.
func Test_dispatch_stale_writer_tip(t *testing.T) {
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("build", writeRun)))
	run := e.MustAdmit(p, def, nil)
	e.Sources.Tip = executiontest.CommitC // the refresh saw a tip the workspace has not accepted
	if _, err := e.Claim(p.Scope.TenantID); err == nil {
		t.Fatal("a claim with stale writer evidence succeeded")
	}
	if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
		t.Fatal("a refused claim left an attempt or reservation")
	}
	e.Sources.Tip = executiontest.CommitA
	if in := e.MustClaim(p.Scope.TenantID); in.Ordinal != 1 {
		t.Fatalf("ordinal %d", in.Ordinal)
	}
}
