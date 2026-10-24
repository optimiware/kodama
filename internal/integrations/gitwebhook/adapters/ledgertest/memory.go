// Package ledgertest holds the memory implementation of adapters.Ledger and the
// exported Contract that it and the PostgreSQL ledger both run, so the refusal
// ledger has one behavior to maintain. The memory ledger models decisions
// (first decision wins, success and refusal exclusive, scoped keys, survival
// across a "restart" of the handle) and is development and test only; it models
// no SQL, locking or isolation.
package ledgertest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
)

type key struct {
	scope domain.Scope
	key   string
}

// State is the persistent part of the memory ledger: what survives a restart.
// Several Memory handles over one State model several processes (or one process
// before and after a restart) over one database.
type State struct {
	mu      sync.Mutex
	entries map[key]adapters.Entry
	runs    map[domain.Scope]map[domain.RunID]bool
}

// NewState returns an empty store.
func NewState() *State {
	return &State{entries: map[key]adapters.Entry{}, runs: map[domain.Scope]map[domain.RunID]bool{}}
}

// Runs returns the number of runs created in the scope.
func (s *State) Runs(scope domain.Scope) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs[scope])
}

// Memory is an adapters.Ledger over a State.
type Memory struct {
	state *State
	// AdmitErr, when set, is consulted by Admit for an undecided key before
	// anything is created; a non-nil result is returned and nothing is recorded,
	// as a rolled-back run admission would.
	AdmitErr func(plan *execution.AdmissionPlan) error
}

var _ adapters.Ledger = (*Memory)(nil)

// NewMemory returns a handle over state.
func NewMemory(state *State) *Memory { return &Memory{state: state} }

// Lookup implements adapters.Ledger.
func (m *Memory) Lookup(_ context.Context, scope domain.Scope, k string) (adapters.Entry, bool, error) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	e, ok := m.state.entries[key{scope, k}]
	return e, ok, nil
}

// Admit implements adapters.Ledger.
func (m *Memory) Admit(_ context.Context, scope domain.Scope, k string, plan *execution.AdmissionPlan) (adapters.Admission, error) {
	if plan == nil || plan.Aggregate == nil || plan.Aggregate.Run == nil || plan.Aggregate.Run.Scope != scope {
		return adapters.Admission{}, fmt.Errorf("%w: the admission plan is not for this scope", gw.ErrInvalid)
	}
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if e, ok := m.state.entries[key{scope, k}]; ok {
		if e.RunID == "" {
			return adapters.Admission{}, gw.ErrAdmissionClosed
		}
		return adapters.Admission{RunID: e.RunID}, nil
	}
	if m.AdmitErr != nil {
		if err := m.AdmitErr(plan); err != nil {
			return adapters.Admission{}, err
		}
	}
	id := plan.Aggregate.Run.ID
	if m.state.runs[scope] == nil {
		m.state.runs[scope] = map[domain.RunID]bool{}
	}
	m.state.runs[scope][id] = true
	m.state.entries[key{scope, k}] = adapters.Entry{RunID: id}
	return adapters.Admission{RunID: id, Created: true}, nil
}

// Decide implements adapters.Ledger.
func (m *Memory) Decide(_ context.Context, scope domain.Scope, k, reason string) (adapters.Entry, error) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if e, ok := m.state.entries[key{scope, k}]; ok {
		return e, nil
	}
	e := adapters.Entry{Refusal: reason}
	m.state.entries[key{scope, k}] = e
	return e, nil
}

// MemEnv is the memory implementation of Env.
type MemEnv struct {
	state *State
	mu    sync.Mutex
	n     int
	// refuse marks the plans RefusedPlan returned.
	refuse map[domain.RunID]bool
}

var _ Env = (*MemEnv)(nil)

// Scopes of the memory environment.
var (
	MemScopeA = domain.Scope{TenantID: "t100_acme", ProjectID: "p100_web"}
	MemScopeB = domain.Scope{TenantID: "t100_globex", ProjectID: "p100_ops"}
)

// NewMemEnv returns an empty memory environment.
func NewMemEnv(testing.TB) Env { return newMemEnv() }

func newMemEnv() *MemEnv {
	return &MemEnv{state: NewState(), refuse: map[domain.RunID]bool{}}
}

func (e *MemEnv) handle() *Memory {
	m := NewMemory(e.state)
	m.AdmitErr = func(plan *execution.AdmissionPlan) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.refuse[plan.Aggregate.Run.ID] {
			return execution.Refusedf("the definition no longer admits")
		}
		return nil
	}
	return m
}

// Ledger implements Env.
func (e *MemEnv) Ledger() adapters.Ledger { return e.handle() }

// Restart implements Env: a new handle; the State is what survives.
func (e *MemEnv) Restart() adapters.Ledger { return e.handle() }

func (e *MemEnv) plan(scope domain.Scope, refuse bool) *execution.AdmissionPlan {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	id := domain.RunID(fmt.Sprintf("run_mem_%d", e.n))
	if refuse {
		e.refuse[id] = true
	}
	return &execution.AdmissionPlan{Aggregate: &execution.Aggregate{Run: &execution.Run{Scope: scope, ID: id}}}
}

// Plan implements Env.
func (e *MemEnv) Plan(_ testing.TB, scope domain.Scope) *execution.AdmissionPlan {
	return e.plan(scope, false)
}

// RefusedPlan implements Env.
func (e *MemEnv) RefusedPlan(_ testing.TB, scope domain.Scope) *execution.AdmissionPlan {
	return e.plan(scope, true)
}

// Runs implements Env.
func (e *MemEnv) Runs(_ testing.TB, scope domain.Scope) int { return e.state.Runs(scope) }

// ScopeA implements Env.
func (e *MemEnv) ScopeA() domain.Scope { return MemScopeA }

// ScopeB implements Env.
func (e *MemEnv) ScopeB() domain.Scope { return MemScopeB }
