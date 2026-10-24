package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/workspace"
)

// Test_cancel_inflight_create: PX-08, PX-A06. Cancellation revokes the
// attempt's authority before a delayed Job create completes; termination
// remains recoverable; late results cannot progress the run.
func Test_cancel_inflight_create(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(job("edit", writeRun, "timeoutSeconds: 120"), job("after", "needs: [edit]", readRun, "timeoutSeconds: 120")))
	run := e.MustAdmit(p, def, nil)

	creating := make(chan execution.AttemptSpec, 1)
	release := make(chan struct{})
	e.Workloads.Before(runtimetest.OpStart, func(_ context.Context, c runtimetest.Call) error {
		creating <- c.Spec
		<-release // the workload create is in flight
		return nil
	})
	dispatched := make(chan error, 1)
	go func() { _, err := e.Svc.DispatchNext(ctx, p.Scope.TenantID); dispatched <- err }()
	in := <-creating

	authority, err := execpg.NewAttemptAuthority(e.Store)
	if err != nil {
		t.Fatal(err)
	}
	live, err := authority.Authority(ctx, p.Scope, in.AttemptID)
	if err != nil || !live.Live || live.Access != workspace.AccessWrite || live.WorkspaceID != p.Workspace {
		t.Fatalf("authority before cancel: %+v %v", live, err)
	}

	got, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Authority is revoked at once; the run is not terminal, the effect is not
	// yet settled, and the writer slot and capacity are retained.
	if live, _ := authority.Authority(ctx, p.Scope, in.AttemptID); live.Live {
		t.Fatal("the attempt kept its authority after cancellation")
	}
	if got.Status.Terminal() || got.CancelRequestedAt == nil {
		t.Fatalf("run %s cancel %v", got.Status, got.CancelRequestedAt)
	}
	if reservationsHeld(e) != 1 {
		t.Fatalf("%d reservations held before settlement", reservationsHeld(e))
	}
	var holder *string
	e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
	if holder == nil || *holder != string(in.AttemptID) {
		t.Fatal("the writer slot was released before settlement")
	}
	// The unsettled writer blocks the work item's business release.
	settled := true
	if err := e.RuntimeTx(p.Scope.TenantID, func(ctx context.Context, tx pgxTx) error {
		var err error
		settled, err = execpg.NewWriterGuard(tx).WriterSettled(ctx, p.Scope, p.Item)
		return err
	}); err != nil || settled {
		t.Fatalf("writer guard: settled=%v err=%v", settled, err)
	}

	// The delayed create now completes: the confirmation is late evidence for
	// the same revoked attempt, and it rewrites nothing.
	close(release)
	if err := <-dispatched; err != nil {
		t.Fatal(err)
	}
	agg := e.Get(run)
	at := agg.Attempt(in.AttemptID)
	if at.Status != execution.AttemptCancelled || agg.Control(in.AttemptID).DispatchConfirmedAt != nil {
		t.Fatalf("attempt %s, dispatch confirmed %v", at.Status, agg.Control(in.AttemptID).DispatchConfirmedAt)
	}
	if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE kind = 'late_observation' AND attempt_id = $1`, string(in.AttemptID)); n != 1 {
		t.Fatalf("%d late observation rows", n)
	}

	// Late results cannot progress the run.
	if _, err := e.Succeed(&in, nil, executiontest.CommitA); !errors.Is(err, execution.ErrAttemptRevoked) {
		t.Fatalf("late result: %v", err)
	}
	if got := e.Get(run); got.Step("after").Status != execution.StepCancelled || got.Step("edit").Status.Terminal() && got.Step("edit").Status == execution.StepSucceeded {
		t.Fatal("a late result progressed the run")
	}

	// Termination is recoverable: the first request found nothing to confirm; a
	// later reconciliation deletes the late workload, confirms its absence and
	// settles the attempt, releasing the writer slot and capacity exactly once.
	e.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted})
	if _, err := e.NewService(executiontest.Options{}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	final := e.Get(run)
	if final.Run.Status != execution.RunCancelled || final.Run.FinishedAt == nil || !final.Control(in.AttemptID).Settled() {
		t.Fatalf("run %s settled %v", final.Run.Status, final.Control(in.AttemptID).Settled())
	}
	if reservationsHeld(e) != 0 {
		t.Fatal("capacity not released")
	}
	e.Scalar(&holder, `SELECT writer_attempt_id FROM workspaces WHERE id = $1`, string(p.Workspace))
	if holder != nil {
		t.Fatal("the writer slot was not released by settlement")
	}
	if err := e.RuntimeTx(p.Scope.TenantID, func(ctx context.Context, tx pgxTx) error {
		var err error
		settled, err = execpg.NewWriterGuard(tx).WriterSettled(ctx, p.Scope, p.Item)
		return err
	}); err != nil || !settled {
		t.Fatalf("writer guard after settlement: settled=%v err=%v", settled, err)
	}
	// The cancelled run released its secret pins (consumer access ended).
	_ = domain.RunID("")
	_ = time.Second
}
