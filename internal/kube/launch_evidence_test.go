package kube_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_runtime_never_started_capability (T-102, O-25): Kubernetes does not
// advertise the affirmative never-started proof, yet keeps every container
// guarantee, so it stays eligible for the production policy; the shared
// contract's fresh-stop assertion accepts its evidence.
func Test_runtime_never_started_capability(t *testing.T) {
	caps := (&kube.Runtime{}).Capabilities()
	if caps.ProvesNeverStarted {
		t.Fatal("Kubernetes cannot prove a fresh workload never started from absent records")
	}
	if caps.Backend != kube.Backend || caps.Guarantees != execution.AllGuarantees {
		t.Fatalf("the missing proof changed the guarantees: %+v", caps)
	}
	if missing := caps.Guarantees.Missing(execution.DefaultRequirements(pipeline.Step{Kind: pipeline.KindContainer})); missing != 0 {
		t.Fatalf("Kubernetes is ineligible for the default policy: %v", missing)
	}
	// Both outcomes the contract allows a non-advertising backend.
	for _, l := range []execution.LaunchEvidence{execution.LaunchUnknown, execution.LaunchNotStarted} {
		if err := runtimetest.CheckFreshStop(caps, execution.StopEvidence{Stopped: true, Launch: l}); err != nil {
			t.Fatalf("%s: %v", l, err)
		}
	}
	if err := runtimetest.CheckFreshStop(caps, execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted}); err == nil {
		t.Fatal("a never-started workload may not report a start")
	}
}

// Test_runtime_stop_unknown_preserved (T-102): missing records, a lost
// admission Secret and an uncertain create never become affirmative
// never-started evidence, and an unknown stop still reports Stopped only once
// no launch can arrive.
func Test_runtime_stop_unknown_preserved(t *testing.T) {
	ctx := context.Background()
	lose := func(e *env, after bool) {
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: after}
			}
			return nil
		}
	}

	t.Run("no Secret, Job or Pod and no create record is unknown, not never-started", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchUnknown {
			t.Fatalf("%+v %v", res, err)
		}
		if err := runtimetest.CheckFreshStop(e.runtime(t).Capabilities(), res); err != nil {
			t.Fatalf("the shared contract rejected unknown evidence: %v", err)
		}
		if e.fake.Calls("create job") != 0 || e.fake.Calls("delete job") != 0 {
			t.Fatal("a stop of an absent workload touched the cluster")
		}
	})
	t.Run("an issued create whose answer was lost and a lost Secret", func(t *testing.T) {
		e := newEnv(t)
		lose(e, true)
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.lostSecret(t)
		e.attempts.Cancel(e.rec.AttemptID)
		for range 3 {
			res, err := e.runtime(t).Stop(ctx, e.workload())
			if err != nil || res.Launch == execution.LaunchNotStarted {
				t.Fatalf("manufactured evidence: %+v %v", res, err)
			}
		}
	})
	t.Run("an uncertain create is not stopped until it can no longer land", func(t *testing.T) {
		e := newEnv(t)
		lose(e, false)
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Stopped || res.Launch == execution.LaunchNotStarted {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

// Test_runtime_stop_no_relaunch (T-102): without the proof capability a stop
// before the start still fences every later launch.
func Test_runtime_stop_no_relaunch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.attempts.Cancel(e.rec.AttemptID)
	if res, err := e.runtime(t).Stop(ctx, e.workload()); err != nil || !res.Stopped {
		t.Fatalf("%+v %v", res, err)
	}
	for range 2 {
		if _, err := e.runtime(t).Start(ctx, e.intent()); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a stopped workload was started: %v", err)
		}
	}
	if e.fake.Calls("create job") != 0 || e.fake.Calls("create secret") != 0 {
		t.Fatal("a stopped workload was created")
	}
	if _, ok := e.job(); ok {
		t.Fatal("a Job exists for a stopped workload")
	}
}
