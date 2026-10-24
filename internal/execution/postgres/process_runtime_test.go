package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_process_runtime_lifecycle: the execution service drives real host
// workloads through the Process runtime: the run pins the runtime it was
// admitted for, dispatch binds the reported instance, reconciliation observes
// a failure, cancellation stops a running workload, and every settled
// workload is removed once with its removal recorded.
func Test_process_runtime_lifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rt, err := process.New(process.Config{Program: process.DeclaredCommand{}, Dir: dir, KillGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e := executiontest.New(t, executiontest.Options{Runtime: rt, Requirements: runtimetest.NoRequirements})
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	tenant := p.Scope.TenantID

	dispatch := func(t *testing.T, script string) (*execution.Run, execution.Workload) {
		t.Helper()
		run := e.MustAdmit(p, e.Define(p.Scope, pipelinetest.Doc(job("build", readRun, "args: ['"+script+"']", "timeoutSeconds: 60"))), nil)
		if b := run.Snapshot.Runtime; b == nil || b.Backend != process.Backend || b.Requires != 0 {
			t.Fatalf("binding %+v", b)
		}
		if claimed, err := e.Svc.DispatchNext(ctx, tenant); err != nil || !claimed {
			t.Fatalf("dispatch: %v %v", claimed, err)
		}
		starts := e.Workloads.Starts()
		at := e.Get(run).Attempt(starts[len(starts)-1].AttemptID)
		if at.WorkloadUID == "" || e.Get(run).Control(at.ID).DispatchConfirmedAt == nil {
			t.Fatalf("the instance was not bound: %+v", at)
		}
		return run, at.Workload()
	}
	removed := func(w execution.Workload) bool {
		return e.Count(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND workload_removed_at IS NOT NULL`, string(w.AttemptID)) == 1
	}

	t.Run("a failing workload fails, settles and is removed", func(t *testing.T) {
		run, w := dispatch(t, "echo failing; exit 3")
		if err := rt.Wait(ctx, w); err != nil {
			t.Fatal(err)
		}
		rep, err := e.Svc.ReconcileTenant(ctx, tenant)
		if err != nil || rep.Observed != 1 || rep.Removed != 1 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		agg := e.Get(run)
		at := agg.Attempt(w.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonWorkloadFailed || !agg.Control(at.ID).Settled() || !removed(w) {
			t.Fatalf("attempt %s/%s settled %v removed %v", at.Status, at.Reason, agg.Control(at.ID).Settled(), removed(w))
		}
		if _, err := os.Stat(dir + "/" + w.Name); !os.IsNotExist(err) {
			t.Fatalf("the workload directory survived removal: %v", err)
		}
		if rep, _ := e.Svc.ReconcileTenant(ctx, tenant); rep.Removed != 0 || len(e.Workloads.Calls(runtimetest.OpRemove)) != 1 {
			t.Fatalf("a recorded removal was repeated: %+v", rep)
		}
	})

	t.Run("another backend never claims the run", func(t *testing.T) {
		run := e.MustAdmit(p, e.Define(p.Scope, pipelinetest.Doc(job("build", readRun, "args: ['true']", "timeoutSeconds: 60"))), nil)
		other := e.NewService(executiontest.Options{Runtime: runtimetest.NewManual(), Requirements: runtimetest.NoRequirements})
		if _, err := other.ClaimRunnable(ctx, tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed by another backend: %v", err)
		}
		if n := attemptCount(e, run.ID); n != 0 {
			t.Fatalf("%d attempts", n)
		}
		if _, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("only the pinned backend lists and removes a settled workload", func(t *testing.T) {
		run, w := dispatch(t, "exit 3")
		if err := rt.Wait(ctx, w); err != nil {
			t.Fatal(err)
		}
		if rep, err := e.Svc.ReconcileRun(ctx, execution.RunRef{Scope: p.Scope, ID: run.ID}); err != nil || rep.Observed != 1 || !e.Get(run).Control(w.AttemptID).Settled() {
			t.Fatalf("settle: %+v %v", rep, err)
		}
		listed := func(backend string) int {
			page, err := e.Store.Removable(ctx, tenant, backend, execution.RemovalCursor{}, 100)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range page {
				if r.Workload.AttemptID == w.AttemptID {
					n++
				}
			}
			return n
		}
		if listed("podman") != 0 || listed("") != 0 || listed(process.Backend) != 1 {
			t.Fatalf("listed: podman %d unbound %d process %d", listed("podman"), listed(""), listed(process.Backend))
		}
		other := e.NewService(executiontest.Options{Runtime: runtimetest.NewManual(), Requirements: runtimetest.NoRequirements})
		if n, err := other.RemoveSettled(ctx, tenant); err != nil || n != 0 || removed(w) {
			t.Fatalf("another backend removed it: %d %v", n, err)
		}
		if n, err := e.Svc.RemoveSettled(ctx, tenant); err != nil || n != 1 || !removed(w) {
			t.Fatalf("the pinned backend did not: %d %v", n, err)
		}
	})

	t.Run("cancellation stops the running workload", func(t *testing.T) {
		run, w := dispatch(t, "exec sleep 60")
		if _, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		c := agg.Control(w.AttemptID)
		if agg.Run.Status != execution.RunCancelled || !c.Settled() || c.Launch != execution.LaunchStarted {
			t.Fatalf("run %s settled %v launch %s", agg.Run.Status, c.Settled(), c.Launch)
		}
		if err := rt.Wait(ctx, w); err != nil {
			t.Fatalf("the stopped workload still runs: %v", err)
		}
		if _, err := e.Svc.ReconcileTenant(ctx, tenant); err != nil || !removed(w) {
			t.Fatalf("removal: %v %v", removed(w), err)
		}
	})
}
