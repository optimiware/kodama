package storage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/storage"
)

// Test_durable_run_state_postgres is the real-database evidence for the durable
// run-state migration. It needs PostgreSQL: in the unit layer (-tags unit) it
// skips, and with KODAMA_REQUIRE_POSTGRES=1 it fails when no service is
// configured. The runtime role it uses is not the owner: it has no superuser,
// no BYPASSRLS, and only the column grants the migration gives it.
//
// The contractual unit tests are Test_unit_storage_durable_run_state and
// Test_unit_storage_run_state_guards; this test shows what PostgreSQL enforces.

const planDigest = "sha256:durable"

var (
	durableScope = domain.Scope{TenantID: "tenant-a", ProjectID: "pa1"}
	t0           = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	okResult     = runtime.Result{Code: runtime.ResultSucceeded}
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func bind(step domain.StepID, attempt int, fence runtime.Fence) runtime.RuntimeBinding {
	return runtime.RuntimeBinding{Scope: durableScope, RunID: "run1", StepKey: step, Attempt: attempt,
		WorkloadID: "wl-run1-" + string(step) + "-" + string(rune('0'+attempt)), SpecDigest: "sha256:spec", Backend: "process", Fence: fence}
}

func durablePlan() runtime.Plan {
	review := runtime.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"}, Request: "build.outputs.summary",
		Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour, OnExpiry: runtime.ExpiryContinue}
	approve := runtime.WaitSpec{Assign: "release-manager", Input: "approval", Outcomes: []string{"approved", "rejected"}, Timeout: 2 * time.Hour}
	late := runtime.WaitSpec{Correlate: map[string]string{"ticket": "inputs.ticket"}, Input: "signoff", Outcomes: []string{"done"}, Timeout: 24 * time.Hour}
	return runtime.Plan{Digest: planDigest, Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &review},
		{Key: "notify", Kind: runtime.StepContainer, DependsOn: []domain.StepID{"review"}, When: "review.outcome == 'rejected'",
			Workload: &runtime.WorkloadSpec{Image: "alpine:3", Commands: []string{"notify"}}},
		{Key: "test", Kind: runtime.StepContainer, DependsOn: []domain.StepID{"review"}, When: "review.outcome == 'approved'",
			Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go test"}}},
		{Key: "approve", Kind: runtime.StepWait, DependsOn: []domain.StepID{"test"}, Wait: &approve},
		{Key: "late", Kind: runtime.StepWait, DependsOn: []domain.StepID{"test"}, Wait: &late},
	}}
}

func evaluation(result bool) runtime.PredicateEvaluation {
	digest, err := runtime.DigestPredicateReads([]runtime.PredicateRead{{Ref: "review.outcome", Type: runtime.InputString, Present: true, Value: "approved"}})
	if err != nil {
		panic(err)
	}
	return runtime.PredicateEvaluation{Evaluated: true, Result: result, InputDigest: digest, At: t0.Add(time.Minute)}
}

// midFlightRecord exercises every part of the record: a lease, a retried launch, a
// settled container result, a resolved wait, a false and a true when evaluation,
// a pending wait (identity recorded, not opened), a wait cancelled before it
// opened, an uncertain launch, workload and resource observations including the
// workspace and a tenant-owned service.
func midFlightRecord(t *testing.T) runtime.RunRecord {
	t.Helper()
	plan := durablePlan()
	run, err := runtime.NewRun(durableScope, "run1", "pl1", plan, 3)
	must(t, err)

	must(t, run.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone))
	b1, b2, b3 := bind("build", 1, 1), bind("build", 2, 1), bind("test", 1, 1)
	launch1, err := runtime.NewWorkloadCommand("c-build-1", b1, runtime.CommandLaunch, t0)
	must(t, err)
	must(t, launch1.Settle(runtime.CommandRejected, runtime.ReasonLaunchNotReached))
	must(t, run.RetryStep("build", launch1))
	launch2, _ := runtime.NewWorkloadCommand("c-build-2", b2, runtime.CommandLaunch, t0.Add(time.Second))
	must(t, launch2.Settle(runtime.CommandConfirmed, runtime.ReasonNone))
	stop2, _ := runtime.NewWorkloadCommand("c-stop-2", b2, runtime.CommandStop, t0.Add(2*time.Second))
	must(t, stop2.Settle(runtime.CommandConfirmed, runtime.ReasonNone))
	must(t, stop2.RecordResult(okResult))
	must(t, run.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded}))

	review, err := runtime.NewWait(durableScope, "w-review", "run1", "review", *plan.Steps[1].Wait)
	must(t, err)
	must(t, review.Open(t0, map[string]string{"candidate": "c1"}, "please review"))
	must(t, run.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone))
	_, err = review.Resolve(runtime.SignalRequest{Identity: runtime.SignalIdentity{Scope: durableScope, Caller: "user-alice", Key: "key-w-review"},
		WaitID: review.ID, ExpectedVersion: review.Version, Outcome: "approved", Answer: "looks good", Actor: "user:alice"}, t0.Add(time.Minute))
	must(t, err)
	must(t, run.AdvanceStep("review", runtime.StepSucceeded, runtime.ReasonNone))

	must(t, run.EvaluateStep("notify", evaluation(false), runtime.StepSkipped, runtime.ReasonConditionFalse))
	must(t, run.EvaluateStep("test", evaluation(true), runtime.StepRunning, runtime.ReasonNone))
	launch3, _ := runtime.NewWorkloadCommand("c-test-1", b3, runtime.CommandLaunch, t0.Add(3*time.Minute))
	must(t, launch3.Settle(runtime.CommandUncertain, runtime.ReasonLaunchUncertain))
	ensure, err := runtime.NewCommand("c-ws", durableScope, "run1", "", runtime.CommandEnsureResource, "run1-ws", "sha256:ws", 1, t0)
	must(t, err)
	must(t, ensure.Settle(runtime.CommandConfirmed, runtime.ReasonNone))

	approve, err := runtime.NewWait(durableScope, "w-approve", "run1", "approve", *plan.Steps[4].Wait)
	must(t, err)
	late, err := runtime.NewWait(durableScope, "w-late", "run1", "late", *plan.Steps[5].Wait)
	must(t, err)
	run.RequestCancel()
	must(t, run.AdvanceStep("late", runtime.StepCancelled, runtime.ReasonRunCancelled))
	_, err = late.Cancel(t0.Add(2*time.Minute), runtime.ReasonRunCancelled)
	must(t, err)

	ws := runtime.ResourceHandle{Scope: durableScope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, durableScope), Lifetime: runtime.LifetimePipeline,
		RunID: "run1", Kind: runtime.ResourceWorkspace, Name: "workspace", Identity: "run1-ws", Fence: 1}
	db := ws
	db.Kind, db.Name, db.Identity = runtime.ResourceService, "db", "run1-db"
	project := runtime.ResourceHandle{Scope: durableScope, Owner: runtime.OwnerFor(runtime.LifetimeProject, durableScope), Lifetime: runtime.LifetimeProject,
		Kind: runtime.ResourceService, Name: "cache", Identity: "pa1-cache", Fence: 2}
	tenant := runtime.ResourceHandle{Scope: durableScope, Owner: runtime.OwnerFor(runtime.LifetimeTenant, durableScope), Lifetime: runtime.LifetimeTenant,
		Kind: runtime.ResourceService, Name: "registry", Identity: "tenant-a-registry", Fence: 2}

	rec := runtime.RunRecord{
		Run: run, Plan: plan,
		Claim:    runtime.Claim{Scope: durableScope, RunID: "run1", Owner: "worker-a", Purpose: runtime.ClaimExecute, Fence: 3, Expires: t0.Add(10 * time.Minute)},
		Waits:    []runtime.Wait{review, approve, late},
		Commands: []runtime.Command{launch1, launch2, stop2, launch3, ensure},
		Workloads: []runtime.WorkloadObservation{
			{Binding: b2, Phase: runtime.PhaseExited, Result: okResult, ObservedAt: t0.Add(2 * time.Second)},
			{Binding: b3, Phase: runtime.PhaseUnknown, ObservedAt: t0.Add(4 * time.Minute)},
		},
		Resources: []runtime.ResourceObservation{
			{Handle: ws, State: runtime.ResourceReady, ObservedAt: t0},
			{Handle: db, State: runtime.ResourceStarting, ObservedAt: t0},
			{Handle: project, State: runtime.ResourceReady, ObservedAt: t0},
			{Handle: tenant, State: runtime.ResourceUnknown, Reason: runtime.ReasonServiceLost, ObservedAt: t0.Add(time.Minute)},
		},
	}
	must(t, rec.Validate())
	return rec
}

// settledRecord is a failed run whose cleanup is exhausted with a resource and a
// workload retained, a stop result that failed and no lease.
func settledRecord(t *testing.T) runtime.RunRecord {
	t.Helper()
	plan := runtime.Plan{Digest: "sha256:settled", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}}}}
	scope := domain.Scope{TenantID: "tenant-a", ProjectID: "pa1"}
	run, err := runtime.NewRun(scope, "run2", "pl1", plan, 2)
	must(t, err)
	must(t, run.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone))
	must(t, run.FinishStep("build", runtime.StepFailed, runtime.ReasonCommandFailed, runtime.Result{Code: runtime.ResultFailed, ExitCode: 2, Reason: runtime.ReasonCommandFailed}))
	must(t, run.Settle(runtime.OutcomeFailed, runtime.ReasonCommandFailed))
	ws := runtime.ResourceHandle{Scope: scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, scope), Lifetime: runtime.LifetimePipeline,
		RunID: "run2", Kind: runtime.ResourceWorkspace, Name: "workspace", Identity: "run2-ws", Fence: 1}
	b := runtime.RuntimeBinding{Scope: scope, RunID: "run2", StepKey: "build", Attempt: 1, WorkloadID: "wl-run2-build-1", SpecDigest: "sha256:spec", Backend: "process", Fence: 1}
	must(t, run.RecordCleanupRemaining(runtime.Remaining{Resources: []runtime.ResourceHandle{ws}, Workloads: []runtime.RuntimeBinding{b}}, runtime.ReasonCleanupFailed))
	must(t, run.RecordCleanupRemaining(runtime.Remaining{Resources: []runtime.ResourceHandle{ws}, Workloads: []runtime.RuntimeBinding{b}}, runtime.ReasonCleanupExhausted))
	stop, _ := runtime.NewWorkloadCommand("c-stop", b, runtime.CommandStop, t0)
	must(t, stop.Settle(runtime.CommandUncertain, runtime.ReasonStopUncertain))
	rec := runtime.RunRecord{Run: run, Plan: plan, Commands: []runtime.Command{stop},
		Workloads: []runtime.WorkloadObservation{{Binding: b, Phase: runtime.PhaseExited, Result: runtime.Result{Code: runtime.ResultFailed, ExitCode: 2, Reason: runtime.ReasonCommandFailed}, ObservedAt: t0}},
		Resources: []runtime.ResourceObservation{{Handle: ws, State: runtime.ResourceUnknown, Reason: runtime.ReasonWorkspaceLost, ObservedAt: t0}}}
	must(t, rec.Validate())
	if rec.Run.Cleanup.State != runtime.CleanupExhausted {
		t.Fatalf("cleanup = %s", rec.Run.Cleanup.State)
	}
	return rec
}

func seedPipeline(t testing.TB, e *env) {
	t.Helper()
	e.exec(t, `INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES
  ('tenant-a', 'pa1', 'pl1', 'sdlc', 'api', '{}'), ('tenant-b', 'pb1', 'plb', 'sdlc', 'api', '{}')`)
}

func Test_durable_run_state_postgres(t *testing.T) {
	t.Run("record_round_trips_as_the_runtime_role", func(t *testing.T) {
		e := newEnv(t)
		seedPipeline(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)

		for _, rec := range []runtime.RunRecord{midFlightRecord(t), settledRecord(t)} {
			scope, id := rec.Run.Scope, rec.Run.ID
			must(t, storage.WithTenant(ctx, pool, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error { return saveRecord(ctx, tx, rec) }))
			var got runtime.RunRecord
			must(t, storage.WithTenant(ctx, pool, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
				got, err = loadRecord(ctx, tx, scope, id)
				return err
			}))
			want, have := mustJSON(canonical(rec)), mustJSON(canonical(got))
			if want != have {
				t.Fatalf("run %s did not round-trip:\nwant %s\n got %s", id, want, have)
			}
			must(t, got.Validate())
			// What the record is for: the decoded record alone answers the engine's questions.
			if id == "run1" {
				if res, ok := got.StepResult("build"); !ok || res != okResult || got.Run.Steps[0].Attempt != 2 {
					t.Errorf("build result %+v %v attempt %d", res, ok, got.Run.Steps[0].Attempt)
				}
				if out, ok := got.WaitOutcome("review"); !ok || out != "approved" {
					t.Errorf("review outcome %q %v", out, ok)
				}
				if u := got.Unresolved(); len(u) != 1 || u[0].ID != "c-test-1" || u[0].Binding.Backend != "process" || u[0].Binding.Attempt != 1 {
					t.Errorf("unresolved commands %+v", u)
				}
				if p, ok := got.Predicate("notify"); !ok || p.Result {
					t.Errorf("notify predicate %+v %v: an evaluated false must differ from an unevaluated predicate", p, ok)
				}
				if _, ok := got.Predicate("approve"); ok {
					t.Error("a step never evaluated has no predicate")
				}
			}
			if id == "run2" && (got.Run.Cleanup.State != runtime.CleanupExhausted || len(got.Run.Cleanup.Retained) != 1 || len(got.Run.Cleanup.RetainedWorkloads) != 1 ||
				got.Run.Cleanup.MaxAttempts != 2 || got.Run.Cleanup.Reason != runtime.ReasonCleanupExhausted) {
				t.Errorf("cleanup %+v", got.Run.Cleanup)
			}
		}
		// The pending wait kept its identity and version while unopened, and the one
		// cancelled before opening kept them and its decision.
		var id, state *string
		var version int64
		must(t, e.mig.QueryRow(ctx, `SELECT wait_id, wait_state, wait_version FROM run_steps WHERE run_id = 'run1' AND step_key = 'approve'`).Scan(&id, &state, &version))
		if id == nil || *id != "w-approve" || state != nil || version != 1 {
			t.Errorf("pending wait: %v %v %d", id, state, version)
		}
		must(t, e.mig.QueryRow(ctx, `SELECT wait_id, wait_state, wait_version FROM run_steps WHERE run_id = 'run1' AND step_key = 'late'`).Scan(&id, &state, &version))
		if id == nil || *id != "w-late" || state == nil || *state != "cancelled" || version != 2 {
			t.Errorf("wait cancelled before opening: %v %v %d", id, state, version)
		}
	})

	t.Run("run_claim_and_cleanup", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		rt := func(sql string) error { return runtimeExec(t, pool, tenantA, sql) }
		ok := func(err error, what string) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		// A claim has a purpose, and only a holder has one.
		requireCode(t, rt(`UPDATE runs SET lease_owner = 'w1', lease_expires_at = now() + interval '1 minute', lease_fence = 1 WHERE id = 'r1'`), "23514", "claim without a purpose")
		requireCode(t, rt(`UPDATE runs SET lease_owner = 'w1', lease_purpose = 'repair', lease_expires_at = now() + interval '1 minute', lease_fence = 1 WHERE id = 'r1'`), "23514", "unknown purpose")
		for _, purpose := range []string{"execute", "reconcile", "cleanup"} {
			ok(rt(`UPDATE runs SET lease_owner = 'w1', lease_purpose = '`+purpose+`', lease_expires_at = now() + interval '1 minute', lease_fence = lease_fence + 1 WHERE id = 'r1'`), purpose)
		}
		requireCode(t, rt(`UPDATE runs SET lease_owner = NULL, lease_expires_at = NULL WHERE id = 'r1'`), "23514", "release leaving a purpose")
		requireCode(t, rt(`UPDATE runs SET lease_purpose = NULL WHERE id = 'r1'`), "23514", "purpose without clearing the holder")
		ok(rt(`UPDATE runs SET lease_owner = NULL, lease_purpose = NULL, lease_expires_at = NULL WHERE id = 'r1'`), "release")
		// The bound is fixed at admission; attempts stay within it; a reason is a code.
		requireCode(t, rt(`UPDATE runs SET cleanup_max_attempts = 9 WHERE id = 'r1'`), "42501", "the runtime role cannot change the bound")
		requireCode(t, tryExec(t, e, `UPDATE runs SET cleanup_max_attempts = 0 WHERE id = 'r1'`), "23514", "bound below one")
		requireCode(t, rt(`UPDATE runs SET cleanup_attempts = 2 WHERE id = 'r1'`), "23514", "attempts beyond the default bound of one")
		ok(tryExec(t, e, `UPDATE runs SET cleanup_max_attempts = 2 WHERE id = 'r1'`), "set the bound as the owner")
		requireCode(t, rt(`UPDATE runs SET cleanup_attempts = 3 WHERE id = 'r1'`), "23514", "attempts beyond the bound")
		requireCode(t, rt(`UPDATE runs SET cleanup_state = 'retained', cleanup_attempts = 2, retained_resources = '[{"resource":{}}]' WHERE id = 'r1'`), "23514", "retained without a reason")
		requireCode(t, rt(`UPDATE runs SET cleanup_state = 'pending', cleanup_reason = 'Not A Code' WHERE id = 'r1'`), "23514", "reason that is not a code")
		requireCode(t, rt(`UPDATE runs SET cleanup_reason = 'cleanup_failed' WHERE id = 'r1'`), "23514", "a reason while no cleanup has started")
		ok(rt(`UPDATE runs SET cleanup_state = 'pending', cleanup_attempts = 1, cleanup_reason = 'cleanup_failed', retained_resources = '[{"resource":{}}]' WHERE id = 'r1'`), "unfinished cleanup")
		ok(rt(`UPDATE runs SET cleanup_state = 'retained', cleanup_attempts = 2, cleanup_reason = 'cleanup_exhausted' WHERE id = 'r1'`), "exhausted cleanup")
		requireCode(t, rt(`UPDATE runs SET cleanup_state = 'done', retained_resources = '[]' WHERE id = 'r1'`), "23514", "completed cleanup keeping its reason")
		ok(rt(`UPDATE runs SET cleanup_state = 'done', cleanup_reason = NULL, retained_resources = '[]' WHERE id = 'r1'`), "completed cleanup")
	})

	t.Run("commands", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		rt := func(sql string, args ...any) error { return runtimeExec(t, pool, tenantA, sql, args...) }
		owner := func(sql string) error { return tryExec(t, e, sql) }
		const insert = `INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, step_key, kind, target, spec_digest, fence, state, reason,
  result_code, result_exit_code, result_reason, binding_attempt, binding_backend, recorded_at) VALUES ('tenant-a', 'pa1', 'r1', `
		launch := func(id, extra string) string {
			return insert + `'` + id + `', 'build', 'launch_workload', 'wl-r1-build-1', 'sha256:spec', 2, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())` + extra
		}
		if err := rt(launch("c1", "")); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(launch("c1", "")), "23505", "a repeated command id")
		// Shape: a workload command carries its binding and names its step; a resource command none.
		requireCode(t, rt(insert+`'c2', 'build', 'launch_workload', 'wl', 'sha256:spec', 2, 'intent', NULL, NULL, NULL, NULL, NULL, NULL, now())`), "23514", "workload command without a binding")
		requireCode(t, rt(insert+`'c2', NULL, 'stop_workload', 'wl', 'sha256:spec', 2, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23514", "workload command without a step")
		requireCode(t, rt(insert+`'c2', NULL, 'ensure_resource', 'ws', 'sha256:ws', 2, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23514", "resource command with a binding")
		if err := rt(insert + `'c-ws', NULL, 'ensure_resource', 'ws', 'sha256:ws', 2, 'intent', NULL, NULL, NULL, NULL, NULL, NULL, now())`); err != nil {
			t.Fatalf("a resource command belongs to no step: %v", err)
		}
		requireCode(t, rt(insert+`'c2', 'build', 'explode_workload', 'wl', 'sha256:spec', 2, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23514", "unknown kind")
		requireCode(t, rt(insert+`'c2', 'build', 'launch_workload', 'wl', 'sha256:spec', 0, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23514", "zero fence")
		requireCode(t, rt(insert+`'c2', 'build', 'launch_workload', 'wl', 'sha256:spec', 2, 'rejected', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23514", "rejected without a reason")
		requireCode(t, rt(insert+`'c2', 'build', 'launch_workload', 'wl', 'sha256:spec', 2, 'intent', 'launch_uncertain', NULL, NULL, NULL, 1, 'process', now())`), "23514", "intent with a reason")
		requireCode(t, rt(insert+`'c2', 'build', 'launch_workload', 'wl', 'sha256:spec', 2, 'intent', NULL, 'succeeded', NULL, NULL, 1, 'process', now())`), "23514", "result on an unconfirmed command")
		requireCode(t, rt(insert+`'c2', 'nostep', 'launch_workload', 'wl', 'sha256:spec', 2, 'intent', NULL, NULL, NULL, NULL, 1, 'process', now())`), "23503", "a step of no run")
		// A command is bound to its own tenant's run.
		requireCode(t, runtimeExec(t, pool, tenantB, launch("c3", "")), "42501", "another tenant's run")

		// Settlement: intent to any outcome, uncertain to confirmed or rejected, no way out of the final states.
		requireCode(t, rt(`UPDATE run_commands SET state = 'uncertain' WHERE command_id = 'c1'`), "23514", "uncertain without a reason")
		if err := rt(`UPDATE run_commands SET state = 'uncertain', reason = 'launch_uncertain' WHERE command_id = 'c1'`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(`UPDATE run_commands SET reason = 'stop_uncertain' WHERE command_id = 'c1'`), "23514", "a reason change without settling")
		requireCode(t, rt(`UPDATE run_commands SET state = 'intent', reason = NULL WHERE command_id = 'c1'`), "23514", "back to intent")
		if err := rt(`UPDATE run_commands SET state = 'confirmed', reason = NULL WHERE command_id = 'c1'`); err != nil {
			t.Fatal(err)
		}
		for _, to := range []string{"rejected", "uncertain", "intent"} {
			requireCode(t, rt(`UPDATE run_commands SET state = '`+to+`', reason = 'launch_not_reached' WHERE command_id = 'c1'`), "23514", "confirmed to "+to)
		}
		// The result of a confirmed workload command is retained once.
		requireCode(t, rt(`UPDATE run_commands SET result_code = 'succeeded', result_exit_code = 3 WHERE command_id = 'c1'`), "23514", "a success with an exit code")
		if err := rt(`UPDATE run_commands SET result_code = 'failed', result_exit_code = 3, result_reason = 'command_failed' WHERE command_id = 'c1'`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(`UPDATE run_commands SET result_code = 'succeeded', result_exit_code = NULL, result_reason = NULL WHERE command_id = 'c1'`), "23514", "a recorded result changed")
		requireCode(t, rt(`UPDATE run_commands SET result_code = NULL, result_exit_code = NULL, result_reason = NULL WHERE command_id = 'c1'`), "23514", "a recorded result cleared")
		// Rejected is final too.
		if err := rt(launch("c4", "")); err != nil {
			t.Fatal(err)
		}
		if err := rt(`UPDATE run_commands SET state = 'rejected', reason = 'launch_not_reached' WHERE command_id = 'c4'`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(`UPDATE run_commands SET state = 'confirmed', reason = NULL WHERE command_id = 'c4'`), "23514", "rejected to confirmed")
		// Identity, target, specification, fence and binding never change: the runtime role has
		// no grant, and the guard holds for the owner as well.
		for _, set := range []string{"target = 'other'", "spec_digest = 'sha256:other'", "fence = 9", "kind = 'stop_workload'", "step_key = 'review'",
			"binding_attempt = 2", "binding_backend = 'podman'", "recorded_at = now()", "command_id = 'cx'"} {
			requireCode(t, rt(`UPDATE run_commands SET `+set+` WHERE command_id = 'c4'`), "42501", "runtime: "+set)
			requireCode(t, owner(`UPDATE run_commands SET `+set+` WHERE command_id = 'c4'`), "23514", "owner: "+set)
		}
		requireCode(t, rt(`DELETE FROM run_commands WHERE command_id = 'c4'`), "42501", "delete")
		requireCode(t, rt(`TRUNCATE run_commands`), "42501", "truncate")
	})

	t.Run("workload_observations", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		rt := func(sql string) error { return runtimeExec(t, pool, tenantA, sql) }
		owner := func(sql string) error { return tryExec(t, e, sql) }
		const insert = `INSERT INTO run_workload_observations (tenant_id, project_id, run_id, step_key, attempt, workload_id, spec_digest, backend, fence, phase,
  result_code, result_exit_code, result_reason, observed_at) VALUES ('tenant-a', 'pa1', 'r1', `
		if err := rt(insert + `'build', 1, 'wl-1', 'sha256:spec', 'process', 2, 'running', NULL, NULL, NULL, '2026-10-30T12:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(insert+`'build', 1, 'wl-1', 'sha256:spec', 'process', 2, 'running', NULL, NULL, NULL, now())`), "23505", "one observation per step attempt")
		requireCode(t, rt(insert+`'build', 0, 'wl-0', 'sha256:spec', 'process', 2, 'running', NULL, NULL, NULL, now())`), "23514", "attempt zero")
		requireCode(t, rt(insert+`'nostep', 1, 'wl-x', 'sha256:spec', 'process', 2, 'running', NULL, NULL, NULL, now())`), "23503", "a step of no run")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 2, 'exited', NULL, NULL, NULL, now())`), "23514", "exited without a result")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 2, 'running', 'failed', 1, NULL, now())`), "23514", "a running workload with a result")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 2, 'absent', 'lost', NULL, NULL, now())`), "23514", "an absent workload with a result")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 2, 'exited', 'succeeded', 1, NULL, now())`), "23514", "a success with an exit code")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 2, 'paused', NULL, NULL, NULL, now())`), "23514", "unknown phase")
		requireCode(t, rt(insert+`'build', 2, 'wl-2', 'sha256:spec', 'process', 0, 'running', NULL, NULL, NULL, now())`), "23514", "fence zero")
		requireCode(t, runtimeExec(t, pool, tenantB, insert+`'build', 3, 'wl-3', 'sha256:spec', 'process', 2, 'running', NULL, NULL, NULL, now())`), "42501", "another tenant's run")

		// Identity is fixed; fence and time never decrease.
		for _, set := range []string{"workload_id = 'wl-9'", "backend = 'podman'", "attempt = 2", "step_key = 'review'"} {
			requireCode(t, owner(`UPDATE run_workload_observations SET `+set), "23514", "owner: "+set)
		}
		requireCode(t, owner(`UPDATE run_workload_observations SET spec_digest = 'sha256:other'`), "23514", "owner: specification change")
		for _, set := range []string{"workload_id = 'wl-9'", "backend = 'podman'", "spec_digest = 'sha256:other'", "attempt = 2"} {
			requireCode(t, rt(`UPDATE run_workload_observations SET `+set), "42501", "runtime: "+set)
		}
		requireCode(t, rt(`UPDATE run_workload_observations SET fence = 1`), "23514", "fence decrease")
		requireCode(t, rt(`UPDATE run_workload_observations SET observed_at = '2026-10-30T11:59:59Z'`), "23514", "older observation")
		if err := rt(`UPDATE run_workload_observations SET fence = 3, observed_at = '2026-10-30T12:01:00Z'`); err != nil {
			t.Fatalf("a newer fence and time: %v", err)
		}
		// An exited workload keeps its result: it may be repeated or confirmed absent.
		if err := rt(`UPDATE run_workload_observations SET phase = 'exited', result_code = 'failed', result_exit_code = 2, result_reason = 'command_failed', observed_at = '2026-10-30T12:02:00Z'`); err != nil {
			t.Fatal(err)
		}
		for name, sql := range map[string]string{
			"running again":     `UPDATE run_workload_observations SET phase = 'running', result_code = NULL, result_exit_code = NULL, result_reason = NULL`,
			"unknown":           `UPDATE run_workload_observations SET phase = 'unknown', result_code = NULL, result_exit_code = NULL, result_reason = NULL`,
			"another exit code": `UPDATE run_workload_observations SET result_exit_code = 3`,
			"another code":      `UPDATE run_workload_observations SET result_code = 'lost'`,
			"another reason":    `UPDATE run_workload_observations SET result_reason = NULL`,
			"a cleared result":  `UPDATE run_workload_observations SET result_code = NULL, result_exit_code = NULL, result_reason = NULL`,
		} {
			requireCode(t, rt(sql), "23514", "exited then "+name)
		}
		if err := rt(`UPDATE run_workload_observations SET observed_at = '2026-10-30T12:03:00Z'`); err != nil {
			t.Fatalf("repeating an exited observation: %v", err)
		}
		if err := rt(`UPDATE run_workload_observations SET phase = 'absent', result_code = NULL, result_exit_code = NULL, result_reason = NULL, observed_at = '2026-10-30T12:04:00Z'`); err != nil {
			t.Fatalf("confirming an exited workload absent: %v", err)
		}
		requireCode(t, rt(`DELETE FROM run_workload_observations`), "42501", "delete")
	})

	t.Run("resource_observations", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		pool := e.runtime(t, 2)
		rt := func(sql string) error { return runtimeExec(t, pool, tenantA, sql) }
		owner := func(sql string) error { return tryExec(t, e, sql) }
		const insert = `INSERT INTO run_resource_observations (tenant_id, project_id, run_id, kind, name, identity, owner_project_id, lifetime, handle_run_id,
  fence, state, reason, observed_at) VALUES ('tenant-a', 'pa1', 'r1', `
		// A workspace is representable, and so is a service of each lifetime.
		for name, sql := range map[string]string{
			"workspace":      insert + `'workspace', 'workspace', 'r1-ws', 'pa1', 'pipeline', 'r1', 1, 'ready', NULL, '2026-10-30T12:00:00Z')`,
			"pipeline":       insert + `'service', 'db', 'r1-db', 'pa1', 'pipeline', 'r1', 1, 'starting', NULL, '2026-10-30T12:00:00Z')`,
			"project":        insert + `'service', 'cache', 'pa1-cache', 'pa1', 'project', NULL, 2, 'ready', NULL, '2026-10-30T12:00:00Z')`,
			"project by run": insert + `'service', 'queue', 'pa1-queue', 'pa1', 'project', 'r1', 2, 'ready', NULL, '2026-10-30T12:00:00Z')`,
			"tenant":         insert + `'service', 'registry', 'a-registry', NULL, 'tenant', NULL, 2, 'unknown', 'service_lost', '2026-10-30T12:00:00Z')`,
		} {
			if err := rt(sql); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		requireCode(t, rt(insert+`'service', 'db', 'r1-db', 'pa1', 'pipeline', 'r1', 1, 'ready', NULL, now())`), "23505", "one observation per resource")
		requireCode(t, rt(insert+`'workspace', 'ws2', 'r1-ws2', 'pa1', 'project', NULL, 1, 'ready', NULL, now())`), "23514", "a shared workspace")
		requireCode(t, rt(insert+`'service', 'a', 'a', 'pa1', 'pipeline', NULL, 1, 'ready', NULL, now())`), "23514", "a pipeline resource of no run")
		requireCode(t, rt(insert+`'service', 'b', 'b', 'pa1', 'pipeline', 'r2', 1, 'ready', NULL, now())`), "23514", "a pipeline resource of another run")
		requireCode(t, rt(insert+`'service', 'c', 'c', 'pa1', 'tenant', NULL, 1, 'ready', NULL, now())`), "23514", "a tenant resource owned by a project")
		requireCode(t, rt(insert+`'service', 'd', 'd', NULL, 'project', NULL, 1, 'ready', NULL, now())`), "23514", "a project resource owned by no project")
		requireCode(t, rt(insert+`'service', 'e', 'e', 'pa2', 'project', NULL, 1, 'ready', NULL, now())`), "23514", "another project's resource")
		requireCode(t, rt(insert+`'service', 'f', 'f', 'pa1', 'project', NULL, 1, 'broken', NULL, now())`), "23514", "unknown state")
		requireCode(t, rt(insert+`'service', 'g', 'g', 'pa1', 'project', NULL, 0, 'ready', NULL, now())`), "23514", "fence zero")
		requireCode(t, rt(insert+`'cron', 'h', 'h', 'pa1', 'project', NULL, 1, 'ready', NULL, now())`), "23514", "unknown kind")
		requireCode(t, runtimeExec(t, pool, tenantB, insert+`'service', 'i', 'i', 'pa1', 'project', NULL, 1, 'ready', NULL, now())`), "42501", "another tenant's run")

		for _, set := range []string{"lifetime = 'project'", "owner_project_id = NULL", "handle_run_id = NULL", "name = 'other'", "identity = 'other'", "kind = 'workspace'"} {
			requireCode(t, owner(`UPDATE run_resource_observations SET `+set+` WHERE name = 'db'`), "23514", "owner: "+set)
		}
		for _, set := range []string{"lifetime = 'project'", "owner_project_id = NULL", "handle_run_id = NULL", "name = 'other'", "identity = 'other'", "kind = 'workspace'"} {
			requireCode(t, rt(`UPDATE run_resource_observations SET `+set+` WHERE name = 'db'`), "42501", "runtime: "+set)
		}
		requireCode(t, rt(`UPDATE run_resource_observations SET fence = 0 WHERE name = 'db'`), "23514", "fence zero")
		if err := rt(`UPDATE run_resource_observations SET fence = 3, state = 'ready', observed_at = '2026-10-30T12:01:00Z' WHERE name = 'db'`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(`UPDATE run_resource_observations SET fence = 2 WHERE name = 'db'`), "23514", "fence decrease")
		requireCode(t, rt(`UPDATE run_resource_observations SET observed_at = '2026-10-30T12:00:30Z' WHERE name = 'db'`), "23514", "older observation")
		if err := rt(`UPDATE run_resource_observations SET state = 'removed', reason = NULL, observed_at = '2026-10-30T12:02:00Z' WHERE name = 'db'`); err != nil {
			t.Fatal(err)
		}
		requireCode(t, rt(`DELETE FROM run_resource_observations`), "42501", "delete")
	})

	t.Run("step_evidence", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		e.exec(t, `INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image) VALUES ('tenant-a', 'pa1', 'r1', 's5', 'test', 5, 'img')`)
		pool := e.runtime(t, 2)
		rt := func(sql string) error { return runtimeExec(t, pool, tenantA, sql) }
		owner := func(sql string) error { return tryExec(t, e, sql) }
		okf := func(err error, what string) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		// The when evaluation: unevaluated and evaluated-false differ, and a record is complete.
		requireCode(t, rt(`UPDATE run_steps SET when_result = true WHERE id = 's1'`), "23514", "a result with no evaluation")
		requireCode(t, rt(`UPDATE run_steps SET when_evaluated = true WHERE id = 's1'`), "23514", "an evaluation with no result")
		requireCode(t, rt(`UPDATE run_steps SET when_evaluated = true, when_result = true, when_input_digest = '', when_evaluated_at = now() WHERE id = 's1'`), "23514", "an empty digest")
		requireCode(t, rt(`UPDATE run_steps SET when_evaluated = true, when_result = true, when_input_digest = repeat('x', 129), when_evaluated_at = now() WHERE id = 's1'`), "23514", "an oversize digest")
		requireCode(t, rt(`UPDATE run_steps SET when_evaluated = true, when_result = false, when_input_digest = 'sha256:x', when_evaluated_at = now(), status = 'running' WHERE id = 's1'`), "23514", "a false evaluation that runs")
		okf(rt(`UPDATE run_steps SET when_evaluated = true, when_result = false, when_input_digest = 'sha256:x', when_evaluated_at = '2026-10-30T12:00:00Z' WHERE id = 's1'`), "record a false evaluation")
		// Write once: never changed, flipped or cleared, whoever writes.
		for _, set := range []string{"when_result = true", "when_input_digest = 'sha256:y'", "when_evaluated_at = now()",
			"when_evaluated = false, when_result = NULL, when_input_digest = NULL, when_evaluated_at = NULL"} {
			requireCode(t, rt(`UPDATE run_steps SET `+set+` WHERE id = 's1'`), "23514", "runtime: "+set)
			requireCode(t, owner(`UPDATE run_steps SET `+set+` WHERE id = 's1'`), "23514", "owner: "+set)
		}
		okf(rt(`UPDATE run_steps SET status = 'skipped', reason = 'condition_false', finished_at = now() WHERE id = 's1'`), "skip for condition_false")
		requireCode(t, rt(`UPDATE run_steps SET when_evaluated = true, when_result = false, when_input_digest = 'sha256:x', when_evaluated_at = now(), status = 'skipped', reason = 'dependency_failed', finished_at = now() WHERE id = 's5'`), "23514", "a false evaluation skipped for another reason")

		// Attempt: a container step only, never back, final once settled; the result agrees with the state.
		requireCode(t, rt(`UPDATE run_steps SET attempt = 1 WHERE id = 's2'`), "23514", "a wait with an attempt")
		requireCode(t, rt(`UPDATE run_steps SET attempt = 1 WHERE id = 's5'`), "23514", "a pending step with an attempt")
		okf(rt(`UPDATE run_steps SET status = 'running', attempt = 1 WHERE id = 's5'`), "start")
		okf(rt(`UPDATE run_steps SET attempt = 2 WHERE id = 's5'`), "retry")
		requireCode(t, rt(`UPDATE run_steps SET attempt = 1 WHERE id = 's5'`), "23514", "attempt back")
		requireCode(t, rt(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), result_code = 'failed' WHERE id = 's5'`), "23514", "a success with a failed result")
		requireCode(t, rt(`UPDATE run_steps SET status = 'failed', reason = 'command_failed', finished_at = now(), result_code = 'succeeded' WHERE id = 's5'`), "23514", "a failure with a succeeded result")
		requireCode(t, rt(`UPDATE run_steps SET status = 'failed', reason = 'command_failed', finished_at = now(), result_code = 'cancelled' WHERE id = 's5'`), "23514", "a failure with a cancelled result")
		requireCode(t, rt(`UPDATE run_steps SET result_code = 'failed' WHERE id = 's5'`), "23514", "a result before the step settles")
		requireCode(t, rt(`UPDATE run_steps SET status = 'failed', reason = 'command_failed', finished_at = now(), result_code = 'failed', result_reason = 'Bad Code' WHERE id = 's5'`), "23514", "a result reason that is not a code")
		requireCode(t, rt(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), result_code = 'succeeded', exit_code = 1 WHERE id = 's5'`), "23514", "a success with an exit code")
		okf(rt(`UPDATE run_steps SET status = 'failed', reason = 'command_failed', finished_at = now(), exit_code = 2, result_code = 'failed', result_reason = 'command_failed' WHERE id = 's5'`), "fail with a result")
		for _, set := range []string{"attempt = 3", "result_code = 'lost'", "result_reason = 'workload_lost'", "result_code = NULL, result_reason = NULL"} {
			requireCode(t, rt(`UPDATE run_steps SET `+set+` WHERE id = 's5'`), "23514", "settled step: "+set)
		}
		requireCode(t, rt(`UPDATE run_steps SET result_code = 'failed' WHERE id = 's2'`), "23514", "a wait with a workload result")
		// Admitted shape stays outside the runtime grants.
		requireCode(t, rt(`UPDATE run_steps SET step_kind = 'wait' WHERE id = 's5'`), "42501", "step kind")
		requireCode(t, rt(`UPDATE run_steps SET image = 'x' WHERE id = 's5'`), "42501", "image")
	})

	t.Run("pre_open_waits", func(t *testing.T) {
		e := newEnv(t)
		seedPipelineRuntime(t, e)
		for i, id := range []string{"s3", "s4", "s7"} {
			addWaitStep(t, e, id, 2+i)
		}
		pool := e.runtime(t, 2)
		rt := func(sql string) error { return runtimeExec(t, pool, tenantA, sql) }
		owner := func(sql string) error { return tryExec(t, e, sql) }
		okf := func(err error, what string) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		// Identity, version and assignment are recorded before the wait opens, together.
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r' WHERE id = 's2'`), "23514", "identity without a version")
		requireCode(t, rt(`UPDATE run_steps SET wait_version = 1 WHERE id = 's2'`), "23514", "version without an identity")
		requireCode(t, rt(`UPDATE run_steps SET wait_assignment = 'reviewer' WHERE id = 's2'`), "23514", "assignment without an identity")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_assignment = '' WHERE id = 's2'`), "23514", "an empty assignment")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_correlation = '{"c":"1"}' WHERE id = 's2'`), "23514", "values before it opens")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_deadline_at = now() WHERE id = 's2'`), "23514", "a deadline before it opens")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_outcome = 'approved' WHERE id = 's2'`), "23514", "a decision before it opens")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_reason = 'run_cancelled' WHERE id = 's2'`), "23514", "a cancellation reason before a cancellation")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1 WHERE id = 's1'`), "23514", "a container step with a wait")
		okf(rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1, wait_assignment = 'reviewer' WHERE id = 's2'`), "record the wait at admission")
		okf(rt(`UPDATE run_steps SET wait_id = 'w-3', wait_version = 1 WHERE id = 's3'`), "record an unassigned wait")
		okf(rt(`UPDATE run_steps SET wait_id = 'w-4', wait_version = 1, wait_assignment = 'ops' WHERE id = 's4'`), "record a third wait")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-r', wait_version = 1 WHERE id = 's7'`), "23505", "a reused wait identity")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = 'w-other', wait_version = 1 WHERE id = 's2'`), "23514", "identity changed once recorded")
		requireCode(t, rt(`UPDATE run_steps SET wait_id = NULL, wait_version = NULL WHERE id = 's2'`), "23514", "the recorded identity cleared")
		requireCode(t, rt(`UPDATE run_steps SET wait_version = 0 WHERE id = 's2'`), "23514", "version zero")
		// Reassignment before the wait opens is one version-checked step.
		requireCode(t, rt(`UPDATE run_steps SET wait_assignment = 'operator' WHERE id = 's2'`), "23514", "reassignment without a new version")
		requireCode(t, rt(`UPDATE run_steps SET wait_assignment = 'operator', wait_version = 1 WHERE id = 's2'`), "23514", "reassignment keeping the version")
		okf(rt(`UPDATE run_steps SET wait_assignment = 'operator', wait_version = 2 WHERE id = 's2'`), "reassign")

		// Opening: the full shape is still required, and the version rises.
		open := func(step, version string, overrides ...string) string {
			return openWaitSQL(step, "w-"+step, "fail", append([]string{"wait_version", version}, overrides...)...)
		}
		requireCode(t, rt(strings.ReplaceAll(open("s2", "2"), "'w-s2'", "'w-r'")), "23514", "opening without raising the version")
		for name, o := range map[string][]string{
			"no correlation": {"wait_correlation", `NULL`},
			"no deadline":    {"wait_deadline_at", `NULL`},
			"no input":       {"wait_input", `NULL`},
			"no outcomes":    {"wait_outcomes", `NULL`},
			"no expiry":      {"wait_on_expiry", `NULL`},
		} {
			requireCode(t, rt(strings.ReplaceAll(open("s2", "3", o...), "'w-s2'", "'w-r'")), "23514", "open with "+name)
		}
		requireCode(t, rt(`UPDATE run_steps SET status = 'waiting', wait_state = 'waiting', wait_version = 3 WHERE id = 's2'`), "23514", "waiting with no correlation, input or deadline")
		requireCode(t, rt(`UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'expired', wait_decided_at = now(), wait_version = 3 WHERE id = 's2'`), "23514", "expired without ever opening")
		requireCode(t, rt(`UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_decided_at = now(), wait_version = 3 WHERE id = 's2'`), "23514", "resolved without ever opening")
		okf(rt(strings.ReplaceAll(open("s2", "3"), "'w-s2'", "'w-r'")), "open the wait")
		// An opened wait keeps its content; every change raises the version.
		requireCode(t, rt(`UPDATE run_steps SET wait_correlation = '{"candidate":"other"}', wait_version = 4 WHERE id = 's2'`), "23514", "correlation changed")
		requireCode(t, rt(`UPDATE run_steps SET wait_assignment = 'ops' WHERE id = 's2'`), "23514", "reassignment without a version")

		// Cancellation before opening: allowed with no values, never with any, and always with a reason.
		cancel := func(step, version string, extra string) string {
			return `UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'cancelled', wait_decided_at = now(), wait_version = ` + version + extra + ` WHERE id = '` + step + `'`
		}
		requireCode(t, rt(cancel("s3", "2", ``)), "23514", "a cancellation with no reason")
		requireCode(t, rt(cancel("s3", "2", `, wait_reason = 'run_finished'`)), "23514", "an unknown cancellation reason")
		requireCode(t, rt(cancel("s3", "2", `, wait_reason = 'run_cancelled', wait_actor_ref = 'user-1'`)), "23514", "a cancellation naming an actor")
		requireCode(t, rt(cancel("s3", "2", `, wait_reason = 'run_cancelled', wait_outcome = 'approved'`)), "23514", "a cancellation with an outcome")
		requireCode(t, rt(cancel("s3", "2", `, wait_reason = 'run_cancelled', wait_deadline_at = now()`)), "23514", "a never-opened cancellation with a deadline")
		requireCode(t, rt(cancel("s3", "2", `, wait_reason = 'run_cancelled', wait_correlation = '{"c":"1"}', wait_input = 'approval', wait_outcomes = '["a"]', wait_on_expiry = 'fail', wait_deadline_at = now()`)), "23514", "opening and cancelling in one statement")
		requireCode(t, rt(cancel("s3", "1", `, wait_reason = 'run_cancelled'`)), "23514", "a cancellation without raising the version")
		requireCode(t, rt(`UPDATE run_steps SET status = 'running', wait_state = 'cancelled', wait_decided_at = now(), wait_reason = 'run_cancelled', wait_version = 2 WHERE id = 's3'`), "23514", "a cancelled wait of a running step")
		okf(rt(cancel("s3", "2", `, wait_reason = 'run_cancelled'`)), "cancel before opening")
		// A decided wait is immutable, including its reason.
		for name, set := range map[string]string{
			"reason":      `wait_reason = 'run_failed', wait_version = 3`,
			"reopen":      `wait_state = 'waiting', status = 'waiting', wait_version = 3`,
			"assignment":  `wait_assignment = 'ops', wait_version = 3`,
			"version":     `wait_version = 1`,
			"cleared":     `wait_version = NULL`,
			"identity":    `wait_id = 'w-x', wait_version = 3`,
			"content":     `wait_correlation = '{"c":"1"}', wait_version = 3`,
			"unversioned": `wait_decided_at = now()`,
		} {
			requireCode(t, rt(`UPDATE run_steps SET `+set+` WHERE id = 's3'`), "23514", "runtime, decided wait: "+name)
			requireCode(t, owner(`UPDATE run_steps SET `+set+` WHERE id = 's3'`), "23514", "owner, decided wait: "+name)
		}
		// The step that never recorded a wait can still skip without one.
		okf(rt(`UPDATE run_steps SET status = 'skipped', reason = 'condition_false', finished_at = now(), when_evaluated = true, when_result = false, when_input_digest = 'sha256:x', when_evaluated_at = now() WHERE id = 's4'`), "skip a pending wait")
		// An opened wait that is cancelled keeps the rule that it carried its content.
		addWaitStep(t, e, "s6", 6)
		okf(rt(`UPDATE run_steps SET wait_id = 'w-6', wait_version = 1 WHERE id = 's6'`), "record")
		okf(rt(openWaitSQL("s6", "w-6", "continue", "wait_version", "2")), "open")
		okf(rt(`UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'cancelled', wait_decided_at = now(), wait_reason = 'run_failed', wait_version = 3 WHERE id = 's6'`), "cancel an open wait")
	})

	t.Run("round_trip_is_tenant_isolated_for_a_non_owner", func(t *testing.T) {
		e := newEnv(t)
		seedPipeline(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)
		var super, bypass bool
		must(t, pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass))
		if super || bypass {
			t.Fatalf("the runtime role is privileged: super=%v bypass=%v", super, bypass)
		}
		for _, table := range []string{"run_commands", "run_workload_observations", "run_resource_observations"} {
			var enabled, forced bool
			must(t, e.mig.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&enabled, &forced))
			if !enabled || !forced {
				t.Errorf("%s: rls enabled=%v forced=%v", table, enabled, forced)
			}
		}
		recA := midFlightRecord(t)
		must(t, storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return saveRecord(ctx, tx, recA) }))

		// Tenant B holds the same project key and the same run id; it never sees tenant A's rows.
		recB := midFlightRecord(t)
		scopeB := domain.Scope{TenantID: tenantB, ProjectID: "pb1"}
		recB.Run.Scope, recB.Run.PipelineID = scopeB, "plb"
		recB.Claim.Scope = scopeB
		for i := range recB.Waits {
			recB.Waits[i].Scope = scopeB
		}
		for i := range recB.Commands {
			recB.Commands[i].Scope = scopeB
			recB.Commands[i].Binding.Scope = scopeB
		}
		for i := range recB.Workloads {
			recB.Workloads[i].Binding.Scope = scopeB
		}
		for i := range recB.Resources {
			h := &recB.Resources[i].Handle
			h.Scope = scopeB
			h.Owner = runtime.OwnerFor(h.Lifetime, scopeB)
		}
		must(t, storage.WithTenant(ctx, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error { return saveRecord(ctx, tx, recB) }))
		for _, tc := range []struct {
			tenant domain.TenantID
			scope  domain.Scope
		}{{tenantA, durableScope}, {tenantB, scopeB}} {
			must(t, storage.WithTenant(ctx, pool, tc.tenant, func(ctx context.Context, tx pgx.Tx) error {
				for _, table := range []string{"run_commands", "run_workload_observations", "run_resource_observations"} {
					var n, foreign int
					if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM `+table, string(tc.tenant)).Scan(&n, &foreign); err != nil {
						return err
					}
					if n == 0 || foreign != 0 {
						t.Errorf("%s sees %d rows of %s, %d foreign", tc.tenant, n, table, foreign)
					}
				}
				got, err := loadRecord(ctx, tx, tc.scope, "run1")
				if err != nil {
					return err
				}
				if got.Run.Scope != tc.scope || got.Claim.Scope != tc.scope {
					t.Errorf("%s read a record of %v", tc.tenant, got.Run.Scope)
				}
				return nil
			}))
		}
		// Another tenant's run cannot be read by naming it, nor extended by writing a row for it.
		err := storage.WithTenant(ctx, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := loadRecord(ctx, tx, durableScope, "run1")
			return err
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("reading another tenant's run: %v", err)
		}
		foreignInsert := func(table, sql string) {
			requireCode(t, runtimeExec(t, pool, tenantB, sql), "42501", "tenant b writing "+table+" of tenant a")
		}
		foreignInsert("run_commands", `INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, kind, target, fence, recorded_at)
VALUES ('tenant-a', 'pa1', 'run1', 'smuggled', 'ensure_resource', 'ws', 1, now())`)
		foreignInsert("run_workload_observations", `INSERT INTO run_workload_observations (tenant_id, project_id, run_id, step_key, attempt, workload_id, spec_digest, backend, fence, phase, observed_at)
VALUES ('tenant-a', 'pa1', 'run1', 'build', 7, 'wl', 'sha256:spec', 'process', 1, 'running', now())`)
		foreignInsert("run_resource_observations", `INSERT INTO run_resource_observations (tenant_id, project_id, run_id, kind, name, identity, owner_project_id, lifetime, handle_run_id, fence, state, observed_at)
VALUES ('tenant-a', 'pa1', 'run1', 'service', 'smuggled', 'smuggled', 'pa1', 'project', NULL, 1, 'ready', now())`)
		// A tenant cannot move another tenant's rows: its own rows are the only ones the update can reach.
		for _, sql := range []string{
			`UPDATE run_commands SET state = 'rejected', reason = 'launch_not_reached' WHERE project_id = 'pa1'`,
			`UPDATE run_workload_observations SET observed_at = now() WHERE project_id = 'pa1'`,
			`UPDATE run_resource_observations SET state = 'removed', observed_at = now() WHERE project_id = 'pa1'`,
		} {
			must(t, storage.WithTenant(ctx, pool, tenantB, func(ctx context.Context, tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, sql)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 0 {
					t.Errorf("tenant b updated %d rows of tenant a: %s", tag.RowsAffected(), sql)
				}
				return nil
			}))
		}
		// Without a tenant context nothing is visible, and the runtime role can neither delete nor truncate.
		for _, table := range []string{"run_commands", "run_workload_observations", "run_resource_observations"} {
			var n int
			must(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
			if n != 0 {
				t.Errorf("no tenant context sees %d rows of %s", n, table)
			}
			requireCode(t, runtimeExec(t, pool, tenantA, `DELETE FROM `+table), "42501", "delete "+table)
			requireCode(t, runtimeExec(t, pool, tenantA, `TRUNCATE `+table), "42501", "truncate "+table)
		}
	})

	t.Run("a_failed_transition_leaves_no_partial_row", func(t *testing.T) {
		e := newEnv(t)
		seedPipeline(t, e)
		pool := e.runtime(t, 2)
		ctx := testCtx(t)
		rec := midFlightRecord(t)
		must(t, storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error { return saveRecord(ctx, tx, rec) }))
		snapshot := func() string {
			var got runtime.RunRecord
			must(t, storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) (err error) {
				got, err = loadRecord(ctx, tx, durableScope, "run1")
				return err
			}))
			return mustJSON(canonical(got))
		}
		before := snapshot()
		counts := func() (n [3]int) {
			for i, table := range []string{"run_commands", "run_workload_observations", "run_resource_observations"} {
				must(t, e.mig.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n[i]))
			}
			return n
		}
		rows := counts()

		// A transition: settle the uncertain launch, observe the workload, advance the step, record a
		// resource, then a final write the database refuses (a decreasing fence). All of it is rolled back.
		boom := errors.New("transition refused")
		err := storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			steps := []string{
				`UPDATE run_commands SET state = 'confirmed', reason = NULL WHERE command_id = 'c-test-1'`,
				`INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, step_key, kind, target, spec_digest, fence, recorded_at, binding_attempt, binding_backend)
VALUES ('tenant-a', 'pa1', 'run1', 'c-new', 'test', 'stop_workload', 'wl-run1-test-1', 'sha256:spec', 3, now(), 1, 'process')`,
				`UPDATE run_workload_observations SET phase = 'running', observed_at = '2026-10-30T12:09:00Z' WHERE step_key = 'test'`,
				`INSERT INTO run_resource_observations (tenant_id, project_id, run_id, kind, name, identity, owner_project_id, lifetime, handle_run_id, fence, state, observed_at)
VALUES ('tenant-a', 'pa1', 'run1', 'service', 'extra', 'run1-extra', 'pa1', 'pipeline', 'run1', 3, 'ready', now())`,
				`UPDATE run_steps SET status = 'failed', reason = 'workload_lost', finished_at = now(), result_code = 'lost', result_reason = 'workload_lost', version = version + 1 WHERE step_key = 'test'`,
				`UPDATE run_resource_observations SET fence = 0 WHERE name = 'db'`, // refused: the transition fails here
			}
			for i, s := range steps {
				if _, err := tx.Exec(ctx, s); err != nil {
					if i != len(steps)-1 {
						t.Errorf("step %d of the transition failed early: %v", i, err)
					}
					requireCode(t, err, "23514", "the refused write")
					return boom
				}
			}
			return nil
		})
		if !errors.Is(err, boom) {
			t.Fatalf("transition: %v", err)
		}
		if after := snapshot(); after != before {
			t.Errorf("a failed transition changed the record:\nbefore %s\n after %s", before, after)
		}
		if got := counts(); got != rows {
			t.Errorf("row counts %v after the failed transition, were %v", got, rows)
		}
		// A decided wait refuses a later write, whoever asks, and the statements before it roll back.
		err = storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, kind, target, fence, recorded_at)
VALUES ('tenant-a', 'pa1', 'run1', 'c-late', 'remove_resource', 'run1-ws', 3, now())`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE run_steps SET wait_consumed_signal_id = 'sig-missing', wait_version = wait_version + 1 WHERE step_key = 'review'`)
			return err
		})
		requireCode(t, err, "23514", "a write to a decided wait")
		if got := counts(); got != rows || snapshot() != before {
			t.Errorf("a refused write left a partial row: counts %v, were %v", got, rows)
		}
		// A refusal that arrives only at commit (the deferred reference from a decision to its signal
		// record) leaves nothing behind either: the command, the opened wait and the decision all roll back.
		err = storage.WithTenant(ctx, pool, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			for _, sql := range []string{
				`INSERT INTO run_commands (tenant_id, project_id, run_id, command_id, kind, target, fence, recorded_at)
VALUES ('tenant-a', 'pa1', 'run1', 'c-late', 'remove_resource', 'run1-ws', 3, now())`,
				openWaitSQL("s-approve", "w-approve", "fail", "wait_version", "2"),
				`UPDATE run_steps SET status = 'succeeded', finished_at = now(), wait_state = 'resolved', wait_outcome = 'approved', wait_actor_ref = 'user-1',
  wait_decided_at = now(), wait_consumed_signal_id = 'sig-missing', wait_version = 3 WHERE tenant_id = 'tenant-a' AND run_id = 'run1' AND id = 's-approve'`,
			} {
				if _, err := tx.Exec(ctx, sql); err != nil {
					t.Fatalf("statement before commit: %v", err)
				}
			}
			return nil
		})
		requireCode(t, err, "23503", "a decision whose signal record does not exist, refused at commit")
		if got := counts(); got != rows || snapshot() != before {
			t.Errorf("a commit-time refusal left a partial row: counts %v, were %v", got, rows)
		}
	})

	t.Run("migration_005_rolls_back_whole_and_upgrades_a_004_database", func(t *testing.T) {
		_, pool, opts := priorSchema(t)
		ctx := testCtx(t)
		base := storage.BaseContribution().Migrations
		upTo004, err := storage.NewManifest(storage.Contribution{Module: "storage", Migrations: base[:4]})
		must(t, err)
		if _, err := storage.Migrate(ctx, pool, upTo004, opts); err != nil {
			t.Fatalf("migrate to 004: %v", err)
		}
		// A database written by the 004 schema: a held lease, an exhausted cleanup and a wait
		// that was cancelled, none of which could record a purpose, a reason or a cancellation reason.
		_, err = pool.Exec(ctx, `
INSERT INTO pipelines (tenant_id, project_id, id, name, source, definition) VALUES ('tenant-a', 'pa1', 'pl1', 'sdlc', 'api', '{}');
INSERT INTO runs (tenant_id, project_id, id, pipeline_id, definition_snapshot, lease_owner, lease_expires_at, lease_fence,
                  cleanup_state, cleanup_attempts, retained_resources) VALUES
  ('tenant-a', 'pa1', 'old1', 'pl1', '{}', 'worker-old', now() + interval '1 minute', 4, 'pending', 3, '[]'),
  ('tenant-a', 'pa1', 'old2', 'pl1', '{}', NULL, NULL, 0, 'retained', 2, '["svc"]'),
  ('tenant-a', 'pa1', 'old3', 'pl1', '{}', NULL, NULL, 0, 'none', 0, '[]');
INSERT INTO run_steps (tenant_id, project_id, run_id, id, step_key, ordinal, image, step_kind) VALUES
  ('tenant-a', 'pa1', 'old3', 'w1', 'review', 0, NULL, 'wait'), ('tenant-a', 'pa1', 'old3', 'w2', 'gate', 1, NULL, 'wait');
UPDATE run_steps SET status = 'waiting', wait_id = 'wid1', wait_assignment = 'reviewer', wait_correlation = '{"c":"1"}', wait_input = 'approval',
  wait_outcomes = '["approved"]', wait_on_expiry = 'fail', wait_deadline_at = now() + interval '1 hour', wait_state = 'waiting', wait_version = 1 WHERE id = 'w1';
UPDATE run_steps SET status = 'cancelled', finished_at = now(), wait_state = 'cancelled', wait_decided_at = now(), wait_version = 2,
  wait_id = 'wid2', wait_correlation = '{"c":"1"}', wait_input = 'approval', wait_outcomes = '["approved"]', wait_on_expiry = 'fail',
  wait_deadline_at = now() + interval '1 hour' WHERE id = 'w2'`)
		must(t, err)
		snapshot := func() (string, int, bool) {
			var s string
			must(t, pool.QueryRow(ctx, `SELECT coalesce(string_agg(id || ':' || lease_fence || ':' || cleanup_attempts, ',' ORDER BY id), '') FROM runs`).Scan(&s))
			return s, applied(t, pool), tableExists(t, pool, "run_commands")
		}
		beforeRuns, beforeLedger, _ := snapshot()

		// A failure anywhere in 005 leaves the database as it was: the same ledger, no new object,
		// no backfill.
		broken := append([]storage.Migration(nil), base...)
		broken[4] = storage.Migration{ID: durableMigration, SQL: base[4].SQL + "\nSELECT 1 / 0;\n"}
		brokenManifest, err := storage.NewManifest(storage.Contribution{Module: "storage", Migrations: broken})
		must(t, err)
		_, err = storage.Migrate(ctx, pool, brokenManifest, opts)
		requireCode(t, err, "22012", "a failure at the end of 005")
		runs, ledger, created := snapshot()
		if runs != beforeRuns || ledger != beforeLedger || created || columnExists(t, pool, "runs", "lease_purpose") || columnExists(t, pool, "run_steps", "wait_reason") {
			t.Errorf("a failed 005 left changes behind: runs %q ledger %d created %v", runs, ledger, created)
		}

		// Applied cleanly, it maps the old rows and drops nothing.
		res, err := storage.Migrate(ctx, pool, fullManifest(t), opts)
		if err != nil || len(res.Applied) != 1 || res.Applied[0] != durableMigration {
			t.Fatalf("migrate 005: %+v %v", res, err)
		}
		type held struct {
			purpose, reason *string
			max, attempts   int
		}
		get := func(id string) held {
			var h held
			must(t, pool.QueryRow(ctx, `SELECT lease_purpose, cleanup_reason, cleanup_max_attempts, cleanup_attempts FROM runs WHERE id = $1`, id).Scan(&h.purpose, &h.reason, &h.max, &h.attempts))
			return h
		}
		if h := get("old1"); h.purpose == nil || *h.purpose != "execute" || h.max != 3 || h.attempts != 3 || h.reason != nil {
			t.Errorf("held lease: %+v", h)
		}
		if h := get("old2"); h.purpose != nil || h.reason == nil || *h.reason != "cleanup_exhausted" || h.max != 2 {
			t.Errorf("retained cleanup: %+v", h)
		}
		if h := get("old3"); h.purpose != nil || h.reason != nil || h.max != 1 {
			t.Errorf("untouched run: %+v", h)
		}
		var reason *string
		must(t, pool.QueryRow(ctx, `SELECT wait_reason FROM run_steps WHERE id = 'w2'`).Scan(&reason))
		if reason == nil || *reason != "run_cancelled" {
			t.Errorf("cancelled wait reason: %v", reason)
		}
		var open, wid *string
		must(t, pool.QueryRow(ctx, `SELECT wait_reason, wait_id FROM run_steps WHERE id = 'w1'`).Scan(&open, &wid))
		if open != nil || wid == nil || *wid != "wid1" {
			t.Errorf("an open wait is untouched: %v %v", open, wid)
		}
		// The ledger and the legacy columns are as they were.
		if n := applied(t, pool); n != 5 {
			t.Errorf("ledger has %d rows, want 5", n)
		}
		for _, c := range [][2]string{{"projects", "task_workflow"}, {"tasks", "state"}, {"tasks", "parked_at"}, {"tasks", "resume_state"}, {"tasks", "blocked_by_id"}, {"runs", "cleanup_pending"}} {
			if !columnExists(t, pool, c[0], c[1]) {
				t.Errorf("legacy column %s.%s is gone", c[0], c[1])
			}
		}
	})
}
