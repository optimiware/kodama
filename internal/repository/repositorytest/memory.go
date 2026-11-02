// Package repositorytest holds the test support of the repository module: the
// in-memory Store over memdb, fakes of the worker and of the authority, and the
// shared Contract suite that every repository.Store adapter and the use cases
// over it must pass. It is development- and test-only; see MEMORY.md for what
// the memory adapter does not model.
package repositorytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/repository"
)

// EventAppender is the transaction-local event append of the in-memory event bus
// (eventstest.Mem.Append), the counterpart of the PostgreSQL adapter's
// pgx-based appender.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// MemOptions configure a MemStore; the zero value uses the wall clock and
// random identifiers.
type MemOptions struct {
	Clock func() time.Time
	NewID func(prefix string) string
}

type repoRec struct{ R repository.Repository }

type auditRec struct {
	ID           string
	RepositoryID repository.ID
	Action       string
	ActorType    string
	ActorID      string
	Reason       string
	RequestID    string
	Version      domain.Version
}

var (
	repositoriesT = memdb.NewTable("repositories",
		func(r repoRec) string { return string(r.R.ID) },
		func(r repoRec) string { return string(r.R.Scope.ProjectID) + "/" + r.R.Name })
	auditT = memdb.NewTable("repository_audit", func(r auditRec) string { return r.ID })
)

// MemStore implements repository.Store over memdb. Insert and Update are one
// memdb.Update: the repository, its audit entries and its outbox events commit
// together or not at all, and the mutation runs on a private copy, so an error
// leaves every record unchanged.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store  *memdb.Store
	events EventAppender
	opts   MemOptions
}

var _ repository.Store = (*MemStore)(nil)

// NewMemStore requires a kernel and an event appender: a repository transition
// without its audit and outbox records would be unobservable.
func NewMemStore(store *memdb.Store, events EventAppender, opts MemOptions) (*MemStore, error) {
	if store == nil || events == nil {
		return nil, fmt.Errorf("%w: a memdb store and an event appender are required", repository.ErrNotConfigured)
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &MemStore{store: store, events: events, opts: opts}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

func (s *MemStore) now() time.Time { return s.opts.Clock().UTC().Truncate(time.Microsecond) }

func load(tx memdb.Tx, scope domain.Scope, id repository.ID) (*repository.Repository, error) {
	r, ok := repositoriesT.Get(tx, string(id))
	if !ok || r.R.Scope != scope {
		return nil, repository.ErrNotFound
	}
	return r.R.Clone(), nil
}

// Insert implements repository.Store.
func (s *MemStore) Insert(ctx context.Context, r *repository.Repository, fx repository.Effects) (*repository.Repository, bool, error) {
	var (
		out     *repository.Repository
		created bool
	)
	err := s.store.Update(ctx, r.Scope.TenantID, func(tx memdb.Tx) (err error) {
		out, created, err = s.InsertInTx(ctx, tx, r, fx)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// InsertInTx is Insert inside a caller-owned memdb.Tx.
func (s *MemStore) InsertInTx(ctx context.Context, tx memdb.Tx, r *repository.Repository, fx repository.Effects) (*repository.Repository, bool, error) {
	if err := r.Validate(); err != nil {
		return nil, false, err
	}
	if tx.Tenant() != r.Scope.TenantID {
		return nil, false, repository.ErrNotFound
	}
	var existing *repository.Repository
	repositoriesT.Scan(tx, func(x repoRec) bool {
		if x.R.Scope == r.Scope && x.R.Name == r.Name {
			existing = x.R.Clone()
			return false
		}
		return true
	})
	if existing != nil {
		return existing, false, nil
	}
	if _, taken := repositoriesT.Get(tx, string(r.ID)); taken {
		// Put replaces a row with the same key: refuse before it can.
		return nil, false, fmt.Errorf("%w: repository identifier %s is already in use", domain.ErrConflict, r.ID)
	}
	stored := r.Clone()
	if err := repositoriesT.Put(tx, repoRec{R: *stored}); err != nil {
		return nil, false, err
	}
	if err := s.writeEffects(ctx, tx, stored, fx); err != nil {
		return nil, false, err
	}
	return stored.Clone(), true, nil
}

// Get implements repository.Store.
func (s *MemStore) Get(ctx context.Context, scope domain.Scope, id repository.ID) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = load(tx, scope, id)
		return err
	})
	return out, err
}

// GetInTenant implements repository.Store.
func (s *MemStore) GetInTenant(ctx context.Context, tenant domain.TenantID, id repository.ID) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		if r, ok := repositoriesT.Get(tx, string(id)); ok {
			out = r.R.Clone()
		}
		return nil
	})
	if err == nil && out == nil {
		err = repository.ErrNotFound
	}
	return out, err
}

// GetByName implements repository.Store.
func (s *MemStore) GetByName(ctx context.Context, scope domain.Scope, name string) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		repositoriesT.Scan(tx, func(r repoRec) bool {
			if r.R.Scope == scope && r.R.Name == name {
				out = r.R.Clone()
				return false
			}
			return true
		})
		return nil
	})
	if err == nil && out == nil {
		err = repository.ErrNotFound
	}
	return out, err
}

// List implements repository.Store.
func (s *MemStore) List(ctx context.Context, scope domain.Scope, q repository.ListQuery) ([]*repository.Repository, error) {
	var all []*repository.Repository
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		repositoriesT.Scan(tx, func(r repoRec) bool {
			if r.R.Scope == scope {
				all = append(all, r.R.Clone())
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	var out []*repository.Repository
	for _, r := range all {
		if q.After != nil && !(r.CreatedAt.After(q.After.CreatedAt) || (r.CreatedAt.Equal(q.After.CreatedAt) && r.ID > q.After.ID)) {
			continue
		}
		if len(out) >= q.Limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

// Update implements repository.Store.
func (s *MemStore) Update(ctx context.Context, scope domain.Scope, id repository.ID, fn repository.Mutation) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		out, err = s.UpdateInTx(ctx, tx, scope, id, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateInTx is Update inside a caller-owned memdb.Tx, as postgres.UpdateInTx
// takes a pgx.Tx. It never opens an Update of its own.
func (s *MemStore) UpdateInTx(ctx context.Context, tx memdb.Tx, scope domain.Scope, id repository.ID, fn repository.Mutation) (*repository.Repository, error) {
	if tx.Tenant() != scope.TenantID {
		return nil, repository.ErrNotFound
	}
	r, err := load(tx, scope, id)
	if err != nil {
		return nil, err
	}
	before := r.Clone()
	fx, err := fn(r)
	if err != nil {
		return nil, err
	}
	if *r != *before {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		// The guard trigger of the SQL adapter, as code: a regression that
		// steps the version twice fails here too (memory has no trigger).
		if err := repository.CheckChange(before, r); err != nil {
			return nil, err
		}
		if err := repositoriesT.Put(tx, repoRec{R: *r.Clone()}); err != nil {
			return nil, err
		}
	}
	if err := s.writeEffects(ctx, tx, r, fx); err != nil {
		return nil, err
	}
	return r, nil
}

// Provisioning implements repository.Store.
func (s *MemStore) Provisioning(ctx context.Context, tenant domain.TenantID, after repository.ID, limit int) ([]repository.Candidate, error) {
	var out []repository.Candidate
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		repositoriesT.Scan(tx, func(r repoRec) bool {
			if len(out) >= limit {
				return false
			}
			if r.R.Status == gitv1.StatusProvisioning && r.R.ID > after {
				out = append(out, repository.Candidate{Scope: r.R.Scope, ID: r.R.ID})
			}
			return true
		})
		return nil
	})
	return out, err
}

func (s *MemStore) writeEffects(ctx context.Context, tx memdb.Tx, r *repository.Repository, fx repository.Effects) error {
	for _, a := range fx.Audit {
		reason, reqID := a.AuditFields(ctx)
		if err := auditT.Put(tx, auditRec{ID: s.opts.NewID("aud"), RepositoryID: r.ID, Action: a.Action, ActorType: a.Actor.AuditActorType(),
			ActorID: a.Actor.ID, Reason: reason, RequestID: reqID, Version: r.Version}); err != nil {
			return fmt.Errorf("repository: write audit: %w", err)
		}
	}
	for i, e := range fx.Events {
		env, err := e.Envelope(r, i, s.now(), repository.RequestID(ctx), s.opts.NewID)
		if err != nil {
			return err
		}
		if _, err := s.events.Append(tx, env); err != nil {
			return fmt.Errorf("repository: append event %s: %w", e.Kind, err)
		}
	}
	return nil
}

// AuditActions lists the recorded audit actions of one repository, sorted.
func (s *MemStore) AuditActions(ctx context.Context, tenant domain.TenantID, id repository.ID) ([]string, error) {
	var out []string
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool {
			if r.RepositoryID == id {
				out = append(out, r.Action)
			}
			return true
		})
		return nil
	})
	sort.Strings(out)
	return out, err
}
