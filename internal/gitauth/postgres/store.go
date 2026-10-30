// Package postgres is the PostgreSQL adapter of gitauth: the git_grants table
// (migration 008) behind gitauth.Store, with tenant predicates on every
// statement over a row-level-security tenant context, and the transaction seam
// (Bind, RevokeAttempt, ConsumeSeed) the execution owner and the push
// authorizer use so their protected mutations and the grant change commit
// together.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
}

// Store implements gitauth.Store.
type Store struct{ db DB }

var _ gitauth.Store = (*Store)(nil)

// NewStore requires the runtime pool.
func NewStore(db DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("gitauth/postgres: a database is required")
	}
	return &Store{db: db}, nil
}

// Do runs fn in a tenant transaction (storage.WithTenant). A failure after fn
// succeeded (the commit) has an unknown outcome and is reported as
// ErrUnavailable; a database failure inside fn is ErrUnavailable too.
func (s *Store) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitauth.GrantTx) error) error {
	ran := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, Bind(tx, tenant)); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		return errors.Join(gitauth.ErrUnavailable, err)
	}
	return err
}

// Bind adapts a transaction that already carries the tenant context (one opened
// by storage.WithTenant, or the execution owner's own) to gitauth.GrantTx. It is
// the integration seam: execution's cancellation and the push authorizer's
// authorization call gitauth.RevokeAttemptIn or gitauth.ConsumeSeedIn through it,
// in the same transaction as their own writes.
func Bind(tx pgx.Tx, tenant domain.TenantID) gitauth.GrantTx { return &grantTx{tx: tx, tenant: tenant} }

// RevokeAttempt revokes every grant of the attempt in tx (which must carry the
// tenant context of scope.TenantID) and returns how many it revoked. The
// execution owner calls it from its cancellation transaction after locking the
// attempt row (the module-wide lock order: attempt first, grants second).
func RevokeAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return gitauth.RevokeAttemptIn(ctx, Bind(tx, scope.TenantID), scope, attempt, now)
}

// RevokeAll revokes every grant of tenant in tx (which must carry that tenant's
// context) and appends the audit row in the same transaction, for the
// offboarding owner's transaction. It returns how many grants it revoked; zero
// means none was left and nothing was recorded (see gitauth.RevokeAllIn).
func RevokeAll(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, reason string, actor gitauth.AuditActor, now time.Time) (int, error) {
	id, err := gitauth.NewAuditID(nil)
	if err != nil {
		return 0, err
	}
	return gitauth.RevokeAllIn(ctx, Bind(tx, tenant), reason, actor, now, id)
}

// ConsumeSeed consumes a seed grant for one receive in tx, the push authorizer's
// authorization transaction (see gitauth.ConsumeSeedIn).
func ConsumeSeed(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, grantID, receiveID string, now time.Time) error {
	return gitauth.ConsumeSeedIn(ctx, Bind(tx, tenant), grantID, receiveID, now)
}

type grantTx struct {
	tx     pgx.Tx
	tenant domain.TenantID
}

var _ gitauth.GrantTx = (*grantTx)(nil)

func isPg(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func dbErr(what string, err error) error {
	return fmt.Errorf("%w: gitauth/postgres: %s: %w", gitauth.ErrUnavailable, what, err)
}

func (t *grantTx) LockSeed(ctx context.Context, repositoryID string) error {
	if _, err := t.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "kodama-git-seed:"+string(t.tenant)+":"+repositoryID); err != nil {
		return dbErr("seed lock", err)
	}
	return nil
}

const grantColumns = `claims, credential_digest, issue_key, consumed_receive_id, consumed_at, revoked_at`

func (t *grantTx) one(ctx context.Context, where string, arg any, lock bool) (gitauth.Grant, error) {
	q := `SELECT ` + grantColumns + ` FROM git_grants WHERE tenant_id = $1 AND ` + where
	if lock {
		q += ` FOR UPDATE`
	}
	var (
		claims, digest   []byte
		issueKey         *string
		receive          *string
		consumed, revoke *time.Time
	)
	err := t.tx.QueryRow(ctx, q, string(t.tenant), arg).Scan(&claims, &digest, &issueKey, &receive, &consumed, &revoke)
	if errors.Is(err, pgx.ErrNoRows) {
		return gitauth.Grant{}, gitauth.ErrNotFound
	}
	if err != nil {
		return gitauth.Grant{}, dbErr("read grant", err)
	}
	g := gitauth.Grant{}
	// Claims are decoded strictly even when read from storage.
	if g.Claims, err = gitv1.DecodeRequest[gitv1.GrantClaims](claims); err != nil {
		return gitauth.Grant{}, dbErr("stored claims", err)
	}
	if g.Digest, err = gitauth.DigestFromBytes(digest); err != nil {
		return gitauth.Grant{}, dbErr("stored digest", err)
	}
	if issueKey != nil {
		g.IssueKey = *issueKey
	}
	if receive != nil && consumed != nil {
		g.Consumption = &gitauth.Consumption{ReceiveID: *receive, At: consumed.UTC()}
	}
	if revoke != nil {
		r := revoke.UTC()
		g.RevokedAt = &r
	}
	return g, nil
}

func (t *grantTx) GrantByDigest(ctx context.Context, d gitauth.Digest) (gitauth.Grant, error) {
	return t.one(ctx, `credential_digest = $2`, d[:], false)
}

func (t *grantTx) GrantByID(ctx context.Context, id string, lock bool) (gitauth.Grant, error) {
	return t.one(ctx, `id = $2`, id, lock)
}

func (t *grantTx) GrantByIssueKey(ctx context.Context, key string, lock bool) (gitauth.Grant, error) {
	return t.one(ctx, `issue_key = $2`, key, lock)
}

func (t *grantTx) Insert(ctx context.Context, g gitauth.Grant) error {
	if g.Claims.TenantID != string(t.tenant) {
		return errors.New("gitauth/postgres: a grant of another tenant cannot be stored")
	}
	if err := g.Claims.Validate(); err != nil || g.Digest.IsZero() {
		return fmt.Errorf("gitauth/postgres: invalid grant: %v", err)
	}
	claims, err := json.Marshal(g.Claims)
	if err != nil {
		return fmt.Errorf("gitauth/postgres: claims: %w", err)
	}
	var issueKey *string
	if g.IssueKey != "" {
		issueKey = &g.IssueKey
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO git_grants
		(tenant_id, id, project_id, kind, actor_type, actor_id, run_id, attempt_id, repository_ids, claims, credential_digest,
		 issue_key, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		g.Claims.TenantID, g.ID(), g.Claims.ProjectID, string(g.Claims.Kind), string(g.Claims.Actor.Type), g.Claims.Actor.ID,
		g.Claims.Actor.RunID, g.Claims.Actor.AttemptID, g.RepositoryIDs(), claims, g.Digest[:], issueKey, g.IssuedAt(), g.ExpiresAt())
	switch {
	case isPg(err, "23505"):
		return errors.Join(gitauth.ErrConflict, errors.New("duplicate grant, credential or issue key"))
	case isPg(err, "23503"):
		return errors.Join(gitauth.ErrNotFound, errors.New("unknown project"))
	case err != nil:
		return dbErr("insert grant", err)
	}
	return nil
}

func (t *grantTx) Update(ctx context.Context, g gitauth.Grant) error {
	var receive *string
	var consumed *time.Time
	if g.Consumption != nil {
		receive, consumed = &g.Consumption.ReceiveID, &g.Consumption.At
	}
	tag, err := t.tx.Exec(ctx, `UPDATE git_grants SET credential_digest = $3, consumed_receive_id = $4, consumed_at = $5, revoked_at = $6
		WHERE tenant_id = $1 AND id = $2`,
		string(t.tenant), g.ID(), g.Digest[:], receive, consumed, g.RevokedAt)
	switch {
	case isPg(err, "23514"):
		return fmt.Errorf("gitauth/postgres: update refused by a transition guard: %w", err)
	case err != nil:
		return dbErr("update grant", err)
	case tag.RowsAffected() != 1:
		return gitauth.ErrNotFound
	}
	return nil
}

func (t *grantTx) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	if scope.TenantID != t.tenant {
		return 0, errors.New("gitauth/postgres: another tenant")
	}
	tag, err := t.tx.Exec(ctx, `UPDATE git_grants SET revoked_at = GREATEST($4::timestamptz, issued_at)
		WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND revoked_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), string(attempt), now.UTC())
	if err != nil {
		return 0, dbErr("revoke attempt", err)
	}
	return int(tag.RowsAffected()), nil
}

func (t *grantTx) RevokeSeeds(ctx context.Context, repositoryID string, now time.Time) (int, error) {
	tag, err := t.tx.Exec(ctx, `UPDATE git_grants SET revoked_at = GREATEST($3::timestamptz, issued_at)
		WHERE tenant_id = $1 AND kind = 'seed' AND consumed_at IS NULL AND revoked_at IS NULL
		  AND repository_ids @> ARRAY[$2::text]`,
		string(t.tenant), repositoryID, now.UTC())
	if err != nil {
		return 0, dbErr("revoke seeds", err)
	}
	return int(tag.RowsAffected()), nil
}

func (t *grantTx) RevokeTenant(ctx context.Context, now time.Time) (int, error) {
	tag, err := t.tx.Exec(ctx, `UPDATE git_grants SET revoked_at = GREATEST($2::timestamptz, issued_at)
		WHERE tenant_id = $1 AND revoked_at IS NULL`, string(t.tenant), now.UTC())
	if err != nil {
		return 0, dbErr("revoke tenant", err)
	}
	return int(tag.RowsAffected()), nil
}

func (t *grantTx) ActiveGrants(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := t.tx.QueryRow(ctx, `SELECT count(*) FROM git_grants WHERE tenant_id = $1 AND revoked_at IS NULL AND expires_at > $2::timestamptz`,
		string(t.tenant), now.UTC()).Scan(&n)
	if err != nil {
		return 0, dbErr("count active grants", err)
	}
	return n, nil
}

// RecordAudit appends the record to the shared audit table: the resource is the
// tenant's grants, with no project and no secret in the metadata.
func (t *grantTx) RecordAudit(ctx context.Context, r gitauth.AuditRecord) error {
	raw, err := json.Marshal(map[string]any{"revoked": r.Revoked})
	if err != nil {
		return fmt.Errorf("gitauth/postgres: audit metadata: %w", err)
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, reason, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $1, $8, $9)`,
		string(t.tenant), r.ID, r.OccurredAt.UTC(), string(r.Actor.Type), r.Actor.ID, r.Action, gitauth.AuditResourceType, r.Reason, raw)
	switch {
	case isPg(err, "23505"):
		return errors.Join(gitauth.ErrConflict, errors.New("duplicate audit record"))
	case err != nil:
		return dbErr("write audit", err)
	}
	return nil
}
