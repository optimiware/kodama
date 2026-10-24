// Package runtimetest supports tests of execution.Runtime consumers and
// adapters: a Decorator that records calls and injects faults around a real
// runtime, the Manual runtime for persistence tests that script workload
// observations themselves, and the contract suite every adapter passes.
package runtimetest

import (
	"context"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Op names a Runtime method.
type Op string

// The runtime operations.
const (
	OpStart   Op = "start"
	OpObserve Op = "observe"
	OpLogs    Op = "logs"
	OpStop    Op = "stop"
	OpRemove  Op = "remove"
)

// Call is one recorded runtime call. Spec is set for Start; Workload names
// the workload of every call (for Start, without an instance UID).
type Call struct {
	Op       Op
	Spec     execution.AttemptSpec
	Workload execution.Workload
}

// Hook inspects a call; a non-nil error becomes the call's result.
type Hook func(ctx context.Context, c Call) error

// Decorator wraps a runtime, records every call and injects faults. It has no
// workload behavior of its own: without hooks every call reaches the inner
// runtime and returns its answer.
type Decorator struct {
	inner execution.Runtime

	mu     sync.Mutex
	calls  []Call
	before map[Op]Hook
	after  map[Op]Hook
}

var _ execution.Runtime = (*Decorator)(nil)

// Decorate wraps inner.
func Decorate(inner execution.Runtime) *Decorator {
	return &Decorator{inner: inner, before: map[Op]Hook{}, after: map[Op]Hook{}}
}

// Inner returns the wrapped runtime.
func (d *Decorator) Inner() execution.Runtime { return d.inner }

// Before runs h ahead of every later call of op. Its error is returned without
// reaching the inner runtime: the call had no effect. A nil h removes the hook.
func (d *Decorator) Before(op Op, h Hook) { d.set(d.before, op, h) }

// After runs h behind every later successful call of op. Its error replaces
// the result after the inner runtime acted: the response was lost. A nil h
// removes the hook.
func (d *Decorator) After(op Op, h Hook) { d.set(d.after, op, h) }

func (d *Decorator) set(hooks map[Op]Hook, op Op, h Hook) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h == nil {
		delete(hooks, op)
		return
	}
	hooks[op] = h
}

// Calls returns the recorded calls of op, oldest first.
func (d *Decorator) Calls(op Op) []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Call
	for _, c := range d.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Starts returns the specs of the recorded Start calls, oldest first.
func (d *Decorator) Starts() []execution.AttemptSpec {
	var out []execution.AttemptSpec
	for _, c := range d.Calls(OpStart) {
		out = append(out, c.Spec)
	}
	return out
}

// Start implements execution.Runtime.
func (d *Decorator) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	spec.Context = append([]byte(nil), spec.Context...)
	return around(ctx, d, Call{Op: OpStart, Spec: spec, Workload: spec.Workload("")}, func() (execution.Workload, error) {
		return d.inner.Start(ctx, spec)
	})
}

// Observe implements execution.Runtime.
func (d *Decorator) Observe(ctx context.Context, w execution.Workload) (execution.Observation, error) {
	return around(ctx, d, Call{Op: OpObserve, Workload: w}, func() (execution.Observation, error) {
		return d.inner.Observe(ctx, w)
	})
}

// Logs implements execution.Runtime.
func (d *Decorator) Logs(ctx context.Context, w execution.Workload, dst io.Writer) error {
	_, err := around(ctx, d, Call{Op: OpLogs, Workload: w}, func() (struct{}, error) {
		return struct{}{}, d.inner.Logs(ctx, w, dst)
	})
	return err
}

// Stop implements execution.Runtime.
func (d *Decorator) Stop(ctx context.Context, w execution.Workload) (execution.StopEvidence, error) {
	return around(ctx, d, Call{Op: OpStop, Workload: w}, func() (execution.StopEvidence, error) {
		return d.inner.Stop(ctx, w)
	})
}

// Remove implements execution.Runtime.
func (d *Decorator) Remove(ctx context.Context, w execution.Workload) error {
	_, err := around(ctx, d, Call{Op: OpRemove, Workload: w}, func() (struct{}, error) {
		return struct{}{}, d.inner.Remove(ctx, w)
	})
	return err
}

// Capabilities implements execution.Runtime.
func (d *Decorator) Capabilities() execution.Capabilities { return d.inner.Capabilities() }

// around records c, then runs the before hook, the call and the after hook.
// Hooks run without the lock held, so a hook may block to coordinate a test.
func around[T any](ctx context.Context, d *Decorator, c Call, call func() (T, error)) (T, error) {
	var zero T
	d.mu.Lock()
	d.calls = append(d.calls, c)
	before, after := d.before[c.Op], d.after[c.Op]
	d.mu.Unlock()
	if before != nil {
		if err := before(ctx, c); err != nil {
			return zero, err
		}
	}
	v, err := call()
	if err == nil && after != nil {
		err = after(ctx, c)
	}
	if err != nil {
		return zero, err
	}
	return v, nil
}

// NoRequirements is the admission policy of tests whose runtime runs host
// fixtures: no step requires any guarantee. Production never selects it.
func NoRequirements(pipeline.Step) execution.Guarantees { return 0 }
