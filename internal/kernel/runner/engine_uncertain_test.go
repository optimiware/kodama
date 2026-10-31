package runner_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

// crashRuntime panics around Start to stand for a process that died there: the
// durable record is whatever had committed by then.
type crashRuntime struct {
	*fakeRuntime
	beforeStart, afterStart bool
}

func (c *crashRuntime) Start(ctx context.Context, req runtime.StartRequest) (runtime.WorkloadObservation, error) {
	if c.beforeStart {
		panic(errCrash)
	}
	obs, err := c.fakeRuntime.Start(ctx, req)
	if c.afterStart {
		panic(errCrash)
	}
	return obs, err
}

// crash drives an engine until it dies in the runtime, as a killed process does.
func (e *env) crash(eng *runner.Engine, id domain.RunID) {
	e.t.Helper()
	defer func() {
		if r := recover(); r != errCrash {
			e.t.Fatalf("expected the simulated crash, got %v", r)
		}
	}()
	_, _ = eng.Drive(context.Background(), e.ref(id, runtime.ClaimExecute))
	e.t.Fatal("the engine did not crash")
}

func (e *env) launch(rec runtime.RunRecord, step domain.StepID, attempt int) runtime.Command {
	e.t.Helper()
	for _, c := range rec.Commands {
		if c.Kind == runtime.CommandLaunch && c.StepKey == step && c.Binding.Attempt == attempt {
			return c
		}
	}
	e.t.Fatalf("no launch command for %s attempt %d", step, attempt)
	return runtime.Command{}
}

func notReached() error { return fmt.Errorf("%w: connection refused", runtime.ErrNotReached) }

// Test_unit_kernel_engine_uncertain_launch proves an uncertain launch or stop is
// reconciled by observing the workload by its stable identity and never
// replayed; only an effect that provably did not reach the backend is issued
// again, as a new attempt.
func Test_unit_kernel_engine_uncertain_launch(t *testing.T) {
	t.Run("a launch whose response was lost is adopted by observation, not launched again", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("lost-ack", ctr("a"), ctr("b", "a")), 3)
		e.rt.startHook = func(n int, _ runtime.StartRequest) (bool, error) {
			if n == 1 {
				return true, errors.New("connection reset by peer")
			}
			return true, nil
		}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded, "run: %+v", got.Run)
		e.expect(e.rt.count(e.rt.starts) == 2 && fmt.Sprint(e.rt.startOrder) == "[a b]", "one launch per step: %v", e.rt.starts)
		c := e.launch(got, "a", 1)
		e.expect(c.State == runtime.CommandConfirmed && stepOf(got, "a").Attempt == 1, "adopted, same attempt: %+v", c)
		e.expect(len(got.Unresolved()) == 0, "nothing is left unresolved: %+v", got.Unresolved())
	})

	t.Run("a launch that may or may not have happened and is not found fails the step without a replay", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("not-found", ctr("a"), ctr("b", "a")), 3)
		e.rt.startHook = func(int, runtime.StartRequest) (bool, error) { return false, context.DeadlineExceeded }
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonLaunchUncertain && a.Attempt == 1, "step a: %+v", a)
		e.expect(got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonLaunchUncertain, "run: %+v", got.Run)
		e.expect(e.rt.count(e.rt.starts) == 1, "the uncertain launch was observed, never issued again: %d starts", e.rt.count(e.rt.starts))
		c := e.launch(got, "a", 1)
		e.expect(c.State == runtime.CommandRejected && c.Reason == runtime.ReasonLaunchUncertain, "the command ended: %+v", c)
		e.expect(e.rt.count(e.rt.observes) >= 1, "the workload was observed by its identity")
	})

	t.Run("only a launch that provably did not reach the backend is issued again, as a new attempt", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("retry", ctr("a")), 3)
		e.rt.startHook = func(n int, _ runtime.StartRequest) (bool, error) {
			if n == 1 {
				return false, notReached()
			}
			return true, nil
		}
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && a.Attempt == 2 && a.State == runtime.StepSucceeded, "step a: %+v run %+v", a, got.Run)
		c1, c2 := e.launch(got, "a", 1), e.launch(got, "a", 2)
		e.expect(c1.State == runtime.CommandRejected && c1.Reason == runtime.ReasonLaunchNotReached && c2.State == runtime.CommandConfirmed, "commands: %+v %+v", c1, c2)
		e.expect(c1.Target != c2.Target && c1.SpecDigest == c2.SpecDigest, "each attempt has its own identity and the same specification")
		e.expect(e.rt.count(e.rt.starts) == 2, "two launches, the first rejected: %v", e.rt.starts)
		e.expect(e.rt.count(e.rt.observes) == 1, "a rejected launch is not observed for adoption")

		// The retry is bounded; past it the step fails.
		e = newEnv(t)
		e.admit("run1", plan("exhaust", ctr("a")), 3)
		e.rt.startHook = func(int, runtime.StartRequest) (bool, error) { return false, notReached() }
		e.drive(e.engine("w1"), "run1")
		got = e.load("run1")
		a = stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonLaunchNotReached && a.Attempt == 3 && e.rt.count(e.rt.starts) == 3, "step a: %+v starts %d", a, e.rt.count(e.rt.starts))

		e = newEnv(t)
		e.admit("run1", plan("noretry", ctr("a")), 3)
		e.rt.startHook = func(int, runtime.StartRequest) (bool, error) { return false, notReached() }
		e.drive(e.engine("w1", func(c *runner.EngineConfig) { c.LaunchRetries = -1 }), "run1")
		e.expect(e.rt.count(e.rt.starts) == 1 && stepOf(e.load("run1"), "a").Reason == runtime.ReasonLaunchNotReached, "retries disabled: %d starts", e.rt.count(e.rt.starts))
	})

	t.Run("a crash after the intent and before the launch is reconciled, never replayed", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("crash-before", ctr("a"), ctr("b", "a")), 3)
		e.crash(e.engine("w1", func(c *runner.EngineConfig) { c.Runtime = &crashRuntime{fakeRuntime: e.rt, beforeStart: true} }), "run1")
		mid := e.load("run1")
		c := e.launch(mid, "a", 1)
		e.expect(c.State == runtime.CommandIntent && len(mid.Unresolved()) == 1 && stepOf(mid, "a").State == runtime.StepRunning, "intent without outcome: %+v", c)

		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonLaunchUncertain && e.rt.count(e.rt.starts) == 0,
			"the second engine did not launch what it cannot tell was launched: %+v starts %d", a, e.rt.count(e.rt.starts))
		e.expect(got.Claim.Fence == 2 && got.Run.Outcome == runtime.OutcomeFailed, "claim %+v run %+v", got.Claim, got.Run)
	})

	t.Run("a crash after the launch is adopted by the next claim", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("crash-after", ctr("a"), ctr("b", "a")), 3)
		e.rt.scripts["a"] = script{polls: 3}
		e.crash(e.engine("w1", func(c *runner.EngineConfig) { c.Runtime = &crashRuntime{fakeRuntime: e.rt, afterStart: true} }), "run1")
		mid := e.load("run1")
		e.expect(e.launch(mid, "a", 1).State == runtime.CommandIntent && e.rt.count(e.rt.starts) == 1, "the workload exists, the record does not know")

		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && e.launch(got, "a", 1).State == runtime.CommandConfirmed, "adopted: %+v", got.Run)
		e.expect(e.rt.count(e.rt.starts) == 2 && fmt.Sprint(e.rt.startOrder) == "[a b]", "step a was launched once, by the first engine: %v", e.rt.starts)
		o, ok := e.launchObservation(got, "a")
		e.expect(ok && o.Binding.Fence == 2, "the retained observation carries the adopting fence: %+v", o.Binding)
	})

	t.Run("an identity that exists with another specification is not adopted", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("mismatch", ctr("a")), 3)
		e.rt.onStart = func(req runtime.StartRequest) {
			other := req.Binding
			other.SpecDigest = "sha256:other"
			e.rt.workloads[req.Binding.WorkloadID] = &fakeWorkload{b: other, phase: runtime.PhaseRunning}
		}
		e.drive(e.engine("w1"), "run1")
		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonLaunchUncertain && e.rt.count(e.rt.starts) == 1, "step a: %+v", a)
	})

	t.Run("an observation that tells nothing leaves the launch for a later claim", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("blind", ctr("a")), 3)
		e.rt.startHook = func(int, runtime.StartRequest) (bool, error) { return true, errors.New("connection reset") }
		e.rt.observeHook = func(n int, _ runtime.RuntimeBinding) error {
			if n <= 5 {
				return errors.New("backend unavailable")
			}
			return nil
		}
		rep := e.drive(e.engine("w1"), "run1")
		mid := e.load("run1")
		e.expect(rep.Status == runner.DriveRetry && e.launch(mid, "a", 1).State == runtime.CommandUncertain && stepOf(mid, "a").State == runtime.StepRunning,
			"the engine stopped with the launch uncertain: %s %+v", rep.Status, e.launch(mid, "a", 1))
		e.expect(e.rt.count(e.rt.observes) == 5 && e.rt.count(e.rt.starts) == 1, "five inconclusive observations, no second launch")

		e.expire()
		e.drive(e.engine("w2"), "run1")
		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && e.rt.count(e.rt.starts) == 1, "the next claim adopted it: %+v", got.Run)
	})

	t.Run("a stop whose response was lost is reconciled by observation and not repeated", func(t *testing.T) {
		e := newEnv(t)
		slow := ctr("a")
		slow.Workload.Timeout = 3 * poll
		e.admit("run1", plan("stop-lost", slow), 3)
		e.rt.scripts["a"] = script{polls: 1000}
		e.rt.stopHook = func(int, runtime.RuntimeBinding) (bool, error) { return true, errors.New("connection reset") }
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepFailed && a.Reason == runtime.ReasonStepTimeout, "step a: %+v", a)
		e.expect(e.rt.count(e.rt.stops) == 1, "the stop took effect once and was confirmed by observation: %d", e.rt.count(e.rt.stops))
		var stop runtime.Command
		for _, c := range got.Commands {
			if c.Kind == runtime.CommandStop {
				stop = c
			}
		}
		e.expect(stop.State == runtime.CommandConfirmed && stop.Result.Code == runtime.ResultCancelled, "the stop is confirmed with the result it saw: %+v", stop)
	})
}

func (e *env) launchObservation(rec runtime.RunRecord, step domain.StepID) (runtime.WorkloadObservation, bool) {
	for _, o := range rec.Workloads {
		if o.Binding.StepKey == step {
			return o, true
		}
	}
	return runtime.WorkloadObservation{}, false
}

// Test_unit_kernel_engine_cancellation proves cancellation blocks new execution
// claims but still permits stop and reconciliation, takes effect at a poll, and
// reconciles an uncertain launch by stopping the workload its identity names.
func Test_unit_kernel_engine_cancellation(t *testing.T) {
	t.Run("a cancellation seen at a poll stops the workload and ends the run cancelled", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("cancel-mid", ctr("a"), ctr("b", "a")), 3)
		e.rt.scripts["a"] = script{polls: 100}
		sleeps := 0
		e.onSleep = func() error {
			if sleeps++; sleeps == 2 {
				_, err := e.state.RequestCancel(context.Background(), scope, "run1")
				return err
			}
			return nil
		}
		rep := e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Outcome == runtime.OutcomeCancelled && got.Run.CancelRequested && rep.Run.Outcome == runtime.OutcomeCancelled, "run: %+v", got.Run)
		a := stepOf(got, "a")
		e.expect(a.State == runtime.StepCancelled && a.Reason == runtime.ReasonCancelled && a.Result.Code == runtime.ResultCancelled, "step a: %+v", a)
		e.expect(stepOf(got, "b").State == runtime.StepCancelled && fmt.Sprint(e.rt.startOrder) == "[a]", "b never started: %v", e.rt.startOrder)
		e.expect(e.rt.count(e.rt.stops) == 1, "the workload was stopped once")
	})

	t.Run("a cancelled run refuses execution claims and ends under a reconciliation claim", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("cancel-early", ctr("a"), ctr("b", "a")), 3)
		if _, err := e.state.RequestCancel(context.Background(), scope, "run1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		rep := e.drive(e.engine("w1"), "run1")
		e.expect(rep.Status == runner.DriveSkipped && e.rt.count(e.rt.starts) == 0 && len(e.res.ensures) == 0, "an execution claim is refused: %s", rep.Status)

		rep, err := e.engine("w1").Drive(context.Background(), e.ref("run1", runtime.ClaimReconcile))
		e.expect(err == nil && rep.Run.Outcome == runtime.OutcomeCancelled, "reconciliation settles it: %v %+v", err, rep.Run)
		got := e.load("run1")
		e.expect(fmt.Sprint(states(got)) == "map[a:cancelled b:cancelled]", "states: %v", states(got))
		e.expect(e.rt.count(e.rt.starts) == 0 && len(e.res.ensures) == 0, "nothing was started or prepared for a cancelled run")
	})

	t.Run("a cancellation after an uncertain launch stops the workload the identity names", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("cancel-uncertain", ctr("a"), ctr("b", "a")), 3)
		e.rt.scripts["a"] = script{polls: 100}
		e.crash(e.engine("w1", func(c *runner.EngineConfig) { c.Runtime = &crashRuntime{fakeRuntime: e.rt, afterStart: true} }), "run1")
		if _, err := e.state.RequestCancel(context.Background(), scope, "run1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		var stopFence runtime.Fence
		e.rt.onEffect = func(kind string, b runtime.RuntimeBinding) {
			if kind == "stop" {
				stopFence = b.Fence
			}
		}
		e.expire()
		rep, err := e.engine("w2").Drive(context.Background(), e.ref("run1", runtime.ClaimReconcile))
		e.expect(err == nil && rep.Run.Outcome == runtime.OutcomeCancelled, "%v %+v", err, rep.Run)
		got := e.load("run1")
		e.expect(stepOf(got, "a").State == runtime.StepCancelled && e.rt.count(e.rt.starts) == 1 && e.rt.count(e.rt.stops) == 1,
			"stopped by identity, never launched again: starts %d stops %d", e.rt.count(e.rt.starts), e.rt.count(e.rt.stops))
		e.expect(stopFence == 2, "the stop carried the new owner's fence: %d", stopFence)
		e.expect(rep.Run.Cleanup.State != runtime.CleanupNotStarted, "execution ended and cleanup began")
	})
}
