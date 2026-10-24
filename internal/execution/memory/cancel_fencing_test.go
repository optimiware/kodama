package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/execution/memory"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

var engineer = execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t080"}

const uid = "11111111-2222-3333-4444-555555555555"

func evidenceKinds(t *testing.T, m *executiontest.Mem, attempt domain.AttemptID) map[execution.EvidenceKind]int {
	t.Helper()
	ev, err := m.Mem.EvidenceOf(context.Background(), scopeA, attempt)
	if err != nil {
		t.Fatal(err)
	}
	out := map[execution.EvidenceKind]int{}
	for _, e := range ev {
		out[e.Kind]++
	}
	return out
}

// TestUnitCancelFencing proves cancelled, revoked and expired work cannot
// launch or accept stale progress (PX-06, PX-08, PX-09): cancellation commits
// before any claim can win, revokes the attempt at once, keeps late workload
// confirmations and results as evidence only, and settles known side effects
// before the run is terminal.
func TestUnitCancelFencing(t *testing.T) {
	ctx := context.Background()
	tenant := scopeA.TenantID
	two := func(m *executiontest.Mem) *execution.Run {
		return m.MustAdmit(m.NewProject(scopeA), define(m, job("build", readRun), job("test", "needs: [build]", readRun)), nil)
	}

	t.Run("a run cancelled before any claim never dispatches", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("build", readRun), job("test", "needs: [build]", readRun)), nil)
		got, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID)
		if err != nil || got.Status != execution.RunCancelled || got.CancelRequestedAt == nil {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed after cancellation: %v", err)
		}
		if ok, err := m.Svc.DispatchNext(ctx, tenant); err != nil || ok || len(m.Workloads.Starts()) != 0 {
			t.Fatalf("dispatched after cancellation: %v %v %d starts", ok, err, len(m.Workloads.Starts()))
		}
		if attempts(m, run) != 0 || held(m) != 0 {
			t.Fatal("cancellation-first left an attempt or reservation")
		}
		again, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID)
		if err != nil || again.Version != got.Version {
			t.Fatalf("repeat cancel: %v version %d -> %d", err, got.Version, again.Version)
		}
		for _, st := range m.Get(run).Steps {
			if st.Status != execution.StepCancelled {
				t.Fatalf("step %s is %s", st.ID, st.Status)
			}
		}
	})

	t.Run("a cancelled work item turns the next claim into run cancellation", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("build", readRun)), nil)
		m.CancelWork(scopeA, p.Item) // intent only: no event reached execution
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed work of a cancelled item: %v", err)
		}
		got := m.Get(run).Run
		if got.Status != execution.RunCancelled || got.CancelReason != execution.ReasonWorkCancelled || attempts(m, run) != 0 || held(m) != 0 {
			t.Fatalf("run %s reason %s, %d attempts", got.Status, got.CancelReason, attempts(m, run))
		}
	})

	t.Run("a work cancellation request revokes a running attempt and refuses later claims", func(t *testing.T) {
		m, p := fixture(t)
		run := two(m)
		_ = p
		in := m.MustClaim(tenant)
		m.Start(in)
		if err := m.Svc.RequestCancellation(ctx, execution.WorkCancellation{Scope: scopeA, WorkItemID: run.WorkItemID, Version: 2}); err != nil {
			t.Fatal(err)
		}
		agg := m.Get(run)
		if agg.Run.CancelRequestedAt == nil || agg.Run.Status.Terminal() || agg.Control(in.AttemptID).Live() || agg.Attempts[0].Status != execution.AttemptCancelled {
			t.Fatalf("run %s attempt %s live %v", agg.Run.Status, agg.Attempts[0].Status, agg.Control(in.AttemptID).Live())
		}
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed after the work cancellation: %v", err)
		}
		if _, err := m.Succeed(in, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a late result of a revoked attempt: %v", err)
		}
		if got := m.Get(run); got.Step("build").Status.Terminal() && got.Step("build").Status == execution.StepSucceeded || got.Step("test").Status != execution.StepCancelled {
			t.Fatal("a late result progressed the run")
		}
		// Still unsettled: the workload is not confirmed gone, the slot is held.
		if held(m) != 1 || m.Get(run).Control(in.AttemptID).Settled() {
			t.Fatal("capacity was released before the workload was confirmed gone")
		}
		if err := m.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		if got := m.Get(run); got.Run.Status != execution.RunCancelled || held(m) != 0 {
			t.Fatalf("run %s, %d reservations after settlement", got.Run.Status, held(m))
		}
	})

	t.Run("a workload created after cancellation is late evidence and is stopped by reconciliation", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("edit", writeRun), job("after", "needs: [edit]", readRun)), nil)
		creating := make(chan execution.AttemptSpec, 1)
		release := make(chan struct{})
		m.Workloads.Before(runtimetest.OpStart, func(_ context.Context, c runtimetest.Call) error {
			creating <- c.Spec
			<-release // the workload create is in flight
			return nil
		})
		dispatched := make(chan error, 1)
		go func() { _, err := m.Svc.DispatchNext(ctx, tenant); dispatched <- err }()
		in := <-creating

		got, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Authority ends at once; the run is not terminal and the slot and
		// capacity are retained until settlement.
		agg := m.Get(run)
		if got.Status.Terminal() || agg.Control(in.AttemptID).Live() || held(m) != 1 || writerOf(m, p) != in.AttemptID {
			t.Fatalf("run %s live %v reservations %d writer %q", got.Status, agg.Control(in.AttemptID).Live(), held(m), writerOf(m, p))
		}
		close(release)
		if err := <-dispatched; err != nil {
			t.Fatal(err)
		}
		agg = m.Get(run)
		if at := agg.Attempt(in.AttemptID); at.Status != execution.AttemptCancelled || agg.Control(in.AttemptID).DispatchConfirmedAt != nil {
			t.Fatalf("attempt %s dispatch confirmed %v", at.Status, agg.Control(in.AttemptID).DispatchConfirmedAt)
		}
		if n := evidenceKinds(t, m, in.AttemptID)[execution.EvidenceLateObservation]; n != 1 {
			t.Fatalf("%d late observations", n)
		}
		if _, err := m.Succeed(&in, nil, executiontest.CommitA); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("late result: %v", err)
		}
		if n := evidenceKinds(t, m, in.AttemptID)[execution.EvidenceLateResult]; n != 1 {
			t.Fatalf("%d late results", n)
		}
		// Termination is recoverable: the first Stop confirmed nothing; a later
		// reconciliation stops the late workload and settles exactly once.
		if n := len(m.Workloads.Calls(runtimetest.OpStop)); n < 1 {
			t.Fatal("the cancellation never asked the runtime to stop the workload")
		}
		m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted})
		if _, err := m.NewService(executiontest.MemOptions{}).Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		final := m.Get(run)
		if final.Run.Status != execution.RunCancelled || final.Run.FinishedAt == nil || !final.Control(in.AttemptID).Settled() || held(m) != 0 || writerOf(m, p) != "" {
			t.Fatalf("run %s settled %v reservations %d writer %q", final.Run.Status, final.Control(in.AttemptID).Settled(), held(m), writerOf(m, p))
		}
	})

	t.Run("a cancelled attempt is never redriven", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		m.Workloads.Before(runtimetest.OpStart, func(context.Context, runtimetest.Call) error { return errors.New("lost") })
		if ok, err := m.Svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatal(ok, err)
		}
		m.Workloads.Before(runtimetest.OpStart, nil)
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		m.Advance(5 * time.Second) // the dispatch lease lapsed
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if n := len(m.Workloads.Starts()); n != 1 {
			t.Fatalf("a cancelled attempt was started again (%d starts)", n)
		}
	})

	t.Run("a failed sibling revokes the others and their results are evidence only", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("a", readRun), job("b", readRun)), nil)
		_ = p
		first, second := m.MustClaim(tenant), m.MustClaim(tenant)
		m.Start(first)
		m.Start(second)
		if _, err := m.Fail(first); err != nil {
			t.Fatal(err)
		}
		agg := m.Get(run)
		if agg.Run.FailureIntentAt == nil || agg.Control(second.AttemptID).Live() || agg.Attempt(second.AttemptID).Status != execution.AttemptCancelled {
			t.Fatalf("the sibling kept its authority: %s", agg)
		}
		if _, err := m.Succeed(second, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a revoked sibling's result: %v", err)
		}
		if got := m.Get(run); got.Step(second.StepID).Status == execution.StepSucceeded {
			t.Fatal("a revoked attempt's result succeeded its step")
		}
		for _, in := range []*execution.AttemptSpec{first, second} {
			if err := m.Terminate(in, execution.LaunchStarted); err != nil {
				t.Fatal(err)
			}
		}
		if got := m.Get(run); got.Run.Status != execution.RunFailed || held(m) != 0 {
			t.Fatalf("run %s, %d reservations", got.Run.Status, held(m))
		}
	})

	t.Run("a refused or revoked result changes nothing and an accepted one replays", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun, "outputs: {report: {type: string}}")), nil)
		_ = p
		in := m.MustClaim(tenant)
		m.Start(in)
		out := map[string]runtimev1.Value{"report": runtimev1.StringValue("ok")}
		ack, err := m.Succeed(in, out, "")
		if err != nil || ack.Replayed {
			t.Fatalf("ack %+v %v", ack, err)
		}
		again, err := m.Succeed(in, out, "")
		if err != nil || !again.Replayed || again.ResultHash != ack.ResultHash {
			t.Fatalf("replay %+v %v", again, err)
		}
		if _, err := m.Succeed(in, map[string]runtimev1.Value{"report": runtimev1.StringValue("other")}, ""); !errors.Is(err, execution.ErrResultConflict) {
			t.Fatalf("a different hash was accepted: %v", err)
		}
		agg := m.Get(run)
		if got := agg.Step("job").Outputs["report"]; got != runtimev1.StringValue("ok") || agg.Run.Status != execution.RunSucceeded {
			t.Fatalf("outputs %v run %s", agg.Step("job").Outputs, agg.Run.Status)
		}
	})

	t.Run("an observation of another workload, instance or spec is refused and kept as evidence", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
		_ = p
		in := m.MustClaim(tenant)
		m.Start(in)
		before := m.Get(run)
		for name, obs := range map[string]execution.Observation{
			"another workload": {Kind: execution.ObservedFailed, WorkloadName: "wl-other", Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure},
			"another spec":     {Kind: execution.ObservedDispatched, SpecFingerprint: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"},
			"another instance": {Kind: execution.ObservedStarted, WorkloadUID: "aaaaaaaa-2222-3333-4444-555555555555"},
		} {
			if err := m.Observe(in, obs); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		after := m.Get(run)
		if after.Attempts[0].Status != execution.AttemptRunning || after.Attempts[0].Version != before.Attempts[0].Version || after.Run.Version != before.Run.Version {
			t.Fatalf("a mismatching observation changed state: %s", after)
		}
		if n := evidenceKinds(t, m, in.AttemptID)[execution.EvidenceMismatch]; n != 3 {
			t.Fatalf("%d mismatch evidence rows", n)
		}
	})

	t.Run("a result after the startup or execution deadline cannot progress the run", func(t *testing.T) {
		for name, advance := range map[string]func(*executiontest.Mem, *execution.AttemptSpec) time.Duration{
			"startup": func(m *executiontest.Mem, in *execution.AttemptSpec) time.Duration { return 2 * time.Minute },
			"execution": func(m *executiontest.Mem, in *execution.AttemptSpec) time.Duration {
				m.Start(in)
				return 2 * time.Minute
			},
		} {
			m, p := fixture(t)
			run := m.MustAdmit(p, define(m, job("job", readRun)), nil)
			in := m.MustClaim(tenant)
			m.Advance(advance(m, in))
			if _, err := m.Succeed(in, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("%s: a late result was accepted: %v", name, err)
			}
			agg := m.Get(run)
			if at := agg.Attempts[0]; at.Status != execution.AttemptFailed || (at.Reason != execution.ReasonStartupTimeout && at.Reason != execution.ReasonTimeout) ||
				agg.Step("job").Status == execution.StepSucceeded || agg.Run.FailureIntentAt == nil {
				t.Fatalf("%s: attempt %s/%s run %s", name, at.Status, at.Reason, agg.Run.Status)
			}
			if n := evidenceKinds(t, m, in.AttemptID)[execution.EvidenceLateResult]; n != 1 {
				t.Fatalf("%s: %d late-result rows", name, n)
			}
			if err := m.Terminate(in, execution.LaunchStarted); err != nil {
				t.Fatal(err)
			}
			if got := m.Get(run); got.Run.Status != execution.RunFailed {
				t.Fatalf("%s: run %s", name, got.Run.Status)
			}
		}
	})

	t.Run("cancellation settles a writer only once its publication is settled", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("edit", writeRun)), nil)
		in := m.MustClaim(tenant)
		m.Start(in)
		m.World.SetPublicationPending(in.AttemptID, true)
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		// The termination confirmation rolls back with the refused settlement.
		if err := m.Terminate(in, execution.LaunchStarted); !errors.Is(err, execution.ErrBusy) {
			t.Fatalf("settled a writer with an unsettled publication: %v", err)
		}
		agg := m.Get(run)
		if agg.Control(in.AttemptID).Settled() || agg.Control(in.AttemptID).TerminationConfirmedAt != nil || writerOf(m, p) != in.AttemptID || held(m) != 1 {
			t.Fatal("a refused settlement left partial state")
		}
		m.World.SetPublicationPending(in.AttemptID, false)
		if err := m.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		if got := m.Get(run); got.Run.Status != execution.RunCancelled || writerOf(m, p) != "" || held(m) != 0 {
			t.Fatalf("run %s writer %q reservations %d", got.Run.Status, writerOf(m, p), held(m))
		}
	})

	t.Run("cancellation ends the run's secret pins and repeats Stop until the workload is gone", func(t *testing.T) {
		m, p := fixture(t)
		ref := m.World.CreateSecret(scopeA, "token")
		run := m.MustAdmit(p, define(m, job("job", readRun, "env: {TOKEN: {secretRef: "+ref+"}}")), nil)
		if pins, _ := m.World.LivePins(ctx, scopeA, run.ID); len(pins) != 1 {
			t.Fatalf("pins %v", pins)
		}
		in := m.MustClaim(tenant)
		m.Start(in)
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		if pins, _ := m.World.LivePins(ctx, scopeA, run.ID); len(pins) != 0 {
			t.Fatalf("a cancelled run kept its secret access: %v", pins)
		}
		for i := 0; i < 2; i++ {
			if _, err := m.Svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(m.Workloads.Calls(runtimetest.OpStop)); n < 3 {
			t.Fatalf("Stop was repeated %d times for a workload that was never confirmed gone", n)
		}
		if got := m.Get(run); got.Run.Status.Terminal() {
			t.Fatal("the run finished before the workload was confirmed gone")
		}
		m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
		if _, err := m.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if got := m.Get(run); got.Run.Status != execution.RunCancelled {
			t.Fatalf("run %s", got.Run.Status)
		}
	})

	t.Run("runtime grants of a revoked attempt end in the same transaction", func(t *testing.T) {
		var mu sync.Mutex
		var revoked []domain.AttemptID
		var fail error
		grants := func(_ memdb.Tx, _ domain.Scope, attempt domain.AttemptID, _ time.Time) error {
			mu.Lock()
			defer mu.Unlock()
			if fail != nil {
				return fail
			}
			revoked = append(revoked, attempt)
			return nil
		}
		m, p := fixture(t, executiontest.MemOptions{Grants: memory.GrantRevoker(grants)})
		run := m.MustAdmit(p, define(m, job("a", readRun), job("b", readRun)), nil)
		first, second := m.MustClaim(tenant), m.MustClaim(tenant)
		m.Start(first)
		m.Start(second)

		mu.Lock()
		fail = errors.New("grant store down")
		mu.Unlock()
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("cancel with the grant store down: %v", err)
		}
		if agg := m.Get(run); agg.Run.CancelRequestedAt != nil || !agg.Control(first.AttemptID).Live() {
			t.Fatal("a cancellation committed although its grants could not be ended")
		}
		mu.Lock()
		fail = nil
		mu.Unlock()
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(revoked) != 2 || revoked[0] > revoked[1] {
			t.Fatalf("grants ended for %v, want both attempts in ID order", revoked)
		}
	})
}
