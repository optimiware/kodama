package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
)

// MaxObservationPage bounds one scoped read.
const MaxObservationPage = 1000

// Watermark is a scope's committed high-water sequence and retention floor.
// Sequences are gapless in commit order, so every committed event with a
// sequence at or below HighWater is visible to a later read; Floor is the
// highest pruned sequence, so a cursor below it has expired.
type Watermark struct {
	HighWater int64
	Floor     int64
}

// Observation is one retained event with its scope sequence.
type Observation struct {
	Sequence   int64
	Envelope   eventsv1.Envelope
	Payload    []byte
	RecordedAt time.Time
}

// Watermark reads the scope watermark in the caller's transaction. A
// REPEATABLE READ transaction that reads a snapshot and then calls this
// (T-024's snapshot-plus-cursor) gets a watermark consistent with the snapshot.
// A scope with no events reports zero values.
func (s *Store) Watermark(ctx context.Context, tx pgx.Tx, scope events.Scope) (Watermark, error) {
	if err := scope.Validate(); err != nil {
		return Watermark{}, err
	}
	var w Watermark
	err := tx.QueryRow(ctx, `SELECT last_seq, floor_seq FROM event_scopes WHERE tenant_id = $1 AND scope_key = $2`,
		string(scope.TenantID), scope.Key()).Scan(&w.HighWater, &w.Floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Watermark{}, nil
	}
	if err != nil {
		return Watermark{}, fmt.Errorf("events: watermark: %w", err)
	}
	return w, nil
}

// ReadObservations returns up to limit observations with sequence > after, in
// order, within one scope of the caller's tenant transaction. A cursor below
// the retention floor yields events.ErrCursorExpired (the caller resyncs from a
// snapshot) instead of silently skipping pruned events.
func (s *Store) ReadObservations(ctx context.Context, tx pgx.Tx, scope events.Scope, after int64, limit int) ([]Observation, Watermark, error) {
	if after < 0 || limit < 1 || limit > MaxObservationPage {
		return nil, Watermark{}, fmt.Errorf("events: cursor must be >= 0 and limit between 1 and %d", MaxObservationPage)
	}
	before, err := s.Watermark(ctx, tx, scope)
	if err != nil {
		return nil, before, err
	}
	if after < before.Floor {
		return nil, before, fmt.Errorf("%w: cursor %d is below the retention floor %d", events.ErrCursorExpired, after, before.Floor)
	}
	rows, err := tx.Query(ctx, `SELECT seq, payload, recorded_at FROM event_observations
		WHERE tenant_id = $1 AND scope_key = $2 AND seq > $3 ORDER BY seq LIMIT $4`,
		string(scope.TenantID), scope.Key(), after, limit)
	if err != nil {
		return nil, before, fmt.Errorf("events: read observations: %w", err)
	}
	var out []Observation
	for rows.Next() {
		var o Observation
		if err := rows.Scan(&o.Sequence, &o.Payload, &o.RecordedAt); err != nil {
			rows.Close()
			return nil, before, fmt.Errorf("events: scan observation: %w", err)
		}
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, before, fmt.Errorf("events: read observations: %w", err)
	}
	// Re-read the floor: retention that ran between the two statements (READ
	// COMMITTED) could have removed events we would otherwise skip over.
	after2, err := s.Watermark(ctx, tx, scope)
	if err != nil {
		return nil, before, err
	}
	if after < after2.Floor {
		return nil, after2, fmt.Errorf("%w: retention advanced to %d during the read", events.ErrCursorExpired, after2.Floor)
	}
	for i := range out {
		env, err := eventsv1.Decode(out[i].Payload)
		if err != nil {
			return nil, after2, fmt.Errorf("events: stored observation %d is undecodable: %w", out[i].Sequence, err)
		}
		out[i].Envelope = env
	}
	return out, after2, nil
}
