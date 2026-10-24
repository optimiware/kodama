package observationtest

import (
	"context"
	"sync"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/observation"
)

// MemLog is an in-memory observation.Log with the port's semantics, for
// contract tests that need no database. Sequences are gapless above the floor.
type MemLog struct {
	mu    sync.Mutex
	recs  []observation.Record
	floor int64
	// Fail, when set, makes every call fail.
	Fail error
	// BeforeRead, when set, runs before the nth Read (1-based) with no lock
	// held, so a test can change the log at an exact point of a stream.
	BeforeRead func(n int)
	reads      int
}

// Add appends an observation and returns its sequence.
func (l *MemLog) Add(env eventsv1.Envelope) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := int64(len(l.recs)) + 1 + l.floor
	l.recs = append(l.recs, observation.Record{Sequence: seq, Envelope: env, Size: 200})
	return seq
}

// Prune drops observations up to and including upTo and raises the floor.
func (l *MemLog) Prune(upTo int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []observation.Record
	for _, r := range l.recs {
		if r.Sequence > upTo {
			keep = append(keep, r)
		}
	}
	l.recs, l.floor = keep, upTo
}

func (l *MemLog) wm() observation.Watermark {
	hw := l.floor
	if n := len(l.recs); n > 0 {
		hw = l.recs[n-1].Sequence
	}
	return observation.Watermark{HighWater: hw, Floor: l.floor}
}

// Watermark implements observation.Log.
func (l *MemLog) Watermark(context.Context, observation.Scope) (observation.Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wm(), l.Fail
}

// Read implements observation.Log.
func (l *MemLog) Read(_ context.Context, _ observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error) {
	l.mu.Lock()
	l.reads++
	n, hook := l.reads, l.BeforeRead
	l.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Fail != nil {
		return nil, observation.Watermark{}, l.Fail
	}
	if after < l.floor {
		return nil, l.wm(), observation.ErrCursorExpired
	}
	var out []observation.Record
	for _, r := range l.recs {
		if r.Sequence > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, l.wm(), nil
}

// MemState is an in-memory observation.State.
type MemState struct {
	mu sync.Mutex
	// VerifyErr and SnapshotErr are returned by Verify and Snapshot.
	VerifyErr, SnapshotErr error
	// Resources and Position are what Snapshot returns.
	Resources []observation.Resource
	Position  int64
}

// Verify implements observation.State.
func (s *MemState) Verify(context.Context, observation.Scope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.VerifyErr
}

// Snapshot implements observation.State.
func (s *MemState) Snapshot(context.Context, observation.Scope, int) (observation.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SnapshotErr != nil {
		return observation.Snapshot{}, s.SnapshotErr
	}
	return observation.Snapshot{Position: s.Position, Resources: append([]observation.Resource(nil), s.Resources...)}, nil
}

// AccessFunc adapts a function to observation.Access.
type AccessFunc func(ctx context.Context, actor observation.Actor, scope observation.Scope, family observation.Family) error

// Authorize implements observation.Access.
func (f AccessFunc) Authorize(ctx context.Context, a observation.Actor, s observation.Scope, fam observation.Family) error {
	return f(ctx, a, s, fam)
}
