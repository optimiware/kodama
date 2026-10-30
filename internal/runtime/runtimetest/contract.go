package runtimetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Scope is the scope of contract workloads.
var Scope = domain.Scope{TenantID: "t086_contract", ProjectID: "p086_contract"}

// HostImage is the pinned image reference of specs whose runtime runs a host
// fixture instead; nothing ever pulls it.
const HostImage = "registry.invalid/kodama/host-fixture@sha256:0000000000000000000000000000000000000000000000000000000000000000"

// Spec returns a planned spec for attempt whose workload runs script with
// /bin/sh -c in image ("" selects HostImage).
func Spec(t testing.TB, attempt domain.AttemptID, image, script string) execution.AttemptSpec {
	t.Helper()
	return specOf(t, domain.RunID("run_"+string(attempt)), attempt, image, script)
}

// specOf is Spec for an attempt of an explicit run.
func specOf(t testing.TB, run domain.RunID, attempt domain.AttemptID, image, script string) execution.AttemptSpec {
	t.Helper()
	if image == "" {
		image = HostImage
	}
	step := "step"
	c := runtimev1.Context{
		TenantID: Scope.TenantID, ProjectID: Scope.ProjectID, RunID: run, StepID: step, AttemptID: attempt,
		ActionKey:  execution.ActionKey(run, step),
		Definition: runtimev1.Definition{ID: "pd_contract", Digest: runtimev1.Digest("sha256:" + strings.Repeat("2", 64))},
		Command:    runtimev1.Command{Image: image, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{"KODAMA_CONTRACT": "1"}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
	plan, err := execution.DefaultPlanner{}.Plan(Scope.TenantID, c)
	if err != nil {
		t.Fatal(err)
	}
	return execution.AttemptSpec{
		Scope: Scope, RunID: run, StepID: step, AttemptID: attempt, Ordinal: 1, ActionKey: c.ActionKey,
		WorkloadName: plan.WorkloadName, SpecFingerprint: plan.SpecFingerprint, ContextDigest: plan.ContextDigest, Context: plan.Context,
		Access: execution.AccessNone, Purpose: execution.PurposeWork,
	}
}

// NewAttemptID returns a fresh attempt ID, so concurrent suites sharing an
// engine never collide on workload names.
func NewAttemptID(t testing.TB) domain.AttemptID {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return domain.AttemptID("att_" + hex.EncodeToString(b))
}

// Harness binds a runtime adapter to the contract.
type Harness struct {
	// New returns the runtime under test.
	New func(t *testing.T) execution.Runtime
	// Image runs the contract scripts ("" for host runtimes).
	Image string
	// Timeout bounds each case (default 30s).
	Timeout time.Duration
}

// Contract runs the behavior every execution.Runtime promises against a real
// backend. Runtime calls are the only coordination: a case waits by repeating
// Observe, Logs or Stop, which is how execution itself learns about progress.
func Contract(t *testing.T, h Harness) {
	if h.Timeout == 0 {
		h.Timeout = 30 * time.Second
	}
	run := func(name string, body func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(script string) execution.AttemptSpec)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
			defer cancel()
			rt := h.New(t)
			body(t, ctx, rt, func(script string) execution.AttemptSpec {
				s := Spec(t, NewAttemptID(t), h.Image, script)
				t.Cleanup(func() { _ = rt.Remove(context.Background(), s.Workload("")) })
				return s
			})
		})
	}

	t.Run("capabilities name the backend", func(t *testing.T) {
		if h.New(t).Capabilities().Backend == "" {
			t.Fatal("a runtime has no backend identity")
		}
	})

	run("lifecycle", func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(string) execution.AttemptSpec) {
		s := spec("echo kodama-contract-ok; exec sleep 60")
		w := mustStart(t, ctx, rt, s)
		if again := mustStart(t, ctx, rt, s); again != w {
			t.Fatalf("a repeated start did not adopt the workload: %+v, %+v", again, w)
		}
		obs := await(t, ctx, "the start", func() (execution.Observation, bool) {
			obs, err := rt.Observe(ctx, w)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			return obs, obs.Kind == execution.ObservedStarted
		})
		if obs.WorkloadUID != w.UID || obs.WorkloadName != w.Name || obs.Launch != execution.LaunchStarted {
			t.Fatalf("started: %+v", obs)
		}
		await(t, ctx, "the output", func() (string, bool) {
			var b bytes.Buffer
			if err := rt.Logs(ctx, w, &b); err != nil {
				t.Fatalf("logs: %v", err)
			}
			return b.String(), strings.Contains(b.String(), "kodama-contract-ok")
		})
		ev := stop(t, ctx, rt, w)
		if ev.Launch != execution.LaunchStarted {
			t.Fatalf("stop: %+v", ev)
		}
		if again := stop(t, ctx, rt, w); again != ev {
			t.Fatalf("a repeated stop: %+v, then %+v", ev, again)
		}
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a stopped workload was started again: %v", err)
		}
		for range 2 {
			if err := rt.Remove(ctx, w); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}
		if _, err := rt.Observe(ctx, w); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("observe after removal: %v", err)
		}
	})

	run("a failing workload fails as infrastructure", func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(string) execution.AttemptSpec) {
		w := mustStart(t, ctx, rt, spec("echo kodama-contract-fail >&2; exit 7"))
		obs := await(t, ctx, "the failure", func() (execution.Observation, bool) {
			obs, err := rt.Observe(ctx, w)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			return obs, obs.Kind == execution.ObservedFailed
		})
		if obs.Class != execution.ClassInfrastructure || obs.Reason != execution.ReasonWorkloadFailed || obs.Launch != execution.LaunchStarted {
			t.Fatalf("failed: %+v", obs)
		}
		var b bytes.Buffer
		if err := rt.Logs(ctx, w, &b); err != nil || !strings.Contains(b.String(), "kodama-contract-fail") {
			t.Fatalf("logs %q: %v", b.String(), err)
		}
	})

	run("a stop before the start means it never launches", func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(string) execution.AttemptSpec) {
		s := spec("exec sleep 60")
		ev := stop(t, ctx, rt, s.Workload(""))
		// Only the proof of launch history depends on the backend's declaration;
		// the fence below holds for every backend.
		if err := CheckFreshStop(rt.Capabilities(), ev); err != nil {
			t.Fatalf("stop: %+v: %v", ev, err)
		}
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a workload stopped first was started: %v", err)
		}
	})

	run("another spec or instance is a conflict", func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(string) execution.AttemptSpec) {
		s := spec("exec sleep 60")
		w := mustStart(t, ctx, rt, s)
		other := s
		other.SpecFingerprint = "sha256:" + strings.Repeat("f", 64)
		if _, err := rt.Start(ctx, other); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another spec at the name: %v", err)
		}
		foreign := w
		foreign.UID = "99999999-9999-4999-8999-999999999999"
		if _, err := rt.Observe(ctx, foreign); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("observe another instance: %v", err)
		}
		if _, err := rt.Stop(ctx, foreign); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("stop another instance: %v", err)
		}
		if err := rt.Remove(ctx, foreign); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("remove another instance: %v", err)
		}
		if obs, err := rt.Observe(ctx, w); err != nil || obs.Kind == execution.ObservedFailed {
			t.Fatalf("the workload was disturbed: %+v %v", obs, err)
		}
	})

	run("removal of a workload that never existed", func(t *testing.T, ctx context.Context, rt execution.Runtime, spec func(string) execution.AttemptSpec) {
		s := spec("true")
		if err := rt.Remove(ctx, s.Workload("")); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Start(ctx, s); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a removed workload was started: %v", err)
		}
	})
}

// CheckFreshStop judges the launch evidence of a confirmed stop of a workload
// that was never started. A backend declaring Capabilities.ProvesNeverStarted
// must report LaunchNotStarted. Any other backend may report LaunchNotStarted
// when it holds concrete evidence and LaunchUnknown when missing records cannot
// prove it; it must never claim the command started.
func CheckFreshStop(caps execution.Capabilities, ev execution.StopEvidence) error {
	if !ev.Stopped {
		return errors.New("the stop was not confirmed")
	}
	switch {
	case caps.ProvesNeverStarted && ev.Launch != execution.LaunchNotStarted:
		return fmt.Errorf("the %s backend declares never-started proof but reported %q", caps.Backend, ev.Launch)
	case ev.Launch != execution.LaunchNotStarted && ev.Launch != execution.LaunchUnknown:
		return fmt.Errorf("a workload that was never started reported %q", ev.Launch)
	}
	return nil
}

func mustStart(t *testing.T, ctx context.Context, rt execution.Runtime, s execution.AttemptSpec) execution.Workload {
	t.Helper()
	w, err := rt.Start(ctx, s)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if w.Name != s.WorkloadName || w.Validate() != nil || w.UID == "" {
		t.Fatalf("start returned %+v", w)
	}
	return w
}

// stop repeats Stop until the runtime reports the workload stopped.
func stop(t *testing.T, ctx context.Context, rt execution.Runtime, w execution.Workload) execution.StopEvidence {
	t.Helper()
	return await(t, ctx, "the stop", func() (execution.StopEvidence, bool) {
		ev, err := rt.Stop(ctx, w)
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
		return ev, ev.Stopped
	})
}

// await repeats probe until it reports done. Backends are external
// processes: asking again is the only way to learn about their progress.
func await[T any](t *testing.T, ctx context.Context, what string, probe func() (T, bool)) T {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		v, done := probe()
		if done {
			return v
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v (last %+v)", what, ctx.Err(), v)
		case <-tick.C:
		}
	}
}
