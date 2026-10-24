package kubetest

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Attempts is an in-memory kube.Attempts whose records tests edit (cancel,
// settle) and whose reads can hook in, to place a cancellation exactly between
// two executor steps.
type Attempts struct {
	mu    sync.Mutex
	recs  map[domain.AttemptID]kube.AttemptRecord
	Reads int
	// OnRead runs before the n-th read of an attempt (1-based) outside the lock.
	OnRead func(n int)
	// Err fails every read (an outage).
	Err error
}

var _ kube.Attempts = (*Attempts)(nil)

// NewAttempts builds the store.
func NewAttempts(recs ...kube.AttemptRecord) *Attempts {
	a := &Attempts{recs: map[domain.AttemptID]kube.AttemptRecord{}}
	for _, r := range recs {
		a.recs[r.AttemptID] = r
	}
	return a
}

// Attempt implements kube.Attempts.
func (a *Attempts) Attempt(_ context.Context, scope domain.Scope, id domain.AttemptID) (kube.AttemptRecord, error) {
	a.mu.Lock()
	a.Reads++
	n, hook, err := a.Reads, a.OnRead, a.Err
	a.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err != nil {
		return kube.AttemptRecord{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.recs[id]
	if !ok || r.Scope != scope {
		return kube.AttemptRecord{}, kube.ErrNotFound
	}
	return r, nil
}

// Unsettled implements kube.Attempts.
func (a *Attempts) Unsettled(_ context.Context, tenant domain.TenantID, _ string, _ int) ([]kube.AttemptRecord, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Err != nil {
		return nil, "", a.Err
	}
	var out []kube.AttemptRecord
	for _, r := range a.recs {
		if r.Scope.TenantID == tenant && !r.Settled {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out, "", nil
}

// Edit changes one record.
func (a *Attempts) Edit(id domain.AttemptID, f func(*kube.AttemptRecord)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.recs[id]
	f(&r)
	a.recs[id] = r
}

// Add inserts a record.
func (a *Attempts) Add(r kube.AttemptRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recs[r.AttemptID] = r
}

// Remove deletes a record (an attempt unknown to the store).
func (a *Attempts) Remove(id domain.AttemptID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.recs, id)
}

// Cancel marks the attempt cancelled and revoked, as execution's cancellation
// transaction does before it requests termination.
func (a *Attempts) Cancel(id domain.AttemptID) {
	a.Edit(id, func(r *kube.AttemptRecord) {
		r.Status, r.Live, r.TerminationRequested = execution.AttemptCancelled, false, true
	})
}

// Registrar emulates runtime admission's registration semantics: idempotent for
// the identical credential, ErrConflict for a different one, ErrAttemptRevoked
// after revocation.
type Registrar struct {
	mu      sync.Mutex
	digests map[domain.AttemptID]runtimeauth.Digest
	revoked map[domain.AttemptID]bool
	Calls   int
	// Fail returns an error for the n-th call (1-based) before it is applied;
	// FailAfter applies the registration and then fails (a lost response).
	Fail      func(n int) error
	FailAfter func(n int) error
}

var _ kube.Registrar = (*Registrar)(nil)

// NewRegistrar builds an empty registrar.
func NewRegistrar() *Registrar {
	return &Registrar{digests: map[domain.AttemptID]runtimeauth.Digest{}, revoked: map[domain.AttemptID]bool{}}
}

// Register implements kube.Registrar.
func (r *Registrar) Register(_ context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls++
	if r.Fail != nil {
		if err := r.Fail(r.Calls); err != nil {
			return runtimeauth.Registration{}, err
		}
	}
	if r.revoked[cmd.AttemptID] {
		return runtimeauth.Registration{}, runtimeauth.ErrAttemptRevoked
	}
	d := cmd.Credential.Digest()
	prev, ok := r.digests[cmd.AttemptID]
	if ok && !prev.Equal(d) {
		return runtimeauth.Registration{}, runtimeauth.ErrConflict
	}
	r.digests[cmd.AttemptID] = d
	if r.FailAfter != nil {
		if err := r.FailAfter(r.Calls); err != nil {
			return runtimeauth.Registration{}, err
		}
	}
	return runtimeauth.Registration{Created: !ok}, nil
}

// Digest returns the registered credential digest.
func (r *Registrar) Digest(id domain.AttemptID) (runtimeauth.Digest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.digests[id]
	return d, ok
}

// Revoke makes later registrations fail as for a cancelled attempt.
func (r *Registrar) Revoke(id domain.AttemptID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[id] = true
}

// Sink records the observations reported to execution.
type Sink struct {
	mu  sync.Mutex
	Obs []execution.Observation
	Err error
}

var _ kube.Observations = (*Sink)(nil)

// ObserveAttempt implements kube.Observations.
func (s *Sink) ObserveAttempt(_ context.Context, _ domain.Scope, _ domain.AttemptID, o execution.Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
	}
	s.Obs = append(s.Obs, o)
	return nil
}

// Kinds lists the observation kinds seen.
func (s *Sink) Kinds() []execution.ObservationKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []execution.ObservationKind
	for _, o := range s.Obs {
		out = append(out, o.Kind)
	}
	return out
}

// Tenants is a static tenant list.
type Tenants []domain.TenantID

// TenantIDs implements kube.Tenants.
func (t Tenants) TenantIDs(context.Context) ([]domain.TenantID, error) { return t, nil }

var ErrOutage = errors.New("injected outage")
