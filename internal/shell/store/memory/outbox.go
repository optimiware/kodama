package memory

import (
	"context"
	"sort"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
)

// Pending returns at most limit committed, unacknowledged event rows in durable
// sequence order. It scans from the requested lower bound every call and keeps
// no publication high-water mark: a row whose transaction commits late with a
// lower sequence than an already-acknowledged row is still returned. The
// committed event table is the only source of publication.
func (s *Store) Pending(ctx context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, faultInvalid("outbox pending limit must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pending := make([]event.PendingEvent, 0, limit)
	for _, committed := range s.state.events {
		if committed.Published || committed.Sequence <= uint64(after) {
			continue
		}
		pending = append(pending, event.PendingEvent{
			Sequence: event.Sequence(committed.Sequence),
			Event:    copyEnvelope(committed.Envelope),
		})
	}
	sort.SliceStable(pending, func(left, right int) bool {
		return pending[left].Sequence < pending[right].Sequence
	})
	if len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

// MarkPublished acknowledges exactly one committed event row by its identity
// and durable sequence. A missing row is not_found and a mismatched sequence is
// version_conflict, so an acknowledgement can never settle another row. An
// already-published row is a successful no-op, which makes a retried
// acknowledgement after a lost response safe.
func (s *Store) MarkPublished(ctx context.Context, eventID id.EventID, sequence event.Sequence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.state.events {
		committed := &s.state.events[index]
		if committed.Envelope.ID != eventID {
			continue
		}
		if committed.Sequence != uint64(sequence) {
			return faultConflict("the acknowledged event sequence does not match the stored row")
		}
		committed.Published = true
		return nil
	}
	return faultNotFound("the acknowledged event row is missing")
}
