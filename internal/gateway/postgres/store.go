// Package postgres is the PostgreSQL adapter of the model gateway: settings and
// their revisions, the immutable per-run binding, atomic admission,
// finalization and reconciliation.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates, and is additionally
// confined by forced row level security and composite foreign keys. Provider
// HTTP calls, credential decryption and key-provider calls never happen inside
// a transaction here: the use case performs them between short transactions.
//
// Lock orders (documented once, followed everywhere). Workload transactions
// (admission, begin-forwarding, heartbeat): the execution run row (FOR SHARE,
// taken inside the workload recheck by execution's LockAttempt, which reads the
// attempt without locking it separately), the runtime grant (FOR SHARE), the
// gateway run binding row (FOR UPDATE), the request row (FOR UPDATE), the secret
// rows (pin registration), and the event append last. Finalization and
// reconciliation need no workload authority and start at the binding row. Human
// settlement: the actor's identity authorization revision (FOR SHARE, through the
// management binder), then the binding row, the request row and the audit insert.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/storage"
)

// EventAppender appends an event to the outbox inside the caller's tenant
// transaction. It must join tx and never commit it. A full backlog must return
// an error wrapping gateway.ErrBackpressure, which rolls the change back.
type EventAppender func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error

// WorkloadAuthority rechecks, inside the caller's transaction, that the
// workload still holds model.invoke authority: the attempt is active at its
// current authority revision and the session is unrevoked and unexpired. It is
// the execution/runtime-authority seam (see adapters.RuntimeAuthority) and must
// take its locks in the module-wide order. Errors are classified:
// gateway.ErrUnauthenticated, ErrForbidden or ErrUnavailable. An absent
// implementation is a construction error: there is no allow-all default.
type WorkloadAuthority interface {
	Check(ctx context.Context, tx pgx.Tx, w gateway.Workload, now time.Time) error
}

// ManagementAuthority is the permission check bound to one open tenant
// transaction. It holds the actor's authorization revision until the
// transaction ends, so a revocation racing the write is ordered with it.
type ManagementAuthority interface {
	Authorize(ctx context.Context, actor gateway.Actor, scope domain.Scope, perm gateway.Permission) error
}

// ManagementBinder binds the management authority to a transaction.
type ManagementBinder func(tx pgx.Tx) ManagementAuthority

// PinRegistrar records, inside the caller's transaction, that the run retains
// the exact credential version (secrets.RegisterPins). It never commits.
type PinRegistrar interface {
	Register(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, ref gateway.CredentialRef) error
}

// Config configures a Store. Every member except Clock is required.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool.
	DB         storage.TxBeginner
	Events     EventAppender
	Workload   WorkloadAuthority
	Management ManagementBinder
	Pins       PinRegistrar
	Clock      func() time.Time
}

// Store implements gateway.Store over PostgreSQL.
type Store struct {
	db         storage.TxBeginner
	events     EventAppender
	workload   WorkloadAuthority
	management ManagementBinder
	pins       PinRegistrar
	now        func() time.Time
}

var _ gateway.Store = (*Store)(nil)

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Events == nil || cfg.Workload == nil || cfg.Management == nil || cfg.Pins == nil {
		return nil, errors.New("gateway/postgres: DB, Events, Workload, Management and Pins are required")
	}
	s := &Store{db: cfg.DB, events: cfg.Events, workload: cfg.Workload, management: cfg.Management, pins: cfg.Pins, now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// atomically runs fn in one tenant transaction. A failure of the commit itself
// after fn succeeded has an unknown outcome and is reported as
// gateway.ErrUnknownCommit wrapped in ErrUnavailable: callers never assume it
// committed and never assume it did not.
func (s *Store) atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	done := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err != nil && done {
		return gateway.FailWith(gateway.ErrUnavailable, errors.Join(gateway.ErrUnknownCommit, err), "the commit outcome is unknown")
	}
	return classify(err)
}

func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// classify maps adapter failures to the module's classes. Domain errors pass
// through; everything else fails closed as unavailable without its text.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			if pg.ConstraintName == "model_requests_one_unresolved" {
				return gateway.FailWith(gateway.ErrBusy, err, "another model request of this run is unresolved")
			}
			if pg.ConstraintName == "model_requests_pkey" {
				// Admission only inserts: an identity already in the project is never reused.
				return gateway.FailWith(gateway.ErrInvalid, err, "the request identifier is already in use")
			}
		case "23503":
			return gateway.FailWith(gateway.ErrNotFound, err, "the referenced resource does not exist in this scope")
		}
		return gateway.FailWith(gateway.ErrUnavailable, err, "the database could not complete the operation")
	}
	return gateway.Classify(err)
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("gateway/postgres: no randomness: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b)
}

type rowScanner interface{ Scan(dest ...any) error }
