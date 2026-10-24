// Package postgres is the PostgreSQL adapter of the variables and secrets
// module: tenant-scoped persistence of the module's entities, the atomic write
// unit shared with the HTTP idempotency record, run pin registration, and the
// standalone encrypted-value adapter behind the version-resolution port.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant (and project) predicates, and is additionally
// confined by forced row level security and composite foreign keys. Key-provider
// calls never happen inside a transaction: the service prepares ciphertext and
// wraps first, and the transaction only rechecks versions, applies entity
// transitions, and writes audit and event rows.
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
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/storage"
)

// EventAppender appends an event to the outbox inside the caller's tenant
// transaction. EventsFrom adapts the event bus's Append; it must join tx and
// never commit it. A full backlog must return an error wrapping
// secrets.ErrBackpressure, which rolls the business change back.
type EventAppender func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error

// Config configures a Store.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool (*storage.Pool).
	DB     storage.TxBeginner
	Events EventAppender
	IDs    secrets.IDGenerator
	// Clock defaults to the UTC wall clock.
	Clock func() time.Time
}

// Store implements secrets.Store, secrets.VariableCatalog and
// secrets.SecretCatalog over PostgreSQL.
type Store struct {
	db     storage.TxBeginner
	events EventAppender
	ids    secrets.IDGenerator
	now    func() time.Time
}

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Events == nil || cfg.IDs == nil {
		return nil, errors.New("secrets/postgres: DB, Events and IDs are required")
	}
	s := &Store{db: cfg.DB, events: cfg.Events, ids: cfg.IDs, now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

var (
	_ secrets.Store           = (*Store)(nil)
	_ secrets.VariableCatalog = (*Store)(nil)
	_ secrets.SecretCatalog   = (*Store)(nil)
)

// Atomically runs fn in one tenant transaction. The Writer is valid only inside
// fn.
func (s *Store) Atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, w secrets.Writer) error) error {
	done := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, &writer{s: s, tx: tx, tenant: tenant}); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err != nil && done {
		// fn succeeded and only the commit failed: the change may or may not be
		// durable. Never report success; the caller re-reads before retrying.
		return fmt.Errorf("%w: commit outcome unknown: %v", secrets.ErrUnavailable, err)
	}
	return classify(err)
}

// read runs a read-only unit.
func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	err := storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
	return classify(err)
}

// classify maps adapter failures to the module's sentinels. Entity and domain
// errors pass through unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40001", "40P01", "55P03", "57014", "53300":
			return fmt.Errorf("%w: %v", secrets.ErrUnavailable, err)
		}
	}
	return err
}

func isUnique(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && (constraint == "" || pg.ConstraintName == constraint)
}

func isForeignKey(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

// ---- reads over any transaction -------------------------------------------

type rowScanner interface{ Scan(dest ...any) error }

const variableColumns = `tenant_id, project_id, id, name, type, description, value_text, value_version, version, created_at, updated_at, deleted_at`

func scanVariable(r rowScanner) (secrets.Variable, error) {
	var (
		rec             secrets.VariableRecord
		tenant, project string
		typ             string
		version         int64
	)
	if err := r.Scan(&tenant, &project, &rec.ID, &rec.Name, &typ, &rec.Description, &rec.ValueText, &rec.ValueVersion, &version,
		&rec.CreatedAt, &rec.UpdatedAt, &rec.DeletedAt); err != nil {
		return secrets.Variable{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.Type, rec.Version = secrets.VariableType(typ), domain.Version(version)
	v, err := secrets.RestoreVariable(rec)
	if err != nil {
		return secrets.Variable{}, fmt.Errorf("%w: variable row: %v", secrets.ErrInconsistent, err)
	}
	return v, nil
}

func getVariable(ctx context.Context, tx pgx.Tx, scope domain.Scope, id string, lock bool) (secrets.Variable, error) {
	q := `SELECT ` + variableColumns + ` FROM variables WHERE tenant_id = $1 AND project_id = $2 AND id = $3`
	if lock {
		q += ` FOR UPDATE`
	}
	v, err := scanVariable(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Variable{}, secrets.ErrNotFound
	}
	return v, err
}

func listVariables(ctx context.Context, tx pgx.Tx, scope domain.Scope, afterName string, limit int) ([]secrets.Variable, error) {
	rows, err := tx.Query(ctx, `SELECT `+variableColumns+` FROM variables
WHERE tenant_id = $1 AND project_id = $2 AND deleted_at IS NULL AND name > $3
ORDER BY name LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), afterName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.Variable
	for rows.Next() {
		v, err := scanVariable(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const secretColumns = `tenant_id, project_id, id, name, kind, current_version, version, created_at, updated_at, deleted_at`

func scanSecret(r rowScanner) (secrets.Secret, error) {
	var (
		rec             secrets.SecretRecord
		tenant, project string
		kind            string
		version         int64
	)
	if err := r.Scan(&tenant, &project, &rec.ID, &rec.Name, &kind, &rec.CurrentVersion, &version, &rec.CreatedAt, &rec.UpdatedAt, &rec.DeletedAt); err != nil {
		return secrets.Secret{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.Kind, rec.Version = secrets.SecretKind(kind), domain.Version(version)
	s, err := secrets.RestoreSecret(rec)
	if err != nil {
		return secrets.Secret{}, fmt.Errorf("%w: secret row: %v", secrets.ErrInconsistent, err)
	}
	return s, nil
}

func getSecret(ctx context.Context, tx pgx.Tx, scope domain.Scope, id, lockMode string) (secrets.Secret, error) {
	q := `SELECT ` + secretColumns + ` FROM secrets WHERE tenant_id = $1 AND project_id = $2 AND id = $3` + lockMode
	s, err := scanSecret(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return s, err
}

func getSecretByName(ctx context.Context, tx pgx.Tx, scope domain.Scope, name, lockMode string) (secrets.Secret, error) {
	q := `SELECT ` + secretColumns + ` FROM secrets WHERE tenant_id = $1 AND project_id = $2 AND name = $3` + lockMode
	s, err := scanSecret(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return s, err
}

func listSecrets(ctx context.Context, tx pgx.Tx, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error) {
	rows, err := tx.Query(ctx, `SELECT `+secretColumns+` FROM secrets
WHERE tenant_id = $1 AND project_id = $2 AND deleted_at IS NULL AND name > $3
ORDER BY name LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), afterName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.Secret
	for rows.Next() {
		s, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const versionColumns = `tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by, revoked_at, revoked_by`

func scanVersion(r rowScanner) (secrets.SecretVersion, error) {
	var (
		rec             secrets.SecretVersionRecord
		tenant, project string
		format          int
		revokedBy       *string
	)
	if err := r.Scan(&tenant, &project, &rec.SecretID, &rec.Version, &rec.ScopeKeyID, &rec.KeyGeneration, &format,
		&rec.Envelope.Algorithm, &rec.Envelope.Nonce, &rec.Envelope.Ciphertext, &rec.CreatedAt, &rec.CreatedBy, &rec.RevokedAt, &revokedBy); err != nil {
		return secrets.SecretVersion{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.Envelope.FormatVersion = format
	if revokedBy != nil {
		rec.RevokedBy = *revokedBy
	}
	v, err := secrets.RestoreSecretVersion(rec)
	if err != nil {
		return secrets.SecretVersion{}, fmt.Errorf("%w: secret version row: %v", secrets.ErrInconsistent, err)
	}
	return v, nil
}

func getVersion(ctx context.Context, tx pgx.Tx, scope domain.Scope, secretID string, version int64, lockMode string) (secrets.SecretVersion, error) {
	q := `SELECT ` + versionColumns + ` FROM secret_versions WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND version = $4` + lockMode
	v, err := scanVersion(tx.QueryRow(ctx, q, string(scope.TenantID), string(scope.ProjectID), secretID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.SecretVersion{}, secrets.ErrNotFound
	}
	return v, err
}

func listVersions(ctx context.Context, tx pgx.Tx, scope domain.Scope, secretID string, after int64, limit int) ([]secrets.SecretVersion, error) {
	rows, err := tx.Query(ctx, `SELECT `+versionColumns+` FROM secret_versions
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND version > $4
ORDER BY version LIMIT $5`, string(scope.TenantID), string(scope.ProjectID), secretID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.SecretVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

const scopeKeySelect = `SELECT k.tenant_id, k.id, k.generation, k.current_revision, k.created_at,
       w.wrapped_key, w.wrap_algorithm, w.kek_provider, w.kek_id, w.kek_version, w.wrapped_at
FROM secret_scope_keys k
JOIN secret_scope_key_wraps w ON w.tenant_id = k.tenant_id AND w.key_id = k.id AND w.revision = k.current_revision
WHERE k.tenant_id = $1`

func scanScopeKey(r rowScanner) (secrets.ScopeKey, error) {
	var (
		rec    secrets.ScopeKeyRecord
		tenant string
		gen    int32
	)
	if err := r.Scan(&tenant, &rec.ID, &gen, &rec.CurrentRevision, &rec.CreatedAt,
		&rec.Wrapped.Bytes, &rec.Wrapped.Algorithm, &rec.Wrapped.KEK.Provider, &rec.Wrapped.KEK.ID, &rec.Wrapped.KEK.Version, &rec.WrappedAt); err != nil {
		return secrets.ScopeKey{}, err
	}
	rec.TenantID, rec.Generation = domain.TenantID(tenant), int64(gen)
	k, err := secrets.RestoreScopeKey(rec)
	if err != nil {
		return secrets.ScopeKey{}, fmt.Errorf("%w: scope key row: %v", secrets.ErrInconsistent, err)
	}
	return k, nil
}

func activeScopeKey(ctx context.Context, tx pgx.Tx, tenant domain.TenantID) (secrets.ScopeKey, error) {
	q := scopeKeySelect + ` ORDER BY k.generation DESC LIMIT 1`
	k, err := scanScopeKey(tx.QueryRow(ctx, q, string(tenant)))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return k, err
}

func getScopeKey(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, id string, lock bool) (secrets.ScopeKey, error) {
	if lock {
		// Lock the key row on its own first. Locking it inside the join below
		// would re-evaluate the join after a concurrent rewrap committed, and the
		// new wrap row is not in this statement's snapshot, so the key would
		// look missing. The statement after the lock reads the committed state.
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM secret_scope_keys WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, string(tenant), id).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return secrets.ScopeKey{}, secrets.ErrNotFound
		}
		if err != nil {
			return secrets.ScopeKey{}, err
		}
	}
	q := scopeKeySelect + ` AND k.id = $2`
	k, err := scanScopeKey(tx.QueryRow(ctx, q, string(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return k, err
}

func retentionReport(ctx context.Context, tx pgx.Tx, scope domain.Scope, secretID string) (secrets.RetentionReport, error) {
	r := secrets.RetentionReport{BackupRetention: secrets.BackupRetentionUnresolved}
	err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE released_at IS NULL) FROM secret_pins
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3`, string(scope.TenantID), string(scope.ProjectID), secretID).Scan(&r.PinnedVersions, &r.ActivePins)
	if err != nil {
		return r, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM secret_consumer_handling
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND state = 'pending'`, string(scope.TenantID), string(scope.ProjectID), secretID).Scan(&r.PendingHandling)
	return r, err
}

const handlingColumns = `tenant_id, project_id, id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at, resolved_at, resolved_by`

func scanHandling(r rowScanner) (secrets.ConsumerHandling, error) {
	var (
		rec             secrets.ConsumerHandlingRecord
		tenant, project string
		effect, state   string
		resolvedBy      *string
	)
	if err := r.Scan(&tenant, &project, &rec.ID, &rec.SecretID, &rec.SecretVersion, &rec.Consumer.Kind, &rec.Consumer.ID,
		&effect, &state, &rec.Delivered, &rec.CreatedAt, &rec.ResolvedAt, &resolvedBy); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.Effect, rec.State = secrets.HandlingEffect(effect), secrets.HandlingState(state)
	if resolvedBy != nil {
		rec.ResolvedBy = *resolvedBy
	}
	h, err := secrets.RestoreConsumerHandling(rec)
	if err != nil {
		return secrets.ConsumerHandling{}, fmt.Errorf("%w: handling row: %v", secrets.ErrInconsistent, err)
	}
	return h, nil
}

func pendingHandling(ctx context.Context, tx pgx.Tx, scope domain.Scope, afterID string, limit int) ([]secrets.ConsumerHandling, error) {
	rows, err := tx.Query(ctx, `SELECT `+handlingColumns+` FROM secret_consumer_handling
WHERE tenant_id = $1 AND project_id = $2 AND state = 'pending' AND id > $3 ORDER BY id LIMIT $4`,
		string(scope.TenantID), string(scope.ProjectID), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.ConsumerHandling
	for rows.Next() {
		h, err := scanHandling(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func versionHandling(ctx context.Context, tx pgx.Tx, scope domain.Scope, secretID string, version int64) ([]secrets.ConsumerHandling, error) {
	rows, err := tx.Query(ctx, `SELECT `+handlingColumns+` FROM secret_consumer_handling
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND secret_version = $4 ORDER BY id`,
		string(scope.TenantID), string(scope.ProjectID), secretID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.ConsumerHandling
	for rows.Next() {
		h, err := scanHandling(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- secrets.Reader on the Store (each call is its own read-only unit) -----

func (s *Store) GetVariable(ctx context.Context, scope domain.Scope, id string) (v secrets.Variable, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) { v, e = getVariable(ctx, tx, scope, id, false); return })
	return v, err
}

func (s *Store) ListVariables(ctx context.Context, scope domain.Scope, afterName string, limit int) (out []secrets.Variable, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		out, e = listVariables(ctx, tx, scope, afterName, limit)
		return
	})
	return out, err
}

func (s *Store) GetSecret(ctx context.Context, scope domain.Scope, id string) (sec secrets.Secret, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) { sec, e = getSecret(ctx, tx, scope, id, ""); return })
	return sec, err
}

func (s *Store) ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) (out []secrets.Secret, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		out, e = listSecrets(ctx, tx, scope, afterName, limit)
		return
	})
	return out, err
}

func (s *Store) GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (v secrets.SecretVersion, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		v, e = getVersion(ctx, tx, scope, secretID, version, "")
		return
	})
	return v, err
}

func (s *Store) ListSecretVersions(ctx context.Context, scope domain.Scope, secretID string, after int64, limit int) (out []secrets.SecretVersion, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		out, e = listVersions(ctx, tx, scope, secretID, after, limit)
		return
	})
	return out, err
}

func (s *Store) ActiveScopeKey(ctx context.Context, tenant domain.TenantID) (k secrets.ScopeKey, err error) {
	err = s.read(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (e error) { k, e = activeScopeKey(ctx, tx, tenant); return })
	return k, err
}

func (s *Store) GetScopeKey(ctx context.Context, tenant domain.TenantID, id string) (k secrets.ScopeKey, err error) {
	err = s.read(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (e error) { k, e = getScopeKey(ctx, tx, tenant, id, false); return })
	return k, err
}

func (s *Store) RetentionReport(ctx context.Context, scope domain.Scope, secretID string) (r secrets.RetentionReport, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		if _, e = getSecret(ctx, tx, scope, secretID, ""); e != nil {
			return e
		}
		r, e = retentionReport(ctx, tx, scope, secretID)
		return
	})
	return r, err
}

func (s *Store) PendingHandling(ctx context.Context, scope domain.Scope, afterID string, limit int) (out []secrets.ConsumerHandling, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (e error) {
		out, e = pendingHandling(ctx, tx, scope, afterID, limit)
		return
	})
	return out, err
}

// ActiveVariable implements secrets.VariableCatalog.
func (s *Store) ActiveVariable(ctx context.Context, scope domain.Scope, name string) (v secrets.Variable, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		v, e = scanVariable(tx.QueryRow(ctx, `SELECT `+variableColumns+` FROM variables
WHERE tenant_id = $1 AND project_id = $2 AND name = $3 AND deleted_at IS NULL`, string(scope.TenantID), string(scope.ProjectID), name))
		if errors.Is(e, pgx.ErrNoRows) {
			return secrets.ErrNotFound
		}
		return e
	})
	return v, err
}

// CurrentVersion implements secrets.SecretCatalog: the highest value version of
// the named, non-deleted secret, unless it is revoked.
func (s *Store) CurrentVersion(ctx context.Context, scope domain.Scope, name string) (version int64, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sec, e := getSecretByName(ctx, tx, scope, name, "")
		if e != nil {
			return e
		}
		if sec.Deleted() {
			return secrets.ErrNotFound
		}
		v, e := getVersion(ctx, tx, scope, sec.ID(), sec.CurrentVersion(), "")
		if e != nil {
			return e
		}
		if v.Revoked() {
			return secrets.ErrNoActiveVersion
		}
		version = v.Version()
		return nil
	})
	return version, err
}

// EnsureScopeKey stores candidate unless its generation is already committed.
func (s *Store) EnsureScopeKey(ctx context.Context, m secrets.Mutation, candidate secrets.ScopeKey) (committed secrets.ScopeKey, created bool, err error) {
	if err := m.Validate(); err != nil {
		return secrets.ScopeKey{}, false, err
	}
	rec := candidate.Record()
	if rec.CurrentRevision != 1 {
		return secrets.ScopeKey{}, false, fmt.Errorf("%w: a new scope key starts at wrapping revision 1", secrets.ErrInvalid)
	}
	done := false
	err = storage.WithTenant(ctx, s.db, rec.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO secret_scope_keys (tenant_id, id, generation, current_revision, created_at)
VALUES ($1, $2, $3, 1, $4) ON CONFLICT (tenant_id, generation) DO NOTHING`,
			string(rec.TenantID), rec.ID, rec.Generation, rec.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Another creator committed this generation first.
			committed, err = scanScopeKey(tx.QueryRow(ctx, scopeKeySelect+` AND k.generation = $2`, string(rec.TenantID), rec.Generation))
			return err
		}
		if err := insertWrap(ctx, tx, rec.TenantID, rec.ID, 1, rec.Wrapped, rec.WrappedAt); err != nil {
			return err
		}
		created, committed = true, candidate
		if err := s.audit(ctx, tx, m, auditEntry{
			Scope: domain.Scope{TenantID: rec.TenantID}, Action: "secret_scope_key.create", ResourceType: "secret_scope_key", ResourceID: rec.ID,
			ResourceVersion: 1, Metadata: map[string]any{"generation": rec.Generation, "kek": kekMeta(rec.Wrapped.KEK)},
		}); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err != nil {
		if done {
			// Only the commit failed. The key may be durable; the next attempt
			// finds it through ActiveScopeKey, and this candidate is discarded.
			return secrets.ScopeKey{}, false, fmt.Errorf("%w: commit outcome unknown: %v", secrets.ErrUnavailable, err)
		}
		return secrets.ScopeKey{}, false, classify(err)
	}
	return committed, created, nil
}

func kekMeta(r secrets.KEKRef) map[string]any {
	return map[string]any{"provider": r.Provider, "id": r.ID, "version": r.Version}
}

func insertWrap(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, keyID string, revision int64, w secrets.WrappedKey, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO secret_scope_key_wraps
(tenant_id, key_id, revision, wrapped_key, wrap_algorithm, kek_provider, kek_id, kek_version, wrapped_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		string(tenant), keyID, revision, w.Bytes, w.Algorithm, w.KEK.Provider, w.KEK.ID, w.KEK.Version, at)
	return err
}
