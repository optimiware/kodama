package runtimetest

import (
	"context"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/execution"
)

// ManualBackend is the backend identity of a Manual runtime.
const ManualBackend = "manual"

// Manual is a runtime that runs nothing. Persistence tests use it, wrapped in
// a Decorator, when they script a workload's observations themselves. Start
// reports no instance, Observe repeats the dispatch confirmation (which
// changes nothing), Remove succeeds and Stop reports the configured evidence:
// by default that the workload still runs. Lifecycle tests use a real runtime
// instead.
type Manual struct {
	mu   sync.Mutex
	caps execution.Capabilities
	stop execution.StopEvidence
}

var _ execution.Runtime = (*Manual)(nil)

// NewManual returns a Manual runtime that claims every guarantee.
func NewManual() *Manual {
	return &Manual{caps: execution.Capabilities{Backend: ManualBackend, Guarantees: execution.AllGuarantees}}
}

// SetStop selects what Stop reports from now on.
func (m *Manual) SetStop(ev execution.StopEvidence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop = ev
}

// SetCapabilities selects what Capabilities reports from now on.
func (m *Manual) SetCapabilities(c execution.Capabilities) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps = c
}

// Start implements execution.Runtime.
func (m *Manual) Start(_ context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	return spec.Workload(""), nil
}

// Observe implements execution.Runtime.
func (m *Manual) Observe(_ context.Context, w execution.Workload) (execution.Observation, error) {
	return execution.Observation{Kind: execution.ObservedDispatched, WorkloadName: w.Name}, nil
}

// Logs implements execution.Runtime.
func (m *Manual) Logs(context.Context, execution.Workload, io.Writer) error { return nil }

// Stop implements execution.Runtime.
func (m *Manual) Stop(context.Context, execution.Workload) (execution.StopEvidence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop, nil
}

// Remove implements execution.Runtime.
func (m *Manual) Remove(context.Context, execution.Workload) error { return nil }

// Capabilities implements execution.Runtime.
func (m *Manual) Capabilities() execution.Capabilities {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps
}
