package rtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// SafeBuffer is a goroutine-safe log sink.
type SafeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SafeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *SafeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ErrInjected is the cause of every injected failure; it must never reach a
// client or a log.
var ErrInjected = errors.New("injected failure SECRET-CAUSE-TEXT")

// Faults switches failures on dependencies of the service. Zero value: none.
type Faults struct {
	mu         sync.Mutex
	locate     bool
	do         bool
	rollback   bool
	loseCommit bool
	observe    bool
	lock       bool
	signing    bool
	afterLock  func()
}

// SetAfterLock runs fn right after the next successful LockAttempt of an
// admission or refresh transaction, while its locks are held.
func (f *Faults) SetAfterLock(fn func()) { f.mu.Lock(); f.afterLock = fn; f.mu.Unlock() }

func (f *Faults) runAfterLock() {
	f.mu.Lock()
	fn := f.afterLock
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (f *Faults) set(p *bool, v bool) { f.mu.Lock(); *p = v; f.mu.Unlock() }
func (f *Faults) get(p *bool) bool    { f.mu.Lock(); defer f.mu.Unlock(); return *p }

// FailLocate makes the credential lookup fail (storage outage).
func (f *Faults) FailLocate(v bool) { f.set(&f.locate, v) }

// FailTransactions makes opening a transaction fail (storage outage).
func (f *Faults) FailTransactions(v bool) { f.set(&f.do, v) }

// RollbackAfterWork rolls back every transaction after its work completed.
func (f *Faults) RollbackAfterWork(v bool) { f.set(&f.rollback, v) }

// LoseCommit commits every transaction but reports an unknown outcome.
func (f *Faults) LoseCommit(v bool) { f.set(&f.loseCommit, v) }

// FailObserveAttempt makes the advisory attempt read fail (execution outage).
func (f *Faults) FailObserveAttempt(v bool) { f.set(&f.observe, v) }

// FailLockAttempt makes the locking attempt read fail.
func (f *Faults) FailLockAttempt(v bool) { f.set(&f.lock, v) }

// FailSigning makes token signing fail.
func (f *Faults) FailSigning(v bool) { f.set(&f.signing, v) }

func (f *Faults) lockErr() error {
	if f.get(&f.lock) {
		return ErrInjected
	}
	return nil
}

type faultStore struct {
	inner runtimeauth.Store
	f     *Faults
}

func (s *faultStore) Locate(ctx context.Context, d runtimeauth.Digest) (runtimeauth.Locator, error) {
	if s.f.get(&s.f.locate) {
		return runtimeauth.Locator{}, ErrInjected
	}
	return s.inner.Locate(ctx, d)
}

func (s *faultStore) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx runtimeauth.GrantTx) error) error {
	if s.f.get(&s.f.do) {
		return ErrInjected
	}
	rollback, lose := s.f.get(&s.f.rollback), s.f.get(&s.f.loseCommit)
	wrapped := fn
	if rollback {
		wrapped = func(ctx context.Context, tx runtimeauth.GrantTx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return ErrInjected
		}
	}
	err := s.inner.Do(ctx, tenant, wrapped)
	if err == nil && lose {
		return ErrInjected
	}
	return err
}

type faultAttempts struct {
	inner runtimeauth.AttemptObserver
	f     *Faults
}

func (a *faultAttempts) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	if a.f.get(&a.f.observe) {
		return runtimeauth.AttemptState{}, ErrInjected
	}
	return a.inner.ObserveAttempt(ctx, scope, attempt)
}

type faultJobs struct {
	inner runtimeauth.JobObserver
	f     *Faults
}

func (j *faultJobs) ObserveJob(ctx context.Context, o runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	return j.inner.ObserveJob(ctx, o)
}

type faultIssuer struct {
	inner runtimeauth.TokenIssuer
	f     *Faults
}

func (i *faultIssuer) Generation() int64 { return i.inner.Generation() }
func (i *faultIssuer) Issue(r runtimeauth.IssueRequest) (string, error) {
	if i.f.get(&i.f.signing) {
		return "", ErrInjected
	}
	return i.inner.Issue(r)
}
