// Package postgres persists the delivery module: project environments,
// immutable configuration snapshots, pre-apply deployment records and their
// append-only history, over PostgreSQL with forced row level security.
//
// Every transaction runs under storage.WithTenant, which sets the
// transaction-local tenant context RLS reads, and every statement also names the
// tenant and project explicitly. A deployment mutation commits its state,
// history row, safe audit entry and deployment.state_changed outbox event
// together; the event is appended last, so a full outbox backlog rolls the
// whole transition back. Runtime mutations first repeat the session check
// (Guard) under the execution-owned attempt lock, then take delivery locks.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/storage"
)

// Config configures a Store.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool.
	DB     storage.TxBeginner
	Events EventAppender
	// Guard is required for runtime (workload) mutations. Without it every
	// runtime mutation is refused.
	Guard *Guard
	IDs   delivery.IDs
	// Clock defaults to the UTC wall clock.
	Clock func() time.Time
}

// Store implements delivery.Store.
type Store struct {
	db       storage.TxBeginner
	appender EventAppender
	guard    *Guard
	ids      delivery.IDs
	now      func() time.Time
}

var _ delivery.Store = (*Store)(nil)

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Events == nil {
		return nil, errors.New("delivery/postgres: DB and Events are required")
	}
	s := &Store{db: cfg.DB, appender: cfg.Events, guard: cfg.Guard, ids: cfg.IDs, now: cfg.Clock}
	if s.ids == nil {
		s.ids = delivery.RandomIDs{}
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// ---- helpers ---------------------------------------------------------------

func (s *Store) tx(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// guardRuntime repeats the session check for a runtime mutation. It must be the
// first statement-level work of the transaction: it takes the execution locks.
func (s *Store) guardRuntime(ctx context.Context, tx pgx.Tx, scope domain.Scope, m delivery.Mutation) error {
	if !m.Runtime {
		return nil
	}
	sess, ok := SessionFrom(ctx)
	if !ok || s.guard == nil {
		return delivery.Fail(delivery.ErrUnauthenticated, nil, "a runtime mutation needs a verified session")
	}
	if _, err := s.guard.Check(ctx, tx, scope, sess); err != nil {
		return mapAuthority(err)
	}
	return nil
}

// classify keeps classified business and authority errors and maps
// infrastructure failures, including an unknown commit outcome, to the module's
// classes: anything unclassified is "unavailable", never success.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if isBusiness(err) {
		return err
	}
	if isAuthority(err) {
		return mapAuthority(err)
	}
	switch {
	case errors.Is(err, events.ErrBacklogFull):
		return delivery.ErrBackpressure
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return delivery.Fail(delivery.ErrUnavailable, err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return delivery.Fail(delivery.ErrNotFound, err, "a referenced resource was not found")
		case "23514":
			return delivery.Fail(delivery.ErrInvalidTransition, err, "the database refused the change")
		case "23505":
			return delivery.Fail(delivery.ErrConflict, err, "the record already exists")
		}
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "the database could not complete the request")
}

func isBusiness(err error) bool {
	for _, k := range []error{delivery.ErrInvalid, delivery.ErrNotFound, delivery.ErrDenied, delivery.ErrUnauthenticated, delivery.ErrAttemptRevoked,
		delivery.ErrConflict, delivery.ErrVersionConflict, delivery.ErrInvalidTransition, delivery.ErrOutcomeUnknown, delivery.ErrNameTaken,
		delivery.ErrCredentialUnavailable, delivery.ErrPinRequired, delivery.ErrProvenance, delivery.ErrNotConfigured, delivery.ErrUnavailable} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

type auditEntry struct {
	Scope           domain.Scope
	Action          string
	ResourceType    string
	ResourceID      string
	ResourceVersion int64
	Metadata        map[string]any
}

// audit appends one safe-metadata row: identifiers, versions and operation
// names only, never configuration values or credentials.
func (s *Store) audit(ctx context.Context, tx pgx.Tx, m delivery.Mutation, e auditEntry) error {
	id, err := s.ids.NewID("aud")
	if err != nil {
		return err
	}
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var reason, request *string
	if m.Reason != "" {
		reason = &m.Reason
	}
	if m.RequestID != "" {
		request = &m.RequestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit
(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		string(e.Scope.TenantID), id, string(e.Scope.ProjectID), m.Now.UTC(), string(m.Actor.Type), m.Actor.ID, e.Action, e.ResourceType, e.ResourceID,
		e.ResourceVersion, reason, request, string(mj))
	return err
}

func utc(t time.Time) time.Time { return t.UTC() }

func timePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ---- environments ----------------------------------------------------------

const environmentColumns = `tenant_id, project_id, id, name, kind, target, namespace, kubeconfig, secrets, config, preview_ttl_seconds, version, created_at, updated_at`

func scanEnvironment(row pgx.Row) (delivery.Environment, error) {
	var rec delivery.EnvironmentRecord
	var tenant, project, id, kind string
	var kube, secrets, config []byte
	var ttl, version int64
	if err := row.Scan(&tenant, &project, &id, &rec.Name, &kind, &rec.Target, &rec.Namespace, &kube, &secrets, &config, &ttl, &version, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return delivery.Environment{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.ID, rec.Kind, rec.PreviewTTL, rec.Version = domain.EnvironmentID(id), delivery.EnvironmentKind(kind), time.Duration(ttl)*time.Second, domain.Version(version)
	rec.CreatedAt, rec.UpdatedAt = utc(rec.CreatedAt), utc(rec.UpdatedAt)
	if err := json.Unmarshal(kube, &rec.Kubeconfig); err != nil {
		return delivery.Environment{}, err
	}
	if err := json.Unmarshal(secrets, &rec.Secrets); err != nil {
		return delivery.Environment{}, err
	}
	if err := json.Unmarshal(config, &rec.Config); err != nil {
		return delivery.Environment{}, err
	}
	return delivery.RestoreEnvironment(rec)
}

func envJSON(rec delivery.EnvironmentRecord) (kube, secrets, config string, err error) {
	k, err := json.Marshal(rec.Kubeconfig)
	if err != nil {
		return "", "", "", err
	}
	sec := rec.Secrets
	if sec == nil {
		sec = []delivery.Credential{}
	}
	sj, err := json.Marshal(sec)
	if err != nil {
		return "", "", "", err
	}
	cfg := rec.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	cj, err := json.Marshal(cfg)
	if err != nil {
		return "", "", "", err
	}
	return string(k), string(sj), string(cj), nil
}

// CreateEnvironment implements delivery.Store.
func (s *Store) CreateEnvironment(ctx context.Context, m delivery.Mutation, env delivery.Environment) error {
	if err := m.Validate(); err != nil {
		return err
	}
	rec := env.Record()
	kube, sec, cfg, err := envJSON(rec)
	if err != nil {
		return err
	}
	return s.tx(ctx, rec.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO environments (`+environmentColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb, $11, $12, $13, $14)`,
			string(rec.Scope.TenantID), string(rec.Scope.ProjectID), string(rec.ID), rec.Name, string(rec.Kind), rec.Target, rec.Namespace, kube, sec, cfg,
			int64(rec.PreviewTTL/time.Second), int64(rec.Version), rec.CreatedAt, rec.UpdatedAt)
		if err != nil {
			if constraint(err) == "environments_name_unique" {
				return delivery.Fail(delivery.ErrNameTaken, err, "the environment name is used in this project")
			}
			return err
		}
		return s.audit(ctx, tx, m, auditEntry{Scope: rec.Scope, Action: "environment.create", ResourceType: "environment", ResourceID: string(rec.ID),
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "kind": string(rec.Kind)}})
	})
}

// GetEnvironment implements delivery.Store.
func (s *Store) GetEnvironment(ctx context.Context, scope domain.Scope, id domain.EnvironmentID) (env delivery.Environment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		env, e = scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM environments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		return e
	})
	return env, err
}

// ListEnvironments implements delivery.Store.
func (s *Store) ListEnvironments(ctx context.Context, scope domain.Scope, afterName string, limit int) (out []delivery.Environment, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+environmentColumns+` FROM environments
WHERE tenant_id = $1 AND project_id = $2 AND name > $3 ORDER BY name LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), afterName, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			env, e := scanEnvironment(rows)
			if e != nil {
				return e
			}
			out = append(out, env)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateEnvironment implements delivery.Store. The version comparison is part of
// the write: the row is locked, compared by the entity and updated only when
// the expected version is still current.
func (s *Store) UpdateEnvironment(ctx context.Context, m delivery.Mutation, scope domain.Scope, id domain.EnvironmentID, expected domain.Version, edit delivery.EnvironmentEdit) (out delivery.Environment, err error) {
	if err := m.Validate(); err != nil {
		return delivery.Environment{}, err
	}
	err = s.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		env, e := scanEnvironment(tx.QueryRow(ctx, `SELECT `+environmentColumns+` FROM environments WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		if e != nil {
			return e
		}
		changed, e := env.Edit(expected, edit, m.Now)
		if e != nil {
			return e
		}
		out = env
		if !changed {
			return nil
		}
		rec := env.Record()
		kube, sec, cfg, e := envJSON(rec)
		if e != nil {
			return e
		}
		tag, e := tx.Exec(ctx, `UPDATE environments SET target = $4, namespace = $5, kubeconfig = $6::jsonb, secrets = $7::jsonb, config = $8::jsonb,
preview_ttl_seconds = $9, version = $10, updated_at = $11 WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $12`,
			string(scope.TenantID), string(scope.ProjectID), string(id), rec.Target, rec.Namespace, kube, sec, cfg, int64(rec.PreviewTTL/time.Second),
			int64(rec.Version), rec.UpdatedAt, int64(expected))
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return delivery.Fail(delivery.ErrVersionConflict, nil, "the environment changed")
		}
		return s.audit(ctx, tx, m, auditEntry{Scope: scope, Action: "environment.update", ResourceType: "environment", ResourceID: string(id),
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{"fromVersion": int64(expected)}})
	})
	if err != nil {
		return delivery.Environment{}, err
	}
	return out, nil
}

// ---- snapshots -------------------------------------------------------------

const snapshotColumns = `tenant_id, project_id, environment_id, digest, kind, target, namespace, config, preview_ttl_seconds, kubeconfig, secrets, created_at`

func scanSnapshot(row pgx.Row) (delivery.ConfigurationSnapshot, error) {
	var s delivery.ConfigurationSnapshot
	var tenant, project, env, digest, kind string
	var config, secrets []byte
	var ttl int64
	if err := row.Scan(&tenant, &project, &env, &digest, &kind, &s.Target, &s.Namespace, &config, &ttl, &s.Kubeconfig, &secrets, &s.CreatedAt); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	s.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	s.Environment, s.Digest, s.Kind, s.PreviewTTL, s.CreatedAt = domain.EnvironmentID(env), runtimev1.Digest(digest), delivery.EnvironmentKind(kind), time.Duration(ttl)*time.Second, utc(s.CreatedAt)
	if err := json.Unmarshal(config, &s.Config); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	if err := json.Unmarshal(secrets, &s.Secrets); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	return s, s.Validate()
}

// RetainSnapshot implements delivery.Store: idempotent by digest.
func (s *Store) RetainSnapshot(ctx context.Context, snap delivery.ConfigurationSnapshot) (out delivery.ConfigurationSnapshot, err error) {
	if err := snap.Validate(); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	cfg, err := json.Marshal(nonNilConfig(snap.Config))
	if err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	sec, err := json.Marshal(snap.Secrets)
	if err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	err = s.tx(ctx, snap.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `INSERT INTO environment_snapshots (`+snapshotColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11::jsonb, $12) ON CONFLICT DO NOTHING`,
			string(snap.Scope.TenantID), string(snap.Scope.ProjectID), string(snap.Environment), string(snap.Digest), string(snap.Kind), snap.Target, snap.Namespace,
			string(cfg), int64(snap.PreviewTTL/time.Second), snap.Kubeconfig, string(sec), snap.CreatedAt.UTC()); e != nil {
			return e
		}
		var e error
		out, e = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM environment_snapshots
WHERE tenant_id = $1 AND project_id = $2 AND environment_id = $3 AND digest = $4`, string(snap.Scope.TenantID), string(snap.Scope.ProjectID), string(snap.Environment), string(snap.Digest)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		return e
	})
	return out, err
}

func nonNilConfig(c map[string]string) map[string]string {
	if c == nil {
		return map[string]string{}
	}
	return c
}

// GetSnapshot implements delivery.Store.
func (s *Store) GetSnapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (out delivery.ConfigurationSnapshot, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		out, e = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM environment_snapshots
WHERE tenant_id = $1 AND project_id = $2 AND environment_id = $3 AND digest = $4`, string(scope.TenantID), string(scope.ProjectID), string(env), string(digest)))
		if errors.Is(e, pgx.ErrNoRows) {
			return delivery.Fail(delivery.ErrNotFound, e, "not found")
		}
		return e
	})
	return out, err
}
