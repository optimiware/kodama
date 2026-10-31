package runner_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// Test_unit_kernel_engine_progression proves the engine progresses a run through
// the kernel ports from durable state alone: claim, immutable plan, intent before
// effect, launch or adopt by stable identity, observe, settle, the next eligible
// step, and reconciliation after a restart or an expired lease.
func Test_unit_kernel_engine_progression(t *testing.T) {
	t.Run("a plain pipeline runs one step at a time in plan order", func(t *testing.T) {
		e := newEnv(t)
		p := plan("seq", ctr("a"), ctr("b", "a"), ctr("c", "b"))
		rec := e.admit("run1", p, 3)
		for _, k := range []domain.StepID{"a", "b", "c"} {
			e.rt.scripts[k] = script{polls: 2}
		}
		e.rt.onStart = func(runtime.StartRequest) {
			e.expect(e.rt.liveCount() == 0, "a step started while a prior workload still held the workspace")
		}
		rep := e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && got.Run.Reason == runtime.ReasonNone, "outcome: %+v", got.Run)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[a b c]", "start order %v", e.rt.startOrder)
		e.expect(e.rt.maxLive == 1, "at most one workload at a time, saw %d", e.rt.maxLive)
		e.expect(rep.Run.Outcome == runtime.OutcomeSucceeded, "the report carries the settled run")
		for _, k := range []domain.StepID{"a", "b", "c"} {
			s := stepOf(got, k)
			e.expect(s.State == runtime.StepSucceeded && s.Attempt == 1 && s.Result.Code == runtime.ResultSucceeded, "step %s: %+v", k, s)
		}
		e.expect(got.Plan.Digest == rec.Plan.Digest && len(got.Plan.Steps) == 3, "the admitted plan snapshot is what ran")
		e.expect(got.Claim.Owner == "w1" && got.Claim.Fence == 1, "the run was claimed with a fence: %+v", got.Claim)
		ev := e.outbox("run1")
		e.expect(countKind(ev, runtime.EventStepStateChanged) >= 6 && countKind(ev, runtime.EventRunStateChanged) >= 1,
			"state changes were committed with outbox intent: %d events", len(ev))
	})

	t.Run("intent is durable before every external effect", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("intent", ctr("a")), 3)
		intent := func(kind runtime.CommandKind, b runtime.RuntimeBinding) {
			rec := e.load("run1")
			for _, c := range rec.Commands {
				if c.Kind == kind && c.Target == b.WorkloadID && c.State == runtime.CommandIntent &&
					c.Fence == b.Fence && c.SpecDigest == b.SpecDigest && c.Binding == b {
					return
				}
			}
			t.Errorf("no %s intent with the workload identity, digest and fence before the effect: %+v", kind, rec.Commands)
		}
		e.rt.onStart = func(req runtime.StartRequest) {
			intent(runtime.CommandLaunch, req.Binding)
			s := stepOf(e.load("run1"), "a")
			e.expect(s.State == runtime.StepRunning, "the step is running before the workload starts")
		}
		e.rt.onEffect = func(kind string, b runtime.RuntimeBinding) {
			intent(map[string]runtime.CommandKind{"stop": runtime.CommandStop, "remove": runtime.CommandRemove}[kind], b)
		}
		e.res.ensureHook = func(_ int, spec runtime.ResourceSpec) (bool, error) {
			rec := e.load("run1")
			for _, c := range rec.Commands {
				if c.Kind == runtime.CommandEnsureResource && c.Target == spec.Identity && c.State == runtime.CommandIntent && c.Fence == spec.Fence {
					return true, nil
				}
			}
			t.Errorf("no ensure intent before the workspace was created: %+v", rec.Commands)
			return true, nil
		}
		e.drive(e.engine("w1"), "run1")

		rec := e.load("run1")
		for _, c := range rec.Commands {
			e.expect(c.State == runtime.CommandConfirmed, "command %s ended %s", c.ID, c.State)
		}
		e.expect(e.rt.count(e.rt.removes) == 1 && e.rt.count(e.rt.starts) == 1, "one start, one removal")
		e.expect(len(rec.Workloads) == 1 && rec.Workloads[0].Phase == runtime.PhaseAbsent, "the removal is the retained observation: %+v", rec.Workloads)
	})

	t.Run("a takeover adopts the running workload and never launches it again", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("take", ctr("a"), ctr("b", "a")), 3)
		e.rt.scripts["a"] = script{polls: 6}
		sleeps := 0
		e.onSleep = func() error {
			if sleeps++; sleeps == 2 {
				return errCrash
			}
			return nil
		}
		_, err := e.engine("w1").Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
		e.expect(errors.Is(err, errCrash), "the first engine crashed: %v", err)
		mid := e.load("run1")
		e.expect(stepOf(mid, "a").State == runtime.StepRunning && mid.Claim.Fence == 1, "the run is mid-step: %+v", mid.Run.Steps)

		// The lease still holds: nobody else may claim yet.
		rep := e.drive(e.engine("w2"), "run1")
		e.expect(rep.Status == runner.DriveSkipped, "a held claim is skipped, got %s", rep.Status)

		e.onSleep = nil
		e.expire()
		rep = e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && got.Claim.Owner == "w2" && got.Claim.Fence == 2, "takeover: %+v %+v", got.Run, got.Claim)
		e.expect(e.rt.count(e.rt.starts) == 2, "each step was launched exactly once across the takeover: %v", e.rt.starts)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[a b]", "start order %v", e.rt.startOrder)
	})

	t.Run("a stale writer cannot commit after a takeover", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("stale", ctr("a")), 3)
		e.rt.scripts["a"] = script{polls: 6}
		old := e.engine("old")
		took := false
		e.onSleep = func() error {
			if took {
				return nil
			}
			took = true
			e.expire()
			// A second worker takes the run and finishes it while the first sleeps.
			e.onSleep = nil
			e.rt.scripts["a"] = script{}
			rep := e.drive(e.engine("new"), "run1")
			e.expect(rep.Run.Outcome == runtime.OutcomeSucceeded, "the new owner finished the run: %+v", rep.Run)
			return nil
		}
		rep, err := old.Drive(context.Background(), e.ref("run1", runtime.ClaimExecute))
		e.expect(err == nil && rep.Status == runner.DriveLost, "the superseded engine stops quietly: %v %v", rep.Status, err)
		got := e.load("run1")
		e.expect(got.Claim.Owner == "new" && got.Claim.Fence == 2, "the new owner holds the claim: %+v", got.Claim)
		s := stepOf(got, "a")
		e.expect(s.State == runtime.StepSucceeded && s.Result.Code == runtime.ResultSucceeded, "the stale writer overwrote nothing: %+v", s)
	})

	t.Run("a failed step fails the run and skips what never started", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("fast", ctr("a"), ctr("b", "a"), ctr("c", "b")), 3)
		e.rt.scripts["b"] = script{result: runtime.Result{Code: runtime.ResultFailed, ExitCode: 3}}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonCommandFailed, "run: %+v", got.Run)
		e.expect(fmt.Sprint(states(got)) == "map[a:succeeded b:failed c:skipped]", "states: %v", states(got))
		b, c := stepOf(got, "b"), stepOf(got, "c")
		e.expect(b.Reason == runtime.ReasonCommandFailed && b.Result.ExitCode == 3, "step b: %+v", b)
		e.expect(c.Reason == runtime.ReasonRunFailed, "step c: %+v", c)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[a b]", "c never started: %v", e.rt.startOrder)
	})

	t.Run("a step past its deadline is stopped and fails with step_timeout", func(t *testing.T) {
		e := newEnv(t)
		slow := ctr("a")
		slow.Workload.Timeout = 5 * time.Second
		e.admit("run1", plan("slow", slow, ctr("b", "a")), 3)
		e.rt.scripts["a"] = script{polls: 1000}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonStepTimeout && a.Result.Code == runtime.ResultTimedOut, "step a: %+v", a)
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonStepTimeout, "run: %+v", got.Run)
		e.expect(e.rt.count(e.rt.stops) == 1 && e.rt.liveCount() == 0, "the workload was stopped and removed")
		e.expect(stepOf(got, "b").State == runtime.StepSkipped, "b did not start")
	})

	t.Run("an unconfirmed removal stops the run before the next step starts", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("release", ctr("a"), ctr("b", "a")), 3)
		e.rt.removeHook = func(n int, _ runtime.RuntimeBinding) (bool, error) {
			if n <= 3 {
				return false, fmt.Errorf("%w: the backend was unreachable", runtime.ErrNotReached)
			}
			return true, nil
		}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(fmt.Sprint(e.rt.startOrder) == "[a]", "b never started while a's workspace was unreleased: %v", e.rt.startOrder)
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonStopUncertain, "run: %+v", got.Run)
		e.expect(stepOf(got, "a").State == runtime.StepSucceeded && stepOf(got, "b").State == runtime.StepSkipped, "states: %v", states(got))
		e.expect(e.rt.count(e.rt.removes) == 4, "a removal that provably did not reach the backend is issued again, boundedly, then cleanup finishes it: %d", e.rt.count(e.rt.removes))
		e.expect(got.Run.Cleanup.State == runtime.CleanupComplete && e.rt.liveCount() == 0, "cleanup removed it: %+v", got.Run.Cleanup)
	})

	t.Run("a lost workspace fails the run and is never recreated", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("lost", ctr("a"), ctr("b", "a")), 3)
		e.rt.onEffect = func(kind string, _ runtime.RuntimeBinding) {
			if kind == "remove" {
				e.res.lose("workspace")
			}
		}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonWorkspaceLost, "run: %+v", got.Run)
		e.expect(fmt.Sprint(e.rt.startOrder) == "[a]" && stepOf(got, "b").State == runtime.StepSkipped, "b never started: %v", e.rt.startOrder)
		e.expect(len(e.res.ensures) == 1, "no replacement workspace was prepared: %v", e.res.ensures)
	})

	t.Run("run services are ready before the first step and a lost one fails the run", func(t *testing.T) {
		e := newEnv(t)
		p := plan("svc", ctr("a"), ctr("b", "a"))
		p.Services = []runtime.ServiceSpec{{Name: "db", Lifetime: runtime.LifetimePipeline, Image: "postgres:16"}}
		e.admit("run1", p, 3)
		e.res.starting["db"] = 3
		e.rt.onStart = func(req runtime.StartRequest) {
			e.expect(len(req.Services) == 1 && req.Services[0].Name == "db", "the workload sees its ready service: %+v", req.Services)
			e.expect(req.Workspace.Kind == runtime.ResourceWorkspace && req.Workspace.RunID == "run1", "and the run's workspace: %+v", req.Workspace)
		}
		e.rt.onEffect = func(kind string, b runtime.RuntimeBinding) {
			if kind == "remove" && b.StepKey == "a" {
				e.res.lose("db")
			}
		}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Reason == runtime.ReasonServiceLost && fmt.Sprint(e.rt.startOrder) == "[a]", "run: %+v starts %v", got.Run, e.rt.startOrder)
		e.expect(len(e.res.ensures) == 2, "the workspace and the service were each created once: %v", e.res.ensures)
	})

	t.Run("a backend that cannot satisfy a step is refused before anything is created", func(t *testing.T) {
		e := newEnv(t)
		hard := ctr("a")
		hard.Workload.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		e.admit("run1", plan("caps", hard), 3)
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Reason == runtime.ReasonCapabilityUnsatisfied && stepOf(got, "a").State == runtime.StepSkipped, "run: %+v", got.Run)
		e.expect(len(e.res.ensures) == 0 && e.rt.count(e.rt.starts) == 0, "nothing was created or started")
	})

	t.Run("declared secrets are resolved for the step and released when it settles", func(t *testing.T) {
		e := newEnv(t)
		a := ctr("a")
		a.Workload.Secrets = []runtime.SecretRef{{Owner: domain.Owner{TenantID: "t1", ProjectID: "p1"}, Name: "TOKEN"}}
		e.admit("run1", plan("sec", a, ctr("b", "a")), 3)
		e.rt.onStart = func(req runtime.StartRequest) {
			if req.Binding.StepKey == "a" {
				e.expect(req.Secrets.StepKey == "a" && len(req.Secrets.Refs) == 1, "the workload receives its declared references: %+v", req.Secrets)
			} else {
				e.expect(req.Secrets.Directory == "", "an undeclared step gets no materialization")
			}
		}
		e.drive(e.engine("w1"), "run1")
		e.expect(fmt.Sprint(e.secrets.resolved) == "[a]" && fmt.Sprint(e.secrets.released) == "[a]", "resolved %v released %v", e.secrets.resolved, e.secrets.released)

		e2 := newEnv(t)
		e2.admit("run1", plan("sec2", a), 3)
		e2.secrets.resolveFn = func(domain.StepID) error { return errors.New("vault down") }
		e2.drive(e2.engine("w1"), "run1")
		got := e2.load("run1")
		s := stepOf(got, "a")
		e2.expect(s.State == runtime.StepFailed && s.Reason == runtime.ReasonSecretUnavailable && e2.rt.count(e2.rt.starts) == 0, "step: %+v", s)
	})

	t.Run("an engine revokes the secrets it materialized before the next step starts", revocationIsFaithful)
	t.Run("a replacement engine revokes from the record and resumes; with no resolver it holds", revocationResumesAfterTakeover)
	t.Run("a Resolve that completes late leaves no material behind", lateResolveLeavesNothingBehind)
	t.Run("a failed revocation is retried in place and then stops the run for the next claim", failedRevocationStopsTheRun)

	// Real backend evidence, apart from the fault-injecting fakes above.
	t.Run("process backend", processProgression)
}
