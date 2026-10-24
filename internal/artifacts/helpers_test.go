package artifacts_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
)

// testPolicy is an explicit, short test policy; the module defines no
// production values.
func testPolicy() artifacts.CleanupPolicy {
	return artifacts.CleanupPolicy{StagingGrace: time.Second, AbandonAfter: time.Minute, DeletionLease: 5 * time.Second, Batch: 50}
}

func newReconciler(t *testing.T, env *artifactstest.Env, deleter artifacts.ObjectDeleter) *artifacts.Reconciler {
	t.Helper()
	return newReconcilerWith(t, env, deleter, testPolicy(), nil)
}

func newReconcilerWith(t *testing.T, env *artifactstest.Env, deleter artifacts.ObjectDeleter, p artifacts.CleanupPolicy, ret artifacts.Retention) *artifacts.Reconciler {
	t.Helper()
	r, err := artifacts.NewReconciler(artifacts.ReconcilerConfig{Store: env.Store, Objects: deleter, Retention: ret, Policy: p, Clock: env.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// sweepUntilQuiet repeats sweeps until one makes no progress, proving the
// sweeps converge.
func sweepUntilQuiet(t *testing.T, r *artifacts.Reconciler, max int) artifacts.SweepReport {
	t.Helper()
	var total artifacts.SweepReport
	for i := 0; i < max; i++ {
		rep, err := r.Sweep(t.Context())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		total.Abandoned += rep.Abandoned
		total.Deleted += rep.Deleted
		total.Removed += rep.Removed
		if rep.Abandoned == 0 && rep.Deleted == 0 && rep.Removed == 0 && rep.Retryable == 0 {
			return total
		}
	}
	t.Fatalf("sweeps did not converge in %d passes", max)
	return total
}
