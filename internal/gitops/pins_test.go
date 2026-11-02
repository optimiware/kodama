package gitops_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

// Test_unit_gitops_pins: a pin is the run's next retained ref, idempotent per
// commit, numbered per run and surviving pruning and the most aggressive gc; a
// release needs the retention gate and is idempotent (GR-13).
func Test_unit_gitops_pins(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	const id = "r_pins"
	r.vol.Init(tenantA, id)
	base := r.vol.Commit(tenantA, id, "", "base", day(0))
	c1 := r.vol.Commit(tenantA, id, base, "c1", day(1))
	c2 := r.vol.Commit(tenantA, id, c1, "c2", day(2))
	r.vol.SetRef(tenantA, id, "refs/heads/kodama/runs/run_1/work", c2)
	gate := &gitopstest.Gate{}
	pins, err := gitops.NewPins(gitops.PinsConfig{Worker: r.worker, Gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	sc := scope(tenantA)

	ref1, err := pins.Pin(ctx, sc, id, "run_1", c1)
	if err != nil || ref1 != "refs/kodama/runs/run_1/1" {
		t.Fatalf("first pin: %q %v", ref1, err)
	}
	if again, err := pins.Pin(ctx, sc, id, "run_1", c1); err != nil || again != ref1 {
		t.Fatalf("a pin is idempotent per (run, commit): %q %v", again, err)
	}
	ref2, err := pins.Pin(ctx, sc, id, "run_1", c2)
	if err != nil || ref2 != "refs/kodama/runs/run_1/2" {
		t.Fatalf("second commit: %q %v", ref2, err)
	}
	if other, err := pins.Pin(ctx, sc, id, "run_2", c1); err != nil || other != "refs/kodama/runs/run_2/1" {
		t.Fatalf("another run numbers from 1: %q %v", other, err)
	}
	if _, err := pins.Pin(ctx, sc, id, "run_1", "nothex"); err == nil {
		t.Fatal("a pin of a non-commit")
	}
	if err := r.vol.Refs(tenantA, id)[ref1]; err == "" {
		t.Fatal("the retained ref does not exist")
	}

	// A pinned commit survives losing every branch and the most aggressive gc.
	r.vol.DelRef(tenantA, id, "refs/heads/kodama/runs/run_1/work")
	r.vol.GC(tenantA, id)
	if !r.vol.HasObject(tenantA, id, c2) || !r.vol.HasObject(tenantA, id, c1) {
		t.Fatal("a pinned commit was lost")
	}

	// A race for one number: the loser takes the next.
	r.worker.Hook = func(op, _ string) {
		if op == "setRetainedRef" {
			r.worker.Hook = nil
			r.vol.SetRef(tenantA, id, "refs/kodama/runs/run_3/1", c2) // another pinner took 1 meanwhile
		}
	}
	if ref, err := pins.Pin(ctx, sc, id, "run_3", c1); err != nil || ref != "refs/kodama/runs/run_3/2" {
		t.Fatalf("racing pin: %q %v", ref, err)
	}

	// Release needs retention.
	if err := pins.Release(ctx, sc, id, "run_1"); !errors.Is(err, gitops.ErrRetention) {
		t.Fatalf("release before retention: %v", err)
	}
	noGate, _ := gitops.NewPins(gitops.PinsConfig{Worker: r.worker})
	if err := noGate.Release(ctx, sc, id, "run_1"); !errors.Is(err, gitops.ErrRetention) {
		t.Fatalf("release without a gate: %v", err)
	}
	if r.vol.Ref(tenantA, id, ref1) == "" {
		t.Fatal("a refused release deleted a pin")
	}
	gate.Allow = true
	if err := pins.Release(ctx, sc, id, "run_1"); err != nil {
		t.Fatal(err)
	}
	if r.vol.Ref(tenantA, id, ref1) != "" || r.vol.Ref(tenantA, id, ref2) != "" {
		t.Fatal("the run's pins were not released")
	}
	if r.vol.Ref(tenantA, id, "refs/kodama/runs/run_2/1") == "" {
		t.Fatal("another run's pin was released")
	}
	if err := pins.Release(ctx, sc, id, "run_1"); err != nil {
		t.Fatalf("release is idempotent: %v", err)
	}
	// Released commits are ordinary garbage again once no other run holds them.
	for _, run := range []domain.RunID{"run_2", "run_3"} {
		if err := pins.Release(ctx, sc, id, run); err != nil {
			t.Fatal(err)
		}
	}
	r.vol.GC(tenantA, id)
	if r.vol.HasObject(tenantA, id, c2) {
		t.Fatal("a released, unreferenced commit was kept forever")
	}
}
