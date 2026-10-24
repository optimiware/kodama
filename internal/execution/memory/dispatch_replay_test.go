package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// TestUnitDispatchReplay proves repeated and recovered dispatch keeps attempt
// and action identity and that unknown external effects are handled safely:
// PX-06 (dispatch rechecks everything it depends on) and PX-07 (bounded,
// classified retry; unknown outcomes reconciled first), on the in-memory store
// with a recording runtime double and controlled time.
func TestUnitDispatchReplay(t *testing.T) {
	ctx := context.Background()
	tenant := scopeA.TenantID

	t.Run("a claim is one attempt with a stable action key", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		in := m.MustClaim(tenant)
		if in.ActionKey != execution.ActionKey(run.ID, "job") || in.Ordinal != 1 || in.RunID != run.ID {
			t.Fatalf("intent %+v", in)
		}
		for i := 0; i < 3; i++ {
			if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("claim %d of a running step: %v", i, err)
			}
		}
		if n := attempts(m, run); n != 1 || held(m) != 1 {
			t.Fatalf("%d attempts, %d reservations", n, held(m))
		}
	})

	t.Run("a dispatch starts the committed identity once and never again", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		if ok, err := m.Svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := m.Get(run)
		at := agg.Attempts[0]
		starts := m.Workloads.Starts()
		if len(starts) != 1 || starts[0].AttemptID != at.ID || starts[0].WorkloadName != at.WorkloadName ||
			starts[0].SpecFingerprint != at.SpecFingerprint || starts[0].ActionKey != at.ActionKey || starts[0].Redrive {
			t.Fatalf("starts %+v for attempt %+v", starts, at)
		}
		if agg.Control(at.ID).DispatchConfirmedAt == nil {
			t.Fatal("the confirmed start was not recorded")
		}
		if ok, err := m.Svc.DispatchNext(ctx, tenant); err != nil || ok {
			t.Fatalf("a second dispatch claimed work: %v %v", ok, err)
		}
		m.Advance(10 * time.Second)
		rep, err := m.Svc.Reconcile(ctx)
		if err != nil || rep.Redriven != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		if n := len(m.Workloads.Starts()); n != 1 {
			t.Fatalf("a confirmed attempt was started %d times", n)
		}
	})

	for name, fault := range map[string]func(*executiontest.Mem, error){
		"a start that never reached the runtime": func(m *executiontest.Mem, err error) {
			m.Workloads.Before(runtimetest.OpStart, func(context.Context, runtimetest.Call) error { return err })
		},
		"a start whose acknowledgment was lost": func(m *executiontest.Mem, err error) {
			m.Workloads.After(runtimetest.OpStart, func(context.Context, runtimetest.Call) error { return err })
		},
	} {
		t.Run(name+" is redriven once with the same identity", func(t *testing.T) {
			m, p := fixture(t)
			run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
			fault(m, errors.New("lost"))
			if ok, err := m.Svc.DispatchNext(ctx, tenant); err != nil || !ok {
				t.Fatalf("dispatch: %v %v", ok, err)
			}
			at := m.Get(run).Attempts[0]
			if m.Get(run).Control(at.ID).DispatchConfirmedAt != nil {
				t.Fatal("a failed start was recorded as confirmed")
			}
			m.Workloads.Before(runtimetest.OpStart, nil)
			m.Workloads.After(runtimetest.OpStart, nil)
			ref := execution.RunRef{Scope: scopeA, ID: run.ID}
			if rep, err := m.Svc.ReconcileRun(ctx, ref); err != nil || rep.Redriven != 0 {
				t.Fatalf("redrove inside the claim lease: %+v %v", rep, err)
			}
			m.Advance(5 * time.Second) // past the lease, inside the startup budget
			other := m.NewService(executiontest.MemOptions{})
			rep, err := m.Svc.ReconcileRun(ctx, ref)
			if err != nil || rep.Redriven != 1 {
				t.Fatalf("redrive: %+v %v", rep, err)
			}
			// A second controller finds the dispatch confirmed and starts nothing.
			if rep, err := other.ReconcileRun(ctx, ref); err != nil || rep.Redriven != 0 {
				t.Fatalf("second controller: %+v %v", rep, err)
			}
			starts := m.Workloads.Starts()
			if len(starts) != 2 || !starts[1].Redrive || starts[0].Redrive {
				t.Fatalf("starts %+v", starts)
			}
			for _, s := range starts {
				if s.AttemptID != at.ID || s.WorkloadName != at.WorkloadName || s.SpecFingerprint != at.SpecFingerprint || s.ActionKey != at.ActionKey || s.ContextDigest != at.ContextDigest {
					t.Fatalf("redrive changed the identity: %+v vs %+v", s, at)
				}
			}
			agg := m.Get(run)
			if len(agg.Attempts) != 1 || agg.Control(at.ID).DispatchConfirmedAt == nil || agg.Step("job").AttemptsStarted != 1 {
				t.Fatalf("redrive left %s", agg)
			}
		})
	}

	t.Run("dispatch checks dependencies, authority, limits and writer evidence under the claim", func(t *testing.T) {
		m, p := fixture(t)
		def := define(m, job("build", readRun), job("test", "needs: [build]", readRun))
		run := m.MustAdmit(p, def, nil)
		build := m.MustClaim(tenant)
		if build.StepID != "build" {
			t.Fatalf("claimed %s before its dependency", build.StepID)
		}
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a step was claimed while its dependency ran: %v", err)
		}
		m.Start(build)
		if _, err := m.Succeed(build, nil, ""); err != nil {
			t.Fatal(err)
		}

		// Current authorization is checked inside the claim: a revoked requester
		// dispatches nothing and consumes no attempt or capacity.
		m.Auth.Revoke(scopeA, executiontest.Engineer)
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("claim by a revoked requester: %v", err)
		}
		m.Auth.SetErr(errors.New("identity down"))
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("claim during an authority outage: %v", err)
		}
		m.Auth.SetErr(nil)
		m.Auth.Grant(scopeA, executiontest.Engineer)
		// Limits are required: missing limits deny dispatch.
		m.Limits.Fail(errors.New("no policy"))
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("claim without limits: %v", err)
		}
		m.Limits.Fail(nil)
		if n := attempts(m, run); n != 1 || held(m) != 0 {
			t.Fatalf("refused claims left %d attempts and %d reservations", n, held(m))
		}
		next := m.MustClaim(tenant)
		if next.StepID != "test" || next.Ordinal != 1 {
			t.Fatalf("recovered claim %+v", next)
		}
	})

	t.Run("a writer is admitted only against the accepted tip", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("edit", writeRun)), nil)
		// The remote branch moved since admission: the claim transaction refuses
		// the stale evidence, leaving no attempt, reservation or writer slot.
		m.World.SetRemoteTip(string(p.Workspace), executiontest.CommitB)
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a writer was admitted on a stale tip: %v", err)
		}
		if n := attempts(m, run); n != 0 || held(m) != 0 || writerOf(m, p) != "" || m.Get(run).Step("edit").AttemptsStarted != 0 {
			t.Fatalf("a refused claim leaked state: %d attempts, %d reservations, writer %q", n, held(m), writerOf(m, p))
		}
		m.World.SetRemoteTip(string(p.Workspace), executiontest.CommitA)
		in := m.MustClaim(tenant)
		if in.Ordinal != 1 || writerOf(m, p) != in.AttemptID || held(m) != 1 {
			t.Fatalf("intent %+v writer %q reservations %d", in, writerOf(m, p), held(m))
		}
		if at := m.Get(run).Attempts[0]; at.InputCommit != executiontest.CommitA || at.ExpectedTip != executiontest.CommitA {
			t.Fatalf("writer evidence %q %q", at.InputCommit, at.ExpectedTip)
		}
	})

	t.Run("retry is bounded and backed off by persisted time and keeps the action key", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun, "retry: {maxAttempts: 3, backoffSeconds: 45, on: [infrastructure, transient]}")), nil)
		var key string
		names := map[string]bool{}
		for ordinal := 1; ordinal <= 3; ordinal++ {
			in := m.MustClaim(tenant)
			if in.Ordinal != ordinal {
				t.Fatalf("ordinal %d, want %d", in.Ordinal, ordinal)
			}
			if key == "" {
				key = in.ActionKey
			}
			if in.ActionKey != key || names[in.WorkloadName] {
				t.Fatalf("attempt %d: action key %q (want %q), workload %q repeated=%v", ordinal, in.ActionKey, key, in.WorkloadName, names[in.WorkloadName])
			}
			names[in.WorkloadName] = true
			infraFail(t, m, in)
			agg := m.Get(run)
			st := agg.Step("job")
			if ordinal == 3 {
				break
			}
			if st.Status != execution.StepPending || st.NextAttemptAt == nil || !st.NextAttemptAt.Equal(m.Clock.Now().Add(45*time.Second)) {
				t.Fatalf("after attempt %d: step %s next %v", ordinal, st.Status, st.NextAttemptAt)
			}
			if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("claimed during backoff: %v", err)
			}
			// A restarted controller reads the same persisted backoff.
			if _, err := m.NewService(executiontest.MemOptions{}).ClaimRunnable(ctx, tenant); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("a restarted controller ignored the backoff: %v", err)
			}
			m.Advance(46 * time.Second)
		}
		agg := m.Get(run)
		if agg.Step("job").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
			t.Fatalf("step %s run %s after exhausting attempts", agg.Step("job").Status, agg.Run.Status)
		}
		m.Advance(time.Hour)
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) || attempts(m, run) != 3 {
			t.Fatalf("exceeded the attempt bound: %v, %d attempts", err, attempts(m, run))
		}
		if held(m) != 0 {
			t.Fatal("reservations leaked across retries")
		}
	})

	t.Run("command and validation failures are not replayed", func(t *testing.T) {
		for name, finish := range map[string]func(*executiontest.Mem, *execution.AttemptSpec){
			"command failure": func(m *executiontest.Mem, in *execution.AttemptSpec) { _, _ = m.Fail(in) },
			"missing outputs": func(m *executiontest.Mem, in *execution.AttemptSpec) { _, _ = m.Succeed(in, nil, "") },
		} {
			m, p := fixture(t)
			def := define(m, job("job", readRun, "outputs: {report: {type: string}}", "retry: {maxAttempts: 5, on: [infrastructure, transient]}"))
			run := m.MustAdmit(p, def, nil)
			in := m.MustClaim(tenant)
			m.Start(in)
			finish(m, in)
			if agg := m.Get(run); agg.Run.FailureIntentAt == nil || agg.Run.Status == execution.RunSucceeded {
				t.Fatalf("%s: run %s without failure intent", name, agg.Run.Status)
			}
			if err := m.Terminate(in, execution.LaunchStarted); err != nil {
				t.Fatal(err)
			}
			if agg := m.Get(run); agg.Step("job").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
				t.Fatalf("%s: step %s run %s after settlement", name, agg.Step("job").Status, agg.Run.Status)
			}
			m.Advance(time.Hour)
			if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) || attempts(m, run) != 1 {
				t.Fatalf("%s: replayed (%v, %d attempts)", name, err, attempts(m, run))
			}
		}
	})

	t.Run("an infrastructure failure after the command started is never replayed", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", readRun, retryInfra)), nil)
		in := m.MustClaim(tenant)
		m.Start(in)
		if err := m.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchStarted}); err != nil {
			t.Fatal(err)
		}
		if err := m.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		if agg := m.Get(run); agg.Run.Status != execution.RunFailed || attempts(m, run) != 1 {
			t.Fatalf("run %s attempts %d: a started command was replayed", agg.Run.Status, attempts(m, run))
		}
	})

	t.Run("not-started evidence makes the retry safe without reconciliation", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", readRun, retryInfra)), nil)
		infraFail(t, m, m.MustClaim(tenant))
		if agg := m.Get(run); agg.Step("deploy").Status != execution.StepPending || agg.Run.Status != execution.RunRunning {
			t.Fatalf("step %s run %s", agg.Step("deploy").Status, agg.Run.Status)
		}
	})

	t.Run("an unknown effect blocks retry and writer reassignment across restarts", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", writeRun, retryInfra)), nil)
		in := m.MustClaim(tenant)
		ambiguous(t, m, in)
		agg := m.Get(run)
		at, c := agg.Attempt(in.AttemptID), agg.Control(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutcomeUnknown || c.Effect != execution.EffectUnknown || c.Settled() {
			t.Fatalf("attempt %s/%s effect %s settled %v", at.Status, at.Reason, c.Effect, c.Settled())
		}
		if st := agg.Step("deploy"); st.Status != execution.StepRunning {
			t.Fatalf("step is %s, want blocked in running", st.Status)
		}
		m.Advance(time.Hour) // far beyond any backoff
		check := func(svc *execution.Service, when string) {
			t.Helper()
			if _, err := svc.ClaimRunnable(ctx, tenant); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("%s: a blocked step was claimed: %v", when, err)
			}
			if attempts(m, run) != 1 || held(m) != 1 || writerOf(m, p) != in.AttemptID {
				t.Fatalf("%s: %d attempts, %d reservations, writer %q", when, attempts(m, run), held(m), writerOf(m, p))
			}
		}
		check(m.Svc, "before restart")
		restarted := m.NewService(executiontest.MemOptions{NoEffects: true})
		if _, err := restarted.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		check(restarted, "after restart without an effect adapter")
		m.Effects.Set(execution.VerdictStillUnknown, nil)
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		check(m.Svc, "after an inconclusive reconciliation")
		if len(m.Effects.Queries) < 1 {
			t.Fatal("the effect adapter was never asked")
		}
		ev, err := m.Mem.EvidenceOf(ctx, scopeA, in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range ev {
			found = found || e.Kind == execution.EvidenceReconciliation
		}
		if !found {
			t.Fatal("the inconclusive reconciliation left no evidence")
		}
	})

	t.Run("replay-safe evidence settles once and allows exactly one new attempt", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", writeRun, retryInfra)), nil)
		in := m.MustClaim(tenant)
		ambiguous(t, m, in)
		m.Effects.Set(execution.VerdictReplaySafe, nil)
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := m.Get(run)
		if at := agg.Attempt(in.AttemptID); at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutcomeUnknown {
			t.Fatalf("the unknown attempt was rewritten to %s/%s", at.Status, at.Reason)
		}
		if !agg.Control(in.AttemptID).Settled() || held(m) != 0 || writerOf(m, p) != "" {
			t.Fatal("reconciliation did not settle and release exactly once")
		}
		if st := agg.Step("deploy"); st.Status != execution.StepPending || st.NextAttemptAt == nil {
			t.Fatalf("step %s next %v, want pending with persisted backoff", st.Status, st.NextAttemptAt)
		}
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("the backoff was ignored: %v", err)
		}
		// Repeating the reconciliation settles nothing twice.
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if held(m) != 0 {
			t.Fatalf("%d reservations after a repeated reconciliation", held(m))
		}
		m.Advance(31 * time.Second)
		next := m.MustClaim(tenant)
		if next.Ordinal != 2 || next.ActionKey != in.ActionKey || next.AttemptID == in.AttemptID {
			t.Fatalf("retry %+v after %+v", next, in)
		}
	})

	t.Run("settled but not replay-safe evidence fails the step", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", writeRun, retryInfra)), nil)
		ambiguous(t, m, m.MustClaim(tenant))
		m.Effects.Set(execution.VerdictSettled, nil)
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := m.Get(run)
		if agg.Step("deploy").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
			t.Fatalf("step %s run %s", agg.Step("deploy").Status, agg.Run.Status)
		}
		m.Advance(time.Hour)
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) || attempts(m, run) != 1 {
			t.Fatalf("a failed run dispatched again: %v (%d attempts)", err, attempts(m, run))
		}
	})

	t.Run("an unreachable effect adapter leaves the step blocked", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("deploy", writeRun, retryInfra)), nil)
		in := m.MustClaim(tenant)
		ambiguous(t, m, in)
		m.Effects.Set("", errors.New("tool down"))
		if _, err := m.Svc.Reconcile(ctx); err == nil {
			t.Fatal("an adapter outage was reported as success")
		}
		if c := m.Get(run).Control(in.AttemptID); c.Settled() || c.ReconciledAt != nil || held(m) != 1 {
			t.Fatalf("an outage settled the attempt: %+v", c)
		}
	})
}
