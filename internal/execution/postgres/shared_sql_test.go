package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

var sharedTables = []string{"run_resources", "run_holders", "attempt_begins", "execution_run_reservations"}

// Test_shared_forced_rls_ownership_and_guards: actual row level security on the
// new tables (tenant isolation, no context, foreign writes), composite ownership,
// deterministic-name and UID uniqueness, the single unreleased holder, and the
// database guards behind the Go entities (UID, stop, removal, receipt and
// reservation are written once; the pinned run_shared columns never change; the
// runtime role cannot rewrite the spec or fingerprint).
func Test_shared_forced_rls_ownership_and_guards(t *testing.T) {
	ctx := context.Background()
	e, s := executiontest.NewSharedPG(t)
	sr := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(true, executiontest.SharedStep("job")))
	sr.Up()
	in, err := sr.Claim()
	if err != nil || in == nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.Svc.RecordBegin(ctx, executiontest.ScopeA, in.AttemptID, beginBody()); err != nil {
		t.Fatal(err)
	}
	run := sr.Run()
	for _, table := range sharedTables {
		if e.Count(fmt.Sprintf(`SELECT count(*) FROM %s`, table)) == 0 {
			t.Fatalf("fixture: %s has no rows", table)
		}
	}

	t.Run("forced row level security with and without tenant context", func(t *testing.T) {
		for _, table := range sharedTables {
			var foreign, none int
			if err := e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&foreign)
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Runtime.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&none); err != nil {
				t.Fatal(err)
			}
			if foreign != 0 || none != 0 {
				t.Errorf("%s: tenant B sees %d rows, no context sees %d", table, foreign, none)
			}
		}
		// Another tenant can neither read the run's pins nor write for tenant A.
		var pins int
		_ = e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE run_shared IS NOT NULL`).Scan(&pins)
		})
		if pins != 0 {
			t.Fatalf("tenant B reads %d run_shared pins of tenant A", pins)
		}
		err := e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO execution_run_reservations (tenant_id, project_id, run_id, services, cpu_millis, memory_mib, ephemeral_mib, storage_mib, held_at)
				VALUES ($1,$2,$3,0,0,0,0,0,now())`, string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA.ProjectID), string(run.ID))
			return err
		})
		if err == nil {
			t.Fatal("a write for another tenant was accepted")
		}
		if _, err := e.Store.Get(ctx, executiontest.ScopeB, run.ID); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a foreign tenant resolves the run: %v", err)
		}
		if _, err := e.Store.Get(ctx, executiontest.ScopeA2, run.ID); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a foreign project resolves the run: %v", err)
		}
	})

	t.Run("composite ownership: a resource of one project cannot attach to a run of another", func(t *testing.T) {
		_, err := e.Migration.Exec(ctx, `INSERT INTO run_resources (tenant_id, project_id, run_id, key, kind, name, ordinal, backend, spec, fingerprint, phase, version, created_at, updated_at)
			VALUES ($1,$2,$3,'service.cache','service','kr-aaaaaaaaaaaaaaaaaaaaaaaa',9,'manual','{}'::jsonb,$4,'pending',1,now(),now())`,
			string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA2.ProjectID), string(run.ID), "sha256:"+strings.Repeat("a", 64))
		if err == nil {
			t.Fatal("a run resource attached to a run of another project")
		}
	})

	owner := func(sql string, args ...any) error {
		_, err := e.Migration.Exec(ctx, sql, args...)
		return err
	}
	runtime := func(sql string, args ...any) error {
		return e.RuntimeTx(executiontest.ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	refuses := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	tenant, project, rid := string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA.ProjectID), string(run.ID)

	t.Run("the database guards behind the entities", func(t *testing.T) {
		refuses("rewrite the bound UID", owner(`UPDATE run_resources SET uid = 'other-uid', version = version + 1 WHERE tenant_id=$1 AND run_id=$2 AND key='service.db'`, tenant, rid))
		refuses("rewrite the spec", owner(`UPDATE run_resources SET spec = '{"x":1}'::jsonb WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("rewrite the fingerprint", owner(`UPDATE run_resources SET fingerprint = $3 WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid, "sha256:"+strings.Repeat("b", 64)))
		refuses("rewrite the receipt", owner(`UPDATE run_resources SET receipt_digest = $3 WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid, "sha256:"+strings.Repeat("c", 64)))
		refuses("rewrite the verified stop", owner(`UPDATE run_resources SET stopped_at = now() + interval '1 hour' WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("forget the ensure intent", owner(`UPDATE run_resources SET ensure_requested_at = NULL WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("remove without a verified stop", owner(`UPDATE run_resources SET removed_at = now() WHERE tenant_id=$1 AND run_id=$2 AND key='service.db'`, tenant, rid))
		refuses("start a stopped resource again", owner(`UPDATE run_resources SET phase = 'ready' WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("delete a resource", owner(`DELETE FROM run_resources WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("a second resource bound to a taken UID", owner(`INSERT INTO run_resources (tenant_id, project_id, run_id, key, kind, name, ordinal, backend, spec, fingerprint, uid, bound_at, phase, version, created_at, updated_at)
			SELECT tenant_id, project_id, run_id, 'service.dup', 'service', 'kr-bbbbbbbbbbbbbbbbbbbbbbbb', 11, backend, spec, fingerprint, uid, now(), 'pending', 1, now(), now()
			FROM run_resources WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("a second resource at a taken name", owner(`INSERT INTO run_resources (tenant_id, project_id, run_id, key, kind, name, ordinal, backend, spec, fingerprint, phase, version, created_at, updated_at)
			SELECT tenant_id, project_id, run_id, 'service.dup', 'service', name, 10, backend, spec, fingerprint, 'pending', 1, now(), now() FROM run_resources WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		// The runtime role has no UPDATE on identity columns at all.
		refuses("runtime role rewrites the spec", runtime(`UPDATE run_resources SET spec = '{}'::jsonb WHERE run_id=$1`, rid))
		refuses("runtime role rewrites the pins", runtime(`UPDATE runs SET run_shared = '{}'::jsonb WHERE id=$1`, rid))
		refuses("owner rewrites the pins", owner(`UPDATE runs SET run_shared = '{}'::jsonb WHERE id=$1`, rid))
		refuses("owner moves the deadline", owner(`UPDATE runs SET deadline_at = deadline_at + interval '1 hour' WHERE id=$1`, rid))
		refuses("lower an observation generation", owner(`UPDATE run_resources SET observe_gen = 0 WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("apply a generation never requested", owner(`UPDATE run_resources SET applied_gen = observe_gen + 1 WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("rewrite a receipt body", owner(`UPDATE run_resources SET receipt = jsonb_set(receipt, '{treeId}', '"4444444444444444444444444444444444444444"') WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("rewrite the receipt time", owner(`UPDATE run_resources SET receipt_at = receipt_at + interval '1 hour' WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("rewrite the bind time", owner(`UPDATE run_resources SET bound_at = bound_at + interval '1 hour' WHERE tenant_id=$1 AND run_id=$2 AND key='network'`, tenant, rid))
		refuses("rewrite the stop reason", owner(`UPDATE run_resources SET stop_reason = 'tampered' WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("rewrite the stop launch evidence", owner(`UPDATE run_resources SET stop_launch = 'not_started' WHERE tenant_id=$1 AND run_id=$2 AND key='preparation'`, tenant, rid))
		refuses("a second unreleased holder", owner(`INSERT INTO run_holders (tenant_id, project_id, run_id, attempt_id, step_id, held_at)
			SELECT tenant_id, project_id, run_id, attempt_id, 'again', now() FROM run_holders WHERE run_id=$1 AND released_at IS NULL`, rid))
		refuses("rewrite a begin", owner(`UPDATE attempt_begins SET workspace_state = 'modified' WHERE run_id=$1`, rid))
		refuses("delete a begin", owner(`DELETE FROM attempt_begins WHERE run_id=$1`, rid))
		refuses("change a reservation's quantities", owner(`UPDATE execution_run_reservations SET cpu_millis = 1 WHERE run_id=$1`, rid))
		refuses("release a reservation twice", func() error {
			if err := owner(`UPDATE execution_run_reservations SET released_at = now() WHERE run_id=$1 AND released_at IS NULL`, rid); err != nil {
				return fmt.Errorf("first release: %w", err)
			}
			return owner(`UPDATE execution_run_reservations SET released_at = now() + interval '1 hour' WHERE run_id=$1`, rid)
		}())
		// The row survived all of that unchanged.
		agg := e.Get(run)
		if agg.Run.Shared == nil || agg.Resource("service.db").UID == "" {
			t.Fatal("the pins or the bound UID were lost")
		}
		_ = project
	})
}

func beginBody() runtimev2.BeginRequest {
	return runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: executiontest.CommitA, TrackedClean: true}}
}

// Test_shared_start_is_atomic_with_its_effects: a failing audit/outbox append
// rolls the whole start back (no resources, no reservation, the run stays
// queued) and the retried start commits once.
func Test_shared_start_is_atomic_with_its_effects(t *testing.T) {
	var fa *failingAppender
	e, s := executiontest.NewSharedPG(t, executiontest.Options{WrapAppender: func(in execpg.EventAppender) execpg.EventAppender {
		fa = &failingAppender{inner: in}
		return fa
	}})
	sr := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(true, executiontest.SharedStep("job")))
	run := sr.Run()
	fa.fail.Store(eventsv1.TypeRunStateChanged)
	if _, err := s.Svc.ClaimRunnable(context.Background(), executiontest.ScopeA.TenantID); !errors.Is(err, errInjected) {
		t.Fatalf("got %v", err)
	}
	for table, n := range map[string]int{
		"run_resources": e.Count(`SELECT count(*) FROM run_resources`), "reservations": e.Count(`SELECT count(*) FROM execution_run_reservations`),
		"resource history": e.Count(`SELECT count(*) FROM run_history WHERE subject_kind = 'resource'`),
		"resource audit":   e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run_resource'`),
	} {
		if n != 0 {
			t.Errorf("%s: %d rows survived the failed start", table, n)
		}
	}
	got := e.Get(run)
	if got.Run.Status != execution.RunQueued || got.Run.DeadlineAt != nil {
		t.Fatalf("the failed start moved the run: %s", got.Run.Status)
	}
	fa.fail.Store("")
	sr.Start()
	if e.Count(`SELECT count(*) FROM run_resources`) != 4 || e.Count(`SELECT count(*) FROM execution_run_reservations WHERE released_at IS NULL`) != 1 {
		t.Fatal("the retried start did not commit its intents and reservation once")
	}
	// Every resource transition has its audit, history and outbox rows atomically,
	// under the domain-valid scoped resource ID of the event contract.
	hist := e.Count(`SELECT count(*) FROM run_history WHERE subject_kind = 'resource'`)
	if hist == 0 || e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run_resource'`) != hist ||
		e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run_resource' AND resource_id ~ '^rr-[0-9a-f]{40}$' AND action = 'run_resource.state_changed'`) != hist ||
		e.CountTenant(executiontest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1`, eventsv1.TypeRunResourceStateChanged) != hist {
		t.Fatalf("audit, history and outbox rows of resource transitions diverged (history %d, audit %d, scoped audit %d, outbox %d)", hist,
			e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run_resource'`),
			e.Count(`SELECT count(*) FROM audit WHERE resource_type = 'run_resource' AND resource_id ~ '^rr-[0-9a-f]{40}$' AND action = 'run_resource.state_changed'`),
			e.CountTenant(executiontest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1`, eventsv1.TypeRunResourceStateChanged))
	}
}

// Test_shared_concurrent_starts_never_exceed_capacity: concurrent starters of
// many queued runs take the quota locks in order; exactly as many runs start as
// the capacity admits and the rest stay queued without a partial reservation.
func Test_shared_concurrent_starts_never_exceed_capacity(t *testing.T) {
	lim := executiontest.SharedLimit()
	lim.SharedTenant.StorageMiB, lim.SharedProject.StorageMiB = 2*(2048+128), 2*(2048+128) // two runs of scratch + service storage
	_, s := executiontest.NewSharedPG(t, executiontest.Options{Limits: &lim})
	e := s.Subject
	_ = e
	def := s.DefineShared(executiontest.SharedSpecDoc(true, executiontest.SharedStep("job")))
	var runs []*execution.Run
	for range 5 {
		run, err := s.Svc.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer}, execution.AdmitCommand{
			Scope: executiontest.ScopeA, WorkItemID: s.Project.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(s.Project), SourceCommit: executiontest.CommitA,
		})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				_, _ = s.Svc.ClaimRunnable(context.Background(), executiontest.ScopeA.TenantID)
			}
		}()
	}
	wg.Wait()
	running, queued := 0, 0
	for _, r := range runs {
		agg, err := s.Store.Get(context.Background(), executiontest.ScopeA, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch agg.Run.Status {
		case execution.RunRunning:
			running++
			if len(agg.Resources) != 4 {
				t.Errorf("a started run has %d resources", len(agg.Resources))
			}
		case execution.RunQueued:
			queued++
			if len(agg.Resources) != 0 {
				t.Errorf("a queued run holds %d resources", len(agg.Resources))
			}
		}
	}
	if running != 2 || queued != 3 || s.RunReservations(executiontest.ScopeA.TenantID) != 2 {
		t.Fatalf("running=%d queued=%d held=%d, want 2/3/2", running, queued, s.RunReservations(executiontest.ScopeA.TenantID))
	}
}

// Test_shared_migration_is_additive: the run_shared migration applies after an
// already deployed schema (the exact application manifest of the prior release)
// that holds REAL historical runs, steps, attempts and bookkeeping written with
// the old column list; those rows stay readable and writable through the changed
// store, the upgrade and a fresh database behave alike, a re-apply changes
// nothing, the ledger prefix is preserved and the new tables are RLS-forced.
func Test_shared_migration_is_additive(t *testing.T) {
	ctx := context.Background()

	legacyFlows := func(t *testing.T, e *executiontest.Env, h executiontest.LegacyRuns) {
		t.Helper()
		e.GrantAll()
		e.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
		for _, id := range []domain.RunID{h.Finished, h.Queued, h.Running} {
			agg, err := e.Store.Get(ctx, executiontest.ScopeA, id)
			if err != nil || agg.Run.Shared != nil || agg.Run.DeadlineAt != nil || len(agg.Resources) != 0 || len(agg.Holders) != 0 || len(agg.Begins) != 0 ||
				agg.Run.Snapshot.Runtime == nil || agg.Run.Snapshot.Runtime.ResourceRequires != 0 || agg.Step("job") == nil {
				t.Fatalf("historical run %s read through the changed store: %+v %v", id, agg, err)
			}
			if agg.SharedView() != nil {
				t.Fatalf("%s: a historical run has no run_shared projection", id)
			}
		}
		fin, _ := e.Store.Get(ctx, executiontest.ScopeA, h.Finished)
		if fin.Run.Status != execution.RunSucceeded || fin.Attempt("att_hist_finished") == nil || fin.Attempt("att_hist_finished").Result == nil ||
			!fin.Control("att_hist_finished").Settled() {
			t.Fatalf("the finished history changed: %+v", fin.Run)
		}
		if runs, err := e.Store.List(ctx, executiontest.ScopeA, execution.ListQuery{Limit: 10}); err != nil || len(runs) != 3 {
			t.Fatalf("list: %d %v", len(runs), err)
		}
		// Updates through the new store: the queued legacy run dispatches and completes ...
		in := e.MustClaim(executiontest.ScopeA.TenantID)
		if in.RunID != h.Running && in.RunID != h.Queued {
			t.Fatalf("claimed %s", in.RunID)
		}
		if in.RunID == h.Queued {
			e.Start(in)
			if _, err := e.Succeed(in, nil, ""); err != nil {
				t.Fatal(err)
			}
			if got := e.Get(&execution.Run{Scope: executiontest.ScopeA, ID: h.Queued}); got.Run.Status != execution.RunSucceeded {
				t.Fatalf("queued legacy run: %s", got.Run.Status)
			}
		}
		// ... and the running one is cancelled, its attempt revoked and settled.
		if _, err := e.Svc.Cancel(ctx, execution.Caller{PrincipalID: executiontest.Engineer}, executiontest.ScopeA, h.Running); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := e.Svc.ReconcileRun(ctx, execution.RunRef{Scope: executiontest.ScopeA, ID: h.Running}); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := e.Store.Get(ctx, executiontest.ScopeA, h.Running); got.Run.Status != execution.RunCancelled {
			t.Fatalf("running legacy run: %s", got.Run.Status)
		}
		// Terminal history stays immutable and the new tables stay empty for legacy runs.
		for _, table := range sharedTables {
			if n := e.Count(fmt.Sprintf(`SELECT count(*) FROM %s`, table)); n != 0 {
				t.Fatalf("%s holds %d rows for legacy runs", table, n)
			}
		}
	}

	t.Run("an upgrade of a database holding real historical runs", func(t *testing.T) {
		var h executiontest.LegacyRuns
		e := executiontest.New(t, executiontest.Options{LegacySeed: func(e *executiontest.Env) { h = executiontest.SeedLegacyRuns(e) }})
		if e.Count(`SELECT count(*) FROM runs WHERE run_shared IS NULL AND deadline_at IS NULL`) != 3 {
			t.Fatal("the historical rows gained run_shared state")
		}
		legacyFlows(t, e, h)
	})

	t.Run("a fresh database behaves alike", func(t *testing.T) {
		var h executiontest.LegacyRuns
		e := executiontest.New(t)
		h = executiontest.SeedLegacyRuns(e) // the new columns exist and default to NULL
		legacyFlows(t, e, h)
	})

	t.Run("the application manifest applies only the new migration after the prior ledger and enables nothing", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
		all := app.Contributions()
		prior, err := storage.NewManifest(all[:len(all)-1]...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, prior, opts); err != nil {
			t.Fatal(err)
		}
		full, err := app.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		res, err := storage.Migrate(ctx, pool, full, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Applied) != 1 || res.Applied[0] != execpg.RunResourcesMigrationID {
			t.Fatalf("only the new migration applies after the prior ledger: %v", res.Applied)
		}
		again, err := storage.Migrate(ctx, pool, full, opts)
		if err != nil || len(again.Applied) != 0 {
			t.Fatalf("reapply: %+v %v", again, err)
		}
		for _, table := range sharedTables {
			var rls bool
			if err := pool.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&rls); err != nil || !rls {
				t.Errorf("%s: forced row level security missing (%v)", table, err)
			}
		}
	})

	t.Run("the base manifest applies it after a deployed execution schema too", func(t *testing.T) {
		db := pgtest.New(t)
		pool := db.MigrationPool(ctx, 2)
		opts := storage.MigrateOptions{RuntimeRole: db.RuntimeRole}
		base := append(peerContributions(), execpg.Schema()...)
		base = append(base, execpg.CleanupMigrations())
		deployed, err := storage.NewManifest(base...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := storage.Migrate(ctx, pool, deployed, opts); err != nil {
			t.Fatal(err)
		}
		full, err := storage.NewManifest(append(base, execpg.RunResourceMigrations())...)
		if err != nil {
			t.Fatal(err)
		}
		res, err := storage.Migrate(ctx, pool, full, opts)
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != execpg.RunResourcesMigrationID {
			t.Fatalf("%+v %v", res, err)
		}
	})
}
