package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

// ResourceAuthority is what the execution owner provides to this module for
// run resources (T-116 bridges it to execution's committed run_resources record):
// the authoritative state of one preparation or service. LockResource runs in the
// caller's transaction and must take a row lock that conflicts with the owner's
// stop intent and the run's cancellation (for example FOR SHARE on the run row,
// with those transitions taking FOR UPDATE), so a decision made under it cannot
// interleave with a stop. It returns runtimeauth.ErrNotFound when the resource
// does not exist in the scope.
type ResourceAuthority interface {
	runtimeauth.ResourceObserver
	LockResource(ctx context.Context, tx pgx.Tx, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error)
}

// ResourceStore implements runtimeauth.ResourceStore.
type ResourceStore struct {
	db        DB
	authority ResourceAuthority
}

var _ runtimeauth.ResourceStore = (*ResourceStore)(nil)

// NewResourceStore requires both dependencies; there is no default authority.
func NewResourceStore(db DB, authority ResourceAuthority) (*ResourceStore, error) {
	if db == nil || authority == nil {
		return nil, errors.New("runtimeauth/postgres: a database and a resource authority are required")
	}
	return &ResourceStore{db: db, authority: authority}, nil
}

// LocateResource resolves a credential digest through
// kodama_runtime_locate_resource_grant.
func (s *ResourceStore) LocateResource(ctx context.Context, digest runtimeauth.Digest) (runtimeauth.ResourceLocator, error) {
	rows, err := s.db.Query(ctx, `SELECT tenant_id, project_id, run_id, resource_key FROM kodama_runtime_locate_resource_grant($1)`, digest[:])
	if err != nil {
		return runtimeauth.ResourceLocator{}, fmt.Errorf("runtimeauth/postgres: locate resource: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return runtimeauth.ResourceLocator{}, fmt.Errorf("runtimeauth/postgres: locate resource: %w", err)
		}
		return runtimeauth.ResourceLocator{}, errNoGrant()
	}
	var tenant, project, run, key string
	if err := rows.Scan(&tenant, &project, &run, &key); err != nil {
		return runtimeauth.ResourceLocator{}, fmt.Errorf("runtimeauth/postgres: locate resource: %w", err)
	}
	return runtimeauth.ResourceLocator{Ref: runtimeauth.ResourceRef{
		Scope: domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}, RunID: domain.RunID(run), Key: key,
	}}, nil
}

// DoResource runs fn in a tenant transaction (storage.WithTenant).
func (s *ResourceStore) DoResource(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx runtimeauth.ResourceTx) error) error {
	return storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, BindResource(tx, tenant, s.authority))
	})
}

// BindResource adapts a transaction that already carries the tenant context (one
// opened by storage.WithTenant, or the execution owner's own) to
// runtimeauth.ResourceTx. It is the integration seam: execution's stop and
// cancellation transactions call RevokeResource or RevokeRunResources through
// it, in the same transaction as their own writes.
func BindResource(tx pgx.Tx, tenant domain.TenantID, authority ResourceAuthority) runtimeauth.ResourceTx {
	return &resourceTx{tx: tx, tenant: tenant, authority: authority}
}

// RevokeResource revokes the resource's grant in tx. The execution owner calls it
// from its stop transaction after locking the run (the module-wide lock order:
// resource state first, grants second). tx must carry the tenant context of
// ref.Scope.TenantID.
func RevokeResource(ctx context.Context, tx pgx.Tx, ref runtimeauth.ResourceRef, now time.Time) (int, error) {
	return BindResource(tx, ref.Scope.TenantID, nil).RevokeResource(ctx, ref, now)
}

// RevokeRunResources revokes every resource grant of the run in tx (run
// cancellation or a terminal run).
func RevokeRunResources(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, now time.Time) (int, error) {
	return BindResource(tx, scope.TenantID, nil).RevokeRun(ctx, scope, run, now)
}

type resourceTx struct {
	tx        pgx.Tx
	tenant    domain.TenantID
	authority ResourceAuthority
}

var _ runtimeauth.ResourceTx = (*resourceTx)(nil)

func (t *resourceTx) LockResource(ctx context.Context, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	if t.authority == nil {
		return runtimeauth.ResourceState{}, errors.Join(runtimeauth.ErrUnavailable, errors.New("no resource authority is bound"))
	}
	if ref.Scope.TenantID != t.tenant {
		return runtimeauth.ResourceState{}, errors.Join(runtimeauth.ErrNotFound, errors.New("another tenant"))
	}
	return t.authority.LockResource(ctx, t.tx, ref)
}

const resourceGrantColumns = `tenant_id, project_id, run_id, resource_key, resource_kind, credential_digest, signing_generation,
	spec_fingerprint, deadline_at, created_at, expires_at, session_id, nonce_digest, bound_workload_name, bound_workload_uid,
	bound_pod_name, bound_pod_uid, consumed_at, max_expires_at, revoked_at`

func scanResourceGrant(row pgx.Row) (runtimeauth.ResourceGrant, error) {
	var (
		g                                 runtimeauth.ResourceGrant
		tenant, project, run, key, kind   string
		credential, nonce                 []byte
		deadline, created, expires        time.Time
		session, wName, wUID, pName, pUID *string
		consumed, maxExpires, revoked     *time.Time
	)
	if err := row.Scan(&tenant, &project, &run, &key, &kind, &credential, &g.Generation, &g.SpecFingerprint, &deadline, &created, &expires,
		&session, &nonce, &wName, &wUID, &pName, &pUID, &consumed, &maxExpires, &revoked); err != nil {
		return runtimeauth.ResourceGrant{}, err
	}
	var err error
	g.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	g.RunID, g.Key = domain.RunID(run), key
	if g.Kind, err = runtimeauth.ResourceKindOf(key); err != nil {
		return runtimeauth.ResourceGrant{}, err
	}
	if g.CredentialDigest, err = runtimeauth.DigestFromBytes(credential); err != nil {
		return runtimeauth.ResourceGrant{}, err
	}
	g.DeadlineAt, g.CreatedAt, g.ExpiresAt = deadline.UTC(), created.UTC(), expires.UTC()
	if session != nil {
		c := &runtimeauth.ResourceConsumption{
			SessionID: runtimeauth.SessionID(*session), ConsumedAt: consumed.UTC(), MaxExpiresAt: maxExpires.UTC(),
			Workload: runtimeauth.WorkloadIdentity{Name: *wName, UID: *wUID},
		}
		if pName != nil {
			c.Workload.PodName, c.Workload.PodUID = *pName, *pUID
		}
		if c.NonceDigest, err = runtimeauth.DigestFromBytes(nonce); err != nil {
			return runtimeauth.ResourceGrant{}, err
		}
		g.Consumption = c
	}
	if revoked != nil {
		r := revoked.UTC()
		g.RevokedAt = &r
	}
	return g, nil
}

func (t *resourceTx) one(ctx context.Context, where string, mode runtimeauth.LockMode, args ...any) (runtimeauth.ResourceGrant, error) {
	g, err := scanResourceGrant(t.tx.QueryRow(ctx, `SELECT `+resourceGrantColumns+` FROM runtime_resource_grants WHERE tenant_id = $1 AND `+where+lockClause(mode), append([]any{string(t.tenant)}, args...)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeauth.ResourceGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("no such grant"))
	}
	if err != nil {
		return runtimeauth.ResourceGrant{}, fmt.Errorf("runtimeauth/postgres: read resource grant: %w", err)
	}
	return g, nil
}

func (t *resourceTx) ResourceGrantByCredential(ctx context.Context, d runtimeauth.Digest, mode runtimeauth.LockMode) (runtimeauth.ResourceGrant, error) {
	return t.one(ctx, `credential_digest = $2`, mode, d[:])
}

func (t *resourceTx) ResourceGrantBySession(ctx context.Context, s runtimeauth.SessionID, mode runtimeauth.LockMode) (runtimeauth.ResourceGrant, error) {
	if err := s.Validate(); err != nil {
		return runtimeauth.ResourceGrant{}, errors.Join(runtimeauth.ErrNotFound, errors.New("malformed session"))
	}
	return t.one(ctx, `session_id = $2`, mode, string(s))
}

func (t *resourceTx) InsertResourceGrant(ctx context.Context, g runtimeauth.ResourceGrant) (runtimeauth.ResourceGrant, bool, error) {
	if g.Scope.TenantID != t.tenant {
		return runtimeauth.ResourceGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	tag, err := t.tx.Exec(ctx, `INSERT INTO runtime_resource_grants
		(tenant_id, project_id, run_id, resource_key, resource_kind, credential_digest, signing_generation, spec_fingerprint,
		 deadline_at, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, run_id, resource_key) DO NOTHING`,
		string(g.Scope.TenantID), string(g.Scope.ProjectID), string(g.RunID), g.Key, string(g.Kind), g.CredentialDigest[:], g.Generation,
		g.SpecFingerprint, g.DeadlineAt, g.CreatedAt, g.ExpiresAt)
	switch {
	case isPg(err, "23505"): // the credential digest belongs to another resource
		return runtimeauth.ResourceGrant{}, false, errors.Join(runtimeauth.ErrConflict, errors.New("credential already registered elsewhere"))
	case isPg(err, "23503"):
		return runtimeauth.ResourceGrant{}, false, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("unknown project"))
	case err != nil:
		return runtimeauth.ResourceGrant{}, false, fmt.Errorf("runtimeauth/postgres: insert resource grant: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return g, true, nil
	}
	existing, err := t.one(ctx, `run_id = $2 AND resource_key = $3`, runtimeauth.LockShare, string(g.RunID), g.Key)
	if err != nil {
		return runtimeauth.ResourceGrant{}, false, err
	}
	return existing, false, nil
}

func (t *resourceTx) SaveResourceConsumption(ctx context.Context, g runtimeauth.ResourceGrant) error {
	c := g.Consumption
	if c == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no consumption to save"))
	}
	var podName, podUID *string
	if c.Workload.HasPod() {
		podName, podUID = &c.Workload.PodName, &c.Workload.PodUID
	}
	tag, err := t.tx.Exec(ctx, `UPDATE runtime_resource_grants SET session_id = $4, nonce_digest = $5, bound_workload_name = $6,
		bound_workload_uid = $7, bound_pod_name = $8, bound_pod_uid = $9, consumed_at = $10, max_expires_at = $11
		WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3 AND session_id IS NULL AND revoked_at IS NULL`,
		string(t.tenant), string(g.RunID), g.Key, string(c.SessionID), c.NonceDigest[:], c.Workload.Name, c.Workload.UID, podName, podUID, c.ConsumedAt, c.MaxExpiresAt)
	if err != nil {
		return fmt.Errorf("runtimeauth/postgres: save resource consumption: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.Join(runtimeauth.ErrConflict, errors.New("the grant is not consumable"))
	}
	return nil
}

func (t *resourceTx) SaveResourceRevocation(ctx context.Context, g runtimeauth.ResourceGrant) error {
	if g.RevokedAt == nil {
		return errors.Join(runtimeauth.ErrInvalidRequest, errors.New("no revocation to save"))
	}
	if _, err := t.tx.Exec(ctx, `UPDATE runtime_resource_grants SET revoked_at = $4
		WHERE tenant_id = $1 AND run_id = $2 AND resource_key = $3 AND revoked_at IS NULL`,
		string(t.tenant), string(g.RunID), g.Key, *g.RevokedAt); err != nil {
		return fmt.Errorf("runtimeauth/postgres: save resource revocation: %w", err)
	}
	return nil
}

func (t *resourceTx) RevokeResource(ctx context.Context, ref runtimeauth.ResourceRef, now time.Time) (int, error) {
	if ref.Scope.TenantID != t.tenant {
		return 0, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	tag, err := t.tx.Exec(ctx, `UPDATE runtime_resource_grants SET revoked_at = GREATEST($5::timestamptz, created_at)
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND resource_key = $4 AND revoked_at IS NULL`,
		string(ref.Scope.TenantID), string(ref.Scope.ProjectID), string(ref.RunID), ref.Key, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("runtimeauth/postgres: revoke resource: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (t *resourceTx) RevokeRun(ctx context.Context, scope domain.Scope, run domain.RunID, now time.Time) (int, error) {
	if scope.TenantID != t.tenant {
		return 0, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("another tenant"))
	}
	tag, err := t.tx.Exec(ctx, `UPDATE runtime_resource_grants SET revoked_at = GREATEST($4::timestamptz, created_at)
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND revoked_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), string(run), now.UTC())
	if err != nil {
		return 0, fmt.Errorf("runtimeauth/postgres: revoke run resources: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
