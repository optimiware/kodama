package runstate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/runstate"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

// memory builds the adapter under test around the suite's clock.
func memory(_ kernelcontracts.TB, clock runtime.Clock) kernelcontracts.Subject {
	return runstate.NewMemory(clock)
}

// Test_unit_kernel_state_contract runs the whole shared state contract suite
// against the memory adapter. It proves the port's policy over memory state
// only; PostgreSQL semantics are qualified by the persistence task.
func Test_unit_kernel_state_contract(t *testing.T) {
	kernelcontracts.Run(t, memory)
}

// Test_unit_kernel_state_claim_races covers claim, lease, fence and takeover
// behaviour, including concurrent claimants.
func Test_unit_kernel_state_claim_races(t *testing.T) {
	kernelcontracts.RunGroup(t, memory, kernelcontracts.GroupClaims, kernelcontracts.GroupCleanup)

	// Repeat the concurrent cases so a scheduling-dependent defect has many chances to show.
	t.Run("repeated races", func(t *testing.T) {
		for _, c := range kernelcontracts.Cases() {
			if c.Name != "claim/race_has_one_winner" && c.Name != "wait/resolution_cancellation_race_has_one_winner" {
				continue
			}
			for i := range 25 {
				t.Run(fmt.Sprintf("%s/%d", c.Name, i), func(t *testing.T) { kernelcontracts.Check(t, memory, c) })
			}
		}
	})
}

// Test_unit_kernel_state_wait_consumption covers wait decisions, signal
// identity routing and duplicate-signal convergence.
func Test_unit_kernel_state_wait_consumption(t *testing.T) {
	kernelcontracts.RunGroup(t, memory, kernelcontracts.GroupWaits)
}

func Test_unit_kernel_state_memory_adapter(t *testing.T) {
	clock := kernelcontracts.NewClock()
	t.Run("a store needs a clock", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("NewMemory(nil) must panic")
			}
		}()
		runstate.NewMemory(nil)
	})
	t.Run("a pending commit fault is consumed once and can be cleared", func(t *testing.T) {
		m := runstate.NewMemory(clock)
		scope := domain.Scope{TenantID: "t1", ProjectID: "p1"}
		plan := runtime.Plan{Digest: "sha256:p", Steps: []runtime.PlanStep{{Key: "build", Kind: runtime.StepContainer,
			Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}}}}
		admit := func(id domain.RunID) error {
			run, err := runtime.NewRun(scope, id, "pipe1", plan, 2)
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Admit(context.Background(), runtime.Admission{Run: run, Plan: plan})
			return err
		}
		boom := errors.New("boom")
		m.FailNextCommit(boom)
		if err := admit("run1"); !errors.Is(err, boom) {
			t.Fatalf("the fault must fail the next write: %v", err)
		}
		if err := admit("run1"); err != nil {
			t.Fatalf("the fault is consumed once: %v", err)
		}
		m.FailNextCommit(boom)
		m.FailNextCommit(nil)
		if err := admit("run2"); err != nil {
			t.Fatalf("a cleared fault must not fire: %v", err)
		}
	})
	t.Run("a run that was never admitted has no outbox", func(t *testing.T) {
		m := runstate.NewMemory(clock)
		_, err := m.Outbox(context.Background(), domain.Scope{TenantID: "t1", ProjectID: "p1"}, "absent")
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeNotFound {
			t.Fatalf("got %v, want not found", err)
		}
	})
	t.Run("runnable is bounded and needs the time", func(t *testing.T) {
		m := runstate.NewMemory(clock)
		if _, err := m.Runnable(context.Background(), runtime.RunnableQuery{Now: time.Time{}, Limit: 1}); err == nil {
			t.Error("a query without the time must be refused")
		}
		if _, err := m.Runnable(context.Background(), runtime.RunnableQuery{Now: clock.Now(), Limit: -1}); err == nil {
			t.Error("a negative limit must be refused")
		}
		refs, err := m.Runnable(context.Background(), runtime.RunnableQuery{Now: clock.Now(), Limit: 5})
		if err != nil || len(refs) != 0 {
			t.Errorf("an empty store has nothing runnable: %v %v", refs, err)
		}
	})
}
