package runtimetest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

func newProcess(t *testing.T) *process.Runtime {
	t.Helper()
	r, err := process.New(process.Config{Program: process.DeclaredCommand{}, Dir: t.TempDir(), KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// unproven behaves like a backend that fences and stops correctly but cannot
// prove a fresh workload never started, as Kubernetes cannot from absent
// records: it does not advertise the capability and reports LaunchUnknown
// instead of affirmative never-started evidence. Everything else is the real
// Process runtime, not a permissive fake.
type unproven struct{ *process.Runtime }

func (u unproven) Capabilities() execution.Capabilities {
	c := u.Runtime.Capabilities()
	c.ProvesNeverStarted = false
	return c
}

func (u unproven) Stop(ctx context.Context, w execution.Workload) (execution.StopEvidence, error) {
	ev, err := u.Runtime.Stop(ctx, w)
	if ev.Launch == execution.LaunchNotStarted {
		ev.Launch = execution.LaunchUnknown
	}
	return ev, err
}

// Test_runtime_never_started_capability (T-102): the shared contract passes for
// a backend advertising the proof (affirmative LaunchNotStarted) and for one
// that does not (LaunchUnknown), and each side of the assertion rejects the
// other's evidence where it should.
func Test_runtime_never_started_capability(t *testing.T) {
	t.Run("advertising backend passes the contract", func(t *testing.T) {
		if !newProcess(t).Capabilities().ProvesNeverStarted {
			t.Fatal("the Process runtime must declare the proof")
		}
		runtimetest.Contract(t, runtimetest.Harness{New: func(t *testing.T) execution.Runtime { return newProcess(t) }})
	})
	t.Run("non-advertising backend passes the contract", func(t *testing.T) {
		runtimetest.Contract(t, runtimetest.Harness{New: func(t *testing.T) execution.Runtime { return unproven{newProcess(t)} }})
	})

	proven := execution.Capabilities{Backend: "b", ProvesNeverStarted: true}
	unprovenCaps := execution.Capabilities{Backend: "b", Guarantees: execution.AllGuarantees}
	for name, c := range map[string]struct {
		caps execution.Capabilities
		ev   execution.StopEvidence
		ok   bool
	}{
		"advertised and affirmative":            {proven, execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted}, true},
		"advertised but unknown":                {proven, execution.StopEvidence{Stopped: true, Launch: execution.LaunchUnknown}, false},
		"advertised but started":                {proven, execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted}, false},
		"unadvertised and unknown":              {unprovenCaps, execution.StopEvidence{Stopped: true, Launch: execution.LaunchUnknown}, true},
		"unadvertised with concrete evidence":   {unprovenCaps, execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted}, true},
		"unadvertised claiming a start":         {unprovenCaps, execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted}, false},
		"unconfirmed stop is never acceptable":  {unprovenCaps, execution.StopEvidence{Launch: execution.LaunchUnknown}, false},
		"unconfirmed stop of a proving backend": {proven, execution.StopEvidence{Launch: execution.LaunchNotStarted}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runtimetest.CheckFreshStop(c.caps, c.ev); (err == nil) != c.ok {
				t.Fatalf("CheckFreshStop = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// Test_runtime_stop_unknown_preserved (T-102): the shared contract never turns
// missing records into affirmative evidence, nor demands it of a backend that
// cannot give it; the proof is never required of a non-advertising backend.
func Test_runtime_stop_unknown_preserved(t *testing.T) {
	ctx := context.Background()
	r := unproven{newProcess(t)}
	s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "true")
	ev, err := r.Stop(ctx, s.Workload(""))
	if err != nil || !ev.Stopped || ev.Launch != execution.LaunchUnknown {
		t.Fatalf("%+v %v", ev, err)
	}
	if err := runtimetest.CheckFreshStop(r.Capabilities(), ev); err != nil {
		t.Fatalf("unknown evidence from a backend without the proof was rejected: %v", err)
	}
	// Unknown evidence is not a pass for a backend that declared the proof.
	if err := runtimetest.CheckFreshStop(newProcess(t).Capabilities(), ev); err == nil {
		t.Fatal("an advertising backend passed with LaunchUnknown")
	}
}

// Test_runtime_stop_no_relaunch (T-102): stopping before the start fences every
// later launch whether or not the backend advertises the proof.
func Test_runtime_stop_no_relaunch(t *testing.T) {
	ctx := context.Background()
	for name, rt := range map[string]execution.Runtime{"advertising": newProcess(t), "non-advertising": unproven{newProcess(t)}} {
		t.Run(name, func(t *testing.T) {
			s := runtimetest.Spec(t, runtimetest.NewAttemptID(t), "", "exec sleep 60")
			if ev, err := rt.Stop(ctx, s.Workload("")); err != nil || !ev.Stopped {
				t.Fatalf("%+v %v", ev, err)
			}
			if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("a workload stopped first was started: %v", err)
			}
			if err := rt.Remove(ctx, s.Workload("")); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
				t.Fatalf("a removed workload was started: %v", err)
			}
		})
	}
}
