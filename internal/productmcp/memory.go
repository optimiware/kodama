package productmcp

import (
	"context"
	"sync"
)

// MemoryResults is an in-memory Results for tests and single-process
// development. It does not survive a restart or span replicas, so a production
// composition supplies a durable adapter.
type MemoryResults struct {
	mu sync.Mutex
	m  map[ReplayKey]Recorded
}

// NewMemoryResults returns an empty store.
func NewMemoryResults() *MemoryResults { return &MemoryResults{m: map[ReplayKey]Recorded{}} }

// Lookup implements Results.
func (s *MemoryResults) Lookup(_ context.Context, k ReplayKey) (Recorded, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[k]
	return r, ok, nil
}

// Record implements Results (put-if-absent).
func (s *MemoryResults) Record(_ context.Context, k ReplayKey, r Recorded) (Recorded, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.m[k]; ok {
		return cur, nil
	}
	s.m[k] = r
	return r, nil
}

var _ Results = (*MemoryResults)(nil)
