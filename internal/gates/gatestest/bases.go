package gatestest

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

// FakeBases is the controllable default-branch tip reader of the PostgreSQL
// fixture: the Git service is not part of the gate module's SQL tests, so the
// tips a source-set subject names as integration bases are set by the test.
type FakeBases struct {
	mu   sync.Mutex
	tips map[string]gates.Tip
	err  error
}

var _ gates.Bases = (*FakeBases)(nil)

// NewFakeBases starts with no repository.
func NewFakeBases() *FakeBases { return &FakeBases{tips: map[string]gates.Tip{}} }

// Set records the default branch of a repository.
func (b *FakeBases) Set(repository, ref, commit string) {
	b.mu.Lock()
	b.tips[repository] = gates.Tip{Ref: ref, Commit: commit}
	b.mu.Unlock()
}

// Fail makes every read an outage with err; nil restores service.
func (b *FakeBases) Fail(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

// DefaultTips implements gates.Bases.
func (b *FakeBases) DefaultTips(_ context.Context, _ domain.Scope, repositories []string) (map[string]gates.Tip, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	out := map[string]gates.Tip{}
	for _, r := range repositories {
		t, ok := b.tips[r]
		if !ok {
			return nil, gates.ErrNotFound
		}
		out[r] = t
	}
	return out, nil
}
