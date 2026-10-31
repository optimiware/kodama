package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

var (
	scope1 = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	scope2 = domain.Scope{TenantID: "t2", ProjectID: "p1"}
)

func must(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func isErr(t testing.TB, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: got %v, want %v", what, err, target)
	}
}

// builderPlan is build (declaring the output commit) -> review (a wait).
func builderPlan() runtime.Plan {
	spec := runtime.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour}
	return runtime.Plan{Digest: "sha256:builder", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Outputs: []string{"commit"},
			Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &spec},
	}}
}

func admission(t testing.TB, scope domain.Scope, id domain.RunID, plan runtime.Plan, at time.Time) runtime.Admission {
	t.Helper()
	run, err := runtime.NewRun(scope, id, "pipe1", plan, 2)
	must(t, err, "build the run")
	a := runtime.Admission{Run: run, Plan: plan}
	for _, p := range plan.Steps {
		if p.Kind == runtime.StepWait {
			w, err := runtime.NewWait(scope, runtime.WaitID(string(id)+"-review"), id, p.Key, *p.Wait)
			must(t, err, "build the wait")
			a.Waits = append(a.Waits, w)
		}
	}
	a.Events = []runtime.Event{{ID: "run.created:" + string(id), Scope: scope, RunID: id, Kind: runtime.EventRunCreated, Version: 1, At: at}}
	return a
}

// Test_unit_runstate_postgres_contract proves the PostgreSQL adapter passes the
// shared kernel state contract suite unchanged, and what the suite cannot show:
// that the state lives in the durable columns and is guarded there, that
// recovery needs nothing but the database, and that a failed commit leaves
// neither state nor outbox rows.
//
// The database subtests need PostgreSQL. In the unit layer (-tags unit) they are
// not registered (see withDatabase: a skipped subtest would void the test), and
// everywhere else with KODAMA_REQUIRE_POSTGRES=1 they fail when no database is
// configured, so a skipped run is never evidence. The real evidence is the run
// with the variable set and without the unit tag; see the task handoff.
func Test_unit_runstate_postgres_contract(t *testing.T) {
	t.Run("the adapter is the kernel port and the suite's subject", func(t *testing.T) {
		var _ runtime.StateStore = (*Store)(nil)
		var _ kernelcontracts.Subject = (*Store)(nil)
		if _, err := New(nil, kernelcontracts.NewClock()); err == nil {
			t.Error("a store needs a database")
		}
	})
	t.Run("times are normalized to the database's precision", testNormalize)
	t.Run("an event identity is a scoped digest, not the event's own id", testEventRowID)
	t.Run("a wait maps to the columns of its lifecycle", testWaitColumns)
	t.Run("database refusals map to the kernel's vocabulary", testMapErr)

	withDatabase(t, func(t *testing.T) {
		e := newEnv(t)
		t.Run("shared suite", func(t *testing.T) { kernelcontracts.Run(t, e.factory) })
		t.Run("shared suite races repeat", func(t *testing.T) {
			for _, c := range kernelcontracts.Cases() {
				if c.Name != "claim/race_has_one_winner" && c.Name != "wait/resolution_cancellation_race_has_one_winner" &&
					c.Name != "wait/concurrent_duplicates_decide_once" {
					continue
				}
				for i := range 10 {
					t.Run(fmt.Sprintf("%s/%d", c.Name, i), func(t *testing.T) { kernelcontracts.Check(t, e.factory, c) })
				}
			}
		})
		t.Run("the durable columns are the state", func(t *testing.T) { testDurableColumns(t, e) })
		t.Run("a replacement store recovers from the database alone", func(t *testing.T) { testRecovery(t, e) })
		t.Run("the durable guards refuse what the rules would", func(t *testing.T) { testGuards(t, e) })
		t.Run("a refusal that lost to a cancellation replays its reason", func(t *testing.T) { testCancellationReplay(t, e) })
		t.Run("a failed commit leaves neither state nor outbox", func(t *testing.T) { testAtomicOutbox(t, e) })
		t.Run("admission refuses a pipeline that is not in the scope", func(t *testing.T) { testUnknownPipeline(t, e) })
		t.Run("an event identity is reused only with the same content", func(t *testing.T) { testEventReuse(t, e) })
	})
}

func testDurableColumns(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	_, err := s.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	must(t, err, "admit")
	rec, err := s.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "w1", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
	must(t, err, "claim")

	var owner, purpose, wait, step string
	var fence int64
	var expires time.Time
	var version int64
	must(t, e.owner.QueryRow(ctx, `SELECT lease_owner, lease_purpose, lease_fence, lease_expires_at, version FROM runs WHERE tenant_id = 't1' AND project_id = 'p1' AND id = 'run1'`).
		Scan(&owner, &purpose, &fence, &expires, &version), "read the lease")
	if owner != "w1" || purpose != "execute" || fence != 1 || !expires.Equal(rec.Claim.Expires) || version != 1 {
		t.Errorf("lease columns = %s %s %d %s v%d, claim %+v", owner, purpose, fence, expires, version, rec.Claim)
	}
	// The plan is the run's definition snapshot; the wait is the review step's row, recorded before it opens.
	var digest string
	must(t, e.owner.QueryRow(ctx, `SELECT definition_snapshot->>'digest' FROM runs WHERE tenant_id = 't1' AND id = 'run1'`).Scan(&digest), "read the plan")
	if digest != "sha256:builder" {
		t.Errorf("plan digest column = %q", digest)
	}
	must(t, e.owner.QueryRow(ctx, `SELECT wait_id, status, step_kind FROM run_steps WHERE tenant_id = 't1' AND run_id = 'run1' AND step_key = 'review'`).
		Scan(&wait, &step, new(string)), "read the wait")
	if wait != "run1-review" || step != "pending" {
		t.Errorf("wait columns = %s %s", wait, step)
	}

	// A step move, its outputs and its settlement are columns of the step row.
	r := rec.Run.Clone()
	must(t, r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone), "start")
	sealed, err := r.Sealed(rec.Run)
	must(t, err, "seal")
	rec, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "run1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: 1, Run: sealed,
		Events: []runtime.Event{{ID: "step.started:run1", Scope: scope1, RunID: "run1", StepKey: "build", Kind: runtime.EventStepStateChanged, Version: 2, At: clock.Now()}}})
	must(t, err, "commit the start")
	r = rec.Run.Clone()
	must(t, r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded}), "finish")
	sealed, err = r.Sealed(rec.Run)
	must(t, err, "seal")
	rec, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "run1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: 2, Run: sealed,
		Outputs: []runtime.StepOutputs{{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": "c1"}}}})
	must(t, err, "commit the settlement")
	var status, outputs, resultCode string
	var attempt int
	must(t, e.owner.QueryRow(ctx, `SELECT status, attempt, result_code, outputs->>'commit' FROM run_steps WHERE tenant_id = 't1' AND run_id = 'run1' AND step_key = 'build'`).
		Scan(&status, &attempt, &resultCode, &outputs), "read the step")
	if status != "succeeded" || attempt != 1 || resultCode != "succeeded" || outputs != "c1" {
		t.Errorf("step columns = %s %d %s %q", status, attempt, resultCode, outputs)
	}
	if v, ok := rec.Output("build", "commit"); !ok || v != "c1" {
		t.Errorf("the record's output = %q %v", v, ok)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id = 't1' AND resource_id = 'run1'`); n != 2 {
		t.Errorf("outbox rows = %d, want the admission event and the step event", n)
	}
}

func testRecovery(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	first := e.store(clock)
	ctx := context.Background()
	_, err := first.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	must(t, err, "admit")
	claimed, err := first.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "engine-a", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
	must(t, err, "claim")

	// The process dies. A replacement store on a new pool sees the same record and the lease.
	replacement := e.store(clock)
	got, err := replacement.Load(ctx, scope1, "run1")
	must(t, err, "load through the replacement")
	if got.Claim != claimed.Claim || got.Run.Version != claimed.Run.Version {
		t.Fatalf("the replacement sees %+v, the first %+v", got.Claim, claimed.Claim)
	}
	if _, err := replacement.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "engine-b", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now()); !errors.Is(err, runtime.ErrClaimHeld) {
		t.Fatalf("the lease survives the restart: %v", err)
	}
	clock.Advance(time.Minute)
	took, err := replacement.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "engine-b", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
	must(t, err, "takeover after the lease expired")
	if took.Claim.Fence != 2 {
		t.Fatalf("takeover fence = %d", took.Claim.Fence)
	}
	// The first process, still alive somewhere, is fenced out.
	r := claimed.Run.Clone()
	must(t, r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone), "start")
	sealed, err := r.Sealed(claimed.Run)
	must(t, err, "seal")
	_, err = first.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "run1", Claim: claimed.Claim, Now: clock.Now(), ExpectedVersion: 1, Run: sealed})
	isErr(t, err, runtime.ErrStaleFence, "the superseded engine's commit")
}

// asRuntime runs fn as the non-owner runtime role in a tenant transaction.
func (e *env) asRuntime(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.rt, tenant, fn)
}

func testGuards(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	_, err := s.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	must(t, err, "admit")
	rec, err := s.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "w1", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
	must(t, err, "claim")
	r := rec.Run.Clone()
	must(t, r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone), "start")
	sealed, _ := r.Sealed(rec.Run)
	rec, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "run1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: 1, Run: sealed})
	must(t, err, "start")
	r = rec.Run.Clone()
	must(t, r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded}), "finish")
	sealed, _ = r.Sealed(rec.Run)
	_, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "run1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: 2, Run: sealed,
		Outputs: []runtime.StepOutputs{{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": "c1"}}}})
	must(t, err, "settle")

	// What the Go rules refuse, the database refuses too, to a writer that skips them.
	for _, c := range []struct{ what, sql string }{
		{"a lease fence that goes backwards", `UPDATE runs SET lease_fence = 0 WHERE tenant_id = 't1' AND id = 'run1'`},
		{"a settled step reopened", `UPDATE run_steps SET status = 'running' WHERE tenant_id = 't1' AND run_id = 'run1' AND step_key = 'build'`},
		{"recorded outputs rewritten", `UPDATE run_steps SET outputs = '{"commit":"evil"}'::jsonb WHERE tenant_id = 't1' AND run_id = 'run1' AND step_key = 'build'`},
		{"a recorded wait's identity changed", `UPDATE run_steps SET wait_id = 'other' WHERE tenant_id = 't1' AND run_id = 'run1' AND step_key = 'review'`},
		{"a signal record rewritten", `UPDATE signal_records SET applied = false`},
		{"an outbox event deleted", `DELETE FROM audit_events`},
	} {
		err := e.asRuntime("t1", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, c.sql)
			return err
		})
		if err == nil {
			t.Errorf("%s: the runtime role was allowed it", c.what)
		}
	}
	got, err := s.Load(ctx, scope1, "run1")
	must(t, err, "load")
	if v, _ := got.Output("build", "commit"); v != "c1" || got.Run.Steps[0].State != runtime.StepSucceeded || got.Claim.Fence != 1 {
		t.Errorf("a refused write changed the record: %+v", got)
	}
}

func testAtomicOutbox(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	boom := errors.New("injected commit failure")
	s.FailNextCommit(boom)
	_, err := s.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	isErr(t, err, boom, "an admission that fails at its commit point")
	if n := e.count(`SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM run_steps) + (SELECT count(*) FROM audit_events)`); n != 0 {
		t.Fatalf("the failed admission left %d rows", n)
	}
	rec, err := s.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	must(t, err, "retry the admission")
	if n := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id = 't1'`); n != 1 {
		t.Fatalf("the retry wrote %d outbox rows", n)
	}
	// A repeat of the admission writes nothing more.
	again, err := s.Admit(ctx, admission(t, scope1, "run1", builderPlan(), clock.Now()))
	must(t, err, "repeat the admission")
	if again.Run.Version != rec.Run.Version || e.count(`SELECT count(*) FROM audit_events`) != 1 {
		t.Fatalf("a repeat changed the store")
	}
}

func testUnknownPipeline(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	a := admission(t, scope1, "run1", builderPlan(), clock.Now())
	a.Run.PipelineID = "nope"
	_, err := s.Admit(context.Background(), a)
	isErr(t, err, domain.ErrInvalidValue, "a pipeline that does not exist in the scope")
	if n := e.count(`SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM run_steps) + (SELECT count(*) FROM audit_events)`); n != 0 {
		t.Fatalf("the refused admission left %d rows", n)
	}
}

func testEventReuse(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	a := admission(t, scope1, "run1", builderPlan(), clock.Now())
	_, err := s.Admit(ctx, a)
	must(t, err, "admit")
	rec, err := s.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: "run1", Owner: "w1", Purpose: runtime.ClaimExecute, TTL: time.Minute}, clock.Now())
	must(t, err, "claim")
	bump := func(ev runtime.Event) runtime.Transition {
		r := rec.Run.Clone()
		r.Version++
		return runtime.Transition{Scope: scope1, RunID: "run1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: r, Events: []runtime.Event{ev}}
	}
	// The run.created event of the admission, reused with other content, is refused and rolls back.
	other := a.Events[0]
	other.Version = 7
	_, err = s.Commit(ctx, bump(other))
	isErr(t, err, domain.ErrConflict, "an event id reused with other content")
	got, err := s.Load(ctx, scope1, "run1")
	must(t, err, "load")
	if got.Run.Version != rec.Run.Version {
		t.Fatalf("the refused commit advanced the run to %d", got.Run.Version)
	}
	// The same event, byte for byte, is recorded once.
	_, err = s.Commit(ctx, bump(a.Events[0]))
	must(t, err, "the same event again")
	if n := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id = 't1'`); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}

// testCancellationReplay covers what signal_records cannot hold. A refusal that
// lost to a cancellation replays with the cancellation's reason; the record keeps
// none (it has no column for it), so the reason is read back from the wait the
// signal targeted. When that wait is gone the replay fails loudly instead of
// guessing a reason. A schema column would make the record self-contained.
func testCancellationReplay(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	for i, reason := range []runtime.Reason{runtime.ReasonRunCancelled, runtime.ReasonRunFailed} {
		id := domain.RunID(fmt.Sprintf("run%d", i+1))
		_, err := s.Admit(ctx, admission(t, scope1, id, builderPlan(), clock.Now()))
		must(t, err, "admit")
		rec, err := s.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: id, Owner: "w1", Purpose: runtime.ClaimExecute, TTL: 48 * time.Hour}, clock.Now())
		must(t, err, "claim")
		steps := func(mutate func(r *runtime.Run) error, fill func(*runtime.Transition)) {
			r := rec.Run.Clone()
			must(t, mutate(&r), "move")
			sealed, err := r.Sealed(rec.Run)
			must(t, err, "seal")
			tr := runtime.Transition{Scope: scope1, RunID: id, Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: sealed}
			fill(&tr)
			rec, err = s.Commit(ctx, tr)
			must(t, err, "commit")
		}
		steps(func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) }, func(*runtime.Transition) {})
		steps(func(r *runtime.Run) error {
			return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
		}, func(*runtime.Transition) {})
		w := rec.Waits[0]
		must(t, w.Open(clock.Now(), map[string]string{"candidate": "c1"}, ""), "open")
		steps(func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) },
			func(tr *runtime.Transition) { tr.Waits = []runtime.Wait{w} })
		if reason == runtime.ReasonRunCancelled {
			_, err = s.RequestCancel(ctx, scope1, id)
			must(t, err, "cancel")
		} else {
			cancelled := w
			_, err = cancelled.Cancel(clock.Now(), runtime.ReasonRunFailed)
			must(t, err, "cancel the wait for a failed run")
			r := rec.Run.Clone()
			r.Version++
			_, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: id, Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: r,
				Waits: []runtime.Wait{cancelled}})
			must(t, err, "commit the cancelled wait")
		}
		req := runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "alice", Key: "k-" + string(id)}, WaitID: w.ID,
			Outcome: "approved", Actor: "user:alice"}
		res, err := s.DecideSignal(ctx, req, clock.Now())
		must(t, err, "decide")
		if res.Applied || res.Refusal != runtime.RefusalDecided || res.Decision.Kind != runtime.DecisionCancelled || res.Decision.Reason != reason {
			t.Fatalf("%s: the refusal = %+v", reason, res)
		}
		again, err := s.DecideSignal(ctx, req, clock.Now())
		must(t, err, "replay")
		look, err := s.LookupSignal(ctx, req.Identity)
		must(t, err, "lookup")
		if !again.Replayed || again.Decision != res.Decision || look.Record == nil || look.Record.Decision != res.Decision {
			t.Fatalf("%s: the replay lost the decision: %+v / %+v", reason, again, look)
		}
		if err := look.Validate(); err != nil {
			t.Fatalf("%s: the replayed record is not valid: %v", reason, err)
		}
		// The reason is not in the record: with the wait gone the replay fails, it never guesses.
		e.ownerExec(`DELETE FROM run_steps WHERE tenant_id = 't1' AND run_id = $1 AND step_key = 'review'`, string(id))
		if _, err := s.LookupSignal(ctx, req.Identity); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("%s: a reason that cannot be recovered must be an error: %v", reason, err)
		}
	}
}
