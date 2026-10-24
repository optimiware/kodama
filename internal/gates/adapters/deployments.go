package adapters

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

// Deployment facts are read from the deployment record of T-035, never from a
// request. Both adapters below answer through one rule (snapshotOf):
//
//   - a record of another tenant or project, or one that does not exist, is
//     gates.ErrNotFound: foreign and absent are indistinguishable and never a
//     grant;
//   - a deployment whose cleanup was requested (in any cleanup state) or whose
//     record was deleted is gates.ErrNotFound as well: a gate cannot bind a
//     deployment that is being, or has been, torn down, and the gate it was
//     bound to becomes stale (deployment changed);
//   - an unreadable or inconsistent record is gates.ErrUnavailable, so a
//     progression is denied and no invalidation is invented;
//   - the digest, scope and candidate come from the record (the configuration
//     digest the deployment pinned at registration and the source commit it
//     recorded), never from the caller.

// DeploymentRecords is the delivery store's committed read of one deployment.
// *delivery/postgres.Store implements it. It serves the preparation of a gate
// (outside the protected transaction); the transaction-bound binder reads
// through the deciding transaction instead.
type DeploymentRecords interface {
	GetDeployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
}

// Deployments implements gates.Deployments over committed deployment records.
type Deployments struct{ records DeploymentRecords }

var _ gates.Deployments = (*Deployments)(nil)

// NewDeployments requires the deployment reader; without it a deployment gate
// fails closed.
func NewDeployments(r DeploymentRecords) (*Deployments, error) {
	if r == nil {
		return nil, errors.New("gates/adapters: a deployment reader is required")
	}
	return &Deployments{records: r}, nil
}

// Snapshot implements gates.Deployments.
func (d *Deployments) Snapshot(ctx context.Context, scope domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	did := delivery.DeploymentID(id)
	if scope.Validate() != nil || did.Validate() != nil {
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	}
	rec, err := d.records.GetDeployment(ctx, scope, did)
	switch {
	case err == nil:
	case errors.Is(err, delivery.ErrNotFound), errors.Is(err, delivery.ErrDenied):
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	default:
		return gates.DeploymentSnapshot{}, gates.Unavailablef(err, "the deployment record is unavailable")
	}
	r := rec.Record()
	return snapshotOf(scope, id, deploymentFacts{
		Scope: r.Scope, ID: string(r.ID), ConfigDigest: string(r.ConfigDigest), Candidate: r.Source.Commit,
		Cleanup: r.CleanupRequestedAt != nil || r.CleanupState != delivery.CleanupNone, Deleted: r.DeletedAt != nil,
	})
}

// RowQuerier is the single-row read of an open transaction. pgx.Tx satisfies
// it.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxDeployments implements gates.Deployments inside one open tenant
// transaction: the gate store's DeploymentBinder gives it the very transaction
// that decides or consumes the gate, so the configuration digest, scope and
// cleanup state are read from the same snapshot as the gate and the execution
// step it settles, and are discarded or committed with them. The read is one
// SELECT scoped by tenant and project; it makes no network call and takes no
// row lock, so it cannot precede the execution locks of the transaction. The
// digest, scope and candidate of a deployment never change after registration,
// and cleanup only moves forward, so a cleanup requested after the read is the
// ordinary race of any decision and is seen by the next check.
type TxDeployments struct{ q RowQuerier }

var _ gates.Deployments = TxDeployments{}

// NewTxDeployments requires the transaction's row reader.
func NewTxDeployments(q RowQuerier) (TxDeployments, error) {
	if q == nil {
		return TxDeployments{}, errors.New("gates/adapters: a transaction row reader is required")
	}
	return TxDeployments{q: q}, nil
}

// BindDeployments is the binder the PostgreSQL gate store takes as
// Config.Deployments (postgres.DeploymentBinder). It joins the reader to one
// open transaction.
func BindDeployments(tx pgx.Tx) gates.Deployments {
	d, err := NewTxDeployments(tx)
	if err != nil {
		return closedDeployments{err: err}
	}
	return d
}

// closedDeployments answers every read as an outage.
type closedDeployments struct{ err error }

func (c closedDeployments) Snapshot(context.Context, domain.Scope, string) (gates.DeploymentSnapshot, error) {
	return gates.DeploymentSnapshot{}, gates.Unavailablef(c.err, "the deployment reader is not bound to the transaction")
}

// Snapshot implements gates.Deployments.
func (t TxDeployments) Snapshot(ctx context.Context, scope domain.Scope, id string) (gates.DeploymentSnapshot, error) {
	did := delivery.DeploymentID(id)
	if scope.Validate() != nil || did.Validate() != nil {
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	}
	var f deploymentFacts
	var tenant, project string
	var requested, deleted *time.Time
	var cleanup string
	err := t.q.QueryRow(ctx, `SELECT tenant_id, project_id, id, config_digest, source_commit, cleanup_requested_at, cleanup_state, deleted_at
FROM deployments WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), id).
		Scan(&tenant, &project, &f.ID, &f.ConfigDigest, &f.Candidate, &requested, &cleanup, &deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	case err != nil:
		return gates.DeploymentSnapshot{}, gates.Unavailablef(err, "the deployment record is unavailable")
	}
	f.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	f.Cleanup, f.Deleted = requested != nil || delivery.CleanupState(cleanup) != delivery.CleanupNone, deleted != nil
	return snapshotOf(scope, id, f)
}

// deploymentFacts are the columns of a deployment record that decide a gate.
type deploymentFacts struct {
	Scope        domain.Scope
	ID           string
	ConfigDigest string
	Candidate    string
	// Cleanup is true once cleanup was requested, whatever became of it.
	Cleanup bool
	Deleted bool
}

// snapshotOf applies the single rule of both adapters.
func snapshotOf(scope domain.Scope, id string, f deploymentFacts) (gates.DeploymentSnapshot, error) {
	switch {
	case f.Scope != scope || f.ID != id:
		// A row that is not the requested one never answers.
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	case f.Cleanup || f.Deleted:
		return gates.DeploymentSnapshot{}, gates.ErrNotFound
	case f.ConfigDigest == "" || f.Candidate == "":
		return gates.DeploymentSnapshot{}, gates.Unavailablef(nil, "the deployment record is incomplete")
	}
	return gates.DeploymentSnapshot{ID: f.ID, ConfigDigest: f.ConfigDigest, Scope: f.Scope, Candidate: f.Candidate}, nil
}
