// Package postgres is the PostgreSQL adapter of the identity module: principal
// mapping, discovery, bindings, projects, authorization revisions, audit and
// event recording, the operator bootstrap and the transaction-bound authority
// used by other modules' mutations.
//
// Every statement runs inside storage.WithTenant (READ COMMITTED, as event
// append requires) under the non-owner runtime role, carries explicit tenant
// predicates and is further constrained by forced row level security. The only
// global reads are three SECURITY DEFINER functions filtered by the verified
// principal; worker tenant enumeration is never used for discovery.
//
// Lock order inside an identity mutation (consistent with the work module's
// project -> work item -> prerequisite -> event order):
//
//  1. the project row (FOR NO KEY UPDATE), only for project-scoped changes;
//  2. authorization revision rows, in ascending user ID: FOR UPDATE for users
//     whose bindings change, FOR SHARE for the acting administrator;
//  3. binding and project rows;
//  4. the event backlog and scope rows inside events/postgres.Store.Append,
//     always last.
//
// A transaction-bound permission check (NewTxAuthority) takes the checked user's
// revision FOR SHARE and keeps it until the caller's transaction ends, so a
// concurrent revocation either waits for the protected mutation or commits before
// it and is seen by the check.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is the runtime-role pool: *storage.Pool and *pgxpool.Pool satisfy it.
type DB interface {
	storage.TxBeginner
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// EventAppender is the transaction-local event append of the event bus
// (events/postgres.Store.Append).
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// Store implements identity.Repository, identity.Directory,
// identity.PrincipalMapper and identity.AccessReader.
type Store struct {
	db     DB
	events EventAppender
}

var (
	_ identity.Repository      = (*Store)(nil)
	_ identity.Directory       = (*Store)(nil)
	_ identity.PrincipalMapper = (*Store)(nil)
	_ identity.AccessReader    = (*Store)(nil)
)

// NewStore requires every collaborator: a store without an event appender could
// not honor atomic outbox writes.
func NewStore(db DB, appender EventAppender) (*Store, error) {
	if db == nil || appender == nil {
		return nil, errors.New("identity/postgres: a database and an event appender are required")
	}
	return &Store{db: db, events: appender}, nil
}

// Transact implements identity.Repository.
func (s *Store) Transact(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx identity.Tx) error) error {
	ranToCommit := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, pgtx pgx.Tx) error {
		if err := fn(ctx, &txImpl{tx: pgtx, tenant: tenant, events: s.events}); err != nil {
			return err
		}
		ranToCommit = true
		return nil
	})
	if err != nil && ranToCommit {
		// The function and every statement succeeded, so the failure is the
		// commit's: it may or may not have been applied.
		return identity.Unavailablef(err, "the outcome of the write is unknown; reread before retrying")
	}
	return classify(err)
}

// classify maps infrastructure failures to the module's classes. Business errors
// pass through; unknown errors become ErrUnavailable (an outage is never access).
func classify(err error) error {
	if err == nil || identity.IsBusinessError(err) {
		return err
	}
	if errors.Is(err, events.ErrBacklogFull) {
		return identity.Unavailablef(err, "the event backlog is full")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return identity.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503":
			return fmt.Errorf("%w: a referenced resource was not found", identity.ErrNotFound)
		case pgErr.Code == "22023":
			return fmt.Errorf("%w: %s", identity.ErrInvalid, "the value is not valid")
		}
	}
	return identity.Unavailablef(err, "the database could not complete the request")
}

// MapPrincipal implements identity.PrincipalMapper.
func (s *Store) MapPrincipal(ctx context.Context, p identity.Principal) (identity.UserID, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	var id string
	if err := s.db.QueryRow(ctx, `SELECT kodama_identity_map_principal($1, $2)`, p.Issuer, p.Subject).Scan(&id); err != nil {
		return "", classify(err)
	}
	return identity.UserID(id), nil
}

// User implements identity.Directory.
func (s *Store) User(ctx context.Context, id identity.UserID) (identity.User, error) {
	if err := id.Validate(); err != nil {
		return identity.User{}, err
	}
	var u identity.User
	var uid string
	err := s.db.QueryRow(ctx, `SELECT id, issuer, subject, created_at FROM kodama_identity_user($1)`, string(id)).Scan(&uid, &u.Issuer, &u.Subject, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.User{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.User{}, classify(err)
	}
	u.ID, u.CreatedAt = identity.UserID(uid), u.CreatedAt.UTC()
	return u, nil
}

// Tenants implements identity.Directory. The query is filtered by the user's
// own ID and can reach no other user's tenants.
func (s *Store) Tenants(ctx context.Context, id identity.UserID, afterID string, limit int) ([]identity.TenantSummary, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT tenant_id, key, display_name, roles FROM kodama_identity_tenants($1, $2, $3)`, string(id), afterID, int32(limit))
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	out := []identity.TenantSummary{}
	for rows.Next() {
		var t identity.TenantSummary
		var tid string
		var roles []string
		if err := rows.Scan(&tid, &t.Key, &t.DisplayName, &roles); err != nil {
			return nil, classify(err)
		}
		t.ID = domain.TenantID(tid)
		for _, r := range roles {
			t.Roles = append(t.Roles, identity.Role(r))
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// CurrentBindings implements identity.AccessReader with a fresh read-only
// tenant transaction.
func (s *Store) CurrentBindings(ctx context.Context, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	var out []identity.Binding
	err := storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = readBindings(ctx, tx, tenant, user)
		return err
	})
	return out, classify(err)
}

const bindingColumns = `id, user_id, project_id, role, version, created_at`

func scanBinding(tenant domain.TenantID, row pgx.Row) (identity.Binding, error) {
	var b identity.Binding
	var id, user, role string
	var project *string
	var version int64
	if err := row.Scan(&id, &user, &project, &role, &version, &b.CreatedAt); err != nil {
		return identity.Binding{}, err
	}
	b.ID, b.Tenant, b.User, b.Role, b.Version, b.CreatedAt = identity.BindingID(id), tenant, identity.UserID(user), identity.Role(role), domain.Version(version), b.CreatedAt.UTC()
	if project != nil {
		b.Project = domain.ProjectID(*project)
	}
	return b, nil
}

func readBindings(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	rows, err := tx.Query(ctx, `SELECT `+bindingColumns+` FROM memberships WHERE tenant_id = $1 AND user_id = $2 ORDER BY id COLLATE "C"`, string(tenant), string(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Binding
	for rows.Next() {
		b, err := scanBinding(tenant, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
