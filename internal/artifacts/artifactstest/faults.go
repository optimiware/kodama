package artifactstest

import (
	"context"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

// FaultRepo wraps a repository with injectable behavior around the calls whose
// outcomes the recovery tests exercise. A nil hook delegates unchanged.
type FaultRepo struct {
	artifacts.Repository
	mu sync.Mutex
	// OnBegin, OnCommit and OnReserve receive the call and a function running
	// the real operation. They may run it, skip it, or run it and then fail (a
	// lost response after a durable commit).
	OnBegin   func(cmd artifacts.BeginCompletion, next func() (artifacts.ClaimResult, error)) (artifacts.ClaimResult, error)
	OnCommit  func(cmd artifacts.CommitCommand, next func() (artifacts.Artifact, error)) (artifacts.Artifact, error)
	OnReserve func(cmd artifacts.ReserveCommand, next func() (artifacts.ReserveResult, error)) (artifacts.ReserveResult, error)
	OnGet     func(next func() (artifacts.Artifact, error)) (artifacts.Artifact, error)
}

func (f *FaultRepo) hooks() (b func(artifacts.BeginCompletion, func() (artifacts.ClaimResult, error)) (artifacts.ClaimResult, error),
	c func(artifacts.CommitCommand, func() (artifacts.Artifact, error)) (artifacts.Artifact, error),
	r func(artifacts.ReserveCommand, func() (artifacts.ReserveResult, error)) (artifacts.ReserveResult, error),
	g func(func() (artifacts.Artifact, error)) (artifacts.Artifact, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.OnBegin, f.OnCommit, f.OnReserve, f.OnGet
}

// Set runs fn under the hook lock.
func (f *FaultRepo) Set(fn func(*FaultRepo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *FaultRepo) BeginCompletion(ctx context.Context, cmd artifacts.BeginCompletion) (artifacts.ClaimResult, error) {
	b, _, _, _ := f.hooks()
	next := func() (artifacts.ClaimResult, error) { return f.Repository.BeginCompletion(ctx, cmd) }
	if b == nil {
		return next()
	}
	return b(cmd, next)
}

func (f *FaultRepo) CommitUsable(ctx context.Context, cmd artifacts.CommitCommand) (artifacts.Artifact, error) {
	_, c, _, _ := f.hooks()
	next := func() (artifacts.Artifact, error) { return f.Repository.CommitUsable(ctx, cmd) }
	if c == nil {
		return next()
	}
	return c(cmd, next)
}

func (f *FaultRepo) Reserve(ctx context.Context, cmd artifacts.ReserveCommand) (artifacts.ReserveResult, error) {
	_, _, r, _ := f.hooks()
	next := func() (artifacts.ReserveResult, error) { return f.Repository.Reserve(ctx, cmd) }
	if r == nil {
		return next()
	}
	return r(cmd, next)
}

func (f *FaultRepo) GetForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (artifacts.Artifact, error) {
	_, _, _, g := f.hooks()
	next := func() (artifacts.Artifact, error) { return f.Repository.GetForAttempt(ctx, scope, attempt, id) }
	if g == nil {
		return next()
	}
	return g(next)
}

// FaultObjects wraps an object store with injectable behavior.
type FaultObjects struct {
	artifacts.ObjectStore
	mu       sync.Mutex
	OnOpen   func(key string, next func() (artifacts.Object, error)) (artifacts.Object, error)
	OnPut    func(spec artifacts.PutSpec, next func() error) error
	OnDelete func(key string, next func() error) error
	Puts     []string
	Deletes  []string
}

// Set runs fn under the hook lock.
func (f *FaultObjects) Set(fn func(*FaultObjects)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// PutKeys returns the destinations written so far.
func (f *FaultObjects) PutKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Puts...)
}

// DeleteKeys returns the keys deleted so far.
func (f *FaultObjects) DeleteKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Deletes...)
}

func (f *FaultObjects) Open(ctx context.Context, key string) (artifacts.Object, error) {
	f.mu.Lock()
	h := f.OnOpen
	f.mu.Unlock()
	next := func() (artifacts.Object, error) { return f.ObjectStore.Open(ctx, key) }
	if h == nil {
		return next()
	}
	return h(key, next)
}

func (f *FaultObjects) Put(ctx context.Context, spec artifacts.PutSpec) error {
	f.mu.Lock()
	h := f.OnPut
	f.Puts = append(f.Puts, spec.Key)
	f.mu.Unlock()
	next := func() error { return f.ObjectStore.Put(ctx, spec) }
	if h == nil {
		return next()
	}
	return h(spec, next)
}

func (f *FaultObjects) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	h := f.OnDelete
	f.Deletes = append(f.Deletes, key)
	f.mu.Unlock()
	next := func() error { return f.ObjectStore.Delete(ctx, key) }
	if h == nil {
		return next()
	}
	return h(key, next)
}

// Wire returns fault wrappers installed through Options.
func Wire() (*FaultRepo, *FaultObjects, Options) {
	fr, fo := &FaultRepo{}, &FaultObjects{}
	opts := Options{
		WrapRepository: func(r artifacts.Repository) artifacts.Repository { fr.Repository = r; return fr },
		WrapObjects:    func(o artifacts.ObjectStore) artifacts.ObjectStore { fo.ObjectStore = o; return fo },
	}
	return fr, fo, opts
}

// Eventually polls cond until it holds or the timeout passes.
func Eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
