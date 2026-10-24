package rtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type attemptRec struct{ State runtimeauth.AttemptState }

type grantRec struct{ G runtimeauth.AdmissionGrant }

var (
	attemptsT = memdb.NewTable("runtime_attempts", func(r attemptRec) string { return string(r.State.AttemptID) })
	// One grant per attempt; the credential digest and the session are unique.
	grantsT = memdb.NewTable("runtime_grants",
		func(r grantRec) string { return string(r.G.AttemptID) },
		func(r grantRec) string { return r.G.CredentialDigest.Hex() },
		func(r grantRec) string {
			if r.G.Consumption == nil {
				return ""
			}
			return string(r.G.Consumption.SessionID)
		})
)

// MemAttempts is the in-memory stand-in for the execution owner's attempt
// authority over memdb: the authoritative attempt rows, the locking read the
// grant transactions use (LockAttempt) and the owner-side cancellation, which
// revokes the attempt's grants in the same Tx.
type MemAttempts struct{ store *memdb.Store }

var _ runtimeauth.AttemptObserver = (*MemAttempts)(nil)

// NewMemAttempts returns an authority over store.
func NewMemAttempts(store *memdb.Store) *MemAttempts { return &MemAttempts{store: store} }

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemAttempts) DevelopmentOnly() bool { return true }

// Seed stores an active attempt at revision 1.
func (a *MemAttempts) Seed(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, job runtimeauth.ExpectedJob, digest runtimeauth.Digest, actions runtimeauth.ActionSet) error {
	return a.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		return attemptsT.Put(tx, attemptRec{State: runtimeauth.AttemptState{
			Scope: scope, RunID: run, AttemptID: attempt, Phase: runtimeauth.AttemptActive, Revision: 1, Job: job, ContextDigest: digest, Actions: actions,
		}})
	})
}

// Modify changes an attempt row as the execution owner would (for example a
// revision bump with narrower actions).
func (a *MemAttempts) Modify(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, fn func(*runtimeauth.AttemptState)) error {
	return a.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := attemptsT.Get(tx, string(attempt))
		if !ok || r.State.Scope != scope {
			return runtimeauth.ErrNotFound
		}
		fn(&r.State)
		return attemptsT.Put(tx, r)
	})
}

func state(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	r, ok := attemptsT.Get(tx, string(attempt))
	if !ok || r.State.Scope != scope {
		return runtimeauth.AttemptState{}, runtimeauth.ErrNotFound
	}
	return r.State, nil
}

// ObserveAttempt implements runtimeauth.AttemptObserver: an advisory read.
func (a *MemAttempts) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	var st runtimeauth.AttemptState
	err := a.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		st, err = state(tx, scope, attempt)
		return err
	})
	return st, err
}

// LockAttempt re-reads the authoritative attempt state in the caller's Tx.
// Serialized writers make a decision made under it atomic with the caller's
// writes: a cancellation commits before or after the whole Update.
func (a *MemAttempts) LockAttempt(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	return state(tx, scope, attempt)
}

// Cancel is the execution owner's cancellation: mark the attempt cancelled and,
// when revoke is true, revoke the attempt's grants through the module's Tx seam
// in the same Update. With revoke false it models an owner that only changed
// state, proving the currentness checks do not depend on revocation having run.
func (a *MemAttempts) Cancel(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, revoke bool, now time.Time) error {
	return a.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := attemptsT.Get(tx, string(attempt))
		if !ok || r.State.Scope != scope {
			return runtimeauth.ErrNotFound
		}
		r.State.Phase = runtimeauth.AttemptCancelled
		if err := attemptsT.Put(tx, r); err != nil {
			return err
		}
		if revoke {
			_, err := RevokeAttemptMem(tx, scope, attempt, now)
			return err
		}
		return nil
	})
}

// MemStore implements runtimeauth.Store over memdb. Do is one memdb.Update:
// everything a transaction wrote commits or is discarded together, and the
// grant transaction reads the attempt authority through the same Tx.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store     *memdb.Store
	authority *MemAttempts

	mu      sync.Mutex
	digests map[runtimeauth.Digest]runtimeauth.Locator
}

var _ runtimeauth.Store = (*MemStore)(nil)

// NewMemStore requires both dependencies; there is no default authority.
func NewMemStore(store *memdb.Store, authority *MemAttempts) (*MemStore, error) {
	if store == nil || authority == nil {
		return nil, errors.New("rtest: a memdb store and an attempt authority are required")
	}
	return &MemStore{store: store, authority: authority, digests: map[runtimeauth.Digest]runtimeauth.Locator{}}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

func errNoGrant() error {
	return errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no such credential"))
}

// Locate resolves a credential digest to its owner. The ownership is read back
// from the owning tenant, so a registration that was rolled back is unknown.
func (s *MemStore) Locate(ctx context.Context, d runtimeauth.Digest) (runtimeauth.Locator, error) {
	s.mu.Lock()
	loc, ok := s.digests[d]
	s.mu.Unlock()
	if !ok {
		return runtimeauth.Locator{}, errNoGrant()
	}
	found := false
	if err := s.store.View(ctx, loc.Scope.TenantID, func(tx memdb.Tx) error {
		r, ok := grantsT.Get(tx, string(loc.AttemptID))
		found = ok && r.G.CredentialDigest.Equal(d)
		return nil
	}); err != nil {
		return runtimeauth.Locator{}, fmt.Errorf("runtimeauth/memory: locate: %w", err)
	}
	if !found {
		return runtimeauth.Locator{}, errNoGrant()
	}
	return loc, nil
}

// Do runs fn in one tenant transaction. A commit that fails after fn
// succeeded has an unknown outcome and is reported as ErrUnavailable.
func (s *MemStore) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx runtimeauth.GrantTx) error) error {
	ran := false
	err := s.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(ctx, &memGrantTx{tx: tx, tenant: tenant, s: s, authority: s.authority}); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		return errors.Join(runtimeauth.ErrUnavailable, err)
	}
	return err
}

// BindMem adapts a Tx that already carries the tenant (one opened by
// memdb.Store.Update, or the execution owner's own) to runtimeauth.GrantTx: the
// integration seam, as runtimeauth/postgres.Bind is for a pgx.Tx.
func BindMem(tx memdb.Tx, authority *MemAttempts) runtimeauth.GrantTx {
	return &memGrantTx{tx: tx, tenant: tx.Tenant(), authority: authority}
}

// RevokeAttemptMem revokes every grant of the attempt in tx and reports how
// many were newly revoked, as runtimeauth/postgres.RevokeAttempt does.
func RevokeAttemptMem(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return (&memGrantTx{tx: tx, tenant: tx.Tenant()}).RevokeAttempt(context.Background(), scope, attempt, now)
}

type memGrantTx struct {
	tx        memdb.Tx
	tenant    domain.TenantID
	s         *MemStore
	authority *MemAttempts
}

var _ runtimeauth.GrantTx = (*memGrantTx)(nil)

func (t *memGrantTx) LockAttempt(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	if t.authority == nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, errors.New("no attempt authority is bound"))
	}
	if scope.TenantID != t.tenant {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrNotFound, errors.New("another tenant"))
	}
	return t.authority.LockAttempt(t.tx, scope, attempt)
}

func noGrant() error { return errors.Join(runtimeauth.ErrNotFound, errors.New("no such grant")) }

func (t *memGrantTx) byCredential(d runtimeauth.Digest) (runtimeauth.AdmissionGrant, bool) {
	var out runtimeauth.AdmissionGrant
	found := false
	grantsT.Scan(t.tx, func(r grantRec) bool {
		if r.G.CredentialDigest.Equal(d) {
			out, found = r.G, true
			return false
		}
		return true
	})
	return out, found
}

func (t *memGrantTx) GrantByCredential(_ context.Context, d runtimeauth.Digest, _ runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	g, ok := t.byCredential(d)
	if !ok {
		return runtimeauth.AdmissionGrant{}, noGrant()
	}
	return cloneGrant(g), nil
}

func (t *memGrantTx) GrantBySession(_ context.Context, s runtimeauth.SessionID, _ runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	if err := s.Validate(); err != nil {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("malformed session"))
	}
	var out runtimeauth.AdmissionGrant
	found := false
	grantsT.Scan(t.tx, func(r grantRec) bool {
		if r.G.Consumption != nil && r.G.Consumption.SessionID == s {
			out, found = r.G, true
			return false
		}
		return true
	})
	if !found {
		return runtimeauth.AdmissionGrant{}, noGrant()
	}
	return cloneGrant(out), nil
}

// cloneGrant copies the parts of a grant that hold slices or pointers, so no
// caller shares them with the stored row.
func cloneGrant(g runtimeauth.AdmissionGrant) runtimeauth.AdmissionGrant {
	g.Context = append([]byte(nil), g.Context...)
	if g.Consumption != nil {
		c := *g.Consumption
		g.Consumption = &c
	}
	if g.RevokedAt != nil {
		r := *g.RevokedAt
		g.RevokedAt = &r
	}
	return g
}

func (t *memGrantTx) InsertGrant(ctx context.Context, g runtimeauth.AdmissionGrant) (runtimeauth.AdmissionGrant, bool, error) {
	if g.Scope.TenantID != t.tenant {
		return runtimeauth.AdmissionGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	if existing, ok := grantsT.Get(t.tx, string(g.AttemptID)); ok {
		// The attempt already has a grant; the credential digest check below is
		// the database's unique key for a different attempt only.
		return cloneGrant(existing.G), false, nil
	}
	if err := t.checkCredentialFree(ctx, g); err != nil {
		return runtimeauth.AdmissionGrant{}, false, err
	}
	stored := cloneGrant(g)
	if err := grantsT.Put(t.tx, grantRec{G: stored}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return runtimeauth.AdmissionGrant{}, false, errors.Join(runtimeauth.ErrConflict, errors.New("credential already registered elsewhere"))
		}
		return runtimeauth.AdmissionGrant{}, false, err
	}
	if t.s != nil {
		t.s.mu.Lock()
		t.s.digests[g.CredentialDigest] = runtimeauth.Locator{Scope: g.Scope, AttemptID: g.AttemptID}
		t.s.mu.Unlock()
	}
	return g, true, nil
}

// checkCredentialFree refuses a digest another attempt holds, in this tenant or
// any other (the credential is unique across tenants). A claim whose
// registration was rolled back is stale and does not count.
func (t *memGrantTx) checkCredentialFree(ctx context.Context, g runtimeauth.AdmissionGrant) error {
	if t.s == nil {
		return nil
	}
	t.s.mu.Lock()
	loc, claimed := t.s.digests[g.CredentialDigest]
	t.s.mu.Unlock()
	if !claimed || (loc.Scope.TenantID == g.Scope.TenantID && loc.AttemptID == g.AttemptID) {
		return nil
	}
	held := false
	if loc.Scope.TenantID == t.tenant {
		r, ok := grantsT.Get(t.tx, string(loc.AttemptID))
		held = ok && r.G.CredentialDigest.Equal(g.CredentialDigest)
	} else if err := t.s.store.View(ctx, loc.Scope.TenantID, func(tx memdb.Tx) error {
		r, ok := grantsT.Get(tx, string(loc.AttemptID))
		held = ok && r.G.CredentialDigest.Equal(g.CredentialDigest)
		return nil
	}); err != nil {
		return err
	}
	if held {
		return errors.Join(runtimeauth.ErrConflict, errors.New("credential already registered elsewhere"))
	}
	return nil
}

func (t *memGrantTx) SaveConsumption(_ context.Context, g runtimeauth.AdmissionGrant) error {
	c := g.Consumption
	if c == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no consumption to save"))
	}
	r, ok := grantsT.Get(t.tx, string(g.AttemptID))
	if !ok || r.G.Consumption != nil || r.G.RevokedAt != nil {
		return errors.Join(runtimeauth.ErrConflict, errors.New("the grant is not consumable"))
	}
	cc := *c
	r.G.Consumption = &cc
	if err := grantsT.Put(t.tx, r); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return errors.Join(runtimeauth.ErrConflict, errors.New("the session is already bound"))
		}
		return err
	}
	return nil
}

func (t *memGrantTx) SaveRevocation(_ context.Context, g runtimeauth.AdmissionGrant) error {
	if g.RevokedAt == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no revocation to save"))
	}
	r, ok := grantsT.Get(t.tx, string(g.AttemptID))
	if !ok || r.G.RevokedAt != nil {
		return nil // an existing revocation is irreversible and keeps its time
	}
	rv := *g.RevokedAt
	r.G.RevokedAt = &rv
	return grantsT.Put(t.tx, r)
}

func (t *memGrantTx) RevokeAttempt(_ context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	if scope.TenantID != t.tenant {
		return 0, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	r, ok := grantsT.Get(t.tx, string(attempt))
	if !ok || r.G.Scope != scope || r.G.RevokedAt != nil {
		return 0, nil
	}
	at := now.UTC().Truncate(time.Microsecond)
	if at.Before(r.G.CreatedAt) {
		at = r.G.CreatedAt
	}
	r.G.RevokedAt = &at
	if err := grantsT.Put(t.tx, r); err != nil {
		return 0, err
	}
	return 1, nil
}
