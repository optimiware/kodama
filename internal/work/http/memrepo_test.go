package workhttp_test

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// memRepo is an in-memory work.Repository for the contract tests (no external
// services). Guards are answered by the shared facts struct.
type memRepo struct {
	mu      sync.Mutex
	items   map[domain.WorkItemID]work.Item
	history map[domain.WorkItemID][]work.HistoryEntry
	order   []domain.WorkItemID
	facts   *facts
}

// facts are the answers of the transaction-bound guards and the authority.
type facts struct {
	mu           sync.Mutex
	actors       map[string]work.Actor
	grants       map[string]map[work.Permission]bool
	writerBusy   bool
	wipFull      bool
	depsErr      error
	subjectErr   error
	authorityErr error
	children     work.ChildSummary
}

func newFacts() *facts {
	return &facts{actors: map[string]work.Actor{}, grants: map[string]map[work.Permission]bool{}}
}

func (f *facts) person(id string, typ work.ActorType, perms ...work.Permission) {
	f.actors[id] = work.Actor{ID: id, Type: typ}
	f.grants[id] = map[work.Permission]bool{}
	for _, p := range perms {
		f.grants[id][p] = true
	}
}

func (f *facts) ResolveActor(_ context.Context, _ domain.Scope, id string) (work.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authorityErr != nil {
		return work.Actor{}, f.authorityErr
	}
	a, ok := f.actors[id]
	if !ok {
		return work.Actor{}, work.ErrHidden
	}
	return a, nil
}

func (f *facts) Authorize(_ context.Context, a work.Actor, _ domain.Scope, p work.Permission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authorityErr != nil {
		return f.authorityErr
	}
	if !f.grants[a.ID][p] {
		return work.ErrDenied
	}
	return nil
}

func newMemRepo(f *facts) *memRepo {
	return &memRepo{items: map[domain.WorkItemID]work.Item{}, history: map[domain.WorkItemID][]work.HistoryEntry{}, facts: f}
}

// memTx implements work.Tx over facts.
type memTx struct {
	f *facts
	r *memRepo
}

func (t memTx) Authorize(ctx context.Context, a work.Actor, s domain.Scope, p work.Permission) error {
	return t.f.Authorize(ctx, a, s, p)
}
func (t memTx) AdmitActive(context.Context, work.Item) error {
	if t.f.wipFull {
		return fmt.Errorf("%w: 1 of 1 active slots are in use", work.ErrWIPLimit)
	}
	return nil
}
func (t memTx) WriterSettled(context.Context, work.Item) (bool, error) { return !t.f.writerBusy, nil }
func (t memTx) DependenciesSatisfied(context.Context, work.Item) error { return t.f.depsErr }
func (t memTx) VerifySubject(_ context.Context, _ work.Item, c work.ReviewClaim) (work.VerifiedSubject, error) {
	if t.f.subjectErr != nil {
		return work.VerifiedSubject{}, t.f.subjectErr
	}
	return work.VerifiedSubject{CandidateCommit: c.CandidateCommit, Evidence: c.Evidence}, nil
}
func (t memTx) ChildSummary(context.Context, work.Item) (work.ChildSummary, error) {
	return t.f.children, nil
}
func (t memTx) AssigneeEligible(context.Context, domain.Scope, string) error { return nil }
func (t memTx) ParentEligible(context.Context, domain.Scope, domain.WorkItemID, domain.WorkItemID) error {
	return nil
}
func (t memTx) VerifyEvidence(context.Context, domain.Scope, []work.EvidenceRef) error { return nil }

func (m *memRepo) Insert(ctx context.Context, scope domain.Scope, fn work.InsertFunc) (work.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, h, err := fn(ctx, memTx{m.facts, m})
	if err != nil {
		return work.Item{}, err
	}
	m.items[it.ID] = it
	m.history[it.ID] = append(m.history[it.ID], h)
	m.order = append(m.order, it.ID)
	return it, nil
}

func (m *memRepo) Update(ctx context.Context, scope domain.Scope, id domain.WorkItemID, expected domain.Version, _ work.UpdateOptions, fn work.UpdateFunc) (work.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.items[id]
	if !ok || cur.Scope != scope {
		return work.Item{}, fmt.Errorf("%w: the work item was not found", work.ErrNotFound)
	}
	if cur.Version != expected {
		return work.Item{}, fmt.Errorf("%w: at version %d", work.ErrVersionConflict, cur.Version)
	}
	next, h, err := fn(ctx, memTx{m.facts, m}, cur)
	if errors.Is(err, work.ErrNoChange) {
		return cur, nil
	}
	if err != nil {
		return work.Item{}, err
	}
	m.items[id] = next
	m.history[id] = append(m.history[id], h)
	return next, nil
}

func (m *memRepo) Get(_ context.Context, scope domain.Scope, id domain.WorkItemID) (work.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok || it.Scope != scope {
		return work.Item{}, fmt.Errorf("%w: the work item was not found", work.ErrNotFound)
	}
	return it, nil
}

func (m *memRepo) List(_ context.Context, scope domain.Scope, q work.ListQuery) ([]work.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []work.Item
	for _, id := range m.order {
		if it := m.items[id]; it.Scope == scope && len(out) < q.Limit {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *memRepo) History(_ context.Context, scope domain.Scope, id domain.WorkItemID, q work.HistoryQuery) ([]work.HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[id]
	if !ok || it.Scope != scope {
		return nil, fmt.Errorf("%w: the work item was not found", work.ErrNotFound)
	}
	var out []work.HistoryEntry
	for _, h := range m.history[id] {
		if h.Version > q.AfterVersion && len(out) < q.Limit {
			out = append(out, h)
		}
	}
	return out, nil
}
