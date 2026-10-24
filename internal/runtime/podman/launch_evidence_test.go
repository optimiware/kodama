package podman_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_runtime_never_started_capability (T-102): Podman keeps the stronger
// launch-history declaration, separate from its container guarantees, and a
// stop before the start proves the command never started.
func Test_runtime_never_started_capability(t *testing.T) {
	e := newCheckoutEnv(t, nil)
	caps := e.rt.Capabilities()
	if !caps.ProvesNeverStarted || caps.Backend != podman.Backend {
		t.Fatalf("capabilities %+v", caps)
	}
	if want := execution.OCIImage | execution.Isolation | execution.ResourceLimits | execution.CrashRecovery; caps.Guarantees != want {
		t.Fatalf("the proof capability changed the guarantees: %v", caps.Guarantees)
	}
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "true")
	ev, err := e.rt.Stop(context.Background(), s.Workload(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimetest.CheckFreshStop(caps, ev); err != nil || ev.Launch != execution.LaunchNotStarted {
		t.Fatalf("%+v: %v", ev, err)
	}
}

// Test_runtime_stop_no_relaunch (T-102): a workload stopped before its start is
// fenced durably and never launches.
func Test_runtime_stop_no_relaunch(t *testing.T) {
	ctx := context.Background()
	e := newCheckoutEnv(t, nil)
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "true")
	if ev, err := e.rt.Stop(ctx, s.Workload("")); err != nil || !ev.Stopped {
		t.Fatalf("%+v %v", ev, err)
	}
	if _, err := e.rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("a workload stopped first was started: %v", err)
	}
	if slices.Contains(e.fake.calls(t), "create") || slices.Contains(e.fake.calls(t), "start") {
		t.Fatalf("a container was launched: %v", e.fake.calls(t))
	}
}
