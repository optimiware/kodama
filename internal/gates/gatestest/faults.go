package gatestest

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/gates"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
)

// Faults injects failures at the gate module's seams: the object store, the
// evidence reader, the evidence pins and the event appender. Every fault is
// off until a test turns it on, and each can be turned off again to model
// recovery.
type Faults struct {
	mu sync.Mutex

	objects  error
	evidence error
	// pinAfter fails the pin call numbered pinAfter (1-based) and every later
	// one; 0 disables the fault.
	pinAfter int
	pins     int
	events   error
	// eventTypes restricts the event fault (all gate events when empty).
	eventTypes map[string]bool
}

// FailObjects makes every object-store read fail with err; nil restores it.
func (f *Faults) FailObjects(err error) { f.mu.Lock(); f.objects = err; f.mu.Unlock() }

// FailEvidence makes artifact metadata reads fail with err; nil restores it.
func (f *Faults) FailEvidence(err error) { f.mu.Lock(); f.evidence = err; f.mu.Unlock() }

// FailPinsAfter lets n pins succeed and fails the rest; 0 restores the port.
func (f *Faults) FailPinsAfter(n int) {
	f.mu.Lock()
	if n == 0 {
		f.pinAfter, f.pins = 0, 0
	} else {
		f.pinAfter, f.pins = n+1, 0
	}
	f.mu.Unlock()
}

// FailEvents makes the append of the listed gate event types fail with err
// (every gate event when none is listed); nil restores it.
func (f *Faults) FailEvents(err error, types ...string) {
	f.mu.Lock()
	f.events = err
	f.eventTypes = map[string]bool{}
	for _, t := range types {
		f.eventTypes[t] = true
	}
	f.mu.Unlock()
}

// Objects wraps the object store.
func (f *Faults) Objects(inner artifacts.ObjectStore) artifacts.ObjectStore {
	return &faultObjects{ObjectStore: inner, f: f}
}

// Evidence wraps the evidence reader.
func (f *Faults) Evidence(inner gates.Evidence) gates.Evidence {
	return &faultEvidence{Evidence: inner, f: f}
}

// Pins wraps the pin port.
func (f *Faults) Pins(inner gates.Pins) gates.Pins { return &faultPins{Pins: inner, f: f} }

// Appender wraps the event appender.
func (f *Faults) Appender(inner gatespg.EventAppender) gatespg.EventAppender {
	return &faultAppender{EventAppender: inner, f: f}
}

type faultObjects struct {
	artifacts.ObjectStore
	f *Faults
}

func (o *faultObjects) Open(ctx context.Context, key string) (artifacts.Object, error) {
	o.f.mu.Lock()
	err := o.f.objects
	o.f.mu.Unlock()
	if err != nil {
		return artifacts.Object{}, err
	}
	return o.ObjectStore.Open(ctx, key)
}

type faultEvidence struct {
	gates.Evidence
	f *Faults
}

func (e *faultEvidence) Describe(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (gates.EvidenceMeta, error) {
	e.f.mu.Lock()
	err := e.f.evidence
	e.f.mu.Unlock()
	if err != nil {
		return gates.EvidenceMeta{}, gates.Unavailablef(err, "the artifact store is unavailable")
	}
	return e.Evidence.Describe(ctx, scope, id)
}

type faultPins struct {
	gates.Pins
	f *Faults
}

func (p *faultPins) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, gate gates.ID) error {
	p.f.mu.Lock()
	p.f.pins++
	fail := p.f.pinAfter != 0 && p.f.pins >= p.f.pinAfter
	p.f.mu.Unlock()
	if fail {
		return gates.Unavailablef(nil, "pinning was interrupted")
	}
	return p.Pins.Pin(ctx, scope, id, gate)
}

type faultAppender struct {
	gatespg.EventAppender
	f *Faults
}

func (a *faultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	a.f.mu.Lock()
	err := a.f.events
	match := len(a.f.eventTypes) == 0 || a.f.eventTypes[env.Type]
	a.f.mu.Unlock()
	if err != nil && match {
		return eventspg.AppendResult{}, err
	}
	return a.EventAppender.Append(ctx, tx, env)
}
