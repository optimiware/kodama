// Package gitpushtest holds the test support of the gitpush module: the
// in-memory Store over memdb, fakes of the peer modules and of the worker, the
// real gitauth service wired over the same transaction seam, the exported
// Contract suite every gitpush.Store adapter and the use cases over it must
// pass, and the PostgreSQL environment that runs the same suite. It is
// development- and test-only; see MEMORY.md for what the memory adapter does
// not model.
package gitpushtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/gitauth"
	gitauthmem "github.com/optimiware/kodama/internal/gitauth/memory"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/memdb"
)

// EventAppender is the transaction-local event append of the in-memory event bus
// (eventstest.Mem.Append).
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// MemOptions configure a MemStore.
type MemOptions struct {
	NewID func(prefix string) string
}

type recRec struct{ R gitpush.Record }

type auditRec struct {
	ID           string
	RepositoryID string
	PushID       string
	Action       string
	ActorType    string
	ActorID      string
	Reason       string
	RequestID    string
	Version      int64
	Metadata     map[string]any
}

type headRec struct {
	RepositoryID string
	Head         int64
}

// resolutionRec and resetRec model migration 015's rows.
type resolutionRec struct {
	Key string // push/ref
	gitpush.Resolution
	PushID string
}

type resetRec struct {
	Key          string
	RepositoryID string
	From, To     int64
	By, Reason   string
}

// appliedRec is the persisted apply order of one entry (git_push_entries.applied_seq
// of migration 015): numbered when the entry becomes applied.
type appliedRec struct {
	Key string // push/ref
	Seq int64
}

var applySeq atomic.Int64

var (
	appliedT = memdb.NewTable("git_push_applied", func(r appliedRec) string { return r.Key })
	recordsT = memdb.NewTable("git_push_records",
		func(r recRec) string { return r.R.ID },
		func(r recRec) string { return r.R.RepositoryID + "/" + r.R.ReceiveID })
	auditT = memdb.NewTable("git_push_audit", func(r auditRec) string { return r.ID })
	headsT = memdb.NewTable("git_journal_heads", func(r headRec) string { return r.RepositoryID })

	resolutionsT = memdb.NewTable("git_push_resolutions", func(r resolutionRec) string { return r.Key })
	resetsT      = memdb.NewTable("git_journal_resets", func(r resetRec) string { return r.Key })
)

// MemStore implements gitpush.Store over memdb. Do is one memdb.Update: the
// record, its audit rows, its outbox event and the gitauth grant change commit
// together or not at all.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store  *memdb.Store
	events EventAppender
	opts   MemOptions

	mu    sync.RWMutex
	repos gitpush.Repositories
}

var _ gitpush.Store = (*MemStore)(nil)

// NewMemStore requires a kernel and an event appender.
func NewMemStore(store *memdb.Store, events EventAppender, opts MemOptions) (*MemStore, error) {
	if store == nil || events == nil {
		return nil, fmt.Errorf("%w: a memdb store and an event appender are required", gitpush.ErrNotConfigured)
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

// Do implements gitpush.Store.
func (s *MemStore) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitpush.Tx) error) error {
	ran := false
	err := s.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(ctx, s.Bind(tx)); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		return errors.Join(gitpush.ErrUnavailable, err)
	}
	return err
}

// View implements gitpush.Store.
func (s *MemStore) View(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitpush.Tx) error) error {
	return s.store.View(ctx, tenant, func(tx memdb.Tx) error { return fn(ctx, s.Bind(tx)) })
}

// Bind adapts a memdb.Tx that already carries the tenant to gitpush.Tx.
func (s *MemStore) Bind(tx memdb.Tx) gitpush.Tx { return &memTx{s: s, tx: tx} }

type memTx struct {
	s  *MemStore
	tx memdb.Tx
}

var _ gitpush.Tx = (*memTx)(nil)

// UseRepositories supplies the repository facts Tx.RepositoryState answers
// from. Memory has no repositories table; the read is therefore not held against
// concurrent change (see MEMORY.md), only the SQL adapter proves that.
func (s *MemStore) UseRepositories(r gitpush.Repositories) {
	s.mu.Lock()
	s.repos = r
	s.mu.Unlock()
}

func (t *memTx) RepositoryState(ctx context.Context, repositoryID string) (gitpush.RepositoryState, error) {
	t.s.mu.RLock()
	repos := t.s.repos
	t.s.mu.RUnlock()
	if repos == nil {
		return gitpush.RepositoryState{}, fmt.Errorf("%w: no repository source", gitpush.ErrNotConfigured)
	}
	f, err := repos.Repository(ctx, t.tx.Tenant(), repositoryID)
	if err != nil {
		return gitpush.RepositoryState{}, err
	}
	return f.State(), nil
}

func (t *memTx) Grants() gitauth.GrantTx { return gitauthmem.Bind(t.tx) }

func (t *memTx) scan(keep func(*gitpush.Record) bool) []*gitpush.Record {
	var out []*gitpush.Record
	recordsT.Scan(t.tx, func(r recRec) bool {
		if keep(&r.R) {
			out = append(out, r.R.Clone())
		}
		return true
	})
	return out
}

func (t *memTx) ByID(_ context.Context, pushID string, _ bool) (*gitpush.Record, error) {
	r, ok := recordsT.Get(t.tx, pushID)
	if !ok {
		return nil, gitpush.ErrNotFound
	}
	return r.R.Clone(), nil
}

func (t *memTx) ByReceive(_ context.Context, repositoryID, receiveID string) (*gitpush.Record, error) {
	rs := t.scan(func(r *gitpush.Record) bool { return r.RepositoryID == repositoryID && r.ReceiveID == receiveID })
	if len(rs) == 0 {
		return nil, gitpush.ErrNotFound
	}
	return rs[0], nil
}

func (t *memTx) Insert(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	if r.Scope.TenantID != t.tx.Tenant() {
		return gitpush.ErrNotFound
	}
	if _, taken := recordsT.Get(t.tx, r.ID); taken {
		return fmt.Errorf("%w: push id", gitpush.ErrConflict)
	}
	if err := recordsT.Put(t.tx, recRec{R: *r.Clone()}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return errors.Join(gitpush.ErrConflict, err)
		}
		return err
	}
	return t.writeEffects(ctx, r, fx)
}

func (t *memTx) Save(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	cur, ok := recordsT.Get(t.tx, r.ID)
	if !ok {
		return gitpush.ErrNotFound
	}
	if cur.R.Version != r.Version-1 {
		return fmt.Errorf("gitpush/memory: push record %s changed concurrently", r.ID)
	}
	if err := recordsT.Put(t.tx, recRec{R: *r.Clone()}); err != nil {
		return err
	}
	if err := t.numberApplied(r); err != nil {
		return err
	}
	return t.writeEffects(ctx, r, fx)
}

// numberApplied gives every entry that became applied its place in the apply
// order, once (the database does it with a trigger).
func (t *memTx) numberApplied(r *gitpush.Record) error {
	for _, e := range r.Entries {
		if e.State != "applied" {
			continue
		}
		key := r.ID + "/" + e.Ref
		if _, done := appliedT.Get(t.tx, key); done {
			continue
		}
		if err := appliedT.Put(t.tx, appliedRec{Key: key, Seq: applySeq.Add(1)}); err != nil {
			return err
		}
	}
	return nil
}

func (t *memTx) List(_ context.Context, scope domain.Scope, repositoryID string, q gitpush.ListQuery) ([]*gitpush.Record, error) {
	all := t.scan(func(r *gitpush.Record) bool { return r.Scope == scope && r.RepositoryID == repositoryID })
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	var out []*gitpush.Record
	for _, r := range all {
		if q.After != nil && !(r.CreatedAt.Before(q.After.CreatedAt) || (r.CreatedAt.Equal(q.After.CreatedAt) && r.ID < q.After.ID)) {
			continue
		}
		if len(out) >= q.Limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

func (t *memTx) Unsettled(_ context.Context, repositoryID, afterID string, limit int) ([]*gitpush.Record, error) {
	all := t.scan(func(r *gitpush.Record) bool {
		if r.RepositoryID != repositoryID || r.ID <= afterID {
			return false
		}
		for _, e := range r.Entries {
			if e.State.InFlight() {
				return true
			}
		}
		return false
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (t *memTx) HasUnsettled(_ context.Context, scope domain.Scope, repositoryID, exceptReceive string) (bool, error) {
	return len(t.scan(func(r *gitpush.Record) bool {
		return r.Scope == scope && r.RepositoryID == repositoryID && r.ReceiveID != exceptReceive && r.Unsettled()
	})) > 0, nil
}

func (t *memTx) UnsettledOnRef(_ context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	return len(t.scan(func(r *gitpush.Record) bool {
		if r.Scope != scope || r.RepositoryID != repositoryID {
			return false
		}
		for _, e := range r.Entries {
			if e.Ref == ref && (e.State.InFlight() || e.State == "unknown") {
				return true
			}
		}
		return false
	})) > 0, nil
}

func (t *memTx) AppliedOnRef(_ context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]*gitpush.Record, error) {
	all := t.scan(func(r *gitpush.Record) bool {
		if r.Scope != scope || r.RepositoryID != repositoryID {
			return false
		}
		for _, e := range r.Entries {
			if e.Ref == ref && e.State == "applied" {
				return true
			}
		}
		return false
	})
	seq := func(r *gitpush.Record) int64 {
		a, _ := appliedT.Get(t.tx, r.ID+"/"+ref)
		return a.Seq
	}
	sort.Slice(all, func(i, j int) bool {
		if si, sj := seq(all[i]), seq(all[j]); si != sj {
			return si > sj
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (t *memTx) HasUnknownEntry(_ context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	return len(t.scan(func(r *gitpush.Record) bool {
		if r.Scope != scope || r.RepositoryID != repositoryID {
			return false
		}
		for _, e := range r.Entries {
			if e.Ref == ref && e.State == "unknown" {
				return true
			}
		}
		return false
	})) > 0, nil
}

func (t *memTx) EmptyReportPending(_ context.Context, repositoryID string, limit int) ([]*gitpush.Record, error) {
	all := t.scan(func(r *gitpush.Record) bool { return r.RepositoryID == repositoryID && r.EmptyReportPending })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (t *memTx) ClearEmptyReport(_ context.Context, pushID string) error {
	r, ok := recordsT.Get(t.tx, pushID)
	if !ok {
		return gitpush.ErrNotFound
	}
	r.R.EmptyReportPending = false
	return recordsT.Put(t.tx, recRec{R: *r.R.Clone()})
}

func (t *memTx) KnownHead(_ context.Context, repositoryID string) (int64, error) {
	h, _ := headsT.Get(t.tx, repositoryID)
	return h.Head, nil
}

func (t *memTx) NoteHead(_ context.Context, repositoryID string, head int64, _ time.Time) error {
	h, _ := headsT.Get(t.tx, repositoryID)
	if head <= h.Head && h.RepositoryID != "" {
		return nil
	}
	return headsT.Put(t.tx, headRec{RepositoryID: repositoryID, Head: max(head, h.Head)})
}

func (t *memTx) EntryCounts(_ context.Context, repositoryID string) (gitpush.EntryCounts, error) {
	var c gitpush.EntryCounts
	for _, r := range t.scan(func(r *gitpush.Record) bool { return r.RepositoryID == repositoryID }) {
		for _, e := range r.Entries {
			switch {
			case e.State.InFlight():
				c.InFlight++
			case e.State == "unknown":
				c.Unknown++
			}
		}
	}
	return c, nil
}

func (t *memTx) ResetHead(_ context.Context, scope domain.Scope, repositoryID string, head int64, reset gitpush.HeadReset) error {
	if scope.TenantID != t.tx.Tenant() {
		return gitpush.ErrNotFound
	}
	h, _ := headsT.Get(t.tx, repositoryID)
	if head >= h.Head {
		return fmt.Errorf("gitpush/memory: a head reset lowers the known head (%d >= %d)", head, h.Head)
	}
	key := fmt.Sprintf("%s/%d/%d/%d", repositoryID, h.Head, head, reset.At.UnixMicro())
	if err := resetsT.Put(t.tx, resetRec{Key: key, RepositoryID: repositoryID, From: h.Head, To: head, By: reset.By, Reason: reset.Reason}); err != nil {
		return err
	}
	if err := auditT.Put(t.tx, auditRec{ID: gitpush.HeadResetAuditID(scope.TenantID, repositoryID, h.Head, head, reset.At), RepositoryID: repositoryID,
		Action: gitpush.AuditHeadReset, ActorType: "user", ActorID: reset.By, Reason: reset.Reason,
		Metadata: map[string]any{"repositoryId": repositoryID, "from": h.Head, "to": head}}); err != nil {
		return err
	}
	return headsT.Put(t.tx, headRec{RepositoryID: repositoryID, Head: head})
}

func (t *memTx) writeEffects(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	for _, res := range fx.Resolutions {
		if err := resolutionsT.Put(t.tx, resolutionRec{Key: r.ID + "/" + res.Ref, Resolution: res, PushID: r.ID}); err != nil {
			return fmt.Errorf("gitpush: write resolution: %w", err)
		}
	}
	for i, a := range fx.Audit {
		reason, reqID := a.AuditFields(ctx)
		if err := auditT.Put(t.tx, auditRec{ID: gitpush.AuditID(r, i), RepositoryID: r.RepositoryID, PushID: r.ID, Action: a.Action,
			ActorType: a.Actor.Type, ActorID: a.Actor.ID, Reason: reason, RequestID: reqID, Version: r.Version, Metadata: a.Metadata}); err != nil {
			return fmt.Errorf("gitpush: write audit: %w", err)
		}
	}
	if fx.Event {
		env, err := r.Envelope(gitpush.RequestID(ctx), t.s.opts.NewID)
		if err != nil {
			return err
		}
		if _, err := t.s.events.Append(t.tx, env); err != nil {
			return fmt.Errorf("gitpush: append event %s: %w", env.Type, err)
		}
	}
	return nil
}

// Resolutions lists the operator resolutions recorded for one push record.
func (s *MemStore) Resolutions(ctx context.Context, tenant domain.TenantID, pushID string) ([]gitpush.Resolution, error) {
	var out []gitpush.Resolution
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		resolutionsT.Scan(tx, func(r resolutionRec) bool {
			if r.PushID == pushID {
				out = append(out, r.Resolution)
			}
			return true
		})
		return nil
	})
	return out, err
}

// AuditActions lists the recorded audit actions of one push record, sorted.
func (s *MemStore) AuditActions(ctx context.Context, tenant domain.TenantID, pushID string) ([]string, error) {
	var out []string
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool {
			if r.PushID == pushID {
				out = append(out, r.Action)
			}
			return true
		})
		return nil
	})
	sort.Strings(out)
	return out, err
}
