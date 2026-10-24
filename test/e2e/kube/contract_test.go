package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Test_kube_runtime_contract runs the shared runtime contract of ADR 0008
// (runtimetest.Contract, the suite the Process and Podman adapters run) against
// the Kubernetes runtime on a real cluster.
//
// The Kubernetes runtime acts only on attempts committed in the execution store
// and only on its own recipe's fingerprint, and the workload it starts is the
// real entrypoint, which admits itself with the runtime API before it runs the
// command. The contract's specs are pure values, so each one is bound, on first
// sight, to a real committed attempt of the same script (a run admitted and
// claimed through the real execution service) and every call is forwarded to
// the real runtime with that attempt. boundRuntime translates identity (the
// contract's attempt, name and instance for the real ones) and performs the
// three duties ADR 0008 leaves to execution above the runtime seam: it binds the
// instance UID after a start (execution.drive), commits the cancellation before
// a workload that was never started is stopped, and settles the attempt before
// its workload is removed. The runtime under test sees real attempts and a real
// cluster; the contract sees the answers of the real runtime.
//
// The runtime API's result route rejects every result: the contract's workloads
// are bare commands whose exit is the runtime's only signal, so a command that
// fails must fail the Pod the way a crashed entrypoint does, not report itself.
//
// The Kubernetes runtime does not advertise ProvesNeverStarted (O-25, ADR 0008):
// a stop of a workload it never started may report LaunchUnknown. The shared
// case still runs, not skipped, and still requires the stop to fence the start.
func Test_kube_runtime_contract(t *testing.T) {
	s := newStack(t)
	if caps := s.runtime.Capabilities(); caps.Backend != kube.Backend || caps.ProvesNeverStarted || caps.Guarantees != execution.AllGuarantees {
		t.Fatalf("the Kubernetes runtime's declarations: %+v", caps)
	}
	s.srv.RejectResults.Store(true)
	s.e.Effects.Set(execution.VerdictSettled, nil)
	runtimetest.Contract(t, runtimetest.Harness{
		New:     func(*testing.T) execution.Runtime { return &boundRuntime{s: s} },
		Timeout: 4 * time.Minute,
	})
}

// boundRuntime forwards the contract's calls to the stack's Kubernetes runtime.
type boundRuntime struct {
	s *stack
}

var (
	bindMu   sync.Mutex
	bindings = map[domain.AttemptID]*binding{}
)

// binding is one contract attempt and the real attempt that stands for it.
type binding struct {
	contract execution.AttemptSpec
	real     execution.AttemptSpec
	run      domain.RunID
	started  bool
	uid      string
}

var _ execution.Runtime = (*boundRuntime)(nil)

// bound returns the real attempt standing for the contract's, creating it
// (admission, then claim: one runnable run at a time, so the claim is the run
// just admitted) on first sight.
func (b *boundRuntime) bound(attempt domain.AttemptID, script string, c execution.AttemptSpec) *binding {
	bindMu.Lock()
	defer bindMu.Unlock()
	if x, ok := bindings[attempt]; ok {
		return x
	}
	s := b.s
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: script, Timeout: 120})
	real := s.claim(executiontest.ScopeA.TenantID)
	if real == nil || real.RunID != run.ID {
		s.t.Fatalf("no runnable attempt for contract attempt %s", attempt)
	}
	x := &binding{contract: c, real: *real, run: run.ID}
	bindings[attempt] = x
	return x
}

// scriptOf extracts the script of a contract spec: /bin/sh -c <script>.
func scriptOf(t testing.TB, spec execution.AttemptSpec) string {
	t.Helper()
	var ctx runtimev1.Context
	if err := json.Unmarshal(spec.Context, &ctx); err != nil {
		t.Fatalf("contract context: %v", err)
	}
	if len(ctx.Command.Args) != 1 {
		t.Fatalf("contract command: %+v", ctx.Command)
	}
	return ctx.Command.Args[0]
}

// forWorkload binds a workload handle that was not preceded by a Start (a stop
// or removal before any start): the script cannot matter, nothing is launched.
func (b *boundRuntime) forWorkload(w execution.Workload) *binding {
	return b.bound(w.AttemptID, "exec sleep 60", execution.AttemptSpec{Scope: w.Scope, AttemptID: w.AttemptID, WorkloadName: w.Name})
}

func (x *binding) realWorkload(w execution.Workload) execution.Workload {
	return execution.Workload{Scope: x.real.Scope, AttemptID: x.real.AttemptID, Name: x.real.WorkloadName, UID: w.UID}
}

// Start implements execution.Runtime.
func (b *boundRuntime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	x := b.bound(spec.AttemptID, scriptOf(b.s.t, spec), spec)
	real := x.real
	if x.contract.SpecFingerprint != "" && spec.SpecFingerprint != x.contract.SpecFingerprint {
		real.SpecFingerprint = spec.SpecFingerprint // another spec at the name
	}
	w, err := b.s.runtime.Start(ctx, real)
	if err != nil {
		return execution.Workload{}, err
	}
	// execution.drive: the confirmation binds the instance the runtime reported.
	if err := b.s.svc.ObserveAttempt(ctx, real.Scope, real.AttemptID, execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: w.Name, WorkloadUID: w.UID, SpecFingerprint: real.SpecFingerprint, ObservedAt: time.Now(),
	}); err != nil {
		return execution.Workload{}, err
	}
	x.started, x.uid = true, w.UID
	return execution.Workload{Scope: spec.Scope, AttemptID: spec.AttemptID, Name: spec.WorkloadName, UID: w.UID}, nil
}

// Observe implements execution.Runtime.
func (b *boundRuntime) Observe(ctx context.Context, w execution.Workload) (execution.Observation, error) {
	x := b.forWorkload(w)
	obs, err := b.s.runtime.Observe(ctx, x.realWorkload(w))
	if err != nil {
		return obs, err
	}
	obs.WorkloadName = w.Name
	if x.contract.SpecFingerprint != "" {
		obs.SpecFingerprint = x.contract.SpecFingerprint
	}
	return obs, nil
}

// Logs implements execution.Runtime.
func (b *boundRuntime) Logs(ctx context.Context, w execution.Workload, dst io.Writer) error {
	return b.s.runtime.Logs(ctx, b.forWorkload(w).realWorkload(w), dst)
}

// Stop implements execution.Runtime. The Kubernetes runtime fences a later
// start through the committed attempt (T-102's own no-relaunch test commits the
// cancellation first), and execution stops a workload only after it committed
// the cancellation: a workload that was never started is cancelled first.
func (b *boundRuntime) Stop(ctx context.Context, w execution.Workload) (execution.StopEvidence, error) {
	x := b.forWorkload(w)
	if !x.started {
		b.cancel(ctx, x)
	}
	return b.s.runtime.Stop(ctx, x.realWorkload(w))
}

// cancel commits the cancellation of the binding's run (idempotent).
func (b *boundRuntime) cancel(ctx context.Context, x *binding) {
	if _, err := b.s.svc.Cancel(ctx, b.s.caller(), x.real.Scope, x.run); err != nil {
		b.s.t.Errorf("cancel: %v", err)
	}
}

// Remove implements execution.Runtime. Execution removes a workload only once
// the attempt is settled: the cancellation is committed and the reconciliation
// passes run until the settlement is.
func (b *boundRuntime) Remove(ctx context.Context, w execution.Workload) error {
	x := b.forWorkload(w)
	if w.UID != "" && x.uid != "" && w.UID != x.uid {
		return b.s.runtime.Remove(ctx, x.realWorkload(w)) // another instance: execution never settles for it
	}
	b.cancel(ctx, x)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, ctl := b.s.attemptOfRun(x.real); ctl.Settled() {
			break
		}
		if _, err := b.s.svc.ReconcileRun(ctx, execution.RunRef{Scope: x.real.Scope, ID: x.run}); err != nil && !errors.Is(err, execution.ErrNotFound) {
			b.s.t.Logf("reconcile: %v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the attempt did not settle")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return b.s.runtime.Remove(ctx, x.realWorkload(w))
}

// Capabilities implements execution.Runtime.
func (b *boundRuntime) Capabilities() execution.Capabilities { return b.s.runtime.Capabilities() }
