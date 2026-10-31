package outbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MemoryStore is the in-process Store of development tests, with real
// transaction semantics: rows and the key/value state a mutation writes through
// MemoryTx commit or roll back together. It is not a deployed adapter.
type MemoryStore struct {
	mu         sync.Mutex
	rows       []*memRow
	state      map[string]string
	seq        int64
	failCommit error
	failMark   error
}

type memRow struct {
	Row
	published bool
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{state: map[string]string{}} }

// DevelopmentOnly flags the store as a development adapter.
func (*MemoryStore) DevelopmentOnly() bool { return true }

// FailCommit makes the next commit fail after fn succeeded (the transaction is
// rolled back); nil clears it.
func (s *MemoryStore) FailCommit(err error) { s.mu.Lock(); s.failCommit = err; s.mu.Unlock() }

// FailMarkPublished makes MarkPublished fail (a crash between the broker's
// acknowledgment and the mark); nil clears it.
func (s *MemoryStore) FailMarkPublished(err error) { s.mu.Lock(); s.failMark = err; s.mu.Unlock() }

// Get reads committed state.
func (s *MemoryStore) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state[key]
	return v, ok
}

// Rows lists committed rows with their published flag, in Seq order.
func (s *MemoryStore) Rows() []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Row, len(s.rows))
	for i, r := range s.rows {
		out[i] = r.Row
	}
	return out
}

// Published reports whether the event is marked published.
func (s *MemoryStore) Published(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			return r.published
		}
	}
	return false
}

// MemoryTx is the Tx of MemoryStore. Put writes the state of the transaction.
type MemoryTx struct {
	s     *MemoryStore
	rows  []*memRow
	state map[string]string
}

// Put stages a state change that commits with the transaction.
func (t *MemoryTx) Put(key, value string) { t.state[key] = value }

// Enqueue implements Tx.
func (t *MemoryTx) Enqueue(_ context.Context, row Row) (bool, error) {
	for _, r := range t.s.rows {
		if r.ID == row.ID {
			return false, nil
		}
	}
	for _, r := range t.rows {
		if r.ID == row.ID {
			return false, nil
		}
	}
	t.rows = append(t.rows, &memRow{Row: row})
	return true, nil
}

// Transact implements Store. Transactions are serialized.
func (s *MemoryStore) Transact(ctx context.Context, fn func(context.Context, Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &MemoryTx{s: s, state: map[string]string{}}
	// fn must not call back into the store: it holds the lock for the whole
	// transaction, as a database serializes conflicting writers.
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if s.failCommit != nil {
		err := s.failCommit
		s.failCommit = nil
		return fmt.Errorf("commit: %w", err)
	}
	for k, v := range tx.state {
		s.state[k] = v
	}
	for _, r := range tx.rows {
		s.seq++
		r.Seq = s.seq
		s.rows = append(s.rows, r)
	}
	return nil
}

// Tenants implements Store.
func (s *MemoryStore) Tenants(context.Context) ([]domain.TenantID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[domain.TenantID]bool{}
	var out []domain.TenantID
	for _, r := range s.rows {
		if !r.published && !seen[r.TenantID] {
			seen[r.TenantID] = true
			out = append(out, r.TenantID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Pending implements Store.
func (s *MemoryStore) Pending(_ context.Context, tenant domain.TenantID, now time.Time, limit int) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Row
	for _, r := range s.rows {
		if r.TenantID == tenant && !r.published && !r.NextAttempt.After(now) {
			due = append(due, r.Row)
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].NextAttempt.Equal(due[j].NextAttempt) {
			return due[i].NextAttempt.Before(due[j].NextAttempt)
		}
		return due[i].Seq < due[j].Seq
	})
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

// Unpublished implements Store.
func (s *MemoryStore) Unpublished(_ context.Context, tenant domain.TenantID, cutoff time.Time, after int64, limit int) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Row
	for _, r := range s.rows {
		if r.TenantID == tenant && !r.published && r.Seq > after && !r.CreatedAt.After(cutoff) {
			out = append(out, r.Row)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *MemoryStore) find(tenant domain.TenantID, id string) *memRow {
	for _, r := range s.rows {
		if r.TenantID == tenant && r.ID == id {
			return r
		}
	}
	return nil
}

// MarkPublished implements Store.
func (s *MemoryStore) MarkPublished(_ context.Context, tenant domain.TenantID, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failMark != nil {
		return s.failMark
	}
	r := s.find(tenant, id)
	if r == nil {
		return errors.New("outbox: no such event")
	}
	r.published = true
	return nil
}

// MarkFailed implements Store.
func (s *MemoryStore) MarkFailed(_ context.Context, tenant domain.TenantID, id string, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(tenant, id)
	if r == nil {
		return errors.New("outbox: no such event")
	}
	r.Attempts++
	r.NextAttempt = next
	return nil
}
