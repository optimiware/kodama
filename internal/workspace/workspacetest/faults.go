package workspacetest

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// FaultPublisher wraps the real publisher and injects the failures a network
// and a remote host produce: an acknowledgment lost after the remote already
// applied the change, and a request that never reached the remote.
type FaultPublisher struct {
	Inner workspace.RefPublisher

	mu sync.Mutex
	// CreateAckLost makes the next n CreateRef calls apply remotely, then fail.
	CreateAckLost int
	// DeleteAckLost does the same for DeleteRef.
	DeleteAckLost int
	// PublishNeverSent makes the next n Publish calls fail before any request.
	PublishNeverSent int
	// AfterPublish runs after each real Publish with its result, so a test can
	// act between the remote update and the control plane's bookkeeping.
	AfterPublish func(err error)
	PublishCalls int
}

func (f *FaultPublisher) Publish(ctx context.Context, repo workspace.RepoRef, req workspace.PublishRequest) error {
	f.mu.Lock()
	f.PublishCalls++
	if f.PublishNeverSent > 0 {
		f.PublishNeverSent--
		f.mu.Unlock()
		return ErrInjected()
	}
	hook := f.AfterPublish
	f.mu.Unlock()
	err := f.Inner.Publish(ctx, repo, req)
	if hook != nil {
		hook(err)
	}
	return err
}

func (f *FaultPublisher) CreateRef(ctx context.Context, repo workspace.RepoRef, ref string, commit workspace.Commit, from string) error {
	err := f.Inner.CreateRef(ctx, repo, ref, commit, from)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && f.CreateAckLost > 0 {
		f.CreateAckLost--
		return ErrInjected()
	}
	return err
}

func (f *FaultPublisher) DeleteRef(ctx context.Context, repo workspace.RepoRef, ref string, expected workspace.Commit) error {
	err := f.Inner.DeleteRef(ctx, repo, ref, expected)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && f.DeleteAckLost > 0 {
		f.DeleteAckLost--
		return ErrInjected()
	}
	return err
}

// FaultStore wraps the real store and fails Update calls on demand, as a
// database outage or crash right after a remote effect would.
type FaultStore struct {
	workspace.Store
	mu       sync.Mutex
	failNext int
	Updates  int
}

// FailNextUpdates makes the next n Update calls fail without running.
func (s *FaultStore) FailNextUpdates(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = n
}

func (s *FaultStore) Update(ctx context.Context, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error) {
	s.mu.Lock()
	s.Updates++
	if s.failNext > 0 {
		s.failNext--
		s.mu.Unlock()
		return nil, ErrInjected()
	}
	s.mu.Unlock()
	return s.Store.Update(ctx, scope, id, load, fn)
}

// Faults is the pair of wrappers installed by WithFaults.
type Faults struct {
	Publisher *FaultPublisher
	Store     *FaultStore
}

// WithFaults installs both wrappers into a Config and returns them.
func WithFaults(cfg *Config) *Faults {
	f := &Faults{Publisher: &FaultPublisher{}, Store: &FaultStore{}}
	cfg.WrapPublisher = func(p workspace.RefPublisher) workspace.RefPublisher { f.Publisher.Inner = p; return f.Publisher }
	cfg.WrapStore = func(s workspace.Store) workspace.Store { f.Store.Store = s; return f.Store }
	return f
}
