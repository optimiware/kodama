package artifacts_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
)

type noDelete struct{}

func (noDelete) Delete(context.Context, string) error { return nil }

// TestReconcilerConfiguration needs no service: missing wiring and an absent
// policy are construction errors, never defaults.
func TestReconcilerConfiguration(t *testing.T) {
	p := testPolicy()
	cases := map[string]artifacts.ReconcilerConfig{
		"no store":        {Objects: noDelete{}, Policy: p},
		"no deleter":      {Store: nopStore{}, Policy: p},
		"no grace":        {Store: nopStore{}, Objects: noDelete{}, Policy: artifacts.CleanupPolicy{DeletionLease: time.Second, Batch: 1}},
		"no lease":        {Store: nopStore{}, Objects: noDelete{}, Policy: artifacts.CleanupPolicy{StagingGrace: time.Second, Batch: 1}},
		"no batch":        {Store: nopStore{}, Objects: noDelete{}, Policy: artifacts.CleanupPolicy{StagingGrace: time.Second, DeletionLease: time.Second}},
		"negative aband.": {Store: nopStore{}, Objects: noDelete{}, Policy: artifacts.CleanupPolicy{StagingGrace: time.Second, DeletionLease: time.Second, Batch: 1, AbandonAfter: -1}},
	}
	for name, cfg := range cases {
		if _, err := artifacts.NewReconciler(cfg); err == nil {
			t.Errorf("%s: constructed", name)
		}
	}
	zero := p
	zero.AbandonAfter = 0
	if _, err := artifacts.NewReconciler(artifacts.ReconcilerConfig{Store: nopStore{}, Objects: noDelete{}, Policy: zero}); err != nil {
		t.Errorf("a zero AbandonAfter disables abandonment and is valid: %v", err)
	}
	r, _ := artifacts.NewReconciler(artifacts.ReconcilerConfig{Store: nopStore{}, Objects: noDelete{}, Policy: p})
	if err := r.Run(context.Background(), 0, nil); err == nil {
		t.Error("a non-positive interval must be refused")
	}
}

type nopStore struct{ artifacts.CleanupStore }

// TestReconcilerRun: the periodic loop sweeps until its context ends and
// reports errors without stopping.
func TestReconcilerRun(t *testing.T) {
	_, fo, opts := artifactstest.Wire()
	env, caller := setup(t, opts)
	id := env.ReserveAndUpload(caller, attempt, "report", artifacts.RoleOutput, []byte("loop"))
	a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
	env.Clock.Advance(2 * time.Hour)
	rec := newReconciler(t, env, fo)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx, 20*time.Millisecond, nil) }()
	if !artifactstest.Eventually(15*time.Second, func() bool { return !env.ObjectExists(a.StagingKey()) }) {
		t.Fatal("the loop never cleaned the abandoned upload")
	}
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
}
