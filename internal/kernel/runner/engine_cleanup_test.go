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

func withService(p runtime.Plan) runtime.Plan {
	p.Services = []runtime.ServiceSpec{{Name: "db", Lifetime: runtime.LifetimePipeline, Image: "postgres:16"}}
	return p
}

func (e *env) cleanupDrive(eng *runner.Engine, id string) runner.Report {
	e.t.Helper()
	rep, err := eng.Drive(context.Background(), e.ref(domain.RunID(id), runtime.ClaimCleanup))
	if err != nil {
		e.t.Fatalf("cleanup drive: %v", err)
	}
	return rep
}

// Test_unit_kernel_engine_cleanup proves execution outcome and cleanup are
// separate: a run is terminal only once its owned workloads and resources are
// confirmed removed, or once bounded attempts are exhausted with what remains
// recorded by identity; a later cleanup claim can still finish the job.
func Test_unit_kernel_engine_cleanup(t *testing.T) {
	t.Run("a run is terminal only after every owned resource is confirmed removed", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", withService(plan("clean", ctr("a"), ctr("b", "a"))), 3)
		e.res.onRemove = func(h runtime.ResourceHandle) {
			rec := e.load("run1")
			e.expect(rec.Run.Outcome == runtime.OutcomeSucceeded && rec.Run.Status() == runtime.StatusCleaning && !rec.Run.Terminal(),
				"while resources remain the run is cleaning, not terminal: %v", rec.Run.Status())
		}
		rep := e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Terminal() && got.Run.Status() == runtime.StatusSucceeded, "run: %+v", got.Run)
		e.expect(got.Run.Cleanup.State == runtime.CleanupComplete && got.Run.Cleanup.Attempts == 0 && len(got.Run.Cleanup.Retained) == 0, "cleanup: %+v", got.Run.Cleanup)
		e.expect(len(e.res.state) == 0 && e.rt.liveCount() == 0, "nothing is left behind: %v", e.res.state)
		e.expect(fmt.Sprint(e.res.stops) == "[db]" && fmt.Sprint(e.res.removes) == "[db workspace]", "services first, the workspace last: stops %v removes %v", e.res.stops, e.res.removes)
		for _, o := range got.Resources {
			e.expect(o.State == runtime.ResourceRemoved, "the retained observation confirms removal: %+v", o)
		}
		for _, c := range got.Commands {
			e.expect(c.State == runtime.CommandConfirmed, "command %s: %s", c.ID, c.State)
		}
		e.expect(countKind(e.outbox("run1"), runtime.EventRunStateChanged) >= 2, "the end of execution and the end of cleanup are separate events")

		// A terminal run admits nothing more.
		rep = e.cleanupDrive(e.engine("w2"), "run1")
		e.expect(rep.Status == runner.DriveSkipped, "a completed run is not claimed again: %s", rep.Status)
	})

	t.Run("bounded attempts end with the outcome intact and what remains recorded by identity", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", withService(plan("exhaust", ctr("a"))), 3)
		e.res.removeHook = func(int, runtime.ResourceHandle) (bool, error) { return false, errors.New("storage unavailable") }
		var seen []runtime.Cleanup
		e.onSleep = func() error {
			rec := e.load("run1")
			e.expect(rec.Run.Outcome == runtime.OutcomeSucceeded && rec.Run.Status() == runtime.StatusCleaning, "pending, visibly: %v", rec.Run.Status())
			seen = append(seen, rec.Run.Cleanup)
			return nil
		}
		start := e.clock.Now()
		rep := e.drive(e.engine("w1"), "run1")

		e.expect(len(seen) == 2 && seen[0].Attempts == 1 && seen[1].Attempts == 2 && seen[1].State == runtime.CleanupPending, "pending between attempts: %+v", seen)
		e.expect(len(seen[0].Retained) == 2 && seen[0].Reason == runtime.ReasonCleanupFailed, "with the retained identities and a reason: %+v", seen[0])
		e.expect(e.clock.Now().Sub(start) == 3*time.Second, "backoff grows between attempts: %v", e.clock.Now().Sub(start))
		got := e.load("run1")
		c := got.Run.Cleanup
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Terminal() && got.Run.Status() == runtime.StatusSucceeded, "terminal, with the execution outcome unchanged: %+v", got.Run)
		e.expect(c.State == runtime.CleanupExhausted && c.Attempts == 3 && c.Reason == runtime.ReasonCleanupFailed && len(c.Retained) == 2 && len(c.RetainedWorkloads) == 0,
			"cleanup: %+v", c)
		names := map[string]bool{}
		for _, h := range c.Retained {
			names[h.Name] = true
			e.expect(h.RunID == "run1" && h.Lifetime == runtime.LifetimePipeline && h.Identity != "", "retained handle: %+v", h)
		}
		e.expect(names["workspace"] && names["db"], "the workspace and the service are retained: %v", names)
		e.expect(e.rt.liveCount() == 0, "the workload was removed")

		// Late cleanup claims: execution and reconciliation stay closed.
		e.onSleep = nil
		e.expire()
		for _, p := range []runtime.ClaimPurpose{runtime.ClaimExecute, runtime.ClaimReconcile} {
			r, err := e.engine("w2").Drive(context.Background(), e.ref("run1", p))
			e.expect(err == nil && r.Status == runner.DriveSkipped, "a %s claim on a terminal run: %v %v", p, r.Status, err)
		}
		e.expect(len(e.res.removes) == 2, "an uncertain removal is observed, not issued again, by any attempt or claim: %v", e.res.removes)

		// The backend's own reconciliation removes one resource; the late claim
		// confirms it by observation and the record narrows.
		delete(e.res.state, e.res.names["db"])
		rep = e.cleanupDrive(e.engine("w2"), "run1")
		got = e.load("run1")
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Cleanup.State == runtime.CleanupExhausted && len(got.Run.Cleanup.Retained) == 1 && got.Run.Cleanup.Retained[0].Name == "workspace",
			"a late claim narrows what is retained: %+v", got.Run.Cleanup)
		e.expect(got.Run.Outcome == runtime.OutcomeSucceeded && got.Claim.Fence == 2, "execution was not reopened: %+v", got.Run)

		delete(e.res.state, e.res.names["workspace"])
		e.expire()
		rep = e.cleanupDrive(e.engine("w3"), "run1")
		got = e.load("run1")
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Cleanup.State == runtime.CleanupComplete && len(got.Run.Cleanup.Retained) == 0 && got.Run.Cleanup.Reason == runtime.ReasonNone,
			"the late reconciliation completes cleanup: %+v", got.Run.Cleanup)
		e.expect(len(e.res.state) == 0 && got.Run.Outcome == runtime.OutcomeSucceeded && len(e.res.removes) == 2, "nothing is left; the outcome stands; nothing was issued again")
		e.expire()
		e.expect(e.cleanupDrive(e.engine("w4"), "run1").Status == runner.DriveSkipped, "a cleanup claim once cleanup is complete is refused")
	})

	t.Run("an uncertain removal is reconciled by observation and not repeated", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", plan("uncertain-remove", ctr("a")), 3)
		e.rt.removeHook = func(int, runtime.RuntimeBinding) (bool, error) { return true, errors.New("connection reset") }
		e.res.removeHook = func(int, runtime.ResourceHandle) (bool, error) { return true, errors.New("connection reset") }
		e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		e.expect(got.Run.Cleanup.State == runtime.CleanupComplete && got.Run.Terminal(), "cleanup: %+v", got.Run.Cleanup)
		e.expect(e.rt.count(e.rt.removes) == 1 && len(e.res.removes) == 1, "each removal took effect once and was confirmed by observation: %d %v", e.rt.count(e.rt.removes), e.res.removes)
		e.expect(len(e.res.state) == 0 && e.rt.liveCount() == 0, "nothing is left")
	})

	t.Run("a workload whose stop cannot be confirmed is retained by its binding and never stopped again", func(t *testing.T) {
		e := newEnv(t)
		slow := ctr("a")
		slow.Workload.Timeout = 2 * poll
		e.admit("run1", plan("retained-workload", slow), 2)
		e.rt.scripts["a"] = script{polls: 1000}
		silent := func(int, runtime.RuntimeBinding) (bool, error) { return false, errors.New("timeout") }
		e.rt.stopHook, e.rt.removeHook = silent, silent
		rep := e.drive(e.engine("w1"), "run1")

		got := e.load("run1")
		c := got.Run.Cleanup
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Outcome == runtime.OutcomeFailed && got.Run.Reason == runtime.ReasonStepTimeout, "run: %+v", got.Run)
		e.expect(c.State == runtime.CleanupExhausted && len(c.RetainedWorkloads) == 1 && len(c.Retained) == 0, "the workload is retained: %+v", c)
		b := c.RetainedWorkloads[0]
		e.expect(b.StepKey == "a" && b.Attempt == 1 && b.Backend == "fake" && b.WorkloadID != "" && b.Fence == 1, "by its scoped binding: %+v", b)
		e.expect(e.rt.liveCount() == 1 && len(e.res.state) == 0, "the workload still exists, the resources are gone")
		stops, removes := e.rt.count(e.rt.stops), e.rt.count(e.rt.removes)
		e.expect(stops == 1 && removes == 1, "each effect was issued once: %d %d", stops, removes)

		// A later claim only observes, under its own fence, and issues nothing.
		var fences []runtime.Fence
		e.rt.observeHook = func(_ int, b runtime.RuntimeBinding) error { fences = append(fences, b.Fence); return nil }
		e.rt.stopHook, e.rt.removeHook = nil, nil
		e.expire()
		rep = e.cleanupDrive(e.engine("w2"), "run1")
		got = e.load("run1")
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Cleanup.State == runtime.CleanupExhausted && len(got.Run.Cleanup.RetainedWorkloads) == 1, "still retained: %+v", got.Run.Cleanup)
		e.expect(e.rt.count(e.rt.stops) == stops && e.rt.count(e.rt.removes) == removes && len(fences) > 0 && fences[0] == 2, "observed under the new fence, nothing issued: %v", fences)

		// The backend's own reconciliation ends the workload; the late claim confirms it.
		e.rt.workloads = map[string]*fakeWorkload{}
		e.expire()
		rep = e.cleanupDrive(e.engine("w3"), "run1")
		got = e.load("run1")
		e.expect(got.Run.Cleanup.State == runtime.CleanupComplete && got.Run.Reason == runtime.ReasonStepTimeout && e.rt.count(e.rt.stops) == stops && e.rt.count(e.rt.removes) == removes,
			"confirmed by observation: %+v", got.Run.Cleanup)
	})

	t.Run("a cancelled run is cleaned up and a run cancelled before it started has nothing to clean", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", withService(plan("cancel-clean", ctr("a"), ctr("b", "a"))), 3)
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
		e.expect(rep.Status == runner.DriveTerminal && got.Run.Status() == runtime.StatusCancelled && got.Run.Cleanup.State == runtime.CleanupComplete, "run: %+v", got.Run)
		e.expect(e.rt.liveCount() == 0 && len(e.res.state) == 0, "the stopped workload, the service and the workspace are gone")

		e2 := newEnv(t)
		e2.admit("run1", plan("never-started", ctr("a")), 3)
		if _, err := e2.state.RequestCancel(context.Background(), scope, "run1"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		r, err := e2.engine("w1").Drive(context.Background(), e2.ref("run1", runtime.ClaimReconcile))
		e2.expect(err == nil && r.Status == runner.DriveTerminal && r.Run.Cleanup.State == runtime.CleanupComplete && r.Run.Status() == runtime.StatusCancelled, "run: %v %+v", err, r.Run)
	})

	t.Run("a crash between the removal intent and the call retains the resource and never repeats the call", func(t *testing.T) {
		e := newEnv(t)
		e.admit("run1", withService(plan("takeover-clean", ctr("a"))), 2)
		e.res.onRemove = func(h runtime.ResourceHandle) {
			if h.Name == "db" {
				panic(errCrash)
			}
		}
		e.crash(e.engine("w1"), "run1")
		mid := e.load("run1")
		e.expect(mid.Run.Settled() && !mid.Run.Terminal() && mid.Run.Cleanup.State == runtime.CleanupPending, "cleaning: %+v", mid.Run.Cleanup)
		e.expect(e.rt.liveCount() == 0 && e.rt.count(e.rt.removes) == 1, "the workload was already removed")
		var open []string
		for _, c := range mid.Unresolved() {
			open = append(open, string(c.Kind))
		}
		e.expect(fmt.Sprint(open) == "[remove_resource]", "one removal is left with its intent: %v", open)

		e.res.onRemove = nil
		e.expire()
		rep := e.cleanupDrive(e.engine("w2"), "run1")
		got := e.load("run1")
		c := got.Run.Cleanup
		e.expect(rep.Status == runner.DriveTerminal && c.State == runtime.CleanupExhausted && len(c.Retained) == 1 && c.Retained[0].Name == "db", "the unconfirmed resource is retained: %+v", c)
		e.expect(fmt.Sprint(e.res.removes) == "[workspace]", "its removal was observed, not issued again; the workspace was removed: %v", e.res.removes)
		e.expect(e.rt.count(e.rt.removes) == 1, "the confirmed workload removal was not repeated")
		delete(e.res.state, e.res.names["db"])
		e.expire()
		e.cleanupDrive(e.engine("w3"), "run1")
		e.expect(e.load("run1").Run.Cleanup.State == runtime.CleanupComplete, "the backend's reconciliation lets a late claim complete it")
	})

	t.Run("a run whose secrets cannot be revoked is not terminal", cleanupRevokesFromTheRecord)

	t.Run("process backend", processCleanup)
}
