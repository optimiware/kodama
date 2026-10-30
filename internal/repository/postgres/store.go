// Package postgres is the PostgreSQL adapter of the repository module. Every
// statement runs inside storage.WithTenant under the non-owner runtime role,
// carries explicit tenant and project predicates and is further constrained by
// forced row level security. Audit rows and event outbox rows are written in
// the same transaction as the state change. No network or Git call is ever made
// inside a transaction.
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

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/repository"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
}

// EventAppender is the transaction-local event append of the event bus.
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// Options configure a Store.
type Options struct {
	Clock func() time.Time
	NewID func(prefix string) string
}

// Store implements repository.Store.
type Store struct {
	db     DB
	events EventAppender
	opts   Options
}

var _ repository.Store = (*Store)(nil)

// NewStore requires a database and an event appender: a repository transition
// without its audit/outbox records would be unobservable, so there is no
// construction without them.
func NewStore(db DB, events EventAppender, opts Options) (*Store, error) {
	if db == nil || events == nil {
		return nil, fmt.Errorf("%w: database and event appender are required", repository.ErrNotConfigured)
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
	return &Store{db: db, events: events, opts: opts}, nil
}

func (s *Store) now() time.Time { return s.opts.Clock().UTC().Truncate(time.Microsecond) }

const columns = `project_id, id, name, default_branch, hosting, status, reason, direct_push_to_default, empty, empty_observed_at,
	external_url, created_by_type, created_by_id, version, created_at, updated_at`

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ntm(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func scan(row pgx.Row, tenant domain.TenantID) (*repository.Repository, error) {
	var (
		project, id, name, branch, hosting, status, creatorType, creatorID string
		reason, url                                                        *string
		direct, empty                                                      bool
		observed                                                           *time.Time
		version                                                            int64
		created, updated                                                   time.Time
	)
	if err := row.Scan(&project, &id, &name, &branch, &hosting, &status, &reason, &direct, &empty, &observed, &url,
		&creatorType, &creatorID, &version, &created, &updated); err != nil {
		return nil, err
	}
	r := &repository.Repository{
		ID: repository.ID(id), Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, Name: name, DefaultBranch: branch,
		Hosting: gitv1.Hosting(hosting), Status: gitv1.RepositoryStatus(status),
		Policy: gitv1.RepositoryPolicy{DirectPushToDefault: direct}, Empty: empty,
		CreatedBy: repository.Actor{Type: repository.ActorType(creatorType), ID: creatorID},
		Version:   domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
	}
	if reason != nil {
		r.Reason = *reason
	}
	if url != nil {
		r.ExternalURL = *url
	}
	if observed != nil {
		r.EmptyObservedAt = observed.UTC()
	}
	return r, nil
}

// mapErr translates the SQLSTATEs the adapter expects. Everything else is
// returned as is (the transport renders a fixed 500).
func mapErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23503": // the project does not exist in the tenant
			return repository.ErrNotFound
		case "23505":
			return fmt.Errorf("%w: %s", domain.ErrConflict, pe.ConstraintName)
		}
	}
	return err
}

func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(ctx, s.db, tenant, fn)
}

// Insert implements repository.Store.
func (s *Store) Insert(ctx context.Context, r *repository.Repository, fx repository.Effects) (*repository.Repository, bool, error) {
	var (
		out     *repository.Repository
		created bool
	)
	err := s.inTenant(ctx, r.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, created, err = s.InsertInTx(ctx, tx, r, fx)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// InsertInTx is Insert inside a caller-owned tenant transaction: the entry
// point of the keyed (idempotent) create, which commits the reservation with
// the stored response. It never commits or rolls back tx.
func (s *Store) InsertInTx(ctx context.Context, tx pgx.Tx, r *repository.Repository, fx repository.Effects) (*repository.Repository, bool, error) {
	if err := r.Validate(); err != nil {
		return nil, false, err
	}
	// Concurrent reservations of one name arbitrate on the name's unique key; the
	// loser waits for the winner's commit, then finds its row below. A
	// suppressed conflict is a replay only if the scoped name lookup finds a row;
	// an identifier collision with another project's row still fails.
	tag, err := tx.Exec(ctx, `INSERT INTO repositories
		(tenant_id, project_id, id, name, default_branch, hosting, status, reason, direct_push_to_default, empty, empty_observed_at,
		 external_url, created_by_type, created_by_id, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT DO NOTHING`,
		r.Scope.TenantID, r.Scope.ProjectID, r.ID, r.Name, r.DefaultBranch, r.Hosting, r.Status, nstr(r.Reason),
		r.Policy.DirectPushToDefault, r.Empty, ntm(r.EmptyObservedAt), nstr(r.ExternalURL),
		string(r.CreatedBy.Type), r.CreatedBy.ID, int64(r.Version), r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if tag.RowsAffected() == 1 {
		if err := s.writeEffects(ctx, tx, r, fx); err != nil {
			return nil, false, err
		}
		return r.Clone(), true, nil
	}
	existing, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repositories
		WHERE tenant_id = $1 AND project_id = $2 AND name = $3`, r.Scope.TenantID, r.Scope.ProjectID, r.Name), r.Scope.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: repository identifier %s is already in use", domain.ErrConflict, r.ID)
	}
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

// Get implements repository.Store.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id repository.ID) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repositories
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, scope.TenantID, scope.ProjectID, id), scope.TenantID)
		return err
	})
	return out, notFound(err)
}

// GetInTenant implements repository.Store.
func (s *Store) GetInTenant(ctx context.Context, tenant domain.TenantID, id repository.ID) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repositories WHERE tenant_id = $1 AND id = $2`, tenant, id), tenant)
		return err
	})
	return out, notFound(err)
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.ErrNotFound
	}
	return err
}

// GetByName implements repository.Store.
func (s *Store) GetByName(ctx context.Context, scope domain.Scope, name string) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repositories
			WHERE tenant_id = $1 AND project_id = $2 AND name = $3`, scope.TenantID, scope.ProjectID, name), scope.TenantID)
		return err
	})
	return out, notFound(err)
}

// List implements repository.Store.
func (s *Store) List(ctx context.Context, scope domain.Scope, q repository.ListQuery) ([]*repository.Repository, error) {
	var out []*repository.Repository
	err := storage.WithTenantOptions(ctx, s.db, scope.TenantID, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var (
			at *time.Time
			id *string
		)
		if q.After != nil {
			t, i := q.After.CreatedAt.UTC(), string(q.After.ID)
			at, id = &t, &i
		}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM repositories
			WHERE tenant_id = $1 AND project_id = $2 AND ($3::timestamptz IS NULL OR (created_at, id) > ($3::timestamptz, $4::text))
			ORDER BY created_at, id LIMIT $5`, scope.TenantID, scope.ProjectID, at, id, q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scan(rows, scope.TenantID)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Update implements repository.Store.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id repository.ID, fn repository.Mutation) (*repository.Repository, error) {
	var out *repository.Repository
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = s.UpdateInTx(ctx, tx, scope, id, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateInTx is Update inside a caller-owned tenant transaction. The row is
// locked for the mutation; the mutation runs on a private copy; the row is
// written (under its version) only when the copy changed, with the effects.
func (s *Store) UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id repository.ID, fn repository.Mutation) (*repository.Repository, error) {
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM repositories
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, scope.TenantID, scope.ProjectID, id), scope.TenantID)
	if err != nil {
		return nil, notFound(err)
	}
	before := r.Clone()
	fx, err := fn(r)
	if err != nil {
		return nil, err
	}
	if *r != *before {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `UPDATE repositories SET default_branch = $4, status = $5, reason = $6, direct_push_to_default = $7,
			empty = $8, empty_observed_at = $9, version = $10, updated_at = $11
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $12`,
			scope.TenantID, scope.ProjectID, id, r.DefaultBranch, r.Status, nstr(r.Reason), r.Policy.DirectPushToDefault,
			r.Empty, ntm(r.EmptyObservedAt), int64(r.Version), r.UpdatedAt, int64(before.Version))
		if err != nil {
			return nil, mapErr(err)
		}
		if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("%w: repository changed concurrently", repository.ErrVersionConflict)
		}
	}
	if err := s.writeEffects(ctx, tx, r, fx); err != nil {
		return nil, err
	}
	return r, nil
}

// Provisioning implements repository.Store.
func (s *Store) Provisioning(ctx context.Context, tenant domain.TenantID, after repository.ID, limit int) ([]repository.Candidate, error) {
	var out []repository.Candidate
	err := storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id, id FROM repositories
			WHERE tenant_id = $1 AND status = 'provisioning' AND id > $2 ORDER BY id LIMIT $3`, tenant, string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var project, id string
			if err := rows.Scan(&project, &id); err != nil {
				return err
			}
			out = append(out, repository.Candidate{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}, ID: repository.ID(id)})
		}
		return rows.Err()
	})
	return out, err
}

// writeEffects writes the audit records and the outbox events of a change whose
// row is already current at r.Version.
func (s *Store) writeEffects(ctx context.Context, tx pgx.Tx, r *repository.Repository, fx repository.Effects) error {
	for _, a := range fx.Audit {
		md := a.Metadata
		if md == nil {
			md = map[string]any{}
		}
		raw, err := json.Marshal(md)
		if err != nil {
			return fmt.Errorf("repository: audit metadata: %w", err)
		}
		reason, reqID := a.AuditFields(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.Scope.TenantID, s.opts.NewID("aud"), r.Scope.ProjectID, s.now(), a.Actor.AuditActorType(), a.Actor.ID, a.Action,
			repository.ResourceType, r.ID, int64(r.Version), nstr(reason), nstr(reqID), raw); err != nil {
			return fmt.Errorf("repository: write audit: %w", err)
		}
	}
	for i, e := range fx.Events {
		env, err := e.Envelope(r, i, s.now(), repository.RequestID(ctx), s.opts.NewID)
		if err != nil {
			return err
		}
		if _, err := s.events.Append(ctx, tx, env); err != nil {
			return fmt.Errorf("repository: append event %s: %w", e.Kind, err)
		}
	}
	return nil
}
