// Package postgres is the PostgreSQL adapter of the MCP proxy: registrations,
// run bindings, caller-bound sessions, call receipts and the run-wide call
// counter.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is additionally
// confined by forced row level security and composite foreign keys. Remote HTTP
// and key-provider calls never happen inside a transaction here: the service
// calls the remote after Admit commits, and records the outcome in a second
// short transaction.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/storage"
)

// RecheckFunc verifies, inside the caller's tenant transaction, that the
// caller's runtime authority is still current (the execution-before-grant lock
// order is the bridge's responsibility). It returns mcpproxy.ErrUnauthenticated
// or ErrDenied when authority ended and any other error for an outage.
type RecheckFunc func(ctx context.Context, tx pgx.Tx, c mcpproxy.Caller, now time.Time) error

// PinFunc registers, inside the caller's tenant transaction, that the run
// retains the exact secret version. It is the bridge to the secrets module's
// RegisterPins.
type PinFunc func(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, secretName string, version int64) error

// Options configures a Store.
type Options struct {
	// Recheck and Pin are required: there is no allow-all default.
	Recheck RecheckFunc
	Pin     PinFunc
	// NewID creates opaque identifiers for audit rows.
	NewID func() (string, error)
}

// Store implements mcpproxy.Store and mcpproxy.SecretChecker over PostgreSQL.
type Store struct {
	db  storage.TxBeginner
	opt Options
}

var (
	_ mcpproxy.Store         = (*Store)(nil)
	_ mcpproxy.SecretChecker = (*Store)(nil)
)

// NewStore validates the configuration.
func NewStore(db storage.TxBeginner, o Options) (*Store, error) {
	if db == nil || o.Recheck == nil || o.Pin == nil {
		return nil, errors.New("mcpproxy/postgres: a database, an authority recheck and a pin registrar are required")
	}
	if o.NewID == nil {
		o.NewID = func() (string, error) {
			var b [16]byte
			if _, err := rand.Read(b[:]); err != nil {
				return "", err
			}
			return "aud_mcp_" + hex.EncodeToString(b[:]), nil
		}
	}
	return &Store{db: db, opt: o}, nil
}

// do runs fn in one tenant transaction. committed reports that fn succeeded:
// a later error is then a commit of unknown outcome.
func (s *Store) do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) (committedUnknown bool, err error) {
	done := false
	err = storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		done = true
		return nil
	})
	return err != nil && done, err
}

func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// classify maps adapter failures to the module's classes. Domain errors pass
// through unchanged.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mcpproxy.ErrInvalid), errors.Is(err, mcpproxy.ErrNotFound), errors.Is(err, mcpproxy.ErrUnauthenticated),
		errors.Is(err, mcpproxy.ErrDenied), errors.Is(err, mcpproxy.ErrVersionConflict), errors.Is(err, mcpproxy.ErrConflict),
		errors.Is(err, mcpproxy.ErrBindingInvalid), errors.Is(err, mcpproxy.ErrSessionInvalid), errors.Is(err, mcpproxy.ErrLimitReached),
		errors.Is(err, mcpproxy.ErrInvalidTransition), errors.Is(err, mcpproxy.ErrCredential), errors.Is(err, mcpproxy.ErrUnavailable):
		return err
	}
	switch pgCode(err) {
	case "23503":
		return fmt.Errorf("%w: a referenced record does not exist in this scope", mcpproxy.ErrNotFound)
	case "23505":
		return fmt.Errorf("%w: the record already exists", mcpproxy.ErrConflict)
	}
	return mcpproxy.Unavailablef(err, "the database failed")
}

// audit appends one safe audit row. metadata holds identifiers and classified
// outcomes only.
type auditEntry struct {
	Scope     domain.Scope
	ActorType string
	ActorID   string
	Action    string
	Resource  string
	Version   int64
	RequestID string
	At        time.Time
	Metadata  map[string]any
}

func (s *Store) audit(ctx context.Context, tx pgx.Tx, e auditEntry) error {
	id, err := s.opt.NewID()
	if err != nil {
		return err
	}
	meta, err := json.Marshal(e.Metadata)
	if err != nil {
		return err
	}
	var req *string
	if e.RequestID != "" {
		req = &e.RequestID
	}
	var ver *int64
	if e.Version > 0 {
		ver = &e.Version
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit
		(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'mcp_server', $8, $9, $10, $11)`,
		string(e.Scope.TenantID), id, string(e.Scope.ProjectID), e.At.UTC(), e.ActorType, e.ActorID, e.Action, e.Resource, ver, req, meta)
	return err
}

func noRows(err error, to error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return to
	}
	return err
}
