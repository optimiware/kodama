package stack

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
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

// DeploymentReader is the delivery store's committed read of one deployment.
type DeploymentReader interface {
	GetDeployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
}

// txReader is delivery's reads bound to one open admission transaction, the
// seam the composition root (T-072) must supply to execution's store
// (execpg.Peers.Environments). Authority is checked through the transaction
// (it takes the user's authorization revision FOR SHARE), and the retained
// configuration snapshot is read in the same transaction with the same tenant
// context, so the admission sees exactly the snapshot rows it commits beside.
// Nothing here makes a network call.
//
// The recorded deployment of a cleanup run is read as committed data through
// the delivery store: a deployment row's cleanup state only moves forward.
type txReader struct {
	tx          pgx.Tx
	deployments DeploymentReader
}

var _ execadapters.TxDelivery = (*txReader)(nil)

func (r *txReader) Authorize(ctx context.Context, actor delivery.Actor, scope domain.Scope, perm delivery.Permission) error {
	access, err := deliveryadapters.NewAccess(identitypg.NewTxAuthority(r.tx))
	if err != nil {
		return err
	}
	return access.Authorize(ctx, actor, scope, perm)
}

const snapshotColumns = `tenant_id, project_id, environment_id, digest, kind, target, namespace, config, preview_ttl_seconds, kubeconfig, secrets, created_at`

func (r *txReader) Snapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	var s delivery.ConfigurationSnapshot
	var tenant, project, envID, dg, kind string
	var config, secrets []byte
	var ttl int64
	err := r.tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM environment_snapshots
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

func (r *txReader) Deployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	return r.deployments.GetDeployment(ctx, scope, id)
}
