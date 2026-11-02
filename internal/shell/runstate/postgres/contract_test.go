package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	t.Run("a wait timeout that is not a whole microsecond survives normalization", testNormalizeKeepsWaitTimeout)
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
		t.Run("a refusal that lost to a cancellation replays on its own record", func(t *testing.T) { testCancellationReplay(t, e) })
		t.Run("a legacy cancelled record fails closed", func(t *testing.T) { testLegacyCancelledRecord(t, e) })
		t.Run("a wait timeout that is not a whole microsecond stays decidable", func(t *testing.T) { testSubMicrosecondTimeout(t, e) })
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

// testCancellationReplay covers a refusal that lost to a cancellation, for both
// reasons. The record carries the reason, so the lookup and the replay are
// unchanged after the run and all its steps are removed (as retention may), a changed
// request under the same key is key reuse, and nothing is rerouted.
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
		var stored string
		must(t, e.owner.QueryRow(ctx, `SELECT decision_reason FROM signal_records WHERE tenant_id = 't1' AND caller_key = $1`, req.Identity.Key).Scan(&stored), "read the column")
		if stored != string(reason) {
			t.Fatalf("%s: decision_reason = %q", reason, stored)
		}
		before, err := s.LookupSignal(ctx, req.Identity)
		must(t, err, "lookup")

		// Retention removes the run with every step (the wait lives on its step row),
		// and whatever hangs off it: the record is all a replay needs.
		for _, table := range []string{"run_commands", "run_workload_observations", "run_resource_observations", "run_materializations"} {
			e.ownerExec(`DELETE FROM `+table+` WHERE tenant_id = 't1' AND project_id = 'p1' AND run_id = $1`, string(id))
		}
		e.ownerExec(`DELETE FROM run_steps WHERE tenant_id = 't1' AND project_id = 'p1' AND run_id = $1`, string(id))
		e.ownerExec(`DELETE FROM runs WHERE tenant_id = 't1' AND project_id = 'p1' AND id = $1`, string(id))
		if n := e.count(`SELECT (SELECT count(*) FROM runs WHERE id = $1) + (SELECT count(*) FROM run_steps WHERE run_id = $1)`, string(id)); n != 0 {
			t.Fatalf("%s: %d run or step rows remain", reason, n)
		}
		if _, err := s.Load(ctx, scope1, id); !hasCode(err, domain.CodeNotFound) {
			t.Fatalf("%s: the removed run is still there: %v", reason, err)
		}
		after, err := s.LookupSignal(ctx, req.Identity)
		must(t, err, "lookup after the run is gone")
		if before.Record == nil || after.Record == nil || *after.Record != *before.Record || after.Record.Decision.Reason != reason {
			t.Fatalf("%s: the lookup changed: %+v then %+v", reason, before.Record, after.Record)
		}
		if err := after.Validate(); err != nil {
			t.Fatalf("%s: the record is not valid: %v", reason, err)
		}
		n := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id = 't1'`)
		again, err := s.DecideSignal(ctx, req, clock.Now())
		must(t, err, "replay after the run is gone")
		if !again.Replayed || again.Applied || again.Refusal != runtime.RefusalDecided || again.Decision != res.Decision || again.Target != res.Target {
			t.Fatalf("%s: the replay = %+v, want %+v", reason, again, res)
		}
		changed := req
		changed.Outcome = "rejected"
		if _, err := s.DecideSignal(ctx, changed, clock.Now()); !errors.Is(err, runtime.ErrKeyReuse) {
			t.Fatalf("%s: changed content under the same key: %v", reason, err)
		}
		changed = req
		changed.Answer = "other"
		if _, err := s.DecideSignal(ctx, changed, clock.Now()); !errors.Is(err, runtime.ErrKeyReuse) {
			t.Fatalf("%s: a changed answer under the same key: %v", reason, err)
		}
		if got := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id = 't1'`); got != n {
			t.Fatalf("%s: replay and key reuse wrote %d outbox rows", reason, got-n)
		}
		if got := e.count(`SELECT count(*) FROM signal_records WHERE tenant_id = 't1' AND caller_key = $1`, req.Identity.Key); got != 1 {
			t.Fatalf("%s: %d records under the key", reason, got)
		}
	}
}

// testLegacyCancelledRecord covers a record written before migration 007: a
// cancelled decision with a null reason. It fails closed: the lookup and the
// replay report that the reason was not recorded, nothing is fabricated, and the
// signal is not routed again (the wait, still open for another identity, is not
// touched and no record or event is added).
func testLegacyCancelledRecord(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	w := waitingRun(t, s, clock, scope1, "run1", "c1")
	// Written as migration 006 allowed it: the constraint added by 007 is NOT VALID
	// for rows that already exist, which the owner reproduces by dropping it for the insert.
	e.ownerExec(`ALTER TABLE signal_records DROP CONSTRAINT signal_records_decision_reason_shape`)
	t.Cleanup(func() {
		e.reset()
		e.ownerExec(`ALTER TABLE signal_records ADD CONSTRAINT signal_records_decision_reason_shape CHECK (
			(decision_kind IS NOT DISTINCT FROM 'cancelled') = (decision_reason IS NOT NULL)) NOT VALID`)
	})
	req := runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "alice", Key: "legacy"}, WaitID: w.ID, Outcome: "approved", Actor: "user:alice"}
	e.ownerExec(`INSERT INTO signal_records (tenant_id, project_id, caller_principal, caller_key, signal_id, shape, request_digest, applied,
		refusal_reason, target_run_id, target_step_id, target_wait_id, decision_kind, decision_version, decided_at)
		VALUES ('t1', 'p1', 'alice', 'legacy', $1, 'decision', $2, false, 'decided', 'run1', 'review', $3, 'cancelled', 3, $4)`,
		string(req.Identity.ID()), req.Digest()[len("sha256:"):], string(w.ID), clock.Now())
	events := e.count(`SELECT count(*) FROM audit_events`)

	if _, err := s.LookupSignal(ctx, req.Identity); !errors.Is(err, domain.ErrInvalidValue) || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("lookup of a legacy cancelled record: %v", err)
	}
	if _, err := s.DecideSignal(ctx, req, clock.Now()); !errors.Is(err, domain.ErrInvalidValue) {
		t.Fatalf("replay of a legacy cancelled record: %v", err)
	}
	rec, err := s.Load(ctx, scope1, "run1")
	must(t, err, "load")
	if rec.Waits[0].State != runtime.WaitOpen || e.count(`SELECT count(*) FROM audit_events`) != events ||
		e.count(`SELECT count(*) FROM signal_records`) != 1 {
		t.Fatalf("the refused replay changed the store: %+v", rec.Waits[0])
	}
	// A column value of a resolved or tombstone record is unaffected: only cancelled decisions need a reason.
	res, err := s.DecideSignal(ctx, runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "bob", Key: "fresh"}, WaitID: w.ID,
		Outcome: "approved", Actor: "user:bob"}, clock.Now())
	must(t, err, "another identity decides the wait")
	if !res.Applied {
		t.Fatalf("another identity: %+v", res)
	}
}

// testSubMicrosecondTimeout is the regression for a valid wait timeout that is
// not a whole number of microseconds (here a minute and a nanosecond). The wait
// is admitted and opened on a microsecond boundary, so the stored deadline is
// the truncated open time plus the timeout; the record is reloaded, which derives
// the open time from the deadline; and the engine's next commit, which carries
// the wait it read back, must still be a legal decision: the immutable open time
// has not moved, whichever way the wait is decided.
func testSubMicrosecondTimeout(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	timeout := time.Minute + time.Nanosecond
	plan := builderPlan()
	spec := *plan.Steps[1].Wait
	spec.Timeout = timeout
	plan.Steps[1].Wait = &spec

	open := func(id domain.RunID) (runtime.RunRecord, runtime.Wait) {
		a := admission(t, scope1, id, plan, clock.Now())
		if a.Waits[0].Spec.Timeout != timeout {
			t.Fatalf("the admitted wait's timeout is %s", a.Waits[0].Spec.Timeout)
		}
		rec, err := s.Admit(ctx, a)
		must(t, err, "admit")
		if got := rec.Waits[0].Spec.Timeout; got != timeout {
			t.Fatalf("the stored wait's timeout is %s", got)
		}
		rec, err = s.Claim(ctx, runtime.ClaimRequest{Scope: scope1, RunID: id, Owner: "engine", Purpose: runtime.ClaimExecute, TTL: 48 * time.Hour}, clock.Now())
		must(t, err, "claim")
		commit := func(mutate func(r *runtime.Run) error, waits ...runtime.Wait) {
			t.Helper()
			r := rec.Run.Clone()
			must(t, mutate(&r), "move")
			sealed, err := r.Sealed(rec.Run)
			must(t, err, "seal")
			rec, err = s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: id, Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: sealed, Waits: waits})
			must(t, err, "commit")
		}
		commit(func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
		commit(func(r *runtime.Run) error {
			return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
		})
		w := rec.Waits[0]
		must(t, w.Open(clock.Now(), map[string]string{"candidate": string(id)}, ""), "open the wait")
		commit(func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) }, w)

		// Reloaded, the pair is canonical: valid, the timeout untouched, the deadline at database precision.
		got, err := s.Load(ctx, scope1, id)
		must(t, err, "reload")
		lw := got.Waits[0]
		must(t, lw.Validate(), "the reloaded wait is valid")
		if lw.Spec.Timeout != timeout || lw.Deadline.Sub(lw.OpenedAt) != timeout || lw.Deadline.Nanosecond()%1000 != 0 {
			t.Fatalf("the reloaded wait: opened %s deadline %s timeout %s", lw.OpenedAt, lw.Deadline, lw.Spec.Timeout)
		}
		if !lw.Deadline.Equal(clock.Now().Add(timeout).Truncate(time.Microsecond)) {
			t.Fatalf("the deadline is %s, want the truncated open time plus the timeout", lw.Deadline)
		}
		// A commit that merely carries the wait back is not a change.
		commit(func(r *runtime.Run) error { r.Version++; return nil }, lw)
		again, err := s.Load(ctx, scope1, id)
		must(t, err, "reload again")
		if !again.Waits[0].OpenedAt.Equal(lw.OpenedAt) || !again.Waits[0].Deadline.Equal(lw.Deadline) {
			t.Fatalf("carrying the wait back moved it: %+v then %+v", lw, again.Waits[0])
		}
		return again, again.Waits[0]
	}

	t.Run("the engine records the expiry", func(t *testing.T) {
		rec, w := open("expire1")
		clock.Set(w.Deadline)
		expired := w
		_, err := expired.Expire(clock.Now())
		must(t, err, "expire")
		r := rec.Run.Clone()
		must(t, r.AdvanceStep("review", runtime.StepFailed, runtime.ReasonWaitExpired), "fail the step")
		sealed, err := r.Sealed(rec.Run)
		must(t, err, "seal")
		got, err := s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "expire1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version,
			Run: sealed, Waits: []runtime.Wait{expired}})
		must(t, err, "an otherwise legal expiry")
		if g := got.Waits[0]; g.State != runtime.WaitExpired || !g.OpenedAt.Equal(w.OpenedAt) || !g.Deadline.Equal(w.Deadline) {
			t.Fatalf("the expired wait: %+v", g)
		}
		clock.Set(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	})
	t.Run("the engine records a cancellation", func(t *testing.T) {
		rec, w := open("cancel1")
		cancelled := w
		_, err := cancelled.Cancel(clock.Now(), runtime.ReasonRunFailed)
		must(t, err, "cancel")
		r := rec.Run.Clone()
		r.Version++
		got, err := s.Commit(ctx, runtime.Transition{Scope: scope1, RunID: "cancel1", Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version,
			Run: r, Waits: []runtime.Wait{cancelled}})
		must(t, err, "an otherwise legal cancellation")
		if g := got.Waits[0]; g.State != runtime.WaitCancelled || !g.OpenedAt.Equal(w.OpenedAt) || !g.Deadline.Equal(w.Deadline) {
			t.Fatalf("the cancelled wait: %+v", g)
		}
	})
	t.Run("a signal and a cancellation request decide it", func(t *testing.T) {
		_, w := open("signal1")
		res, err := s.DecideSignal(ctx, runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "alice", Key: "k1"},
			WaitID: w.ID, Outcome: "approved", Actor: "user:alice"}, clock.Now())
		must(t, err, "decide")
		if !res.Applied {
			t.Fatalf("the signal was refused: %+v", res)
		}
		_, w2 := open("cancel2")
		_, err = s.RequestCancel(ctx, scope1, "cancel2")
		must(t, err, "cancel")
		got, err := s.Load(ctx, scope1, "cancel2")
		must(t, err, "load")
		if g := got.Waits[0]; g.State != runtime.WaitCancelled || !g.Deadline.Equal(w2.Deadline) || !g.OpenedAt.Equal(w2.OpenedAt) {
			t.Fatalf("the cancelled wait: %+v", g)
		}
	})
}
