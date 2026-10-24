// Package postgres is the PostgreSQL adapter of the execution module: the run,
// step, attempt, bookkeeping, history, evidence, reservation and cancellation
// tables, the transactional mutation port, peer-module transaction bindings
// and the keyed (idempotent) creation helper.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is further
// constrained by forced row level security. State, history, audit and outbox
// rows commit together. No network call is ever made inside a transaction.
//
// Lock order, which every execution transaction follows:
//
//  1. execution quota scope rows, tenant then project (dispatch only);
//  2. the work item admission lock (FOR SHARE, via work LockActionable);
//  3. the run row (FOR UPDATE), then its step, attempt and bookkeeping rows;
//     an item-scoped cancellation takes every affected run row, in run ID
//     order, before any lower-order lock of any of those runs;
//  4. the identity authorization revision (read FOR SHARE by a mutation that
//     checks its caller, inside the mutation and before anything is saved),
//     then the runtime grants of the attempts whose authority ended without an
//     accepted result (attempt ID order), then the secret rows (pin, release);
//  5. the workspace row (reservation, settlement, retained reference);
//  6. the event backlog and scope rows inside events/postgres Append, last.
//
// The grant revocation and the later levels are scheduled by this adapter, not
// by the order in which the entities queue their peer effects: UpdateInTx
// revokes grants and runs the queued effects with workspace writer settlement
// deferred behind them, and a batch (RequestCancellationInTx) finishes level 4
// for every run before level 5 and level 6 of any run.
//
// Work mutations take project then item then read dependents without locking
// them, and the workspace module takes its row first only for its own
// mutations, so the orders cannot cycle (see internal/work/postgres and
// internal/workspace/postgres).
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
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// EventAppender is the transaction-local event append of the event bus.
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// WorkspaceTx is the workspace store's transaction-joining entry point.
type WorkspaceTx interface {
	UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error)
}

// SecretPins is the secrets store's transaction-joining pin API.
type SecretPins interface {
	RegisterPins(ctx context.Context, tx pgx.Tx, req secretspg.PinRequest) error
	ReleasePins(ctx context.Context, tx pgx.Tx, scope domain.Scope, consumer secrets.ConsumerRef) (int64, error)
}

// WorkLock is the work module's transaction-local admission lock
// (workpg.LockActionable).
type WorkLock func(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.WorkItemID) (workpg.Admission, error)

// AuthorityBinder binds current-authorization checks to one open transaction.
type AuthorityBinder func(tx pgx.Tx) execution.TxAuthority

// EnvironmentBinder binds delivery's retained-snapshot and authority
// revalidation to one open tenant transaction. It is optional: without it a
// plan that pins an environment fails closed (ErrNotConfigured), while
// no-environment admission is unchanged.
type EnvironmentBinder func(tx pgx.Tx) execution.TxEnvironments

// Peers are the peer-module transaction seams. Only this adapter binds them;
// the execution entities and use cases never see pgx. A nil peer fails closed.
type Peers struct {
	Authority AuthorityBinder
	// Environments is delivery's transaction-bound revalidation; see
	// EnvironmentBinder.
	Environments EnvironmentBinder
	Workspaces   WorkspaceTx
	Secrets      SecretPins
	// Work defaults to workpg.LockActionable.
	Work WorkLock
	// RetainedRefPrefix is the workspace module's retained-reference namespace
	// (it must equal the workspace service's option).
	RetainedRefPrefix string
}

// AttemptGrantRevoker revokes every runtime grant of an attempt inside the
// caller's open tenant transaction and returns how many it revoked. It is
// idempotent: a repeat revokes nothing. It is the adapter-local seam to the
// runtime authority module (see execution/adapters.RevokeAttemptGrants); the
// execution entities never see it. It must not open another transaction.
type AttemptGrantRevoker func(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error)

// Options configure a Store.
type Options struct {
	Clock func() time.Time
	NewID func(prefix string) string
}

// Store implements execution.Store.
type Store struct {
	db     storage.TxBeginner
	events EventAppender
	peers  Peers
	// grants is nil only for a component store built by NewStore, which has no
	// runtime grant schema; NewRuntimeStore always sets it.
	grants AttemptGrantRevoker
	opts   Options
}

var _ execution.Store = (*Store)(nil)

// NewStore requires a database, an event appender and every peer: a run
// transition without its audit/outbox records, or a dispatch without authority,
// workspace and secret bindings, must not be constructible.
//
// NewStore builds the component store used where the runtime grant schema is
// not installed: it revokes no grants. Any composition that serves runtime
// credentials must use NewRuntimeStore, so that omitting the revoker cannot
// silently leave grants of cancelled attempts alive.
func NewStore(db storage.TxBeginner, appender EventAppender, peers Peers, opts Options) (*Store, error) {
	return newStore(db, appender, peers, nil, opts)
}

// NewRuntimeStore is NewStore for a composition whose database carries the
// runtime grants: it additionally requires the grant revoker, which runs in the
// same transaction as every transition that ends an attempt's authority without
// an accepted result (cancellation, timeout and failure without a result).
func NewRuntimeStore(db storage.TxBeginner, appender EventAppender, peers Peers, grants AttemptGrantRevoker, opts Options) (*Store, error) {
	if grants == nil {
		return nil, fmt.Errorf("%w: the runtime grant revoker is required", execution.ErrNotConfigured)
	}
	return newStore(db, appender, peers, grants, opts)
}

func newStore(db storage.TxBeginner, appender EventAppender, peers Peers, grants AttemptGrantRevoker, opts Options) (*Store, error) {
	if db == nil || appender == nil || peers.Authority == nil || peers.Workspaces == nil || peers.Secrets == nil {
		return nil, fmt.Errorf("%w: database, event appender, authority, workspace and secret bindings are required", execution.ErrNotConfigured)
	}
	if peers.Work == nil {
		peers.Work = workpg.LockActionable
	}
	if peers.RetainedRefPrefix == "" {
		peers.RetainedRefPrefix = workspace.DefaultRetainedRefPrefix
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &Store{db: db, events: appender, peers: peers, grants: grants, opts: opts}, nil
}

// AdmitsEnvironments implements execution.EnvironmentStore: admission binds
// environments in its transaction exactly when the delivery peer is configured.
func (s *Store) AdmitsEnvironments() bool { return s.peers.Environments != nil }

func (s *Store) now() time.Time { return s.opts.Clock().UTC().Truncate(time.Microsecond) }

// inTenant runs fn in a tenant transaction and classifies its failure. A
// failure after fn returned nil came from the commit: whether it applied is
// unknown, so it is unavailable (the caller retries and observes whatever
// committed), never an unclassified error and never an acknowledgment.
func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	completed := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil && completed {
		return execution.Unavailablef(err, "the commit outcome is unknown")
	}
	return classify(err)
}

func (s *Store) inTenantRead(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// classify maps infrastructure failures to the module's classes. Business
// errors pass through; unknown ones stay unclassified (a 500).
func classify(err error) error {
	if err == nil || execution.IsBusinessError(err) {
		return err
	}
	switch {
	case errors.Is(err, events.ErrBacklogFull):
		return execution.Unavailablef(err, "the event backlog is full")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return execution.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503":
			return fmt.Errorf("%w: a referenced resource was not found", execution.ErrNotFound)
		case pgErr.Code == "23514":
			return fmt.Errorf("%w: the database refused the change (%s)", execution.ErrInvalidTransition, pgErr.ConstraintName+pgErr.Hint)
		case pgErr.Code == "55P03": // lock_not_available
			return execution.ErrBusy
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "40" || pgErr.Code[:2] == "53" || pgErr.Code[:2] == "57" || pgErr.Code[:2] == "08"):
			return execution.Unavailablef(err, "the database could not complete the request")
		}
	}
	var connErr interface{ Timeout() bool }
	if errors.As(err, &connErr) {
		return execution.Unavailablef(err, "the database could not be reached")
	}
	return err
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ntm(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := micros(*t)
	return &u
}

func tmp(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	u := p.UTC()
	return &u
}
