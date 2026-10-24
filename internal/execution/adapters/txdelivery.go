package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

// RowQuerier is the single-row read of an open transaction. pgx.Tx satisfies
// it; the reader needs nothing else from the transaction.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxDeliveryReader is delivery's reads bound to one open admission transaction
// (execution/postgres.Peers.Environments). It satisfies TxDelivery:
//
//   - authority is the actor's current deployment authority read through the
//     transaction (the identity transaction authority holds the user's
//     authorization revision FOR SHARE until the admission ends, so a revocation
//     committed while the admission waited is seen and a racing revocation waits
//     for the admission);
//   - the retained configuration snapshot and the recorded deployment of a
//     cleanup run are read by the same transaction with the same tenant context,
//     so the admission sees exactly the rows it commits beside.
//
// It makes no network call and takes no row lock: snapshots are immutable and
// the execution lock order is not extended. Every read is scoped by tenant and
// project in its WHERE clause; a row of another scope is indistinguishable from
// none (delivery.ErrNotFound), an unreadable row is an error (an outage) and
// neither is ever a grant.
type TxDeliveryReader struct {
	q      RowQuerier
	access delivery.Access
}

var _ TxDelivery = (*TxDeliveryReader)(nil)

// NewTxDeliveryReader requires the transaction's row reader and the transaction
// bound current-authority check.
func NewTxDeliveryReader(q RowQuerier, access delivery.Access) (*TxDeliveryReader, error) {
	if q == nil || access == nil {
		return nil, errors.New("execution/adapters: a transaction row reader and an access check are required")
	}
	return &TxDeliveryReader{q: q, access: access}, nil
}

// BindDeliveryEnvironments is the binder execution's PostgreSQL store takes as
// Peers.Environments (execpg.EnvironmentBinder). For each transaction it joins
// delivery's revalidation to that transaction. A binder that cannot be built
// binds a closed failure: the admission is refused as unavailable, never
// admitted without the check.
func BindDeliveryEnvironments(tx pgx.Tx) execution.TxEnvironments {
	access, err := deliveryadapters.NewAccess(identitypg.NewTxAuthority(tx))
	if err != nil {
		return closedEnvironments{err: err}
	}
	reader, err := NewTxDeliveryReader(tx, access)
	if err != nil {
		return closedEnvironments{err: err}
	}
	bound, err := NewTxEnvironments(reader)
	if err != nil {
		return closedEnvironments{err: err}
	}
	return bound
}

// closedEnvironments refuses every binding as an outage.
type closedEnvironments struct{ err error }

func (c closedEnvironments) BindEnvironment(context.Context, execution.EnvironmentBinding) error {
	return execution.Unavailablef(c.err, "the delivery reader is not bound to the admission transaction")
}

// Authorize implements TxDelivery. ErrDenied and ErrNotFound refuse; any other
// error is an outage.
func (r *TxDeliveryReader) Authorize(ctx context.Context, actor delivery.Actor, scope domain.Scope, perm delivery.Permission) error {
	return r.access.Authorize(ctx, actor, scope, perm)
}

const txSnapshotColumns = `tenant_id, project_id, environment_id, digest, kind, target, namespace, config, preview_ttl_seconds, kubeconfig, secrets, created_at`

// Snapshot implements TxDelivery.
func (r *TxDeliveryReader) Snapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	var s delivery.ConfigurationSnapshot
	var tenant, project, envID, dg, kind string
	var config, secrets []byte
	var ttl int64
	err := r.q.QueryRow(ctx, `SELECT `+txSnapshotColumns+` FROM environment_snapshots
WHERE tenant_id = $1 AND project_id = $2 AND environment_id = $3 AND digest = $4`,
		string(scope.TenantID), string(scope.ProjectID), string(env), string(digest)).
		Scan(&tenant, &project, &envID, &dg, &kind, &s.Target, &s.Namespace, &config, &ttl, &s.Kubeconfig, &secrets, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, err, "not found")
	}
	if err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	s.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	s.Environment, s.Digest, s.Kind = domain.EnvironmentID(envID), runtimev1.Digest(dg), delivery.EnvironmentKind(kind)
	s.PreviewTTL, s.CreatedAt = time.Duration(ttl)*time.Second, s.CreatedAt.UTC()
	if err := json.Unmarshal(config, &s.Config); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	if err := json.Unmarshal(secrets, &s.Secrets); err != nil {
		return delivery.ConfigurationSnapshot{}, err
	}
	return s, s.Validate()
}

const txDeploymentColumns = `tenant_id, project_id, id, environment_id, config_digest, namespace, source_repository_url, source_commit,
image_artifact_id, image_digest, producer_attempt_id, producer_run_id, producer_step_id, tool_image, run_id, step_id, action_key, attempt_id,
request_fingerprint, status, reason, objects, expires_at, cleanup_requested_at, cleanup_state, cleanup_reason, deleted_at, version, created_at, updated_at`

// Deployment implements TxDelivery. The row is read in the admission
// transaction, so a cleanup that was requested or completed before the
// admission's snapshot is seen.
func (r *TxDeliveryReader) Deployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	var rec delivery.DeploymentRecord
	var tenant, project, did, env, digest, imageArtifact, imageDigest, prodAttempt, prodRun, run, attempt, fp, status, cleanup string
	var objects []byte
	var version int64
	err := r.q.QueryRow(ctx, `SELECT `+txDeploymentColumns+` FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).
		Scan(&tenant, &project, &did, &env, &digest, &rec.Namespace, &rec.Source.RepositoryURL, &rec.Source.Commit,
			&imageArtifact, &imageDigest, &prodAttempt, &prodRun, &rec.Producer.Step, &rec.Producer.ToolImage, &run, &rec.Step, &rec.ActionKey, &attempt,
			&fp, &status, &rec.Reason, &objects, &rec.ExpiresAt, &rec.CleanupRequestedAt, &cleanup, &rec.CleanupReason, &rec.DeletedAt, &version, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, err, "not found")
	}
	if err != nil {
		return delivery.Deployment{}, err
	}
	rec.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	rec.ID, rec.Environment, rec.ConfigDigest = delivery.DeploymentID(did), domain.EnvironmentID(env), runtimev1.Digest(digest)
	rec.Image = delivery.Image{ArtifactID: domain.ArtifactID(imageArtifact), Digest: runtimev1.Digest(imageDigest)}
	rec.Producer.Attempt, rec.Producer.Run = domain.AttemptID(prodAttempt), domain.RunID(prodRun)
	rec.Run, rec.Attempt, rec.Fingerprint = domain.RunID(run), domain.AttemptID(attempt), runtimev1.Digest(fp)
	rec.Status, rec.CleanupState, rec.Version = delivery.Status(status), delivery.CleanupState(cleanup), domain.Version(version)
	rec.CreatedAt, rec.UpdatedAt = rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()
	rec.ExpiresAt, rec.CleanupRequestedAt, rec.DeletedAt = utcPtr(rec.ExpiresAt), utcPtr(rec.CleanupRequestedAt), utcPtr(rec.DeletedAt)
	if err := json.Unmarshal(objects, &rec.Objects); err != nil {
		return delivery.Deployment{}, err
	}
	if rec.Scope != scope || rec.ID != id {
		// A row of another scope or identity never answers: treat it as absent.
		return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return delivery.RestoreDeployment(rec)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
