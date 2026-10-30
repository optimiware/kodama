package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
)

var (
	scope  = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	other  = domain.Scope{TenantID: "t2", ProjectID: "p1"}
	t0     = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	minute = time.Minute
	owner  = domain.Owner{TenantID: "t1", ProjectID: "p1"}
	okRes  = rt.Result{Code: rt.ResultSucceeded}
)

// bind returns the workload binding of one attempt of a step of run1.
func bind(step domain.StepID, attempt int, fence rt.Fence) rt.RuntimeBinding {
	return rt.RuntimeBinding{Scope: scope, RunID: "run1", StepKey: step, Attempt: attempt,
		WorkloadID: fmt.Sprintf("wl-run1-%s-%d", step, attempt), SpecDigest: "sha256:s", Backend: "process", Fence: fence}
}

func mustPlan(t *testing.T) rt.Plan {
	t.Helper()
	return rt.Plan{
		Digest: "sha256:abc",
		Steps: []rt.PlanStep{
			{Key: "build", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
			{Key: "review", Kind: rt.StepWait, DependsOn: []domain.StepID{"build"},
				Wait: func() *rt.WaitSpec { s := reviewSpec(); return &s }()},
			{Key: "notify", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, When: `review.outcome == "rejected"`,
				Workload: &rt.WorkloadSpec{Image: "alpine:3", Commands: []string{"notify"}}},
			{Key: "test", Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, When: `review.outcome == "approved"`,
				Workload: &rt.WorkloadSpec{Image: "golang:1", Commands: []string{"go test"}}},
		},
	}
}

func mustRun(t *testing.T) rt.Run {
	t.Helper()
	r, err := rt.NewRun(scope, "run1", "pipe1", mustPlan(t), 2)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// predicate returns an evaluation of a when predicate that read one value.
func predicate(t *testing.T, result bool) rt.PredicateEvaluation {
	t.Helper()
	d, err := rt.DigestPredicateReads([]rt.PredicateRead{{Ref: "review.outcome", Type: rt.InputString, Present: true, Value: "approved"}})
	must(t, err)
	return rt.PredicateEvaluation{Evaluated: true, Result: result, InputDigest: d, At: t0}
}

// skipForCondition records a false evaluation and skips the step for condition_false.
func skipForCondition(t *testing.T, r *rt.Run, key domain.StepID) {
	t.Helper()
	must(t, r.RecordPredicate(key, predicate(t, false)))
	must(t, r.AdvanceStep(key, rt.StepSkipped, rt.ReasonConditionFalse))
}

// reviewSpec is the wait the fixtures use: assigned to a role, correlated on the
// candidate commit, and with approved as its single approving outcome.
func reviewSpec() rt.WaitSpec {
	return rt.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "implement.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour}
}

// openWait returns the review wait of run1 opened at t0 for candidate c1.
func openWait(t *testing.T) rt.Wait {
	t.Helper()
	w, err := rt.NewWait(scope, "w1", "run1", "review", reviewSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Open(t0, map[string]string{"candidate": "c1"}, "please review"); err != nil {
		t.Fatal(err)
	}
	return w
}

// signalReq is the inbox resolution of w by alice under the given key.
func signalReq(w rt.Wait, key string) rt.SignalRequest {
	return rt.SignalRequest{Identity: rt.SignalIdentity{Scope: w.Scope, Caller: "user-alice", Key: key}, WaitID: w.ID,
		ExpectedVersion: w.Version, Outcome: "approved", Answer: "looks good", Actor: "user:alice"}
}

func Test_unit_kernel_runtime_contract(t *testing.T) {
	t.Run("step transitions", func(t *testing.T) {
		const (
			P, R, W = rt.StepPending, rt.StepRunning, rt.StepWaiting
			S, F    = rt.StepSucceeded, rt.StepFailed
			K, C    = rt.StepSkipped, rt.StepCancelled
		)
		for _, tc := range []struct {
			kind     rt.StepKind
			from, to rt.StepState
			ok       bool
		}{
			{rt.StepContainer, P, R, true},
			{rt.StepContainer, R, S, true},
			{rt.StepContainer, R, F, true},
			{rt.StepContainer, P, K, true},
			{rt.StepContainer, P, S, false}, // cannot succeed without running
			{rt.StepContainer, P, W, false}, // a container never waits
			{rt.StepContainer, S, R, false}, // terminal is final
			{rt.StepContainer, R, K, false},
			{rt.StepWait, P, W, true},
			{rt.StepWait, W, S, true},
			{rt.StepWait, W, F, true},
			{rt.StepWait, W, C, true},
			{rt.StepWait, P, R, false}, // a wait never runs a workload
			{rt.StepWait, P, S, false}, // consumption needs an open wait
			{rt.StepWait, P, K, true},  // a wait behind a false when, join or requires is skipped
			{rt.StepWait, P, F, true},  // correlation_missing: the wait never opens
			{rt.StepWait, W, K, false},
			{rt.StepWait, S, W, false},
			{rt.StepKind("condition"), P, S, false}, // there is no condition step kind
			{rt.StepKind("condition"), P, K, false},
			{rt.StepKind(""), P, R, false},
		} {
			if got := rt.CanStep(tc.kind, tc.from, tc.to); got != tc.ok {
				t.Errorf("%s %s->%s = %v, want %v", tc.kind, tc.from, tc.to, got, tc.ok)
			}
		}
	})

	t.Run("step advance needs a reason exactly where required", func(t *testing.T) {
		s, err := rt.NewStep("build", rt.StepContainer)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Advance(rt.StepRunning, rt.ReasonNone); err != nil {
			t.Fatal(err)
		}
		before := s
		for name, fn := range map[string]func() error{
			"failed without reason":      func() error { return s.Advance(rt.StepFailed, rt.ReasonNone) },
			"succeeded with reason":      func() error { return s.Advance(rt.StepSucceeded, rt.ReasonCommandFailed) },
			"malformed reason":           func() error { return s.Advance(rt.StepFailed, "Bad Reason") },
			"illegal move":               func() error { return s.Advance(rt.StepWaiting, rt.ReasonNone) },
			"illegal move keeps version": func() error { return s.Advance(rt.StepPending, rt.ReasonNone) },
		} {
			if err := fn(); err == nil {
				t.Errorf("%s: want error", name)
			}
			if s != before {
				t.Errorf("%s: rejected advance mutated the step", name)
			}
		}
		if err := s.Advance(rt.StepFailed, rt.ReasonCommandFailed); err != nil || s.Version != 3 {
			t.Errorf("advance to failed: %v version %d", err, s.Version)
		}
		if err := s.Advance(rt.StepRunning, rt.ReasonNone); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("terminal step moved: %v", err)
		}
	})

	t.Run("step kinds, wait outcomes and protected steps", testStepKindsAndWaits)
	t.Run("wait decisions", testWaitDecisions)
	t.Run("signal identity, routing and durable records", testSignalRecords)
	t.Run("wait resolution checks its own scope and address", testResolveAddress)
	t.Run("the admitted plan carries join, outputs, bindings and typed inputs", testPlanRepresentation)
	t.Run("a step retains its when evaluation", testPredicateEvaluation)

	t.Run("fence comparison rejects a stale writer", func(t *testing.T) {
		for _, tc := range []struct {
			held, current rt.Fence
			want          error
		}{
			{3, 3, nil},
			{2, 3, rt.ErrStaleFence},
			{4, 3, domain.ErrInvalidValue},
			{0, 3, domain.ErrInvalidValue},
			{3, 0, domain.ErrInvalidValue},
		} {
			err := rt.CheckFence(tc.held, tc.current)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("CheckFence(%d,%d) = %v, want %v", tc.held, tc.current, err, tc.want)
			}
		}
	})

	t.Run("claims are leases with a monotonic fence", func(t *testing.T) {
		req := rt.ClaimRequest{Scope: scope, RunID: "run1", Owner: "worker-a", Purpose: rt.ClaimExecute, TTL: minute}
		a, err := rt.AcquireClaim(rt.Claim{}, req, t0)
		if err != nil || a.Fence != 1 || !a.Active(t0) || a.Active(t0.Add(minute)) {
			t.Fatalf("first claim: %+v %v", a, err)
		}
		// Another owner cannot take an active lease.
		req.Owner = "worker-b"
		if _, err := rt.AcquireClaim(a, req, t0.Add(30*time.Second)); !errors.Is(err, rt.ErrClaimHeld) {
			t.Fatalf("takeover of an active lease: %v", err)
		}
		// Renewal keeps the fence and extends the lease.
		renewed, err := a.Renew(t0.Add(30*time.Second), minute)
		if err != nil || renewed.Fence != 1 || !renewed.Active(t0.Add(80*time.Second)) {
			t.Fatalf("renew: %+v %v", renewed, err)
		}
		// After expiry the lease cannot be renewed, only taken over, and the fence increases.
		if _, err := a.Renew(t0.Add(2*minute), minute); !errors.Is(err, rt.ErrClaimExpired) {
			t.Fatalf("renew expired: %v", err)
		}
		b, err := rt.AcquireClaim(a, req, t0.Add(2*minute))
		if err != nil || b.Fence != 2 || b.Owner != "worker-b" {
			t.Fatalf("takeover: %+v %v", b, err)
		}
		// The old writer is stale after takeover, even though it still presents a once-valid claim.
		now := t0.Add(2*minute + time.Second)
		if err := a.Authorizes(b, now); !errors.Is(err, rt.ErrStaleFence) {
			t.Fatalf("stale writer: %v", err)
		}
		if err := b.Authorizes(b, now); err != nil {
			t.Fatalf("current writer: %v", err)
		}
		if err := b.Authorizes(b, now.Add(10*minute)); !errors.Is(err, rt.ErrClaimExpired) {
			t.Fatalf("expired current writer: %v", err)
		}
		// Same fence, different owner is also stale: a forged owner cannot write.
		forged := b
		forged.Owner = "worker-c"
		if err := forged.Authorizes(b, now); !errors.Is(err, rt.ErrStaleFence) {
			t.Fatalf("forged owner: %v", err)
		}
		// A claim for another tenant's run is refused.
		foreign := b
		foreign.Scope = other
		if err := foreign.Authorizes(b, now); err == nil {
			t.Fatal("foreign scope accepted")
		}
		if _, err := rt.AcquireClaim(a, rt.ClaimRequest{Scope: other, RunID: "run1", Owner: "x", Purpose: rt.ClaimExecute, TTL: minute}, t0.Add(2*minute)); err == nil {
			t.Fatal("claim over another scope's previous claim accepted")
		}
		for name, bad := range map[string]rt.ClaimRequest{
			"no owner":   {Scope: scope, RunID: "run1", Purpose: rt.ClaimExecute, TTL: minute},
			"no purpose": {Scope: scope, RunID: "run1", Owner: "o", TTL: minute},
			"no ttl":     {Scope: scope, RunID: "run1", Owner: "o", Purpose: rt.ClaimExecute},
			"no scope":   {RunID: "run1", Owner: "o", Purpose: rt.ClaimExecute, TTL: minute},
		} {
			if _, err := rt.AcquireClaim(rt.Claim{}, bad, t0); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("outcome and cleanup are separate; terminal needs final cleanup", func(t *testing.T) {
		r := mustRun(t)
		if r.Terminal() || r.Settled() || r.Status() != rt.StatusQueued {
			t.Fatalf("new run: terminal %v settled %v status %s", r.Terminal(), r.Settled(), r.Status())
		}
		// Execution cannot settle while a step is still live.
		if err := r.Settle(rt.OutcomeSucceeded, rt.ReasonNone); err == nil {
			t.Fatal("settled with live steps")
		}
		for _, k := range []domain.StepID{"build"} {
			must(t, r.AdvanceStep(k, rt.StepRunning, rt.ReasonNone))
			if r.Status() != rt.StatusRunning {
				t.Fatalf("status %s", r.Status())
			}
			must(t, r.FinishStep(k, rt.StepSucceeded, rt.ReasonNone, okRes))
		}
		must(t, r.AdvanceStep("review", rt.StepWaiting, rt.ReasonNone))
		if r.Status() != rt.StatusRunning { // a run with an open wait stays running
			t.Fatalf("status %s", r.Status())
		}
		must(t, r.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
		skipForCondition(t, &r, "notify")
		must(t, r.AdvanceStep("test", rt.StepRunning, rt.ReasonNone))
		must(t, r.FinishStep("test", rt.StepSucceeded, rt.ReasonNone, okRes))

		must(t, r.Settle(rt.OutcomeSucceeded, rt.ReasonNone))
		if !r.Settled() || r.Terminal() || r.Status() != rt.StatusCleaning || r.Cleanup.State != rt.CleanupPending {
			t.Fatalf("settled but cleanup pending: terminal %v status %s cleanup %s", r.Terminal(), r.Status(), r.Cleanup.State)
		}
		if err := r.Settle(rt.OutcomeFailed, rt.ReasonCommandFailed); !errors.Is(err, rt.ErrIllegalTransition) || r.Outcome != rt.OutcomeSucceeded {
			t.Fatalf("second settle: %v outcome %s", err, r.Outcome)
		}
		if err := r.AdvanceStep("test", rt.StepFailed, rt.ReasonCommandFailed); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Fatalf("step moved after settle: %v", err)
		}
		retained := []rt.ResourceHandle{{Scope: scope, Owner: owner, Lifetime: rt.LifetimePipeline, RunID: "run1", Kind: rt.ResourceService, Name: "db", Identity: "run1-db", Fence: 1}}
		must(t, r.RecordCleanupAttempt(retained, rt.ReasonCleanupFailed))
		if r.Terminal() || r.Cleanup.Attempts != 1 || r.Cleanup.State != rt.CleanupPending || len(r.Cleanup.Retained) != 1 {
			t.Fatalf("after one attempt: %+v", r.Cleanup)
		}
		// The bound is reached: terminal, but cleanup stays visibly unfinished with identities retained.
		must(t, r.RecordCleanupAttempt(retained, rt.ReasonCleanupFailed))
		if !r.Terminal() || r.Cleanup.State != rt.CleanupExhausted || r.Status() != rt.StatusSucceeded || len(r.Cleanup.Retained) != 1 {
			t.Fatalf("exhausted: terminal %v %+v", r.Terminal(), r.Cleanup)
		}
		if err := r.RecordCleanupAttempt(retained, rt.ReasonCleanupFailed); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Fatalf("attempt after exhaustion: %v", err)
		}
		// A late reconciliation can still complete it.
		must(t, r.CompleteCleanup())
		if r.Cleanup.State != rt.CleanupComplete || len(r.Cleanup.Retained) != 0 || r.Cleanup.Reason != rt.ReasonNone {
			t.Fatalf("complete: %+v", r.Cleanup)
		}
		if err := r.CompleteCleanup(); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Fatalf("complete twice: %v", err)
		}
		if err := r.Admits(rt.ClaimReconcile); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Fatalf("terminal run admits a claim: %v", err)
		}
		if err := r.RecordCleanupAttempt(nil, rt.ReasonNone); err == nil {
			t.Fatal("empty attempt accepted")
		}
	})

	t.Run("cleanup completes directly when nothing is retained", func(t *testing.T) {
		r := mustRun(t)
		r.RequestCancel()
		for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
			must(t, r.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
		}
		must(t, r.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
		must(t, r.CompleteCleanup())
		if !r.Terminal() || r.Status() != rt.StatusCancelled {
			t.Fatalf("terminal %v status %s", r.Terminal(), r.Status())
		}
	})

	t.Run("cancellation blocks execution but not reconciliation or cleanup", func(t *testing.T) {
		r := mustRun(t)
		for p, want := range map[rt.ClaimPurpose]error{rt.ClaimExecute: nil, rt.ClaimReconcile: nil, rt.ClaimCleanup: rt.ErrIllegalTransition} {
			if err := r.Admits(p); !errors.Is(err, want) && !(want == nil && err == nil) {
				t.Errorf("running run admits %s: %v", p, err)
			}
		}
		v := r.Version
		r.RequestCancel()
		r.RequestCancel() // idempotent
		if !r.CancelRequested || r.Version != v+1 {
			t.Fatalf("cancel: %v version %d", r.CancelRequested, r.Version)
		}
		if err := r.Admits(rt.ClaimExecute); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("execute after cancel: %v", err)
		}
		for _, p := range []rt.ClaimPurpose{rt.ClaimReconcile, rt.ClaimCleanup} {
			if err := r.Admits(p); err != nil {
				t.Errorf("%s after cancel: %v", p, err)
			}
		}
		if err := r.Admits("bogus"); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("bogus purpose: %v", err)
		}
		// No step may start once cancellation is requested; steps can still be cancelled.
		if err := r.AdvanceStep("build", rt.StepRunning, rt.ReasonNone); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("start after cancel: %v", err)
		}
		if err := r.AdvanceStep("build", rt.StepCancelled, rt.ReasonCancelled); err != nil {
			t.Errorf("cancel step: %v", err)
		}
		// A run is cancelled only after cancellation was requested.
		fresh := mustRun(t)
		for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
			must(t, fresh.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
		}
		if err := fresh.Settle(rt.OutcomeCancelled, rt.ReasonCancelled); err == nil {
			t.Error("cancelled outcome without a request")
		}
	})

	t.Run("one container step holds the workspace; success excludes failure", func(t *testing.T) {
		plan := rt.Plan{Digest: "d", Steps: []rt.PlanStep{
			{Key: "a", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "i", Commands: []string{"c"}}},
			{Key: "b", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "i", Commands: []string{"c"}}},
		}}
		r, err := rt.NewRun(scope, "run1", "pipe1", plan, 1)
		must(t, err)
		must(t, r.AdvanceStep("a", rt.StepRunning, rt.ReasonNone))
		if err := r.AdvanceStep("b", rt.StepRunning, rt.ReasonNone); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Fatalf("second container started: %v", err)
		}
		must(t, r.AdvanceStep("a", rt.StepFailed, rt.ReasonCommandFailed))
		must(t, r.AdvanceStep("b", rt.StepSkipped, rt.ReasonDependencyFailed))
		if err := r.Settle(rt.OutcomeSucceeded, rt.ReasonNone); err == nil {
			t.Fatal("succeeded with a failed step")
		}
		if err := r.Settle(rt.OutcomeFailed, rt.ReasonNone); err == nil {
			t.Fatal("failed without a reason")
		}
		must(t, r.Settle(rt.OutcomeFailed, rt.ReasonCommandFailed))
		if err := r.AdvanceStep("missing", rt.StepRunning, rt.ReasonNone); err == nil {
			t.Fatal("unknown step accepted")
		}
	})

	t.Run("command intent is never replayed when uncertain", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			want rt.CommandState
		}{
			"ok":                    {nil, rt.CommandConfirmed},
			"not reached":           {fmt.Errorf("dial: %w", rt.ErrNotReached), rt.CommandRejected},
			"uncertain":             {rt.ErrUncertain, rt.CommandUncertain},
			"not reached+uncertain": {errors.Join(rt.ErrNotReached, rt.ErrUncertain), rt.CommandUncertain},
			"unknown error":         {errors.New("connection reset"), rt.CommandUncertain},
			"context cancelled":     {context.Canceled, rt.CommandUncertain},
			"deadline":              {context.DeadlineExceeded, rt.CommandUncertain},
		} {
			if got := rt.ClassifyEffect(tc.err); got != tc.want {
				t.Errorf("%s: %s, want %s", name, got, tc.want)
			}
		}
		c, err := rt.NewWorkloadCommand("cmd1", bind("build", 1, 1), rt.CommandLaunch, t0)
		must(t, err)
		if c.MayRetry() {
			t.Fatal("an intent with no recorded outcome must not be retried")
		}
		must(t, c.Settle(rt.CommandUncertain, rt.ReasonLaunchUncertain))
		if c.MayRetry() {
			t.Fatal("an uncertain command must be reconciled, not retried")
		}
		if err := c.Settle(rt.CommandIntent, rt.ReasonNone); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Fatalf("uncertain -> intent: %v", err)
		}
		must(t, c.Settle(rt.CommandRejected, rt.ReasonLaunchNotReached))
		if !c.MayRetry() {
			t.Fatal("a rejected command is known not to have reached the backend")
		}
		if err := c.Settle(rt.CommandConfirmed, rt.ReasonNone); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Fatalf("rejected is final: %v", err)
		}
		c2, _ := rt.NewWorkloadCommand("cmd2", bind("build", 1, 1), rt.CommandLaunch, t0)
		if err := c2.Settle(rt.CommandRejected, rt.ReasonNone); err == nil {
			t.Fatal("rejection without a reason accepted")
		}
		if err := c2.Settle(rt.CommandConfirmed, rt.ReasonCommandFailed); err == nil {
			t.Fatal("confirmation with a reason accepted")
		}
		must(t, c2.Settle(rt.CommandConfirmed, rt.ReasonNone))
		if _, err := rt.NewWorkloadCommand("cmd3", bind("build", 1, 0), rt.CommandLaunch, t0); err == nil {
			t.Fatal("a command without a fence accepted")
		}
	})

	t.Run("plan and admission", func(t *testing.T) {
		plan := mustPlan(t)
		must(t, plan.Validate())
		for name, mutate := range map[string]func(*rt.Plan){
			"no digest":           func(p *rt.Plan) { p.Digest = "" },
			"no steps":            func(p *rt.Plan) { p.Steps = nil },
			"duplicate step":      func(p *rt.Plan) { p.Steps[3].Key = "build" },
			"forward dependency":  func(p *rt.Plan) { p.Steps[0].DependsOn = []domain.StepID{"test"} },
			"self dependency":     func(p *rt.Plan) { p.Steps[0].DependsOn = []domain.StepID{"build"} },
			"container no image":  func(p *rt.Plan) { p.Steps[0].Workload.Image = "" },
			"container with wait": func(p *rt.Plan) { p.Steps[0].Wait = p.Steps[1].Wait },
			"wait with workload":  func(p *rt.Plan) { p.Steps[1].Workload = p.Steps[0].Workload },
			"wait no input":       func(p *rt.Plan) { p.Steps[1].Wait.Input = "" },
			"condition step kind": func(p *rt.Plan) { p.Steps[2].Kind = "condition"; p.Steps[2].Workload = nil },
			"when too long":       func(p *rt.Plan) { p.Steps[2].When = strings.Repeat("a", rt.MaxWhenBytes+1) },
			"unknown kind":        func(p *rt.Plan) { p.Steps[2].Kind = "agent" },
			"service lifetime":    func(p *rt.Plan) { p.Services = []rt.ServiceSpec{{Name: "db", Lifetime: "forever", Image: "pg"}} },
		} {
			p := mustPlan(t)
			mutate(&p)
			if err := p.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}

		run := mustRun(t)
		wait, _ := rt.NewWait(scope, "w1", "run1", "review", *plan.Steps[1].Wait)
		ev := rt.Event{ID: "e1", Scope: scope, RunID: "run1", Kind: rt.EventRunCreated, Version: 1, At: t0}
		adm := rt.Admission{Run: run, Plan: plan, Waits: []rt.Wait{wait}, Events: []rt.Event{ev}}
		must(t, adm.Validate())

		bad := adm
		bad.Waits = nil
		if bad.Validate() == nil {
			t.Error("wait step without a wait admitted")
		}
		bad = adm
		bad.Plan = mustPlan(t)
		bad.Plan.Steps = bad.Plan.Steps[:3]
		if bad.Validate() == nil {
			t.Error("run and plan with different steps admitted")
		}
		bad = adm
		open := openWait(t)
		bad.Waits = []rt.Wait{open}
		if bad.Validate() == nil {
			t.Error("an already-open wait admitted")
		}
		bad = adm
		fw := wait
		fw.Scope = other
		bad.Waits = []rt.Wait{fw}
		if bad.Validate() == nil {
			t.Error("a foreign wait admitted")
		}
		bad = adm
		fe := ev
		fe.Scope = other
		bad.Events = []rt.Event{fe}
		if bad.Validate() == nil {
			t.Error("a foreign event admitted")
		}
	})

	t.Run("transition is atomic, scoped and version-checked", func(t *testing.T) {
		run := mustRun(t)
		claim := rt.Claim{Scope: scope, RunID: "run1", Owner: "w", Purpose: rt.ClaimExecute, Fence: 1, Expires: t0.Add(minute)}
		next := run
		must(t, next.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
		cmd, _ := rt.NewWorkloadCommand("c1", bind("build", 1, 1), rt.CommandLaunch, t0)
		ev := rt.Event{ID: "e1", Scope: scope, RunID: "run1", StepKey: "build", Kind: rt.EventStepStateChanged, Version: 2, At: t0}
		tr := rt.Transition{Scope: scope, RunID: "run1", Claim: claim, Now: t0, ExpectedVersion: run.Version, Run: next,
			Commands: []rt.Command{cmd}, Events: []rt.Event{ev}}
		must(t, tr.Validate())

		for name, mutate := range map[string]func(*rt.Transition){
			"version skipped":   func(x *rt.Transition) { x.ExpectedVersion = 5 },
			"version unchanged": func(x *rt.Transition) { x.Run = run },
			"foreign claim":     func(x *rt.Transition) { x.Claim.Scope = other },
			"foreign run":       func(x *rt.Transition) { x.Run.Scope = other },
			"foreign command":   func(x *rt.Transition) { c := cmd; c.Scope = other; x.Commands = []rt.Command{c} },
			"foreign event":     func(x *rt.Transition) { e := ev; e.RunID = "run2"; x.Events = []rt.Event{e} },
			"foreign resource": func(x *rt.Transition) {
				x.Resources = []rt.ResourceObservation{{Handle: rt.ResourceHandle{Scope: other, Owner: domain.Owner{TenantID: "t2", ProjectID: "p1"}, Lifetime: rt.LifetimeProject, Kind: rt.ResourceService, Name: "db", Identity: "i", Fence: 1}, State: rt.ResourceReady, ObservedAt: t0}}
			},
			"no time":    func(x *rt.Transition) { x.Now = time.Time{} },
			"zero fence": func(x *rt.Transition) { x.Claim.Fence = 0 },
		} {
			x := tr
			mutate(&x)
			if err := x.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("resources, workspace inputs and secrets", func(t *testing.T) {
		prep := rt.PreparationInputs{Scope: scope, RunID: "run1", Digest: "d",
			Sources: []rt.SourceInput{{Name: "app", Locator: "repo://app", Revision: "abc123", Path: "src/app"}}}
		ws := rt.ResourceSpec{Scope: scope, Owner: owner, RunID: "run1", Kind: rt.ResourceWorkspace, Name: "workspace", Lifetime: rt.LifetimePipeline,
			Identity: "run1-ws", SpecDigest: "d", Fence: 1, Preparation: &prep}
		must(t, ws.Validate())
		src := ws
		src.Preparation = &rt.PreparationInputs{Scope: scope, RunID: "run1", Digest: "d"} // source-free
		must(t, src.Validate())

		for name, mutate := range map[string]func(*rt.ResourceSpec){
			"no preparation":   func(s *rt.ResourceSpec) { s.Preparation = nil },
			"other run inputs": func(s *rt.ResourceSpec) { p := prep; p.RunID = "run2"; s.Preparation = &p },
			"other tenant":     func(s *rt.ResourceSpec) { p := prep; p.Scope = other; s.Preparation = &p },
			"floating source": func(s *rt.ResourceSpec) {
				p := prep
				p.Sources = []rt.SourceInput{{Name: "app", Locator: "l", Path: "x"}}
				s.Preparation = &p
			},
			"escaping path": func(s *rt.ResourceSpec) {
				p := prep
				p.Sources = []rt.SourceInput{{Name: "app", Locator: "l", Revision: "r", Path: "../etc"}}
				s.Preparation = &p
			},
			"absolute path": func(s *rt.ResourceSpec) {
				p := prep
				p.Sources = []rt.SourceInput{{Name: "app", Locator: "l", Revision: "r", Path: "/etc"}}
				s.Preparation = &p
			},
			"shared workspace": func(s *rt.ResourceSpec) { s.Lifetime = rt.LifetimeProject },
			"no fence":         func(s *rt.ResourceSpec) { s.Fence = 0 },
			"service on workspace": func(s *rt.ResourceSpec) {
				s.Service = &rt.ServiceSpec{Name: "db", Lifetime: rt.LifetimePipeline, Image: "pg"}
			},
		} {
			s := ws
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}

		svc := rt.ResourceSpec{Scope: scope, Owner: owner, Kind: rt.ResourceService, Name: "db", Lifetime: rt.LifetimeProject, Identity: "p1-db", SpecDigest: "d", Fence: 1,
			Service: &rt.ServiceSpec{Name: "db", Lifetime: rt.LifetimeProject, Image: "pg", Ports: []int{5432}}}
		must(t, svc.Validate())
		runSvc := svc
		runSvc.Lifetime, runSvc.Service = rt.LifetimePipeline, &rt.ServiceSpec{Name: "db", Lifetime: rt.LifetimePipeline, Image: "pg"}
		if err := runSvc.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("a run-lifetime service needs a run id: %v", err)
		}
		mismatch := svc
		mismatch.Lifetime = rt.LifetimeTenant
		if err := mismatch.Validate(); err == nil {
			t.Error("lifetime mismatch accepted")
		}

		obs := rt.ResourceObservation{Handle: rt.ResourceHandle{Scope: scope, Owner: owner, Lifetime: rt.LifetimeProject, Kind: rt.ResourceService, Name: "db", Identity: "i", Fence: 1}, State: rt.ResourceUnknown, ObservedAt: t0}
		must(t, obs.Validate())
		if obs.ConfirmedRemoved() {
			t.Error("unknown must never count as removed")
		}
		obs.State = rt.ResourceRemoved
		if !obs.ConfirmedRemoved() {
			t.Error("removed is confirmation")
		}
		obs.State = "gone"
		if obs.Validate() == nil {
			t.Error("unknown state name accepted")
		}

		declared := []rt.SecretRef{{Owner: domain.Owner{TenantID: "t1", ProjectID: "p1"}, Name: "TOKEN"}, {Owner: domain.Owner{TenantID: "t1"}, Name: "REGISTRY"}}
		got, err := rt.SelectSecrets(declared, []string{"REGISTRY"})
		if err != nil || len(got) != 1 || got[0].Name != "REGISTRY" {
			t.Fatalf("select: %+v %v", got, err)
		}
		if _, err := rt.SelectSecrets(declared, []string{"TOKEN", "UNDECLARED"}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("undeclared secret: %v", err)
		}
		if (rt.SecretRef{Owner: domain.Owner{TenantID: "t1"}, Name: "bad-name"}).Validate() == nil {
			t.Error("invalid secret name accepted")
		}
	})

	t.Run("capabilities refuse an unsatisfiable run", func(t *testing.T) {
		have := rt.CapabilitySet{rt.CapNonRoot, rt.CapFencedStop}
		if err := have.Satisfies(rt.CapabilitySet{rt.CapNonRoot}); err != nil {
			t.Error(err)
		}
		err := have.Satisfies(rt.CapabilitySet{rt.CapNonRoot, rt.CapNetworkIsolation, rt.CapNetworkIsolation})
		if !errors.Is(err, rt.ErrCapability) || !strings.Contains(err.Error(), "network_isolation") {
			t.Errorf("missing capability: %v", err)
		}
		if m := have.Missing(rt.CapabilitySet{rt.CapNetworkIsolation, rt.CapNetworkIsolation}); len(m) != 1 {
			t.Errorf("duplicates in missing: %v", m)
		}
	})

	t.Run("bindings, observations and logs carry scope", func(t *testing.T) {
		b := rt.RuntimeBinding{Scope: scope, RunID: "run1", StepKey: "build", Attempt: 1, WorkloadID: "wl", SpecDigest: "d", Backend: "process", Fence: 1}
		must(t, b.Validate())
		for name, mutate := range map[string]func(*rt.RuntimeBinding){
			"no scope":   func(x *rt.RuntimeBinding) { x.Scope = domain.Scope{} },
			"no attempt": func(x *rt.RuntimeBinding) { x.Attempt = 0 },
			"no digest":  func(x *rt.RuntimeBinding) { x.SpecDigest = "" },
			"no fence":   func(x *rt.RuntimeBinding) { x.Fence = 0 },
		} {
			x := b
			mutate(&x)
			if x.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		exited := rt.WorkloadObservation{Binding: b, Phase: rt.PhaseExited, Result: rt.Result{Code: rt.ResultFailed, ExitCode: 2, Reason: rt.ReasonCommandFailed}, ObservedAt: t0}
		must(t, exited.Validate())
		for name, o := range map[string]rt.WorkloadObservation{
			"exited without result": {Binding: b, Phase: rt.PhaseExited, ObservedAt: t0},
			"running with result":   {Binding: b, Phase: rt.PhaseRunning, Result: rt.Result{Code: rt.ResultSucceeded}, ObservedAt: t0},
			"succeeded with exit":   {Binding: b, Phase: rt.PhaseExited, Result: rt.Result{Code: rt.ResultSucceeded, ExitCode: 1}, ObservedAt: t0},
			"unknown phase":         {Binding: b, Phase: "paused", ObservedAt: t0},
			"no observation time":   {Binding: b, Phase: rt.PhaseAbsent},
		} {
			if o.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		chunk := rt.LogChunk{Scope: scope, RunID: "run1", StepKey: "build", Source: "workload", Sequence: 1, At: t0, Data: []byte("ok")}
		must(t, chunk.Validate())
		chunk.Data = make([]byte, rt.MaxLogChunkBytes+1)
		if chunk.Validate() == nil {
			t.Error("unbounded log chunk accepted")
		}
		chunk.Data, chunk.Scope = nil, domain.Scope{}
		if chunk.Validate() == nil {
			t.Error("unscoped log chunk accepted")
		}
	})

	t.Run("codes are stable lower_snake_case", func(t *testing.T) {
		re := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
		for _, r := range []rt.Reason{rt.ReasonCommandFailed, rt.ReasonStepTimeout, rt.ReasonWaitExpired, rt.ReasonCancelled,
			rt.ReasonDependencyFailed, rt.ReasonConditionFalse, rt.ReasonLaunchNotReached, rt.ReasonLaunchUncertain,
			rt.ReasonStopUncertain, rt.ReasonWorkloadLost, rt.ReasonServiceLost, rt.ReasonWorkspaceLost,
			rt.ReasonSecretUnavailable, rt.ReasonCapabilityUnsatisfied, rt.ReasonPreparationFailed,
			rt.ReasonCleanupFailed, rt.ReasonCleanupExhausted, rt.ReasonPrerequisiteSkipped, rt.ReasonRequiresUnmet,
			rt.ReasonRunFailed, rt.ReasonRunCancelled, rt.ReasonCorrelationMissing} {
			if !re.MatchString(string(r)) || r.Validate() != nil {
				t.Errorf("reason %q", r)
			}
		}
		for _, c := range []rt.ResultCode{rt.ResultSucceeded, rt.ResultFailed, rt.ResultCancelled, rt.ResultTimedOut, rt.ResultLost} {
			if !re.MatchString(string(c)) || !c.Valid() {
				t.Errorf("result code %q", c)
			}
		}
		if rt.Reason("Not Snake").Validate() == nil || rt.ResultCode("nope").Valid() {
			t.Error("malformed code accepted")
		}
		// Pinned wire values.
		for got, want := range map[string]string{
			string(rt.StepContainer): "container", string(rt.StepWait): "wait",
			string(rt.EventWaitResolved): "wait.resolved", string(rt.EventStepStateChanged): "run_step.state_changed",
			string(rt.CommandLaunch): "launch_workload",
		} {
			if got != want {
				t.Errorf("wire value %q, want %q", got, want)
			}
		}
	})

	t.Run("execution evidence round-trips", testExecutionEvidence)
	t.Run("retained workloads", testRetainedWorkloads)
	t.Run("late reconciliation after exhaustion", testLateReconciliation)
	t.Run("lease renewal", testLeaseRenewal)
	t.Run("tenant ownership of resources", testTenantOwnership)

	t.Run("zero values are safe and invalid", func(t *testing.T) {
		var (
			run   rt.Run
			step  rt.Step
			wait  rt.Wait
			claim rt.Claim
			cmd   rt.Command
			plan  rt.Plan
		)
		if run.Terminal() || run.Settled() || run.Validate() == nil || run.Status() != rt.StatusQueued {
			t.Error("zero run")
		}
		if err := run.Admits(rt.ClaimExecute); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("zero run admits execution: %v", err)
		}
		if err := run.AdvanceStep("x", rt.StepRunning, ""); err == nil {
			t.Error("zero run advanced a step")
		}
		if err := run.Settle(rt.OutcomeFailed, rt.ReasonCommandFailed); err == nil {
			t.Error("zero run settled")
		}
		run.RequestCancel() // must not panic
		if step.Validate() == nil || step.Advance(rt.StepRunning, "") == nil || rt.StepKind("").Valid() || rt.StepState("").Terminal() {
			t.Error("zero step")
		}
		if wait.Validate() == nil || wait.State.Closed() {
			t.Error("zero wait")
		}
		if _, err := wait.Resolve(rt.SignalRequest{}, t0); err == nil {
			t.Error("zero wait resolved by a zero signal")
		}
		_, errExpire := wait.Expire(t0)
		_, errCancel := wait.Cancel(t0, rt.ReasonRunCancelled)
		if errOpen := wait.Open(t0, nil, ""); errOpen == nil || errExpire == nil || errCancel == nil {
			t.Error("zero wait transitioned")
		}
		if claim.Active(t0) || claim.Validate() == nil || claim.Fence.Validate() == nil {
			t.Error("zero claim")
		}
		if claim.Authorizes(claim, t0) == nil {
			t.Error("zero claim authorizes a write")
		}
		if cmd.Validate() == nil || cmd.MayRetry() || cmd.Settle(rt.CommandConfirmed, "") == nil {
			t.Error("zero command")
		}
		if plan.Validate() == nil || (rt.Admission{}).Validate() == nil || (rt.Transition{}).Validate() == nil {
			t.Error("zero plan, admission or transition")
		}
		if rt.CleanupState("").Final() || rt.Outcome("").Decided() || (rt.Cleanup{}).State.Final() {
			t.Error("zero outcome or cleanup is final")
		}
		var caps rt.CapabilitySet
		if caps.Has(rt.CapNonRoot) || len(caps.Missing(nil)) != 0 || caps.Satisfies(nil) != nil {
			t.Error("zero capability set")
		}
		if (rt.RuntimeBinding{}).Validate() == nil || (rt.ResourceSpec{}).Validate() == nil ||
			(rt.ResourceHandle{}).Validate() == nil || (rt.Event{}).Validate() == nil || (rt.LogChunk{}).Validate() == nil ||
			(rt.WorkloadObservation{}).Validate() == nil || (rt.ResourceObservation{}).Validate() == nil ||
			(rt.Result{}).Validate() == nil || (rt.SignalRequest{}).Validate() == nil || (rt.ClaimRequest{}).Validate() == nil {
			t.Error("a zero value validated")
		}
		if rt.ClassifyEffect(nil) != rt.CommandConfirmed {
			t.Error("nil effect")
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
