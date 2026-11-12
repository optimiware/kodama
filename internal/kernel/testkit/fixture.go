package testkit

import (
	"time"
)

// Fixture wires the deterministic fakes into one isolated Kernel unit subject:
// a fake clock, a persisted store with a State handle, a runtime with stable
// resources, a guarded workspace and a secret-delivery fake.
//
// Fixtures share no mutable state. Restart returns a new State handle over the
// same Store, which is the only thing a controller restart preserves: the
// clock, runtime resources and workspace retention are external to it.
type Fixture struct {
	Clock     *Clock
	Store     *Store
	State     *State
	Runtime   *Runtime
	Workspace *Workspace
	Secrets   *Secrets
}

// NewFixture builds an isolated fixture frozen at now.
func NewFixture(now time.Time) *Fixture {
	clock := NewClock(now)
	store := NewStore()
	runtime := NewRuntime()
	secrets := NewSecrets()
	workspace := NewWorkspace()

	runtime.SetSecrets(secrets)
	workspace.SetGuard(runtime.OwnershipHeld)

	return &Fixture{
		Clock:     clock,
		Store:     store,
		State:     store.Handle(),
		Runtime:   runtime,
		Workspace: workspace,
		Secrets:   secrets,
	}
}

// Restart drops the controller's State handle and returns a fresh one over the
// same persisted store. Runtime resources, workspace retention and the clock
// stay as they are, because a restart does not recreate external effects.
func (f *Fixture) Restart() *State {
	f.State = f.Store.Handle()
	return f.State
}
