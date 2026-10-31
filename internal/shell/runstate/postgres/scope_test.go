package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

// kernelTables are the tables a run record lives in, with the column that is
// the tenant. audit_events holds the outbox.
var kernelTables = []string{
	"runs", "run_steps", "run_commands", "run_workload_observations", "run_resource_observations",
	"run_materializations", "signal_records", "audit_events",
}

// populate writes a run through the adapter that has a row in every kernel
// table: a claimed run, a launched build with its command, observation,
// service, material and outputs, an opened wait and a signal that decided it.
func populate(t *testing.T, s *Store, clock *kernelcontracts.Clock, scope domain.Scope) {
	t.Helper()
	ctx := context.Background()
	id := domain.RunID("run1")
	a := admission(t, scope, id, builderPlan(), clock.Now())
	_, err := s.Admit(ctx, a)
	must(t, err, "admit")
	rec, err := s.Claim(ctx, runtime.ClaimRequest{Scope: scope, RunID: id, Owner: "engine", Purpose: runtime.ClaimExecute, TTL: 48 * time.Hour}, clock.Now())
	must(t, err, "claim")
	commit := func(mutate func(r *runtime.Run) error, fill func(tr *runtime.Transition)) {
		t.Helper()
		r := rec.Run.Clone()
		must(t, mutate(&r), "move")
		sealed, err := r.Sealed(rec.Run)
		must(t, err, "seal")
		tr := runtime.Transition{Scope: scope, RunID: id, Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: sealed}
		fill(&tr)
		rec, err = s.Commit(ctx, tr)
		must(t, err, "commit")
	}

	binding := runtime.RuntimeBinding{Scope: scope, RunID: id, StepKey: "build", Attempt: 1, WorkloadID: "wl-run1-build",
		SpecDigest: "sha256:spec", Backend: "process", Fence: rec.Claim.Fence}
	launch, err := runtime.NewWorkloadCommand("cmd-launch", binding, runtime.CommandLaunch, clock.Now())
	must(t, err, "command")
	ref, err := runtime.NewMaterializationRef(scope, id, "build", 1)
	must(t, err, "material")
	service := runtime.ResourceHandle{Scope: scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, scope), Lifetime: runtime.LifetimePipeline,
		RunID: id, Kind: runtime.ResourceService, Name: "db", Identity: "run1-db", Fence: rec.Claim.Fence}
	commit(func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) }, func(tr *runtime.Transition) {
		tr.Commands = []runtime.Command{launch}
		tr.Workloads = []runtime.WorkloadObservation{{Binding: binding, Phase: runtime.PhaseRunning, ObservedAt: clock.Now()}}
		tr.Resources = []runtime.ResourceObservation{{Handle: service, State: runtime.ResourceReady, ObservedAt: clock.Now()}}
		tr.Materializations = []runtime.MaterializationRecord{{Ref: ref, State: runtime.MaterializationIssued, IssuedAt: clock.Now()}}
		tr.Events = []runtime.Event{{ID: "build.started:run1", Scope: scope, RunID: id, StepKey: "build", Kind: runtime.EventStepStateChanged, Version: 2, At: clock.Now()}}
	})
	commit(func(r *runtime.Run) error {
		return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	}, func(tr *runtime.Transition) {
		tr.Outputs = []runtime.StepOutputs{{StepKey: "build", Attempt: 1, Values: map[string]string{"commit": "c1"}}}
	})
	w := rec.Waits[0]
	must(t, w.Open(clock.Now(), map[string]string{"candidate": "c1"}, "please"), "open the wait")
	commit(func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) }, func(tr *runtime.Transition) {
		tr.Waits = []runtime.Wait{w}
	})
	res, err := s.DecideSignal(ctx, runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope, Caller: "alice", Key: "k1"},
		WaitID: w.ID, Outcome: "approved", Actor: "user:alice"}, clock.Now())
	must(t, err, "decide")
	if !res.Applied {
		t.Fatalf("the signal was not applied: %+v", res)
	}
}

// Test_unit_runstate_postgres_scope proves tenant and project authority of the
// PostgreSQL adapter: every statement is tenant scoped (statically, in the unit
// layer), and in the real database the adapter runs as a non-owner role under
// forced row level security, so a foreign scope is not found and a foreign
// tenant's rows cannot be read or written even by SQL that skips the adapter.
//
// The database subtests need PostgreSQL: they are not registered in the unit
// layer (see withDatabase) and fail, rather than skip, with
// KODAMA_REQUIRE_POSTGRES=1 when no database is configured.
func Test_unit_runstate_postgres_scope(t *testing.T) {
	t.Run("every statement is tenant scoped and none is built from values", statementsAreTenantScoped)

	withDatabase(t, func(t *testing.T) {
		e := newEnv(t)

		t.Run("the store's role is not the owner and cannot bypass row security", func(t *testing.T) {
			var super, bypass bool
			var user string
			must(t, e.rt.QueryRow(context.Background(), `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).
				Scan(&user, &super, &bypass), "read the role")
			if user != e.db.RuntimeRole || super || bypass {
				t.Fatalf("role %s super=%v bypassrls=%v", user, super, bypass)
			}
			var owned int
			must(t, e.rt.QueryRow(context.Background(), `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned), "owned tables")
			if owned != 0 {
				t.Fatalf("the runtime role owns %d tables", owned)
			}
			for _, table := range kernelTables {
				var rls, forced bool
				must(t, e.owner.QueryRow(context.Background(), `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls, &forced), "read "+table)
				if !rls || !forced {
					t.Errorf("%s: row security enabled=%v forced=%v", table, rls, forced)
				}
			}
		})

		t.Run("shared suite: scope, release and signal identity", func(t *testing.T) {
			kernelcontracts.RunGroup(t, e.factory, kernelcontracts.GroupScope, kernelcontracts.GroupRelease)
		})

		e.reset()
		clock := kernelcontracts.NewClock()
		s := e.store(clock)
		populate(t, s, clock, scope1)

		t.Run("another tenant's context sees no row of any kernel table", func(t *testing.T) {
			for _, table := range kernelTables {
				var own, foreign int
				must(t, e.asRuntime("t1", func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&own)
				}), "count as the owning tenant")
				must(t, e.asRuntime("t2", func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&foreign)
				}), "count as another tenant")
				if own == 0 || foreign != 0 {
					t.Errorf("%s: %d rows for the owner, %d for another tenant", table, own, foreign)
				}
			}
			// No tenant context at all sees nothing either.
			for _, table := range kernelTables {
				var n int
				must(t, e.rt.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n), "count without a tenant")
				if n != 0 {
					t.Errorf("%s: %d rows without a tenant context", table, n)
				}
			}
		})

		t.Run("another tenant's context cannot change or plant a row", func(t *testing.T) {
			var affected int64
			must(t, e.asRuntime("t2", func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, `UPDATE runs SET lease_expires_at = now() WHERE id = 'run1'`)
				affected = tag.RowsAffected()
				return err
			}), "update as another tenant")
			if affected != 0 {
				t.Errorf("another tenant updated %d rows", affected)
			}
			err := e.asRuntime("t2", func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot) VALUES ('t1', 'p1', 'planted', 'pipe1', '{}')`)
				return err
			})
			var pe *pgconn.PgError
			if err == nil || !asPg(err, &pe) || pe.Code != "42501" {
				t.Errorf("planting a row of another tenant: %v", err)
			}
			err = e.asRuntime("t2", func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO audit_events (tenant_id, id, actor_type, actor_id, event_type, resource_type, resource_id)
					VALUES ('t1', 'planted', 'system', 'x', 'x', 'run', 'run1')`)
				return err
			})
			if err == nil || !asPg(err, &pe) || pe.Code != "42501" {
				t.Errorf("planting an outbox event of another tenant: %v", err)
			}
			if n := e.count(`SELECT count(*) FROM runs WHERE id = 'planted'`); n != 0 {
				t.Errorf("a planted run exists")
			}
		})

		t.Run("the adapter finds a run only in its own tenant and project", func(t *testing.T) {
			ctx := context.Background()
			if _, err := s.Load(ctx, scope1, "run1"); err != nil {
				t.Fatalf("the owning scope: %v", err)
			}
			for _, foreign := range []domain.Scope{scope2, {TenantID: "t1", ProjectID: "p2"}} {
				if _, err := s.Load(ctx, foreign, "run1"); !hasCode(err, domain.CodeNotFound) {
					t.Errorf("Load in %s: %v", foreign, err)
				}
				if _, err := s.Outbox(ctx, foreign, "run1"); !hasCode(err, domain.CodeNotFound) {
					t.Errorf("Outbox in %s: %v", foreign, err)
				}
				look, err := s.LookupSignal(ctx, runtime.SignalIdentity{Scope: foreign, Caller: "alice", Key: "k1"})
				if err != nil || look.Found() {
					t.Errorf("a signal identity of alice in %s: %+v %v", foreign, look, err)
				}
			}
			if look, err := s.LookupSignal(ctx, runtime.SignalIdentity{Scope: scope1, Caller: "alice", Key: "k1"}); err != nil || look.Record == nil {
				t.Errorf("the owning scope's record: %+v %v", look, err)
			}
		})

		t.Run("the outbox rows are the tenant's and carry its scope", func(t *testing.T) {
			events, err := s.Outbox(context.Background(), scope1, "run1")
			must(t, err, "outbox")
			if len(events) < 3 {
				t.Fatalf("outbox = %+v", events)
			}
			for _, ev := range events {
				if ev.Scope != scope1 || ev.RunID != "run1" {
					t.Errorf("event %+v is not of the run's scope", ev)
				}
			}
			if n := e.count(`SELECT count(*) FROM audit_events WHERE tenant_id <> 't1' OR project_id <> 'p1'`); n != 0 {
				t.Errorf("%d outbox rows of another scope", n)
			}
		})

		t.Run("concurrent signals and cancellations decide each wait once", func(t *testing.T) { testConcurrentDecisions(t, e) })
	})
}

func asPg(err error, target **pgconn.PgError) bool { return errors.As(err, target) }

// waitingRun admits a run of builderPlan into scope, claims it, runs build to
// success and opens the review wait for candidate.
func waitingRun(t *testing.T, s *Store, clock *kernelcontracts.Clock, scope domain.Scope, id domain.RunID, candidate string) runtime.Wait {
	t.Helper()
	ctx := context.Background()
	rec, err := s.Admit(ctx, admission(t, scope, id, builderPlan(), clock.Now()))
	must(t, err, "admit")
	rec, err = s.Claim(ctx, runtime.ClaimRequest{Scope: scope, RunID: id, Owner: "engine", Purpose: runtime.ClaimExecute, TTL: 48 * time.Hour}, clock.Now())
	must(t, err, "claim")
	commit := func(mutate func(r *runtime.Run) error, waits ...runtime.Wait) {
		t.Helper()
		r := rec.Run.Clone()
		must(t, mutate(&r), "move")
		sealed, err := r.Sealed(rec.Run)
		must(t, err, "seal")
		rec, err = s.Commit(ctx, runtime.Transition{Scope: scope, RunID: id, Claim: rec.Claim, Now: clock.Now(), ExpectedVersion: rec.Run.Version, Run: sealed, Waits: waits})
		must(t, err, "commit")
	}
	commit(func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
	commit(func(r *runtime.Run) error {
		return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	})
	w := rec.Waits[0]
	must(t, w.Open(clock.Now(), map[string]string{"candidate": candidate}, ""), "open")
	commit(func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) }, w)
	return w
}

// testConcurrentDecisions mixes signals by identity, signals by a correlation
// that reaches several runs (which lock several runs), and cancellations over
// the same waits at once. Whatever the interleaving, no operation fails or
// deadlocks, every wait is decided exactly once, and every decision is one
// outbox event.
func testConcurrentDecisions(t *testing.T, e *env) {
	e.reset()
	clock := kernelcontracts.NewClock()
	s := e.store(clock)
	ctx := context.Background()
	const runs = 8
	waits := make([]runtime.Wait, runs)
	for i := range runs {
		// Every run shares one correlation value, so a correlation signal is ambiguous and reaches them all.
		waits[i] = waitingRun(t, s, clock, scope1, domain.RunID(fmt.Sprintf("run%d", i)), "shared")
	}
	errs := make(chan error, 4*runs)
	var wg sync.WaitGroup
	now := clock.Now()
	for i := range runs {
		id := domain.RunID(fmt.Sprintf("run%d", i))
		for _, op := range []func() error{
			func() error {
				_, err := s.DecideSignal(ctx, runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "alice", Key: "id-" + string(id)},
					WaitID: waits[i].ID, Outcome: "approved", Actor: "user:alice"}, now)
				return err
			},
			func() error {
				_, err := s.DecideSignal(ctx, runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: scope1, Caller: "svc", Key: "corr-" + string(id)},
					Correlation: map[string]string{"candidate": "shared"}, Outcome: "approved", Actor: "svc"}, now)
				return err
			},
			func() error { _, err := s.RequestCancel(ctx, scope1, id); return err },
		} {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- op() }()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent operation failed: %v", err)
		}
	}
	for i := range runs {
		id := domain.RunID(fmt.Sprintf("run%d", i))
		rec, err := s.Load(ctx, scope1, id)
		must(t, err, "load")
		w := rec.Waits[0]
		if !w.State.Closed() {
			t.Errorf("run %s: the wait is %s", id, w.State)
		}
		events, err := s.Outbox(ctx, scope1, id)
		must(t, err, "outbox")
		decisions := 0
		for _, ev := range events {
			if ev.Kind == runtime.EventWaitResolved || ev.Kind == runtime.EventWaitCancelled {
				decisions++
			}
		}
		if decisions != 1 {
			t.Errorf("run %s: %d decision events for one wait (%s)", id, decisions, w.State)
		}
	}
	if n := e.count(`SELECT count(*) FROM signal_records WHERE applied`); n > runs {
		t.Errorf("%d winning signals for %d waits", n, runs)
	}
	if n := e.count(`SELECT count(*) FROM (SELECT target_wait_id FROM signal_records WHERE applied GROUP BY target_wait_id HAVING count(*) > 1) d`); n != 0 {
		t.Errorf("%d waits have two winning signals", n)
	}
}
