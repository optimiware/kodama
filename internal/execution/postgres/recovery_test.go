package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_dispatch_redrive: PX-03, PX-A02. A claim whose start response is lost
// is redriven with the same attempt and workload identity, never a fresh
// attempt.
func Test_dispatch_redrive(t *testing.T) {
	ctx := context.Background()
	e := executiontest.New(t)
	e.GrantAll()
	p := e.NewProject(executiontest.ScopeA)
	def := e.Define(p.Scope, pipelinetest.Doc(job("job", readRun, "timeoutSeconds: 60")))
	run := e.MustAdmit(p, def, nil)

	var calls atomic.Int32
	e.Workloads.After(runtimetest.OpStart, func(context.Context, runtimetest.Call) error {
		if calls.Add(1) == 1 {
			return errors.New("connection lost after the workload create") // the controller crashed or lost the answer
		}
		return nil
	})
	claimed, err := e.Svc.DispatchNext(ctx, p.Scope.TenantID)
	if err != nil || !claimed {
		t.Fatalf("dispatch: %v %v", claimed, err)
	}
	first := e.Workloads.Starts()[0]
	if c := e.Get(run).Control(first.AttemptID); c.DispatchConfirmedAt != nil {
		t.Fatal("an unconfirmed create was recorded as confirmed")
	}
	// While the claim lease is fresh nobody else redrives it.
	if rep, err := e.Svc.ReconcileTenant(ctx, p.Scope.TenantID); err != nil || rep.Redriven != 0 {
		t.Fatalf("early reconcile: %+v %v", rep, err)
	}
	// A restarted controller redrives after the lease lapses, with the same identity.
	e.Clock.Advance(3 * time.Second)
	rep, err := e.NewService(executiontest.Options{}).ReconcileTenant(ctx, p.Scope.TenantID)
	if err != nil || rep.Redriven != 1 {
		t.Fatalf("redrive: %+v %v", rep, err)
	}
	starts := e.Workloads.Starts()
	if len(starts) != 2 {
		t.Fatalf("%d start calls", len(starts))
	}
	second := starts[1]
	if !second.Redrive || second.AttemptID != first.AttemptID || second.WorkloadName != first.WorkloadName || second.SpecFingerprint != first.SpecFingerprint || second.Ordinal != 1 {
		t.Fatalf("redrive changed identity: %+v vs %+v", second, first)
	}
	if attemptCount(e, run.ID) != 1 || reservationsHeld(e) != 1 {
		t.Fatalf("%d attempts, %d reservations", attemptCount(e, run.ID), reservationsHeld(e))
	}
	if c := e.Get(run).Control(first.AttemptID); c.DispatchConfirmedAt == nil {
		t.Fatal("the redriven create was not confirmed")
	}
	// Once confirmed it is not redriven again.
	e.Clock.Advance(time.Minute)
	if rep, _ := e.Svc.ReconcileTenant(ctx, p.Scope.TenantID); rep.Redriven != 0 {
		t.Fatalf("a confirmed attempt was redriven: %+v", rep)
	}
	// Concurrent controllers redrive an expired claim exactly once.
	e2 := executiontest.New(t)
	e2.GrantAll()
	p2 := e2.NewProject(executiontest.ScopeA)
	def2 := e2.Define(p2.Scope, pipelinetest.Doc(job("job", readRun, "timeoutSeconds: 60")))
	e2.MustAdmit(p2, def2, nil)
	e2.Workloads.After(runtimetest.OpStart, func(context.Context, runtimetest.Call) error { return errors.New("lost") })
	if _, err := e2.Svc.DispatchNext(ctx, p2.Scope.TenantID); err != nil {
		t.Fatal(err)
	}
	e2.Clock.Advance(3 * time.Second)
	e2.Workloads.After(runtimetest.OpStart, nil)
	done := make(chan int, 6)
	for range 6 {
		go func() {
			rep, _ := e2.NewService(executiontest.Options{}).ReconcileTenant(ctx, p2.Scope.TenantID)
			done <- rep.Redriven
		}()
	}
	total := 0
	for range 6 {
		total += <-done
	}
	if total != 1 {
		t.Fatalf("%d controllers redrove the same claim", total)
	}
}

// Test_secret_pins_released: PX-08, WS-11 boundaries. The run's secret-consumer
// pins end at the revocation boundary (cancellation or failure intent) and at
// completion, while historical reference metadata is retained.
func Test_secret_pins_released(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*executiontest.Env, executiontest.Project, *execution.Run) {
		e := executiontest.New(t)
		e.GrantAll()
		e.Secret(executiontest.ScopeA, "token", "value")
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("job", readRun, "timeoutSeconds: 60", secretEnv)))
		return e, p, e.MustAdmit(p, def, nil)
	}
	released := func(e *executiontest.Env, run *execution.Run) bool {
		var n, total int
		e.Scalar(&n, `SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND released_at IS NOT NULL`, string(run.ID))
		e.Scalar(&total, `SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, string(run.ID))
		return total > 0 && n == total
	}
	t.Run("cancellation", func(t *testing.T) {
		e, p, run := setup(t)
		if released(e, run) {
			t.Fatal("released before the run ended")
		}
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if _, err := e.Svc.Cancel(ctx, engineer, p.Scope, run.ID); err != nil {
			t.Fatal(err)
		}
		if !released(e, run) {
			t.Fatal("cancellation did not end the secret-consumer access")
		}
		if e.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, string(run.ID)) != 1 {
			t.Fatal("the historical pin record was not retained")
		}
	})
	t.Run("failure intent", func(t *testing.T) {
		e, p, run := setup(t)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if _, err := e.Fail(in); err != nil {
			t.Fatal(err)
		}
		if !released(e, run) {
			t.Fatal("failure intent did not end the secret-consumer access")
		}
	})
	t.Run("completion", func(t *testing.T) {
		e, p, run := setup(t)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if released(e, run) {
			t.Fatal("released while the step ran")
		}
		if _, err := e.Succeed(in, nil, ""); err != nil {
			t.Fatal(err)
		}
		if !released(e, run) {
			t.Fatal("a terminal run kept its secret-consumer access")
		}
	})
}
