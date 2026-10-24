package kube_test

import (
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
)

func ownedWorkload(t *testing.T, f fixture) (*kube.Workload, kube.Verification) {
	t.Helper()
	v, err := kube.VerifyJob(f.exp, f.created(t))
	if err != nil {
		t.Fatal(err)
	}
	w := kube.NewWorkload(f.exp.Identity)
	for _, step := range []func() error{w.Credentialed, w.Registered, func() error { return w.IssueCreate(time.Now()) }, func() error { return w.Adopt(v) }} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return w, v
}

// Test_workload_transitions: accepted/rejected adoption, creation, deletion and
// uncertainty transitions independently of infrastructure.
func Test_workload_transitions(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	t.Run("happy path", func(t *testing.T) {
		f := newFixture(t)
		w, v := ownedWorkload(t, f)
		if w.Phase != kube.PhaseOwned || w.JobUID != v.UID() {
			t.Fatal("not owned")
		}
		if err := w.BeginDelete(v); err != nil || w.Phase != kube.PhaseDeleting {
			t.Fatalf("delete: %v", err)
		}
		if err := w.ObserveGone(true); err != nil || w.Phase != kube.PhaseGone {
			t.Fatalf("gone: %v", err)
		}
	})

	t.Run("order is enforced", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		if err := w.Registered(); !errors.Is(err, kube.ErrInvalidTransition) {
			t.Fatal("registered before credentialed")
		}
		if err := w.IssueCreate(now); !errors.Is(err, kube.ErrInvalidTransition) {
			t.Fatal("create before registration")
		}
		if w.CanCreate() {
			t.Fatal("a planned workload may not create")
		}
		_ = w.Credentialed()
		_ = w.Registered()
		if !w.CanCreate() {
			t.Fatal("a registered workload may create")
		}
	})

	t.Run("unverified objects cannot become owned", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		_ = w.Credentialed()
		_ = w.Registered()
		if err := w.Adopt(kube.Verification{}); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("adopted without verification: %v", err)
		}
		if err := w.BeginDelete(kube.Verification{}); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("deleted without verification: %v", err)
		}
		// A verification for another attempt is not usable either.
		other := newFixture(t)
		other.exp.Identity.AttemptID = "att_other"
		v, err := kube.VerifyJob(f.exp, f.created(t))
		if err != nil {
			t.Fatal(err)
		}
		w2 := kube.NewWorkload(other.exp.Identity)
		_ = w2.Credentialed()
		_ = w2.Registered()
		if err := w2.Adopt(v); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("adopted another attempt's verification: %v", err)
		}
	})

	t.Run("a confirmed UID never changes", func(t *testing.T) {
		f := newFixture(t)
		w, _ := ownedWorkload(t, f)
		replacement := newFixture(t)
		v2, err := kube.VerifyJob(replacement.exp, replacement.created(t))
		if err != nil {
			t.Fatal(err)
		}
		if v2.UID() == w.JobUID {
			t.Fatal("fixture UIDs collide")
		}
		// The replacement carries the same identity (same attempt) but another UID.
		w2 := *w
		if err := w2.Adopt(v2); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("UID substitution on adopt: %v", err)
		}
		if err := w2.BeginDelete(v2); !errors.Is(err, kube.ErrForeignWorkload) {
			t.Fatalf("UID substitution on delete: %v", err)
		}
	})

	t.Run("an issued create blocks another until refused", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		_ = w.Credentialed()
		_ = w.Registered()
		if err := w.IssueCreate(now); err != nil {
			t.Fatal(err)
		}
		if w.CanCreate() {
			t.Fatal("a second create while one is issued")
		}
		if err := w.IssueCreate(now); !errors.Is(err, kube.ErrInvalidTransition) {
			t.Fatal("double issue")
		}
		if err := w.CreateUnknown(); err != nil || !w.Uncertain {
			t.Fatal("unknown outcome not recorded")
		}
		if w.NeverCreated() {
			t.Fatal("an issued create is not 'never created'")
		}
		if err := w.ObserveGone(false); !errors.Is(err, kube.ErrCreateUncertain) {
			t.Fatalf("gone while a create may land: %v", err)
		}
		if err := w.ObserveGone(true); err != nil {
			t.Fatalf("gone after quiescence: %v", err)
		}
		// After a definite refusal, creating again is allowed.
		w2 := kube.NewWorkload(f.exp.Identity)
		_ = w2.Credentialed()
		_ = w2.Registered()
		_ = w2.IssueCreate(now)
		if err := w2.CreateRefused(); err != nil || !w2.CanCreate() {
			t.Fatalf("refusal: %v", err)
		}
	})

	t.Run("deleting and gone never return to creating", func(t *testing.T) {
		f := newFixture(t)
		w, v := ownedWorkload(t, f)
		_ = w.BeginDelete(v)
		for name, fn := range map[string]func() error{
			"credentialed": w.Credentialed, "registered": w.Registered, "issue": func() error { return w.IssueCreate(now) },
			"refused": w.CreateRefused, "unknown": w.CreateUnknown, "adopt": func() error { return w.Adopt(v) },
		} {
			if err := fn(); err == nil {
				t.Fatalf("%s accepted while deleting", name)
			}
		}
		if w.CanCreate() {
			t.Fatal("a deleting workload may create")
		}
		_ = w.ObserveGone(true)
		if w.CanCreate() {
			t.Fatal("a gone workload may create")
		}
		if err := w.IssueCreate(now); err == nil {
			t.Fatal("create after gone")
		}
		if err := w.Adopt(v); err == nil {
			t.Fatal("adopt after gone: a late create is deleted, never adopted")
		}
		// But a late-created verified Job may still be deleted from gone.
		if err := w.BeginDelete(v); err != nil {
			t.Fatalf("late create deletion: %v", err)
		}
	})

	t.Run("never created is affirmative", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		if !w.NeverCreated() {
			t.Fatal("planned")
		}
		_ = w.Credentialed()
		_ = w.Registered()
		if !w.NeverCreated() {
			t.Fatal("registered without an issued create")
		}
		_ = w.IssueCreate(now)
		if w.NeverCreated() {
			t.Fatal("issued")
		}
	})

	t.Run("launch evidence only becomes more informative", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		if w.NoteLaunch(execution.LaunchUnknown) {
			t.Fatal("unknown is not evidence")
		}
		if !w.NoteLaunch(execution.LaunchStarted) || w.Launch != execution.LaunchStarted {
			t.Fatal("started")
		}
		if w.NoteLaunch(execution.LaunchNotStarted) || w.Launch != execution.LaunchStarted {
			t.Fatal("started yielded to not_started")
		}
		if w.NoteLaunch(execution.LaunchUnknown) || w.Launch != execution.LaunchStarted {
			t.Fatal("started yielded to unknown")
		}
	})

	t.Run("started upgrades persisted not_started and survives restoration", func(t *testing.T) {
		f := newFixture(t)
		w := kube.NewWorkload(f.exp.Identity)
		if !w.NoteLaunch(execution.LaunchNotStarted) || w.Launch != execution.LaunchNotStarted {
			t.Fatal("not_started")
		}
		if w.NoteLaunch(execution.LaunchNotStarted) || w.NoteLaunch(execution.LaunchUnknown) {
			t.Fatal("repeat or unknown changed the record")
		}
		if !w.NoteLaunch(execution.LaunchStarted) || w.Launch != execution.LaunchStarted {
			t.Fatal("started did not upgrade not_started")
		}
		if w.NoteLaunch(execution.LaunchNotStarted) || w.Launch != execution.LaunchStarted {
			t.Fatal("started was downgraded")
		}
	})
}
