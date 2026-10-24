package process

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_runtime_never_started_capability (T-102): the Process runtime declares
// the proof, with no container guarantee, and proves it for a fresh stop.
func Test_runtime_never_started_capability(t *testing.T) {
	r := newRuntime(t, DeclaredCommand{})
	caps := r.Capabilities()
	if !caps.ProvesNeverStarted || caps.Guarantees != 0 || caps.Backend != Backend {
		t.Fatalf("capabilities %+v", caps)
	}
	ev, err := r.Stop(context.Background(), spec(t, "true").Workload(""))
	if err != nil || ev.Launch != execution.LaunchNotStarted {
		t.Fatalf("%+v %v", ev, err)
	}
	if err := runtimetest.CheckFreshStop(caps, ev); err != nil {
		t.Fatal(err)
	}
}

// Test_runtime_stop_no_relaunch (T-102): a stop before the start fences the
// launch, and a repeated stop keeps the same evidence.
func Test_runtime_stop_no_relaunch(t *testing.T) {
	ctx := context.Background()
	r := newRuntime(t, DeclaredCommand{})
	s := spec(t, "true")
	first, err := r.Stop(ctx, s.Workload(""))
	if err != nil || !first.Stopped {
		t.Fatalf("%+v %v", first, err)
	}
	if _, err := r.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("a workload stopped first was started: %v", err)
	}
	if again, err := r.Stop(ctx, s.Workload("")); err != nil || again != first {
		t.Fatalf("%+v then %+v: %v", first, again, err)
	}
}
