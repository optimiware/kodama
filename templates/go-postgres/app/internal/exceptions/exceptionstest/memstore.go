// Package exceptionstest provides in-memory doubles of the exceptions ports for
// unit tests. They model the documented semantics; SQL, RLS and locking are
// proven separately against real PostgreSQL.
package exceptionstest

import (
	"context"
	"sort"
	"sync"
	"time"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/tenancy"
)

type intent struct {
	exceptions.Intent
	status    string
	leaseOwn  string
	leaseTill time.Time
	nextAt    time.Time
	stalled   bool
}

type idem struct {
	hash string
	id   exceptions.ID
}

// Store is a concurrency-safe in-memory exceptions.Store.
type Store struct {
	mu      sync.Mutex
	rows    map[tenancy.TenantID]map[exceptions.ID]exceptions.Exception
	intents map[tenancy.TenantID]map[string]*intent
	keys    map[tenancy.TenantID]map[string]idem
	audit   map[tenancy.TenantID][]exceptions.AuditEntry
	seq     int64
	// FailAudit makes every audited write fail, to prove rollback.
	FailAudit bool
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{rows: map[tenancy.TenantID]map[exceptions.ID]exceptions.Exception{}, intents: map[tenancy.TenantID]map[string]*intent{},
		keys: map[tenancy.TenantID]map[string]idem{}, audit: map[tenancy.TenantID][]exceptions.AuditEntry{}}
}

func (s *Store) tenant(t tenancy.TenantID) {
	if s.rows[t] == nil {
		s.rows[t] = map[exceptions.ID]exceptions.Exception{}
		s.intents[t] = map[string]*intent{}
		s.keys[t] = map[string]idem{}
	}
}

func (s *Store) record(t tenancy.TenantID, id exceptions.ID, actor, action, detail string, at time.Time) error {
	if s.FailAudit {
		return exceptions.ErrUnavailable
	}
	s.seq++
	s.audit[t] = append(s.audit[t], exceptions.AuditEntry{Seq: s.seq, ExceptionID: id, Actor: actor, Action: action, Detail: detail, At: at})
	return nil
}

// Create implements exceptions.Store.
func (s *Store) Create(_ context.Context, a tenancy.Access, e exceptions.Exception) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant(a.Tenant())
	if e.Tenant != a.Tenant() {
		return exceptions.ErrInvalid
	}
	if err := s.record(a.Tenant(), e.ID, a.Subject(), "created", "", e.CreatedAt); err != nil {
		return err
	}
	s.rows[a.Tenant()][e.ID] = e
	return nil
}

// Get implements exceptions.Store.
func (s *Store) Get(_ context.Context, a tenancy.Access, id exceptions.ID) (exceptions.Exception, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[a.Tenant()][id]
	if !ok {
		return exceptions.Exception{}, exceptions.ErrNotFound
	}
	return e, nil
}

// List implements exceptions.Store.
func (s *Store) List(_ context.Context, a tenancy.Access, q exceptions.ListQuery) (exceptions.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []exceptions.Exception
	for _, e := range s.rows[a.Tenant()] {
		if q.State == "" || e.State == q.State {
			all = append(all, e)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.Before(all[j].CreatedAt)
		}
		return all[i].ID < all[j].ID
	})
	var out []exceptions.Exception
	for _, e := range all {
		if q.After != nil && !(e.CreatedAt.After(q.After.CreatedAt) || (e.CreatedAt.Equal(q.After.CreatedAt) && e.ID > q.After.ID)) {
			continue
		}
		out = append(out, e)
	}
	page := exceptions.Page{}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		page.Next = &exceptions.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	page.Items = out
	return page, nil
}

// History implements exceptions.Store.
func (s *Store) History(_ context.Context, a tenancy.Access, id exceptions.ID) ([]exceptions.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[a.Tenant()][id]; !ok {
		return nil, exceptions.ErrNotFound
	}
	var out []exceptions.AuditEntry
	for _, e := range s.audit[a.Tenant()] {
		if e.ExceptionID == id {
			out = append(out, e)
		}
	}
	return out, nil
}

// Submit implements exceptions.Store.
func (s *Store) Submit(_ context.Context, a tenancy.Access, id exceptions.ID, req exceptions.ResolveRequest, decide func(*exceptions.Exception) (exceptions.Intent, error)) (exceptions.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := a.Tenant()
	s.tenant(t)
	cur, ok := s.rows[t][id]
	if !ok {
		return exceptions.Receipt{}, exceptions.ErrNotFound
	}
	hash := req.PayloadHash(id)
	if rec, ok := s.keys[t][req.IdempotencyKey]; ok {
		if rec.hash != hash || rec.id != id {
			return exceptions.Receipt{}, exceptions.ErrIdempotencyConflict
		}
		return exceptions.Receipt{Exception: cur, Replayed: true}, nil
	}
	next := cur
	in, err := decide(&next)
	if err != nil {
		return exceptions.Receipt{}, err
	}
	if err := s.record(t, id, a.Subject(), "resolution_requested", "", next.UpdatedAt); err != nil {
		return exceptions.Receipt{}, err
	}
	in.ID = string(id)
	s.rows[t][id] = next
	s.intents[t][in.ID] = &intent{Intent: in, status: "queued"}
	s.keys[t][req.IdempotencyKey] = idem{hash: hash, id: id}
	return exceptions.Receipt{Exception: next}, nil
}

// Claim implements exceptions.Store.
func (s *Store) Claim(_ context.Context, a tenancy.Access, owner string, lease time.Duration, now time.Time, maxAttempts int) (*exceptions.Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id := range s.intents[a.Tenant()] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		in := s.intents[a.Tenant()][id]
		due := in.status == "queued" ||
			(in.status == "dispatching" && now.After(in.leaseTill)) ||
			(in.status == "unknown" && !in.stalled && !now.Before(in.nextAt) && in.Attempts < maxAttempts)
		if !due {
			continue
		}
		reconcile := in.status != "queued"
		in.status, in.leaseOwn, in.leaseTill = "dispatching", owner, now.Add(lease)
		in.Attempts++
		return &exceptions.Claim{Intent: in.Intent, Owner: owner, Reconcile: reconcile}, nil
	}
	return nil, nil
}

// Observe implements exceptions.Store.
func (s *Store) Observe(_ context.Context, a tenancy.Access, o exceptions.Observation) (exceptions.Exception, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := s.intents[a.Tenant()][o.IntentID]
	if in == nil {
		return exceptions.Exception{}, exceptions.ErrNotFound
	}
	if o.Kind == exceptions.ObservedUnknown && (in.status != "dispatching" || in.leaseOwn != o.Owner) {
		return exceptions.Exception{}, exceptions.ErrLeaseLost
	}
	e := s.rows[a.Tenant()][in.ExceptionID]
	if err := exceptions.ApplyObservation(&e, o); err != nil {
		return exceptions.Exception{}, err
	}
	if err := s.record(a.Tenant(), e.ID, a.Subject(), "erp_"+string(o.Kind), o.Detail, o.At); err != nil {
		return exceptions.Exception{}, err
	}
	switch o.Kind {
	case exceptions.ObservedUnknown:
		in.status, in.nextAt, in.stalled = "unknown", o.RetryAt, o.RetryAt.IsZero()
	default:
		in.status = string(o.Kind)
	}
	s.rows[a.Tenant()][e.ID] = e
	return e, nil
}

// Expire makes every outstanding dispatch lease reclaimable, as a crashed
// worker's leases eventually become.
func (s *Store) Expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.intents {
		for _, in := range m {
			in.leaseTill = time.Time{}
		}
	}
}
