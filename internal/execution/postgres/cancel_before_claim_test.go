package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

var engineer = execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t020"}

// Test_cancel_before_claim: PX-08, PX-06. A committed run or work-item
// cancellation defeats concurrent claims; cancellation-first produces no
// attempt and no reservation.
func Test_cancel_before_claim(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*executiontest.Env, executiontest.Project, *execution.Run) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("build", readRun), pipelinetest.Container("test", "needs: [build]", readRun)))
		return e, p, e.MustAdmit(p, def, nil)
	}

	t.Run("run cancellation committed first", func(t *testing.T) {
		e, p, run := setup(t)
		got, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing was admitted, so there is nothing to settle: the run is
		// cancelled at once, and it still records the request.
		if got.Status != execution.RunCancelled || got.CancelRequestedAt == nil {
			t.Fatalf("run %s cancel requested %v", got.Status, got.CancelRequestedAt)
		}
		if won := claimAll(t, e, p.Scope.TenantID, 8); len(won) != 0 {
			t.Fatalf("%d claims won after cancellation", len(won))
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("cancellation-first left an attempt or reservation")
		}
		// Idempotent: the same request again changes nothing.
		again, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID)
		if err != nil || again.Version != got.Version {
			t.Fatalf("repeat cancel: %v version %d -> %d", err, got.Version, again.Version)
		}
		for _, st := range e.Get(run).Steps {
			if st.Status != execution.StepCancelled {
				t.Fatalf("step %s is %s", st.ID, st.Status)
			}
		}
	})

	t.Run("work item cancellation committed first", func(t *testing.T) {
		e, p, run := setup(t)
		e.CancelWork(p.Scope, p.Item)
		if won := claimAll(t, e, p.Scope.TenantID, 8); len(won) != 0 {
			t.Fatalf("%d claims won for a cancelled work item", len(won))
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("work cancellation-first left an attempt or reservation")
		}
		got := e.Get(run).Run
		if got.Status != execution.RunCancelled || got.CancelReason != execution.ReasonWorkCancelled {
			t.Fatalf("run %s reason %s: the claim must convert the cancelled item into run cancellation", got.Status, got.CancelReason)
		}
		// A cancelled item admits no new run either.
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("x", readRun)))
		if _, err := e.Admit(p, def, nil); !errors.Is(err, execution.ErrWorkCancelled) {
			t.Fatalf("admission of a cancelled item: %v", err)
		}
	})

	t.Run("a work cancellation holding the item lock defeats a waiting claim", func(t *testing.T) {
		e, p, run := setup(t)
		tx, err := e.Migration.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `UPDATE work_items SET state = 'cancelled', version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			string(p.Scope.TenantID), string(p.Item)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := e.Claim(p.Scope.TenantID); done <- err }()
		select {
		case err := <-done:
			t.Fatalf("the claim did not wait for the uncommitted cancellation: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("the claim after the cancellation committed: %v", err)
		}
		if attemptCount(e, run.ID) != 0 || reservationsHeld(e) != 0 {
			t.Fatal("a claim that lost to cancellation left state")
		}
	})

	t.Run("concurrent cancel and claim never leave live work on a cancelled run", func(t *testing.T) {
		for i := 0; i < 6; i++ {
			e, p, run := setup(t)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = e.Claim(p.Scope.TenantID) }()
			go func() {
				defer wg.Done()
				if _, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID); err != nil {
					t.Errorf("cancel: %v", err)
				}
			}()
			wg.Wait()
			agg := e.Get(run)
			if agg.Run.CancelRequestedAt == nil {
				t.Fatal("cancellation was not recorded")
			}
			for _, at := range agg.Attempts {
				if agg.Control(at.ID).Live() {
					t.Fatalf("attempt %s is still live on a cancelled run", at.ID)
				}
			}
			if agg.Run.Status == execution.RunCancelled && len(agg.Unsettled()) != 0 {
				t.Fatal("a cancelled run has unsettled attempts")
			}
		}
	})
}
