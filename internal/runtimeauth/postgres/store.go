// Package postgres is the PostgreSQL adapter of runtimeauth: module-owned
// grant and session storage, the restricted credential lookup, and the
// transaction-bound helpers the execution owner uses so cancellation and
// protected mutations share one current-authority boundary.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
	storage.Querier
}

// AttemptAuthority is what the execution owner (T-020) provides to this
// module. Both methods must read the authoritative attempt state.
// LockAttempt runs in the caller's transaction and must take a row lock that
// conflicts with the owner's cancellation (for example FOR SHARE on the
// attempt row, with cancellation taking FOR UPDATE), so that a decision made
// under it cannot interleave with a cancel. It returns runtimeauth.ErrNotFound
// when the attempt does not exist in the scope.
type AttemptAuthority interface {
	runtimeauth.AttemptObserver
	LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error)
}

// Store implements runtimeauth.Store.
type Store struct {
	db        DB
	authority AttemptAuthority
}

var _ runtimeauth.Store = (*Store)(nil)

// NewStore requires both dependencies; there is no default authority.
func NewStore(db DB, authority AttemptAuthority) (*Store, error) {
	if db == nil || authority == nil {
		return nil, errors.New("runtimeauth/postgres: a database and an attempt authority are required")
	}
	return &Store{db: db, authority: authority}, nil
}

// Locate resolves a credential digest through kodama_runtime_locate_grant.
func (s *Store) Locate(ctx context.Context, digest runtimeauth.Digest) (runtimeauth.Locator, error) {
	rows, err := s.db.Query(ctx, `SELECT tenant_id, project_id, attempt_id FROM kodama_runtime_locate_grant($1)`, digest[:])
	if err != nil {
		return runtimeauth.Locator{}, fmt.Errorf("runtimeauth/postgres: locate: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return runtimeauth.Locator{}, fmt.Errorf("runtimeauth/postgres: locate: %w", err)
		}
		return runtimeauth.Locator{}, errNoGrant()
	}
	var tenant, project, attempt string
	if err := rows.Scan(&tenant, &project, &attempt); err != nil {
		return runtimeauth.Locator{}, fmt.Errorf("runtimeauth/postgres: locate: %w", err)
	}
	return runtimeauth.Locator{
		Scope:     domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)},
		AttemptID: domain.AttemptID(attempt),
	}, nil
}

func errNoGrant() error {
	return errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no such credential"))
}

// Do runs fn in a tenant transaction (storage.WithTenant).
func (s *Store) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx runtimeauth.GrantTx) error) error {
	return storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, Bind(tx, tenant, s.authority))
	})
}

// Bind adapts a transaction that already carries the tenant context (for
// example one opened by storage.WithTenant, or the execution owner's own
// transaction) to runtimeauth.GrantTx. It is the integration seam: execution's
// cancellation and protected mutations call runtimeauth.CheckSession or
// RevokeAttempt through it, in the same transaction as their own writes.
func Bind(tx pgx.Tx, tenant domain.TenantID, authority AttemptAuthority) runtimeauth.GrantTx {
	return &grantTx{tx: tx, tenant: tenant, authority: authority}
}

// RevokeAttempt revokes every grant of the attempt in tx. The execution owner
// calls it from its cancellation transaction after locking the attempt row
// (the module-wide lock order: attempt first, grants second). tx must carry the
// tenant context of scope.TenantID.
func RevokeAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return Bind(tx, scope.TenantID, nil).RevokeAttempt(ctx, scope, attempt, now)
}

type grantTx struct {
	tx        pgx.Tx
	tenant    domain.TenantID
	authority AttemptAuthority
}

var _ runtimeauth.GrantTx = (*grantTx)(nil)

func (t *grantTx) LockAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	if t.authority == nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, errors.New("no attempt authority is bound"))
	}
	if scope.TenantID != t.tenant {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrNotFound, errors.New("another tenant"))
	}
	return t.authority.LockAttempt(ctx, t.tx, scope, attempt)
}

const grantColumns = `tenant_id, project_id, run_id, attempt_id, job_namespace, job_name, credential_digest,
	signing_generation, actions, authority_revision, context_digest, context_doc, created_at, expires_at,
	session_id, nonce_digest, bound_job_uid, bound_pod_name, bound_pod_uid, consumed_at, max_expires_at, revoked_at`

func lockClause(m runtimeauth.LockMode) string {
	switch m {
	case runtimeauth.LockShare:
		return " FOR SHARE"
	case runtimeauth.LockUpdate:
		return " FOR UPDATE"
	}
	return ""
}

func scanGrant(row pgx.Row) (runtimeauth.AdmissionGrant, error) {
	var (
		g                                       runtimeauth.AdmissionGrant
		tenant, project, run, attempt, ns, name string
		credential, contextDigest, doc          []byte
		actions                                 []string
		session, jobUID, podName, podUID        *string
		nonce                                   []byte
		created, expires                        time.Time
		consumed, maxExpires, revoked           *time.Time
	)
	if err := row.Scan(&tenant, &project, &run, &attempt, &ns, &name, &credential, &g.Generation, &actions, &g.AuthorityRevision,
		&contextDigest, &doc, &created, &expires, &session, &nonce, &jobUID, &podName, &podUID, &consumed, &maxExpires, &revoked); err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	var err error
	g.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	g.RunID, g.AttemptID, g.Job = domain.RunID(run), domain.AttemptID(attempt), runtimeauth.ExpectedJob{Namespace: ns, Name: name}
	if g.CredentialDigest, err = runtimeauth.DigestFromBytes(credential); err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	if g.ContextDigest, err = runtimeauth.DigestFromBytes(contextDigest); err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	if g.Actions, err = runtimeauth.ParseActionSet(actions); err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	g.Context, g.CreatedAt, g.ExpiresAt = doc, created.UTC(), expires.UTC()
	if session != nil {
		c := &runtimeauth.Consumption{
			SessionID: runtimeauth.SessionID(*session), ConsumedAt: consumed.UTC(), MaxExpiresAt: maxExpires.UTC(),
			Pod: runtimeauth.PodBinding{JobUID: *jobUID, PodName: *podName, PodUID: *podUID},
		}
		if c.NonceDigest, err = runtimeauth.DigestFromBytes(nonce); err != nil {
			return runtimeauth.AdmissionGrant{}, err
		}
		g.Consumption = c
	}
	if revoked != nil {
		r := revoked.UTC()
		g.RevokedAt = &r
	}
	return g, nil
}

func (t *grantTx) one(ctx context.Context, where string, arg any, mode runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	g, err := scanGrant(t.tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM runtime_grants WHERE tenant_id = $1 AND `+where+lockClause(mode), string(t.tenant), arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("no such grant"))
	}
	if err != nil {
		return runtimeauth.AdmissionGrant{}, fmt.Errorf("runtimeauth/postgres: read grant: %w", err)
	}
	return g, nil
}

func (t *grantTx) GrantByCredential(ctx context.Context, d runtimeauth.Digest, mode runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	return t.one(ctx, `credential_digest = $2`, d[:], mode)
}

func (t *grantTx) GrantBySession(ctx context.Context, s runtimeauth.SessionID, mode runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	if err := s.Validate(); err != nil {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("malformed session"))
	}
	return t.one(ctx, `session_id = $2`, string(s), mode)
}

func isPg(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func (t *grantTx) InsertGrant(ctx context.Context, g runtimeauth.AdmissionGrant) (runtimeauth.AdmissionGrant, bool, error) {
	if g.Scope.TenantID != t.tenant {
		return runtimeauth.AdmissionGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	tag, err := t.tx.Exec(ctx, `INSERT INTO runtime_grants
		(tenant_id, project_id, run_id, attempt_id, job_namespace, job_name, credential_digest, signing_generation, actions,
		 authority_revision, context_digest, context_doc, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (tenant_id, attempt_id) DO NOTHING`,
		string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.RunID), string(g.AttemptID), g.Job.Namespace, g.Job.Name,
		g.CredentialDigest[:], g.Generation, g.Actions.Strings(), g.AuthorityRevision, g.ContextDigest[:], g.Context, g.CreatedAt, g.ExpiresAt)
	switch {
	case isPg(err, "23505"): // the credential digest belongs to another attempt
		return runtimeauth.AdmissionGrant{}, false, errors.Join(runtimeauth.ErrConflict, errors.New("credential already registered elsewhere"))
	case isPg(err, "23503"):
		return runtimeauth.AdmissionGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("unknown project"))
	case err != nil:
		return runtimeauth.AdmissionGrant{}, false, fmt.Errorf("runtimeauth/postgres: insert grant: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return g, true, nil
	}
	existing, err := t.one(ctx, `attempt_id = $2`, string(g.AttemptID), runtimeauth.LockShare)
	if err != nil {
		return runtimeauth.AdmissionGrant{}, false, err
	}
	return existing, false, nil
}

func (t *grantTx) SaveConsumption(ctx context.Context, g runtimeauth.AdmissionGrant) error {
	c := g.Consumption
	if c == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no consumption to save"))
	}
	tag, err := t.tx.Exec(ctx, `UPDATE runtime_grants SET session_id = $3, nonce_digest = $4, bound_job_uid = $5, bound_pod_name = $6,
		bound_pod_uid = $7, consumed_at = $8, max_expires_at = $9
		WHERE tenant_id = $1 AND attempt_id = $2 AND session_id IS NULL AND revoked_at IS NULL`,
		string(t.tenant), string(g.AttemptID), string(c.SessionID), c.NonceDigest[:], c.Pod.JobUID, c.Pod.PodName, c.Pod.PodUID, c.ConsumedAt, c.MaxExpiresAt)
	if err != nil {
		return fmt.Errorf("runtimeauth/postgres: save consumption: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.Join(runtimeauth.ErrConflict, errors.New("the grant is not consumable"))
	}
	return nil
}

func (t *grantTx) SaveRevocation(ctx context.Context, g runtimeauth.AdmissionGrant) error {
	if g.RevokedAt == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no revocation to save"))
	}
	if _, err := t.tx.Exec(ctx, `UPDATE runtime_grants SET revoked_at = $3 WHERE tenant_id = $1 AND attempt_id = $2 AND revoked_at IS NULL`,
		string(t.tenant), string(g.AttemptID), *g.RevokedAt); err != nil {
		return fmt.Errorf("runtimeauth/postgres: save revocation: %w", err)
	}
	return nil
}

func (t *grantTx) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	if scope.TenantID != t.tenant {
		return 0, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	tag, err := t.tx.Exec(ctx, `UPDATE runtime_grants SET revoked_at = GREATEST($4::timestamptz, created_at)
		WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND revoked_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt), now.UTC())
	if err != nil {
		return 0, fmt.Errorf("runtimeauth/postgres: revoke attempt: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
