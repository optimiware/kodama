package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
)

// recordingRuntime is a Runtime that remembers only the bindings it is asked
// about. Recovery tests drive it from a decoded record to prove the record alone
// is enough to reconcile a workload.
type recordingRuntime struct{ observed []rt.RuntimeBinding }

var _ rt.Runtime = (*recordingRuntime)(nil)

func (r *recordingRuntime) Capabilities(context.Context) (rt.CapabilitySet, error) { return nil, nil }
func (r *recordingRuntime) Start(context.Context, rt.StartRequest) (rt.WorkloadObservation, error) {
	return rt.WorkloadObservation{}, nil
}
func (r *recordingRuntime) Observe(_ context.Context, b rt.RuntimeBinding) (rt.WorkloadObservation, error) {
	r.observed = append(r.observed, b)
	return rt.WorkloadObservation{Binding: b, Phase: rt.PhaseRunning, ObservedAt: t0.Add(time.Hour)}, nil
}
func (r *recordingRuntime) Logs(context.Context, rt.RuntimeBinding, rt.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (r *recordingRuntime) Stop(context.Context, rt.RuntimeBinding) (rt.WorkloadObservation, error) {
	return rt.WorkloadObservation{}, nil
}
func (r *recordingRuntime) Remove(context.Context, rt.RuntimeBinding) (rt.WorkloadObservation, error) {
	return rt.WorkloadObservation{}, nil
}

func projectHandle(s domain.Scope, name string) rt.ResourceHandle {
	return rt.ResourceHandle{Scope: s, Owner: rt.OwnerFor(rt.LifetimeProject, s), Lifetime: rt.LifetimeProject,
		Kind: rt.ResourceService, Name: name, Identity: string(s.ProjectID) + "-" + name, Fence: 1}
}

func runHandle(name string) rt.ResourceHandle {
	return rt.ResourceHandle{Scope: scope, Owner: owner, Lifetime: rt.LifetimePipeline, RunID: "run1",
		Kind: rt.ResourceService, Name: name, Identity: "run1-" + name, Fence: 1}
}

func tenantHandle(s domain.Scope, name string) rt.ResourceHandle {
	return rt.ResourceHandle{Scope: s, Owner: rt.OwnerFor(rt.LifetimeTenant, s), Lifetime: rt.LifetimeTenant,
		Kind: rt.ResourceService, Name: name, Identity: string(s.TenantID) + "-" + name, Fence: 1}
}

// evidenceRecord builds a mid-flight run record that exercises every kind of
// execution evidence: a retried launch, a settled container result, a consumed
// wait, a condition, an uncertain launch of the next attempt, workload
// observations, resource observations and a lease.
func evidenceRecord(t *testing.T) rt.RunRecord {
	t.Helper()
	plan := mustPlan(t)
	run := mustRun(t)

	must(t, run.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
	b1 := bind("build", 1, 1)
	launch1, err := rt.NewWorkloadCommand("c-build-1", b1, rt.CommandLaunch, t0)
	must(t, err)
	must(t, launch1.Settle(rt.CommandRejected, rt.ReasonLaunchNotReached))
	must(t, run.RetryStep("build", launch1))
	b2 := bind("build", 2, 1)
	launch2, _ := rt.NewWorkloadCommand("c-build-2", b2, rt.CommandLaunch, t0.Add(time.Second))
	must(t, launch2.Settle(rt.CommandConfirmed, rt.ReasonNone))
	stop2, _ := rt.NewWorkloadCommand("c-stop-2", b2, rt.CommandStop, t0.Add(2*time.Second))
	must(t, stop2.Settle(rt.CommandConfirmed, rt.ReasonNone))
	must(t, stop2.RecordResult(okRes))
	must(t, run.FinishStep("build", rt.StepSucceeded, rt.ReasonNone, okRes))

	wait, err := rt.NewWait(scope, "w1", "run1", "review", *plan.Steps[1].Wait)
	must(t, err)
	must(t, wait.Open(t0, map[string]string{"candidate": "c1"}, "please review"))
	must(t, run.AdvanceStep("review", rt.StepWaiting, rt.ReasonNone))
	if _, err := wait.Resolve(signalReq(wait, "sig-1"), t0.Add(minute)); err != nil {
		t.Fatal(err)
	}
	must(t, run.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
	skipForCondition(t, &run, "notify")

	must(t, run.RecordPredicate("test", predicate(t, true)))
	must(t, run.AdvanceStep("test", rt.StepRunning, rt.ReasonNone))
	b3 := bind("test", 1, 1)
	launch3, _ := rt.NewWorkloadCommand("c-test-1", b3, rt.CommandLaunch, t0.Add(3*minute))
	must(t, launch3.Settle(rt.CommandUncertain, rt.ReasonLaunchUncertain))
	ensure, err := rt.NewCommand("c-ws", scope, "run1", "", rt.CommandEnsureResource, "run1-ws", "d", 1, t0)
	must(t, err)
	must(t, ensure.Settle(rt.CommandConfirmed, rt.ReasonNone))

	return rt.RunRecord{
		Run:      run,
		Plan:     plan,
		Claim:    rt.Claim{Scope: scope, RunID: "run1", Owner: "worker-a", Purpose: rt.ClaimExecute, Fence: 1, Expires: t0.Add(10 * minute)},
		Waits:    []rt.Wait{wait},
		Commands: []rt.Command{launch1, launch2, stop2, launch3, ensure},
		Workloads: []rt.WorkloadObservation{
			{Binding: b2, Phase: rt.PhaseExited, Result: okRes, ObservedAt: t0.Add(2 * time.Second)},
			{Binding: b3, Phase: rt.PhaseUnknown, ObservedAt: t0.Add(4 * minute)},
		},
		Resources: []rt.ResourceObservation{
			{Handle: runHandle("ws"), State: rt.ResourceReady, ObservedAt: t0},
			{Handle: tenantHandle(scope, "cache"), State: rt.ResourceReady, ObservedAt: t0},
		},
	}
}

func roundTrip[T any](t *testing.T, v T) (T, string, string) {
	t.Helper()
	a, err := json.Marshal(v)
	must(t, err)
	var out T
	must(t, json.Unmarshal(a, &out))
	b, err := json.Marshal(out)
	must(t, err)
	return out, string(a), string(b)
}

// testExecutionEvidence proves finding 1: durable reads and atomic transitions
// round-trip the evidence a restarted engine needs.
func testExecutionEvidence(t *testing.T) {
	t.Run("record round-trips and reconstructs without backend configuration", func(t *testing.T) {
		rec := evidenceRecord(t)
		must(t, rec.Validate())
		got, a, b := roundTrip(t, rec)
		if a != b {
			t.Fatalf("round trip changed the record:\n%s\n%s", a, b)
		}
		must(t, got.Validate())

		// Conditions read prior results and wait outcomes from the decoded record alone.
		if res, ok := got.StepResult("build"); !ok || res != okRes {
			t.Errorf("build result %+v %v", res, ok)
		}
		if _, ok := got.StepResult("review"); ok {
			t.Error("a wait step has no workload result")
		}
		if out, ok := got.WaitOutcome("review"); !ok || out != "approved" {
			t.Errorf("review outcome %q %v", out, ok)
		}
		if got.Run.Steps[0].Attempt != 2 || got.Run.Steps[3].Attempt != 1 {
			t.Errorf("attempts %d %d", got.Run.Steps[0].Attempt, got.Run.Steps[3].Attempt)
		}

		// Recovery: the only unresolved command is the uncertain launch, and its binding
		// carries everything needed to observe that workload by stable identity.
		unresolved := got.Unresolved()
		if len(unresolved) != 1 || unresolved[0].ID != "c-test-1" || unresolved[0].State != rt.CommandUncertain {
			t.Fatalf("unresolved %+v", unresolved)
		}
		var runtime recordingRuntime
		want := bind("test", 1, 1)
		obs, err := runtime.Observe(context.Background(), unresolved[0].Binding)
		if err != nil || len(runtime.observed) != 1 || runtime.observed[0] != want || obs.Binding.Backend != "process" ||
			obs.Binding.Attempt != 1 || obs.Binding.Fence != 1 {
			t.Fatalf("reconstructed binding %+v %v", runtime.observed, err)
		}
		// The retained observation of the settled attempt keeps its result.
		if w := got.Workloads[0]; w.Phase != rt.PhaseExited || w.Result != okRes || w.Binding.Attempt != 2 {
			t.Errorf("retained observation %+v", w)
		}
		if c := got.Commands[2]; c.Kind != rt.CommandStop || c.Result != okRes {
			t.Errorf("a confirmed stop retains the result: %+v", c)
		}
	})

	t.Run("transition round-trips its execution evidence", func(t *testing.T) {
		rec := evidenceRecord(t)
		next := rec.Run
		next.Cleanup = rec.Run.Cleanup
		must(t, next.FinishStep("test", rt.StepFailed, rt.ReasonWorkloadLost, rt.Result{Code: rt.ResultLost, Reason: rt.ReasonWorkloadLost}))
		tr := rt.Transition{Scope: scope, RunID: "run1", Claim: rec.Claim, Now: t0.Add(5 * minute), ExpectedVersion: rec.Run.Version, Run: next,
			Commands:  []rt.Command{rec.Commands[3]},
			Workloads: []rt.WorkloadObservation{rec.Workloads[1]},
			Resources: rec.Resources,
			Events:    []rt.Event{{ID: "e1", Scope: scope, RunID: "run1", StepKey: "test", Kind: rt.EventStepStateChanged, Version: next.Steps[3].Version, Reason: rt.ReasonWorkloadLost, At: t0}},
			RenewTTL:  time.Minute}
		must(t, tr.Validate())
		got, a, b := roundTrip(t, tr)
		if a != b {
			t.Fatalf("round trip changed the transition:\n%s\n%s", a, b)
		}
		must(t, got.Validate())
		if got.Run.Steps[3].Result.Code != rt.ResultLost || got.Workloads[0].Binding != rec.Workloads[1].Binding || got.RenewTTL != time.Minute {
			t.Errorf("evidence lost in transit: %+v", got)
		}

		for name, mutate := range map[string]func(*rt.Transition){
			"workload attempt beyond the step": func(x *rt.Transition) {
				w := x.Workloads[0]
				w.Binding.Attempt = 9
				x.Workloads = []rt.WorkloadObservation{w}
			},
			"workload of another run": func(x *rt.Transition) {
				w := x.Workloads[0]
				w.Binding.RunID = "run2"
				x.Workloads = []rt.WorkloadObservation{w}
			},
			"workload of another tenant": func(x *rt.Transition) {
				w := x.Workloads[0]
				w.Binding.Scope = other
				x.Workloads = []rt.WorkloadObservation{w}
			},
			"workload of a wait step": func(x *rt.Transition) {
				w := x.Workloads[0]
				w.Binding.StepKey = "review"
				x.Workloads = []rt.WorkloadObservation{w}
			},
			"negative renewal": func(x *rt.Transition) { x.RenewTTL = -time.Second },
		} {
			x := tr
			mutate(&x)
			if err := x.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("record validation rejects incoherent evidence", func(t *testing.T) {
		for name, mutate := range map[string]func(*rt.RunRecord){
			"wait disagrees with step": func(r *rt.RunRecord) { w := r.Waits[0]; w.State = rt.WaitCancelled; r.Waits = []rt.Wait{w} },
			"missing wait":             func(r *rt.RunRecord) { r.Waits = nil },
			"foreign command":          func(r *rt.RunRecord) { c := r.Commands[0]; c.Scope = other; r.Commands = []rt.Command{c} },
			"duplicate command":        func(r *rt.RunRecord) { r.Commands = append(r.Commands, r.Commands[0]) },
			"duplicate attempt":        func(r *rt.RunRecord) { r.Workloads = append(r.Workloads, r.Workloads[0]) },
			"foreign claim":            func(r *rt.RunRecord) { r.Claim.RunID = "run2" },
			"foreign resource": func(r *rt.RunRecord) {
				r.Resources = []rt.ResourceObservation{{Handle: projectHandle(domain.Scope{TenantID: "t1", ProjectID: "p2"}, "db"), State: rt.ResourceReady, ObservedAt: t0}}
			},
		} {
			rec := evidenceRecord(t)
			mutate(&rec)
			if err := rec.Validate(); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if err := (rt.RunRecord{}).Validate(); err == nil {
			t.Error("zero record accepted")
		}
	})

	t.Run("step results agree with state and only container steps carry them", func(t *testing.T) {
		step := func() rt.Step {
			s, _ := rt.NewStep("build", rt.StepContainer)
			must(t, s.Advance(rt.StepRunning, rt.ReasonNone))
			return s
		}
		s := step()
		if s.Attempt != 1 {
			t.Fatalf("a started container step is attempt 1, got %d", s.Attempt)
		}
		if err := s.Advance(rt.StepSucceeded, rt.ReasonNone); !errors.Is(err, domain.ErrInvalidValue) || s.State != rt.StepRunning {
			t.Errorf("a succeeded container step without a result: %v", err)
		}
		for name, tc := range map[string]struct {
			to  rt.StepState
			why rt.Reason
			res rt.Result
		}{
			"success with failed result":   {rt.StepSucceeded, rt.ReasonNone, rt.Result{Code: rt.ResultFailed, ExitCode: 1, Reason: rt.ReasonCommandFailed}},
			"failure with success result":  {rt.StepFailed, rt.ReasonCommandFailed, okRes},
			"cancelled with failed result": {rt.StepCancelled, rt.ReasonCancelled, rt.Result{Code: rt.ResultFailed, ExitCode: 1}},
			"malformed result":             {rt.StepFailed, rt.ReasonCommandFailed, rt.Result{Code: "bogus"}},
		} {
			s := step()
			if err := s.Finish(tc.to, tc.why, tc.res); err == nil || s.State != rt.StepRunning || s.Result != (rt.Result{}) {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, tc := range map[string]struct {
			to  rt.StepState
			why rt.Reason
			res rt.Result
		}{
			"timeout": {rt.StepFailed, rt.ReasonStepTimeout, rt.Result{Code: rt.ResultTimedOut, Reason: rt.ReasonStepTimeout}},
			"lost":    {rt.StepFailed, rt.ReasonWorkloadLost, rt.Result{Code: rt.ResultLost, Reason: rt.ReasonWorkloadLost}},
			"exit":    {rt.StepFailed, rt.ReasonCommandFailed, rt.Result{Code: rt.ResultFailed, ExitCode: 3, Reason: rt.ReasonCommandFailed}},
			"cancel":  {rt.StepCancelled, rt.ReasonCancelled, rt.Result{Code: rt.ResultCancelled}},
		} {
			s := step()
			if err := s.Finish(tc.to, tc.why, tc.res); err != nil || s.Result != tc.res {
				t.Errorf("%s: %v %+v", name, err, s.Result)
			}
		}
		// A wait step carries no workload result.
		w, _ := rt.NewStep("review", rt.StepWait)
		must(t, w.Advance(rt.StepWaiting, rt.ReasonNone))
		if err := w.Finish(rt.StepSucceeded, rt.ReasonNone, okRes); err == nil {
			t.Error("a wait step recorded a workload result")
		}
		w.Attempt = 1
		if w.Validate() == nil {
			t.Error("a wait step has no attempt")
		}
	})

	t.Run("a launch is retried only when it provably did not reach the backend", func(t *testing.T) {
		newRun := func() rt.Run {
			r := mustRun(t)
			must(t, r.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
			return r
		}
		launch := func(state rt.CommandState, reason rt.Reason, attempt int) rt.Command {
			c, err := rt.NewWorkloadCommand("c", bind("build", attempt, 1), rt.CommandLaunch, t0)
			must(t, err)
			if state != rt.CommandIntent {
				must(t, c.Settle(state, reason))
			}
			return c
		}
		r := newRun()
		for name, c := range map[string]rt.Command{
			"intent without outcome": launch(rt.CommandIntent, "", 1),
			"uncertain":              launch(rt.CommandUncertain, rt.ReasonLaunchUncertain, 1),
			"confirmed":              launch(rt.CommandConfirmed, "", 1),
			"older attempt":          launch(rt.CommandRejected, rt.ReasonLaunchNotReached, 2),
		} {
			v := r.Version
			if err := r.RetryStep("build", c); err == nil || r.Steps[0].Attempt != 1 || r.Version != v {
				t.Errorf("%s: retried (%v)", name, err)
			}
		}
		stop, _ := rt.NewWorkloadCommand("s", bind("build", 1, 1), rt.CommandStop, t0)
		must(t, stop.Settle(rt.CommandRejected, rt.ReasonLaunchNotReached))
		if err := r.RetryStep("build", stop); err == nil {
			t.Error("a stop is not a launch")
		}
		must(t, r.RetryStep("build", launch(rt.CommandRejected, rt.ReasonLaunchNotReached, 1)))
		if r.Steps[0].Attempt != 2 {
			t.Fatalf("attempt %d", r.Steps[0].Attempt)
		}
		if err := r.RetryStep("review", launch(rt.CommandRejected, rt.ReasonLaunchNotReached, 1)); err == nil {
			t.Error("a wait step cannot be retried")
		}
		r.RequestCancel()
		if err := r.RetryStep("build", launch(rt.CommandRejected, rt.ReasonLaunchNotReached, 2)); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("retry while cancelling: %v", err)
		}
	})

	t.Run("workload commands carry a matching binding", func(t *testing.T) {
		b := bind("build", 1, 1)
		c, err := rt.NewWorkloadCommand("c", b, rt.CommandLaunch, t0)
		must(t, err)
		if c.Binding != b || c.Target != b.WorkloadID || c.Fence != b.Fence {
			t.Fatalf("command %+v", c)
		}
		if _, err := rt.NewCommand("c", scope, "run1", "build", rt.CommandLaunch, "wl", "d", 1, t0); err == nil {
			t.Error("a workload command without a binding accepted")
		}
		if _, err := rt.NewWorkloadCommand("c", b, rt.CommandEnsureResource, t0); err == nil {
			t.Error("a resource command with a binding accepted")
		}
		for name, mutate := range map[string]func(*rt.Command){
			"other target": func(x *rt.Command) { x.Target = "other" },
			"other step":   func(x *rt.Command) { x.StepKey = "test" },
			"other fence":  func(x *rt.Command) { x.Fence = 2 },
			"other digest": func(x *rt.Command) { x.SpecDigest = "sha256:x" },
			"no backend":   func(x *rt.Command) { x.Binding.Backend = "" },
		} {
			x := c
			mutate(&x)
			if x.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if err := c.RecordResult(okRes); err == nil {
			t.Error("a result on an unsettled command")
		}
		must(t, c.Settle(rt.CommandConfirmed, rt.ReasonNone))
		if err := c.RecordResult(rt.Result{Code: "bogus"}); err == nil {
			t.Error("malformed result accepted")
		}
		must(t, c.RecordResult(okRes))
		res, _ := rt.NewCommand("r", scope, "run1", "", rt.CommandEnsureResource, "ws", "d", 1, t0)
		must(t, res.Settle(rt.CommandConfirmed, rt.ReasonNone))
		if res.RecordResult(okRes) == nil {
			t.Error("a resource command recorded a workload result")
		}
	})

	t.Run("a retained observation never regresses", func(t *testing.T) {
		b := bind("build", 1, 2)
		exited := rt.WorkloadObservation{Binding: b, Phase: rt.PhaseExited, Result: okRes, ObservedAt: t0.Add(minute)}
		running := rt.WorkloadObservation{Binding: b, Phase: rt.PhaseRunning, ObservedAt: t0}
		must(t, rt.CheckObservationUpdate(running, exited))
		must(t, rt.CheckObservationUpdate(exited, exited))
		absent := exited
		absent.Phase, absent.Result, absent.ObservedAt = rt.PhaseAbsent, rt.Result{}, t0.Add(2*minute)
		must(t, rt.CheckObservationUpdate(exited, absent))

		older := running
		older.ObservedAt = t0.Add(-time.Second)
		stale := running
		stale.Binding.Fence = 1
		stale.ObservedAt = t0.Add(2 * minute)
		other := running
		other.Binding.Attempt = 2
		digest := running
		digest.Binding.SpecDigest = "sha256:other"
		changed := exited
		changed.Result = rt.Result{Code: rt.ResultFailed, ExitCode: 1}
		for name, tc := range map[string]struct {
			prev, next rt.WorkloadObservation
			want       error
		}{
			"exited back to running":    {exited, rt.WorkloadObservation{Binding: b, Phase: rt.PhaseRunning, ObservedAt: t0.Add(3 * minute)}, rt.ErrIllegalTransition},
			"exited changes its result": {exited, changed, rt.ErrIllegalTransition},
			"older observation":         {exited, older, domain.ErrInvalidValue},
			"older fence":               {exited, stale, rt.ErrStaleFence},
			"another attempt":           {running, other, domain.ErrInvalidValue},
			"another specification":     {running, digest, rt.ErrSpecMismatch},
		} {
			if err := rt.CheckObservationUpdate(tc.prev, tc.next); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// exhaustedRun returns a cancelled run whose cleanup is exhausted with one
// workload and one service retained.
func exhaustedRun(t *testing.T, rem rt.Remaining) rt.Run {
	t.Helper()
	r := mustRun(t)
	must(t, r.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
	r.RequestCancel()
	must(t, r.AdvanceStep("build", rt.StepCancelled, rt.ReasonCancelled))
	for _, k := range []domain.StepID{"review", "notify", "test"} {
		must(t, r.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
	}
	must(t, r.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
	must(t, r.RecordCleanupRemaining(rem, rt.ReasonStopUncertain))
	must(t, r.RecordCleanupRemaining(rem, rt.ReasonStopUncertain))
	return r
}

// testRetainedWorkloads proves finding 2.
func testRetainedWorkloads(t *testing.T) {
	t.Run("exhaustion with only a workload remaining", func(t *testing.T) {
		b := bind("build", 1, 1)
		r := exhaustedRun(t, rt.Remaining{Workloads: []rt.RuntimeBinding{b}})
		if !r.Terminal() || r.Cleanup.State != rt.CleanupExhausted || len(r.Cleanup.Retained) != 0 ||
			len(r.Cleanup.RetainedWorkloads) != 1 || r.Cleanup.RetainedWorkloads[0] != b || r.Cleanup.Reason != rt.ReasonStopUncertain {
			t.Fatalf("exhausted: %+v", r.Cleanup)
		}
		got, a, c := roundTrip(t, r)
		if a != c || got.Validate() != nil || got.Cleanup.RetainedWorkloads[0] != b {
			t.Fatalf("the retained workload does not survive storage: %v", got.Validate())
		}
		must(t, r.CompleteCleanup())
		if len(r.Cleanup.RetainedWorkloads) != 0 || r.Cleanup.State != rt.CleanupComplete {
			t.Fatalf("complete: %+v", r.Cleanup)
		}
	})

	t.Run("retained identities must belong to the run", func(t *testing.T) {
		b := bind("build", 1, 1)
		bad := map[string]rt.Remaining{
			"workload of another tenant":  {Workloads: []rt.RuntimeBinding{func() rt.RuntimeBinding { x := b; x.Scope = other; return x }()}},
			"workload of another run":     {Workloads: []rt.RuntimeBinding{func() rt.RuntimeBinding { x := b; x.RunID = "run2"; return x }()}},
			"workload of an unknown step": {Workloads: []rt.RuntimeBinding{bind("nope", 1, 1)}},
			"unbound workload":            {Workloads: []rt.RuntimeBinding{{}}},
			"shared resource":             {Resources: []rt.ResourceHandle{projectHandle(scope, "db")}},
			"another run's resource":      {Resources: []rt.ResourceHandle{func() rt.ResourceHandle { h := runHandle("db"); h.RunID = "run2"; return h }()}},
			"nothing":                     {},
		}
		for name, rem := range bad {
			r := mustRun(t)
			r.RequestCancel()
			for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
				must(t, r.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
			}
			must(t, r.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
			before := r
			if err := r.RecordCleanupRemaining(rem, rt.ReasonStopUncertain); err == nil || r.Version != before.Version || r.Cleanup.Attempts != 0 {
				t.Errorf("%s accepted: %v", name, err)
			}
		}
		// Both kinds together are recorded.
		r := exhaustedRun(t, rt.Remaining{Workloads: []rt.RuntimeBinding{b}, Resources: []rt.ResourceHandle{runHandle("db")}})
		if len(r.Cleanup.Retained) != 1 || len(r.Cleanup.RetainedWorkloads) != 1 {
			t.Errorf("retained %+v", r.Cleanup)
		}
	})
}

// testLateReconciliation proves finding 3.
func testLateReconciliation(t *testing.T) {
	t.Run("exhaustion, takeover and confirmed completion", func(t *testing.T) {
		b := bind("build", 1, 1)
		db := runHandle("db")
		r := exhaustedRun(t, rt.Remaining{Workloads: []rt.RuntimeBinding{b}, Resources: []rt.ResourceHandle{db}})
		if !r.Terminal() || r.Status() != rt.StatusCancelled {
			t.Fatalf("terminal %v status %s", r.Terminal(), r.Status())
		}

		// Execution and general reconciliation stay closed; only a cleanup claim is admitted.
		for _, p := range []rt.ClaimPurpose{rt.ClaimExecute, rt.ClaimReconcile} {
			if err := r.Admits(p); !errors.Is(err, rt.ErrExecutionClosed) {
				t.Errorf("%s after exhaustion: %v", p, err)
			}
		}
		must(t, r.Admits(rt.ClaimCleanup))
		if err := r.AdvanceStep("build", rt.StepRunning, rt.ReasonNone); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("a step moved after exhaustion: %v", err)
		}
		if err := r.RecordCleanupRemaining(rt.Remaining{Resources: []rt.ResourceHandle{db}}, rt.ReasonCleanupFailed); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("a new attempt after exhaustion: %v", err)
		}

		// The earlier lease expired; another worker takes over with a higher fence.
		prev := rt.Claim{Scope: scope, RunID: "run1", Owner: "worker-a", Purpose: rt.ClaimExecute, Fence: 3, Expires: t0.Add(minute)}
		now := t0.Add(time.Hour)
		cleanup, err := rt.AcquireClaim(prev, rt.ClaimRequest{Scope: scope, RunID: "run1", Owner: "worker-b", Purpose: rt.ClaimCleanup, TTL: minute}, now)
		if err != nil || cleanup.Fence != 4 || cleanup.Purpose != rt.ClaimCleanup {
			t.Fatalf("takeover: %+v %v", cleanup, err)
		}
		must(t, cleanup.Authorizes(cleanup, now))

		// A reconciliation that finds something new is refused.
		v := r.Version
		if err := r.ReconcileExhausted(rt.Remaining{Resources: []rt.ResourceHandle{runHandle("other")}}); err == nil || r.Version != v {
			t.Errorf("unretained resource accepted: %v", err)
		}
		if err := r.ReconcileExhausted(rt.Remaining{Workloads: []rt.RuntimeBinding{bind("build", 2, 1)}}); err == nil {
			t.Error("unretained workload accepted")
		}
		// The workload is confirmed gone; the service remains. Still terminal and exhausted.
		must(t, r.ReconcileExhausted(rt.Remaining{Resources: []rt.ResourceHandle{db}}))
		if !r.Terminal() || r.Cleanup.State != rt.CleanupExhausted || len(r.Cleanup.RetainedWorkloads) != 0 || len(r.Cleanup.Retained) != 1 {
			t.Fatalf("partial: %+v", r.Cleanup)
		}
		must(t, r.Validate())
		// Everything confirmed: cleanup completes and the run stays terminal.
		must(t, r.ReconcileExhausted(rt.Remaining{}))
		if r.Cleanup.State != rt.CleanupComplete || !r.Terminal() || r.Cleanup.Reason != rt.ReasonNone || r.Cleanup.Attempts != 2 {
			t.Fatalf("complete: %+v", r.Cleanup)
		}
		if err := r.Admits(rt.ClaimCleanup); !errors.Is(err, rt.ErrExecutionClosed) {
			t.Errorf("a completed run admits a claim: %v", err)
		}
		if err := r.ReconcileExhausted(rt.Remaining{}); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("reconcile after completion: %v", err)
		}
	})

	t.Run("pending cleanup cannot use the exhausted path", func(t *testing.T) {
		r := mustRun(t)
		r.RequestCancel()
		for _, k := range []domain.StepID{"build", "review", "notify", "test"} {
			must(t, r.AdvanceStep(k, rt.StepCancelled, rt.ReasonCancelled))
		}
		must(t, r.Settle(rt.OutcomeCancelled, rt.ReasonCancelled))
		if err := r.ReconcileExhausted(rt.Remaining{}); !errors.Is(err, rt.ErrIllegalTransition) {
			t.Errorf("pending: %v", err)
		}
	})
}

// testLeaseRenewal proves finding 4.
func testLeaseRenewal(t *testing.T) {
	req := rt.ClaimRequest{Scope: scope, RunID: "run1", Owner: "worker-a", Purpose: rt.ClaimExecute, TTL: minute}

	t.Run("same-owner renewal keeps the fence and persists a later expiry", func(t *testing.T) {
		held, err := rt.AcquireClaim(rt.Claim{}, req, t0)
		must(t, err)
		// The lease is active, so even the owner cannot re-acquire it: renewal is the path.
		if _, err := rt.AcquireClaim(held, req, t0.Add(10*time.Second)); !errors.Is(err, rt.ErrClaimHeld) {
			t.Fatalf("re-acquire of the caller's own lease: %v", err)
		}
		now := t0.Add(30 * time.Second)
		renewed, err := rt.RenewClaim(held, held, now, 2*minute)
		if err != nil || renewed.Fence != held.Fence || renewed.Owner != held.Owner || renewed.Purpose != held.Purpose ||
			!renewed.Expires.Equal(now.Add(2*minute)) {
			t.Fatalf("renew: %+v %v", renewed, err)
		}
		// A shorter renewal never shortens the lease.
		shorter, err := rt.RenewClaim(renewed, renewed, now.Add(time.Second), time.Second)
		if err != nil || !shorter.Expires.Equal(renewed.Expires) {
			t.Fatalf("shorter renewal: %+v %v", shorter, err)
		}
		// A holder presenting its pre-renewal copy is still authorized against the stored claim.
		must(t, held.Authorizes(renewed, now.Add(time.Minute)))
		again, err := rt.RenewClaim(renewed, held, now.Add(time.Minute), minute)
		if err != nil || again.Fence != held.Fence {
			t.Fatalf("renew with an old copy: %+v %v", again, err)
		}

		// Renewal is also carried by a commit: a transition validates with RenewTTL.
		run := mustRun(t)
		next := run
		must(t, next.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
		tr := rt.Transition{Scope: scope, RunID: "run1", Claim: held, Now: now, ExpectedVersion: run.Version, Run: next, RenewTTL: minute}
		must(t, tr.Validate())
	})

	t.Run("renewal is refused after takeover, expiry, and for foreign or forged claims", func(t *testing.T) {
		held, _ := rt.AcquireClaim(rt.Claim{}, req, t0)
		takeover := req
		takeover.Owner = "worker-b"
		later := t0.Add(2 * minute)
		current, err := rt.AcquireClaim(held, takeover, later)
		must(t, err)
		if current.Fence != 2 {
			t.Fatalf("fence %d", current.Fence)
		}
		// The superseded writer cannot renew, and nothing it presents changes the stored claim.
		if _, err := rt.RenewClaim(current, held, later.Add(time.Second), minute); !errors.Is(err, rt.ErrStaleFence) {
			t.Errorf("superseded writer: %v", err)
		}
		// Expired: neither the stale nor the current holder can renew without a takeover.
		if _, err := rt.RenewClaim(held, held, t0.Add(time.Hour), minute); !errors.Is(err, rt.ErrClaimExpired) {
			t.Errorf("expired lease: %v", err)
		}
		// Same fence with another owner is forged.
		forged := current
		forged.Owner = "worker-c"
		if _, err := rt.RenewClaim(current, forged, later.Add(time.Second), minute); !errors.Is(err, rt.ErrStaleFence) {
			t.Errorf("forged owner: %v", err)
		}
		// Another tenant's or run's claim is refused.
		foreign := current
		foreign.Scope = other
		if _, err := rt.RenewClaim(current, foreign, later.Add(time.Second), minute); err == nil {
			t.Error("foreign scope renewed")
		}
		foreign = current
		foreign.RunID = "run2"
		if _, err := rt.RenewClaim(current, foreign, later.Add(time.Second), minute); err == nil {
			t.Error("foreign run renewed")
		}
		if _, err := rt.RenewClaim(current, current, later, 0); !errors.Is(err, domain.ErrInvalidValue) {
			t.Errorf("zero ttl: %v", err)
		}
		if _, err := rt.RenewClaim(rt.Claim{}, rt.Claim{}, later, minute); err == nil {
			t.Error("the zero claim renewed")
		}
	})
}

// testTenantOwnership proves finding 5.
func testTenantOwnership(t *testing.T) {
	p1 := scope
	p2 := domain.Scope{TenantID: "t1", ProjectID: "p2"}
	svc := func(s domain.Scope, lifetime rt.Lifetime, run domain.RunID) rt.ResourceSpec {
		return rt.ResourceSpec{Scope: s, Owner: rt.OwnerFor(lifetime, s), RunID: run, Kind: rt.ResourceService, Name: "cache",
			Lifetime: lifetime, Identity: "cache", SpecDigest: "d", Fence: 1,
			Service: &rt.ServiceSpec{Name: "cache", Lifetime: lifetime, Image: "valkey", Ports: []int{6379}}}
	}

	t.Run("a tenant service is owned by the tenant and shared across projects", func(t *testing.T) {
		a, b := svc(p1, rt.LifetimeTenant, "run1"), svc(p2, rt.LifetimeTenant, "run2")
		must(t, a.Validate())
		must(t, b.Validate())
		if a.Owner != (domain.Owner{TenantID: "t1"}) || a.Owner != b.Owner || a.Scope == b.Scope {
			t.Fatalf("owners %+v %+v", a.Owner, b.Owner)
		}
		ha, hb := a.Handle(), b.Handle()
		must(t, ha.Validate())
		must(t, hb.Validate())
		if ha.Owner != hb.Owner || ha.Lifetime != rt.LifetimeTenant || ha.Identity != hb.Identity {
			t.Fatalf("handles %+v %+v", ha, hb)
		}
		// Each project's run may use the shared service; the requesting scope stays explicit.
		must(t, ha.Reachable(p1))
		must(t, ha.Reachable(p2))
		got, x, y := roundTrip(t, ha)
		if x != y || got != ha {
			t.Fatalf("the tenant handle does not survive storage: %+v", got)
		}
		if tenantOwned := (rt.ResourceHandle{}); tenantOwned.Validate() == nil {
			t.Error("zero handle")
		}

		// A project-lifetime service of p1 is not reachable from p2; neither is another tenant's resource.
		proj := svc(p1, rt.LifetimeProject, "").Handle()
		must(t, proj.Validate())
		must(t, proj.Reachable(p1))
		if err := proj.Reachable(p2); !errors.Is(err, rt.ErrForeignResource) {
			t.Errorf("project resource from another project: %v", err)
		}
		foreignTenant := domain.Scope{TenantID: "t2", ProjectID: "p1"}
		for _, h := range []rt.ResourceHandle{ha, proj, runHandle("db")} {
			if err := h.Reachable(foreignTenant); !errors.Is(err, rt.ErrForeignResource) {
				t.Errorf("%s/%s reachable by a foreign tenant: %v", h.Lifetime, h.Name, err)
			}
		}
		// The same name in another tenant is a different resource.
		if o := svc(foreignTenant, rt.LifetimeTenant, "run9"); o.Owner == a.Owner || o.Handle().Reachable(p1) == nil {
			t.Error("another tenant's service is not the same resource")
		}
	})

	t.Run("ownership must follow from lifetime and requesting scope", func(t *testing.T) {
		for name, mutate := range map[string]func(*rt.ResourceSpec){
			"tenant service owned by a project": func(s *rt.ResourceSpec) { s.Owner = domain.Owner{TenantID: "t1", ProjectID: "p1"} },
			"project service owned by a tenant": func(s *rt.ResourceSpec) { s.Lifetime, s.Service.Lifetime = rt.LifetimeProject, rt.LifetimeProject },
			"owned by another tenant":           func(s *rt.ResourceSpec) { s.Owner = domain.Owner{TenantID: "t2"} },
			"no owner":                          func(s *rt.ResourceSpec) { s.Owner = domain.Owner{} },
		} {
			s := svc(p1, rt.LifetimeTenant, "")
			mutate(&s)
			if err := s.Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		h := tenantHandle(p1, "cache")
		for name, mutate := range map[string]func(*rt.ResourceHandle){
			"no lifetime":         func(x *rt.ResourceHandle) { x.Lifetime = "" },
			"project owner":       func(x *rt.ResourceHandle) { x.Owner = domain.Owner{TenantID: "t1", ProjectID: "p1"} },
			"foreign tenant":      func(x *rt.ResourceHandle) { x.Owner = domain.Owner{TenantID: "t2"} },
			"no requesting scope": func(x *rt.ResourceHandle) { x.Scope = domain.Scope{} },
			"run lifetime w/o run": func(x *rt.ResourceHandle) {
				x.Lifetime, x.Owner = rt.LifetimePipeline, rt.OwnerFor(rt.LifetimePipeline, x.Scope)
			},
		} {
			x := h
			mutate(&x)
			if x.Validate() == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})

	t.Run("a run's transition observes the shared service and refuses foreign ones", func(t *testing.T) {
		run, err := rt.NewRun(p2, "run2", "pipe1", mustPlan(t), 2)
		must(t, err)
		next := run
		must(t, next.AdvanceStep("build", rt.StepRunning, rt.ReasonNone))
		claim := rt.Claim{Scope: p2, RunID: "run2", Owner: "w", Purpose: rt.ClaimExecute, Fence: 1, Expires: t0.Add(minute)}
		obs := func(h rt.ResourceHandle) rt.ResourceObservation {
			return rt.ResourceObservation{Handle: h, State: rt.ResourceReady, ObservedAt: t0}
		}
		tr := func(o rt.ResourceObservation) rt.Transition {
			return rt.Transition{Scope: p2, RunID: "run2", Claim: claim, Now: t0, ExpectedVersion: run.Version, Run: next, Resources: []rt.ResourceObservation{o}}
		}
		// The tenant-owned service, requested by this project's run, is accepted.
		shared := obs(tenantHandle(p2, "cache"))
		must(t, shared.Validate())
		must(t, tr(shared).Validate())
		// Another project's handle, another tenant's, and a project-owned resource of p1 are refused.
		for name, h := range map[string]rt.ResourceHandle{
			"handle requested by another project": tenantHandle(p1, "cache"),
			"another tenant":                      tenantHandle(domain.Scope{TenantID: "t2", ProjectID: "p2"}, "cache"),
			"another project's own resource":      projectHandle(p1, "db"),
		} {
			if err := tr(obs(h)).Validate(); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Another run's pipeline-lifetime resource is refused too.
		other := runHandle("db")
		other.Scope, other.Owner, other.RunID = p2, rt.OwnerFor(rt.LifetimePipeline, p2), "run9"
		if err := tr(obs(other)).Validate(); err == nil {
			t.Error("another run's resource accepted")
		}
	})
}
