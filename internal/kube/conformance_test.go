package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// Test_kube_start_stopped_conflict (T-095, kube_start_stopped_conflict): the
// shared runtime contract answers a Start of a stopped or removed workload with
// execution.ErrConflict. The Kubernetes cause stays in the chain, and nothing
// is created.
func Test_kube_start_stopped_conflict(t *testing.T) {
	ctx := context.Background()
	conflict := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("not an execution conflict: %v", err)
		}
		if !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("the cause is not named: %v", err)
		}
	}
	untouched := func(t *testing.T, e *env, jobs, secrets int) {
		t.Helper()
		if e.fake.Calls("create job") != jobs || e.fake.Calls("create secret") != secrets {
			t.Fatalf("a Start of a stopped workload created objects: job %d, secret %d", e.fake.Calls("create job"), e.fake.Calls("create secret"))
		}
	}

	t.Run("stopped workload", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		if _, err := x.Start(ctx, e.intent()); err != nil {
			t.Fatal(err)
		}
		e.started(t)
		// The attempt stays live: the record, not the committed attempt, fences it.
		for i := 0; ; i++ {
			res, err := x.Stop(ctx, e.workload())
			if err != nil {
				t.Fatal(err)
			}
			if res.Stopped {
				break
			}
			if i > 5 {
				t.Fatal("never stopped")
			}
		}
		_, err := e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		untouched(t, e, 1, 1)
		if _, ok := e.job(); ok {
			t.Fatal("a stopped workload was started again")
		}
	})

	t.Run("workload stopped before its create", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.OnRead = func(n int) {
			if n == 2 { // between the dispatch read and the create
				e.attempts.Cancel(e.rec.AttemptID)
			}
		}
		_, err := e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		e.attempts.OnRead = nil
		if res, err := e.runtime(t).Stop(ctx, e.workload()); err != nil || !res.Stopped {
			t.Fatalf("%+v %v", res, err)
		}
		_, err = e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		untouched(t, e, 0, 1)
	})

	t.Run("cancelled attempt", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.Cancel(e.rec.AttemptID)
		_, err := e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		untouched(t, e, 0, 0)
	})

	t.Run("removed workload", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		w, err := x.Start(ctx, e.intent())
		if err != nil {
			t.Fatal(err)
		}
		e.started(t)
		now := e.clock.Now()
		e.attempts.Edit(e.rec.AttemptID, func(r *kube.AttemptRecord) {
			r.Status, r.Live, r.Settled, r.SettledAt = execution.AttemptSucceeded, false, true, &now
		})
		for i := 0; ; i++ {
			err := x.Remove(ctx, w)
			if err == nil {
				break
			}
			if !errors.Is(err, execution.ErrBusy) || i > 5 {
				t.Fatal(err)
			}
		}
		if _, ok := e.job(); ok {
			t.Fatal("the Job survived removal")
		}
		_, err = e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		untouched(t, e, 1, 1)
	})

	t.Run("a deleted workload with a live attempt is never recreated", func(t *testing.T) {
		e := newEnv(t)
		x := e.runtime(t)
		if _, err := x.Start(ctx, e.intent()); err != nil {
			t.Fatal(err)
		}
		e.fake.DropJob(e.ns(), e.rec.JobName)
		_, err := e.runtime(t).Start(ctx, e.intent())
		conflict(t, err)
		untouched(t, e, 1, 1)
	})

	t.Run("other conflicts and outages keep their class", func(t *testing.T) {
		e := newEnv(t)
		in := e.intent()
		in.SpecFingerprint = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
		if _, err := e.runtime(t).Start(ctx, in); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another spec: %v", err)
		}
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "get job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		_, err := e.runtime(t).Start(ctx, e.intent())
		if !errors.Is(err, kube.ErrUnavailable) || errors.Is(err, execution.ErrConflict) {
			t.Fatalf("an outage became a conflict: %v", err)
		}
	})
}

// Test_kube_stop_before_create_not_started (T-095,
// kube_stop_before_create_not_started): Stop reports LaunchNotStarted only with
// affirmative evidence that no create was issued, and a create that may be in
// flight or whose answer was lost stays LaunchUnknown.
func Test_kube_stop_before_create_not_started(t *testing.T) {
	ctx := context.Background()
	lose := func(e *env, after bool) {
		e.fake.Fault = func(op string, n int) *kubetest.Fault {
			if op == "create job" {
				return &kubetest.Fault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: after}
			}
			return nil
		}
	}

	t.Run("no create was ever issued", func(t *testing.T) {
		e := newEnv(t)
		e.attempts.OnRead = func(n int) {
			if n == 2 {
				e.attempts.Cancel(e.rec.AttemptID)
			}
		}
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.attempts.OnRead = nil
		if e.fake.Calls("create job") != 0 {
			t.Fatal("a create was sent")
		}
		for range 2 {
			res, err := e.runtime(t).Stop(ctx, e.workload())
			if err != nil || !res.Stopped || res.Launch != execution.LaunchNotStarted {
				t.Fatalf("%+v %v", res, err)
			}
		}
	})

	t.Run("a create that may still land is unknown", func(t *testing.T) {
		e := newEnv(t)
		lose(e, false)
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.attempts.Cancel(e.rec.AttemptID)
		x := e.runtime(t)
		res, err := x.Stop(ctx, e.workload())
		if err != nil || res.Stopped || res.Launch == execution.LaunchNotStarted {
			t.Fatalf("%+v %v", res, err)
		}
		e.clock.Advance(10 * time.Minute)
		res, err = x.Stop(ctx, e.workload())
		if err != nil || !res.Stopped || res.Launch != execution.LaunchUnknown {
			t.Fatalf("after quiescence: %+v %v", res, err)
		}
	})

	t.Run("a create whose answer was lost is unknown", func(t *testing.T) {
		e := newEnv(t)
		lose(e, true) // applied; the answer never arrived
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Launch == execution.LaunchNotStarted {
			t.Fatalf("manufactured evidence: %+v %v", res, err)
		}
	})

	t.Run("a lost record of an issued create is unknown", func(t *testing.T) {
		e := newEnv(t)
		lose(e, false)
		_, _ = e.runtime(t).Start(ctx, e.intent())
		e.fake.Fault = nil
		e.lostSecret(t)
		e.attempts.Cancel(e.rec.AttemptID)
		res, err := e.runtime(t).Stop(ctx, e.workload())
		if err != nil || res.Launch == execution.LaunchNotStarted {
			t.Fatalf("manufactured evidence: %+v %v", res, err)
		}
	})
}
