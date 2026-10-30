package podman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// lockPoll is how often a waiting operation retries the file lock.
const lockPoll = 5 * time.Millisecond

// lock serializes the operations of one workload across every Runtime, and
// every process, sharing the state directory: a Stop waits for a Start that
// is deciding whether to launch, and a Start that follows a confirmed Stop
// reads the stopped record rather than launching. The lock file lives beside
// the workload directory, never inside it, so removing the workload's files
// does not replace the lock another operation holds. A lock is released when
// its holder exits, so a crashed process never wedges a workload.
func (r *Runtime) lock(ctx context.Context, name string) (unlock func(), err error) {
	return r.fileLocks.lock(ctx, name)
}

// lockSet serializes operations by name through lock files in dir.
type lockSet struct {
	dir string
	mu  sync.Mutex
	// turns queue the waiters of one name within this process.
	turns map[string]chan struct{}
}

func newLockSet(dir string) *lockSet { return &lockSet{dir: dir, turns: map[string]chan struct{}{}} }

func (l *lockSet) lock(ctx context.Context, name string) (unlock func(), err error) {
	// Waiters of one name within this set queue here; only the head polls the
	// file.
	l.mu.Lock()
	turn, ok := l.turns[name]
	if !ok {
		turn = make(chan struct{}, 1)
		l.turns[name] = turn
	}
	l.mu.Unlock()
	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		err = flock(ctx, f)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		<-turn
		return nil, err
	}
	return func() {
		f.Close() // closing the descriptor releases the lock
		<-turn
	}, nil
}

// flock takes the exclusive lock of f, giving up when ctx ends.
func flock(ctx context.Context, f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-time.After(lockPoll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
