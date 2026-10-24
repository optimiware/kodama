// Package postgres is the PostgreSQL adapter of the observation module: durable
// scoped reads over the event store's retained observations and atomic
// snapshot-plus-watermark reads over the resource owners' tables.
//
// Every read runs in storage.WithTenantOptions under the non-owner runtime role
// with the tenant context installed, carries explicit tenant and project
// predicates, and is further constrained by forced row level security. Reads
// are read-only transactions; the package never writes. A snapshot reads state
// and the event watermark in one REPEATABLE READ transaction, so the position it
// issues is exactly the position its state reflects.
//
// Resource owners other than work management have no persistence merged yet.
// Their current state enters through the RunBinding seam, which is required:
// a store without it is a configuration error, never an empty snapshot.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/observation"
	"github.com/optimiware/kodama/internal/storage"
)

// EventLog is the part of the event store this adapter uses: the observation
// reads that run inside the caller's tenant transaction.
type EventLog interface {
	Watermark(ctx context.Context, tx pgx.Tx, scope events.Scope) (eventspg.Watermark, error)
	ReadObservations(ctx context.Context, tx pgx.Tx, scope events.Scope, after int64, limit int) ([]eventspg.Observation, eventspg.Watermark, error)
}

var _ EventLog = (*eventspg.Store)(nil)

// ResourceQuery asks a binding for the current state of one family set in one
// scope. The binding must use only the supplied transaction (it is the snapshot
// transaction) and must make no network call.
type ResourceQuery struct {
	Scope observation.Scope
	// Families lists the families the binding should return.
	Families []observation.Family
	// Limit bounds the rows to return. The store asks for one more than it can
	// accept so it can tell a complete read from a truncated one; a binding must
	// never return fewer rows than exist unless Limit is reached.
	Limit int
}

// ResourceBinding reads the current state of resources owned by another module.
type ResourceBinding interface {
	ReadResources(ctx context.Context, tx pgx.Tx, q ResourceQuery) ([]observation.Resource, error)
}

// RunBinding is the execution module's seam: current run and step-attempt state
// plus scoped run lookup. It is supplied by the composition root once run
// persistence exists.
type RunBinding interface {
	ResourceBinding
	// RunExists reports whether the run exists in the tenant and project of
	// scope. The tenant context of tx already restricts it; the implementation
	// must still carry the explicit predicates.
	RunExists(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID) (bool, error)
}

// Bindings are the resource owners' current-state readers. Both are required.
type Bindings struct {
	Work ResourceBinding
	Runs RunBinding
}

// Store implements observation.Log and observation.State.
type Store struct {
	db       storage.TxBeginner
	events   EventLog
	bindings Bindings
}

var (
	_ observation.Log   = (*Store)(nil)
	_ observation.State = (*Store)(nil)
)

// NewStore requires the database, the event log and both resource bindings.
func NewStore(db storage.TxBeginner, log EventLog, b Bindings) (*Store, error) {
	switch {
	case db == nil || log == nil:
		return nil, fmt.Errorf("%w: a database and an event log are required", observation.ErrInvalidConfiguration)
	case b.Work == nil:
		return nil, fmt.Errorf("%w: the work-state binding is required", observation.ErrInvalidConfiguration)
	case b.Runs == nil:
		return nil, fmt.Errorf("%w: the run-state binding is required (supplied by the execution module's integration)", observation.ErrInvalidConfiguration)
	}
	return &Store{db: db, events: log, bindings: b}, nil
}

func eventScope(scope observation.Scope) events.Scope {
	return events.Scope{TenantID: scope.Tenant, ProjectID: scope.Project}
}

// classify maps infrastructure failures to the module's classes. Context
// endings pass through; everything unknown is an outage and never a result.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, observation.ErrCursorExpired), errors.Is(err, observation.ErrNotFound),
		errors.Is(err, observation.ErrSnapshotTooLarge), errors.Is(err, observation.ErrUnavailable),
		errors.Is(err, observation.ErrInvalid):
		return err
	case errors.Is(err, events.ErrCursorExpired):
		return fmt.Errorf("%w: %v", observation.ErrCursorExpired, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return observation.Unavailablef(err, "reading observations")
}

var readOnly = pgx.TxOptions{AccessMode: pgx.ReadOnly}

// Watermark implements observation.Log.
func (s *Store) Watermark(ctx context.Context, scope observation.Scope) (observation.Watermark, error) {
	var out observation.Watermark
	err := storage.WithTenantOptions(ctx, s.db, scope.Tenant, readOnly, func(ctx context.Context, tx pgx.Tx) error {
		wm, err := s.events.Watermark(ctx, tx, eventScope(scope))
		out = observation.Watermark{HighWater: wm.HighWater, Floor: wm.Floor}
		return err
	})
	return out, classify(err)
}

// Read implements observation.Log with one bounded read-only page. The page,
// the retention floor checks and the returned watermark are read in one
// REPEATABLE READ transaction: under READ COMMITTED the event store re-reads
// the watermark after the rows, so a commit in between would report a
// high-water mark above a page that does not contain it. The transaction ends
// before the caller writes anything to a client.
func (s *Store) Read(ctx context.Context, scope observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error) {
	var (
		recs []observation.Record
		out  observation.Watermark
	)
	err := storage.WithTenantOptions(ctx, s.db, scope.Tenant, repeatableReadOnly, func(ctx context.Context, tx pgx.Tx) error {
		obs, wm, err := s.events.ReadObservations(ctx, tx, eventScope(scope), after, limit)
		out = observation.Watermark{HighWater: wm.HighWater, Floor: wm.Floor}
		if err != nil {
			return err
		}
		recs = make([]observation.Record, 0, len(obs))
		for _, o := range obs {
			recs = append(recs, observation.Record{Sequence: o.Sequence, Envelope: o.Envelope, Size: len(o.Payload)})
		}
		return nil
	})
	if err != nil {
		return nil, out, classify(err)
	}
	return recs, out, nil
}
