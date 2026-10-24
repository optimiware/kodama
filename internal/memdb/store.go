package memdb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/optimiware/kodama/internal/domain"
)

// ErrConflict is returned by Table.Put when a unique key is already held by a
// different row. It is domain.ErrConflict; callers match it with errors.Is.
var ErrConflict = domain.ErrConflict

// ErrReadOnly is returned by Table.Put on a Tx opened by Store.View.
var ErrReadOnly = errors.New("memdb: read-only transaction")

// Option configures a Store.
type Option func(*Store)

// WithCommitFault makes every Update call fn at commit time, after the update
// function has succeeded and before anything is published. A non-nil result
// discards the whole update and is returned (wrapped) from Update. It exists
// for rollback tests.
func WithCommitFault(fn func() error) Option {
	return func(s *Store) { s.fault = fn }
}

// Store is the only holder of in-memory state. The zero value is not usable;
// create one with New.
type Store struct {
	state atomic.Pointer[state]
	// writer is a one-slot semaphore serializing Update. A channel, not a
	// mutex, so a waiting Update honours context cancellation.
	writer chan struct{}
	fault  func() error
}

// New returns an empty Store.
func New(opts ...Option) *Store {
	s := &Store{writer: make(chan struct{}, 1)}
	s.state.Store(&state{})
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Update runs fn over a Tx bound to tenant, on a snapshot that only this call
// can write. It publishes all writes when fn returns nil and the commit
// succeeds, and otherwise leaves every table unchanged. Writers are
// serialized; a call waits for the running Update, or for ctx.
//
// fn must not call Update on the same Store and wait for it: that Update
// queues behind the caller's own and only ends when ctx does. Peers take the
// caller's Tx instead.
func (s *Store) Update(ctx context.Context, tenant domain.TenantID, fn func(Tx) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	select {
	case s.writer <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.writer }()
	if err := ctx.Err(); err != nil {
		return err
	}

	tx := &txState{tenant: tenant, base: s.state.Load(), dirty: map[*tableID]*rows{}, writable: true}
	defer func() { tx.closed = true }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fault != nil {
		if err := s.fault(); err != nil {
			return fmt.Errorf("memdb: commit: %w", err)
		}
	}
	if len(tx.dirty) > 0 {
		s.state.Store(tx.base.with(tenant, tx.dirty))
	}
	return nil
}

// View runs fn over a read-only Tx bound to tenant on the last published
// state. It takes no writer slot, so it never waits for an Update, and fn's
// error is returned unchanged.
func (s *Store) View(ctx context.Context, tenant domain.TenantID, fn func(Tx) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &txState{tenant: tenant, base: s.state.Load()}
	defer func() { tx.closed = true }()
	return fn(tx)
}

// Tx is a transaction bound to one tenant. It is valid only inside the Update
// or View that created it; using it afterwards panics. It cannot be
// implemented outside this package.
type Tx interface {
	// Tenant reports the only tenant whose rows this Tx can reach.
	Tenant() domain.TenantID
	state() *txState
}

type txState struct {
	tenant   domain.TenantID
	base     *state
	dirty    map[*tableID]*rows // tables this Tx wrote, already cloned
	writable bool
	closed   bool
}

func (t *txState) Tenant() domain.TenantID { return t.tenant }
func (t *txState) state() *txState         { return t }

func (t *txState) live(name string) {
	if t.closed {
		panic("memdb: table " + name + " used with a Tx after its Update or View returned")
	}
}

// read returns the rows visible to the Tx for a table, or nil.
func (t *txState) read(id *tableID) *rows {
	if r, ok := t.dirty[id]; ok {
		return r
	}
	return t.base.rows(id, t.tenant)
}

// write returns the Tx's private copy of a table's rows, cloning on first use.
func (t *txState) write(id *tableID) *rows {
	if r, ok := t.dirty[id]; ok {
		return r
	}
	r := t.base.rows(id, t.tenant).clone(id.uniques)
	t.dirty[id] = r
	return r
}

func txOf(tx Tx, name string) *txState {
	if tx == nil {
		panic("memdb: table " + name + " used with a nil Tx")
	}
	t := tx.state()
	t.live(name)
	return t
}

// state is an immutable published snapshot: table -> tenant -> rows.
type state struct {
	tables map[*tableID]map[domain.TenantID]*rows
}

func (s *state) rows(id *tableID, tenant domain.TenantID) *rows {
	if s == nil {
		return nil
	}
	return s.tables[id][tenant]
}

// with returns a new snapshot replacing one tenant's rows in the given tables;
// everything else is shared with s.
func (s *state) with(tenant domain.TenantID, dirty map[*tableID]*rows) *state {
	tables := make(map[*tableID]map[domain.TenantID]*rows, len(s.tables)+len(dirty))
	for id, byTenant := range s.tables {
		tables[id] = byTenant
	}
	for id, r := range dirty {
		byTenant := make(map[domain.TenantID]*rows, len(tables[id])+1)
		for tn, old := range tables[id] {
			byTenant[tn] = old
		}
		if len(r.byKey) == 0 {
			delete(byTenant, tenant)
		} else {
			byTenant[tenant] = r
		}
		tables[id] = byTenant
	}
	return &state{tables: tables}
}

// rows holds one tenant's rows of one table and its unique indexes.
type rows struct {
	byKey  map[string]any
	unique []map[string]string // per unique function: value -> primary key
}

func (r *rows) clone(uniques int) *rows {
	c := &rows{byKey: map[string]any{}, unique: make([]map[string]string, uniques)}
	for i := range c.unique {
		c.unique[i] = map[string]string{}
	}
	if r == nil {
		return c
	}
	for k, v := range r.byKey {
		c.byKey[k] = v
	}
	for i, idx := range r.unique {
		for v, k := range idx {
			c.unique[i][v] = k
		}
	}
	return c
}
