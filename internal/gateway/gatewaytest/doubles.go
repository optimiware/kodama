package gatewaytest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/storage"
)

// ErrInjected is the cause of every injected failure. Its text carries a
// sentinel so tests can prove it never reaches a client, a log or a row.
var ErrInjected = errors.New("injected failure CAUSE-SENTINEL-t032")

// Sink is a recording gateway.Sink.
type Sink struct {
	mu       sync.Mutex
	Frames   []string
	started  bool
	Finished bool
	FailCode string
	// FailAfter makes Frame fail from the n-th write on (a disconnected client);
	// 0 disables.
	FailAfter int
	// OnFrame runs after each frame is written.
	OnFrame func(n int)
}

// Started implements gateway.Sink.
func (s *Sink) Started() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.started }

// Frame implements gateway.Sink.
func (s *Sink) Frame(_ context.Context, data []byte) error {
	s.mu.Lock()
	if s.FailAfter > 0 && len(s.Frames)+1 >= s.FailAfter {
		s.mu.Unlock()
		return ErrInjected
	}
	s.started = true
	s.Frames = append(s.Frames, string(data))
	n, hook := len(s.Frames), s.OnFrame
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return nil
}

// Done implements gateway.Sink.
func (s *Sink) Done(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started, s.Finished = true, true
	return nil
}

// Fail implements gateway.Sink.
func (s *Sink) Fail(_ context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started, s.FailCode = true, code
	return nil
}

// Snapshot returns the frames, completion flag and failure code.
func (s *Sink) Snapshot() (frames []string, finished bool, failCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Frames...), s.Finished, s.FailCode
}

// Recorder is a gateway.Recorder double.
type Recorder struct {
	mu    sync.Mutex
	Calls []gateway.Exchange
	Err   error
}

// Record implements gateway.Recorder.
func (r *Recorder) Record(_ context.Context, x gateway.Exchange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, x)
	return r.Err
}

// Count returns the number of recorder calls.
func (r *Recorder) Count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.Calls) }

// Wrapper is a secrets.KeyWrapper that can fail unwrapping.
type Wrapper struct {
	Inner secrets.KeyWrapper
	mu    sync.Mutex
	err   error
	hook  func()
}

// FailUnwrap makes Unwrap fail with err (nil restores it).
func (w *Wrapper) FailUnwrap(err error) { w.mu.Lock(); w.err = err; w.mu.Unlock() }

// BeforeUnwrap runs fn inside every Unwrap (to race a revocation against it).
func (w *Wrapper) BeforeUnwrap(fn func()) { w.mu.Lock(); w.hook = fn; w.mu.Unlock() }

// Wrap implements secrets.KeyWrapper.
func (w *Wrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	return w.Inner.Wrap(ctx, wc, key)
}

// Unwrap implements secrets.KeyWrapper.
func (w *Wrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, k secrets.WrappedKey) ([]byte, error) {
	w.mu.Lock()
	err, hook := w.err, w.hook
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	return w.Inner.Unwrap(ctx, wc, k)
}

// DB wraps a transaction beginner to inject storage faults into one replica.
type DB struct {
	Inner storage.TxBeginner

	loseCommits atomic.Int64
	skipCommits atomic.Int64
	failBegin   atomic.Bool
	failNth     atomic.Int64
	begins      atomic.Int64
	hook        atomic.Pointer[func()]
}

// LoseNextCommits makes the next n commits succeed in the database but report
// an error to the caller: an ambiguous commit outcome.
func (d *DB) LoseNextCommits(n int) { d.LoseCommits(0, n) }

// LoseCommits lets the next skip commits through and then loses n.
func (d *DB) LoseCommits(skip, n int) {
	d.skipCommits.Store(int64(skip))
	d.loseCommits.Store(int64(n))
}

// FailNthBegin makes the n-th transaction begun from now on fail (1 is the next).
func (d *DB) FailNthBegin(n int) {
	d.begins.Store(0)
	d.failNth.Store(int64(n))
}

// FailBegin makes beginning any transaction fail (a database outage).
func (d *DB) FailBegin(v bool) { d.failBegin.Store(v) }

// OnBegin runs fn before each transaction begins.
func (d *DB) OnBegin(fn func()) {
	if fn == nil {
		d.hook.Store(nil)
		return
	}
	d.hook.Store(&fn)
}

// BeginTx implements storage.TxBeginner.
func (d *DB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	if h := d.hook.Load(); h != nil {
		(*h)()
	}
	if d.failBegin.Load() {
		return nil, ErrInjected
	}
	if n := d.failNth.Load(); n > 0 && d.begins.Add(1) == n {
		return nil, ErrInjected
	}
	tx, err := d.Inner.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, db: d}, nil
}

type faultTx struct {
	pgx.Tx
	db *DB
}

func (t *faultTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.db.skipCommits.Add(-1) >= 0 {
		return nil
	}
	t.db.skipCommits.Store(0)
	if t.db.loseCommits.Add(-1) >= 0 {
		return ErrInjected
	}
	t.db.loseCommits.Store(0)
	return nil
}

// Wait polls until cond holds or the timeout passes.
func Wait(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
