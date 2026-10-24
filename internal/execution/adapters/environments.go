package adapters

import (
	"context"
	"errors"
	"fmt"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// The adapters in this file bind execution's environment seams to the delivery
// module, which owns immutable configuration and exact credential references.
// Nothing here has an allow-all default: a missing, foreign, revoked or altered
// snapshot refuses, an outage is unavailable, and neither is ever access.

// SnapshotResolver is the delivery use case that, under the actor's current
// authority, retains and returns an immutable configuration snapshot.
// *delivery.Service implements it.
type SnapshotResolver interface {
	ResolveSnapshot(ctx context.Context, scope domain.Scope, actor delivery.Actor, id domain.EnvironmentID) (delivery.ConfigurationSnapshot, error)
}

// DeliveryEnvironments implements execution.EnvironmentResolver over delivery.
// It runs outside the admission transaction; the transaction later revalidates
// the result through TxEnvironments, so an environment edited, revoked or
// removed in between cannot be admitted, and an edit never rewrites a pin.
type DeliveryEnvironments struct{ svc SnapshotResolver }

var _ execution.EnvironmentResolver = (*DeliveryEnvironments)(nil)

// NewDeliveryEnvironments requires the delivery service.
func NewDeliveryEnvironments(svc SnapshotResolver) (*DeliveryEnvironments, error) {
	if svc == nil {
		return nil, errors.New("execution/adapters: a delivery snapshot resolver is required")
	}
	return &DeliveryEnvironments{svc: svc}, nil
}

// ResolveEnvironment implements execution.EnvironmentResolver. Delivery's
// refusals (denied, missing, invalid, unavailable credential) refuse the
// admission; any other failure is unavailable so the admission can be retried.
// A snapshot whose digest does not cover its content, or that names another
// scope or environment, is refused rather than trusted.
func (d *DeliveryEnvironments) ResolveEnvironment(ctx context.Context, req execution.EnvironmentRequest) (execution.EnvironmentSnapshot, error) {
	if req.Scope.Validate() != nil || req.Actor.Validate() != nil || req.EnvironmentID.Validate() != nil {
		return execution.EnvironmentSnapshot{}, execution.Refusedf("the environment request is not valid")
	}
	actor := delivery.Actor{Type: delivery.ActorType(req.Actor.Type), ID: req.Actor.ID}
	snap, err := d.svc.ResolveSnapshot(ctx, req.Scope, actor, req.EnvironmentID)
	if err != nil {
		if refusedByDelivery(err) {
			return execution.EnvironmentSnapshot{}, execution.Refusedf("the declared environment is not admissible")
		}
		return execution.EnvironmentSnapshot{}, execution.Unavailablef(err, "environment resolution is unavailable")
	}
	if snap.Scope != req.Scope || snap.Environment != req.EnvironmentID || snap.Validate() != nil {
		return execution.EnvironmentSnapshot{}, execution.Refusedf("the environment snapshot does not match the request")
	}
	return environmentSnapshot(snap), nil
}

func refusedByDelivery(err error) bool {
	return errors.Is(err, delivery.ErrNotFound) || errors.Is(err, delivery.ErrDenied) || errors.Is(err, delivery.ErrInvalid) ||
		errors.Is(err, delivery.ErrCredentialUnavailable) || errors.Is(err, delivery.ErrConflict)
}

func environmentSnapshot(s delivery.ConfigurationSnapshot) execution.EnvironmentSnapshot {
	return execution.EnvironmentSnapshot{Scope: s.Scope, Pin: s.Pin(), Secrets: append([]runtimev1.SecretBinding(nil), s.Secrets...)}
}

// RetainedViewer is the delivery use case that returns a recorded deployment
// with the immutable configuration snapshot it pinned. *delivery.Service
// implements it.
type RetainedViewer interface {
	RetainedView(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.CleanupView, error)
}

// CleanupEnvironments implements execution.DeploymentEnvironments: the
// retained configuration of a deployment, read through its own record and never
// through the environment's current definition.
type CleanupEnvironments struct{ svc RetainedViewer }

var _ execution.DeploymentEnvironments = (*CleanupEnvironments)(nil)

// NewCleanupEnvironments requires the delivery service.
func NewCleanupEnvironments(svc RetainedViewer) (*CleanupEnvironments, error) {
	if svc == nil {
		return nil, errors.New("execution/adapters: a delivery retained-view reader is required")
	}
	return &CleanupEnvironments{svc: svc}, nil
}

// RetainedEnvironment implements execution.DeploymentEnvironments. The
// deployment must belong to the scope, have a requested and not completed
// cleanup, and its retained snapshot must exist, be self-consistent and be the
// very pin the deployment recorded. Anything else refuses (fails closed);
// only an outage is unavailable.
func (c *CleanupEnvironments) RetainedEnvironment(ctx context.Context, scope domain.Scope, deployment string) (execution.EnvironmentSnapshot, error) {
	id := delivery.DeploymentID(deployment)
	if scope.Validate() != nil || id.Validate() != nil {
		return execution.EnvironmentSnapshot{}, execution.Refusedf("the deployment is not valid")
	}
	view, err := c.svc.RetainedView(ctx, scope, id)
	if err != nil {
		if refusedByDelivery(err) {
			return execution.EnvironmentSnapshot{}, execution.Refusedf("the deployment's retained configuration is not available")
		}
		return execution.EnvironmentSnapshot{}, execution.Unavailablef(err, "the deployment's retained configuration is unavailable")
	}
	if err := checkRetained(scope, id, view.Deployment, view.Snapshot); err != nil {
		return execution.EnvironmentSnapshot{}, err
	}
	return environmentSnapshot(view.Snapshot), nil
}

// checkRetained proves that snap is exactly the retained configuration of the
// recorded deployment d, that cleanup is still requested and not complete, and
// that the snapshot is intact.
func checkRetained(scope domain.Scope, id delivery.DeploymentID, d delivery.Deployment, snap delivery.ConfigurationSnapshot) error {
	rec := d.Record()
	switch {
	case d.ID() != id || d.Scope() != scope || snap.Scope != scope:
		return execution.Refusedf("the deployment's retained configuration is not available")
	case rec.CleanupRequestedAt == nil || d.Deleted():
		return execution.ErrDenied
	case snap.Validate() != nil || snap.Pin() != d.Pin() || snap.Namespace != rec.Namespace:
		return execution.Refusedf("the retained configuration does not match the deployment")
	}
	return nil
}

// TxDelivery is delivery's reads bound to one open admission transaction. The
// composition root (T-072) supplies an implementation joined to that
// transaction; it makes no network call and takes no lock beyond the execution
// lock order (snapshots are immutable and need none). Exact credential
// versions are not read here: the secrets owner revalidates them when the
// admission registers its pins in the same transaction.
type TxDelivery interface {
	// Authorize answers whether the actor currently holds permission.
	Authorize(ctx context.Context, actor delivery.Actor, scope domain.Scope, perm delivery.Permission) error
	// Snapshot returns the retained snapshot of the pin; delivery.ErrNotFound for
	// a missing or foreign one.
	Snapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (delivery.ConfigurationSnapshot, error)
	// Deployment returns the recorded deployment; delivery.ErrNotFound for a
	// missing or foreign one.
	Deployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
}

type txEnvironments struct{ tx TxDelivery }

var _ execution.TxEnvironments = txEnvironments{}

// NewTxEnvironments binds delivery's revalidation to one open transaction's
// reader. Call it once per transaction (see postgres.EnvironmentBinder).
func NewTxEnvironments(tx TxDelivery) (execution.TxEnvironments, error) {
	if tx == nil {
		return nil, errors.New("execution/adapters: a transaction-bound delivery reader is required")
	}
	return txEnvironments{tx: tx}, nil
}

// BindEnvironment implements execution.TxEnvironments. A work run needs the
// actor's current deployment authority; a cleanup run needs its deployment's
// cleanup to still be requested and not complete. Both need the retained
// snapshot of the pin to exist in the scope, be intact and carry exactly the
// credential bindings the admission pinned.
func (t txEnvironments) BindEnvironment(ctx context.Context, b execution.EnvironmentBinding) error {
	if b.Scope.Validate() != nil || b.Pin.ID.Validate() != nil || b.Pin.ConfigDigest.Validate() != nil {
		return execution.Refusedf("the environment binding is not valid")
	}
	cleanup := b.Deployment != ""
	if !cleanup {
		if err := b.Actor.Validate(); err != nil {
			return execution.Refusedf("the environment binding is not valid")
		}
		actor := delivery.Actor{Type: delivery.ActorType(b.Actor.Type), ID: b.Actor.ID}
		if err := t.tx.Authorize(ctx, actor, b.Scope, delivery.PermDeploymentManage); err != nil {
			return txError(err, "the delivery authority is unavailable")
		}
	}
	snap, err := t.tx.Snapshot(ctx, b.Scope, b.Pin.ID, b.Pin.ConfigDigest)
	if err != nil {
		return txError(err, "the retained environment snapshot is unavailable")
	}
	if snap.Scope != b.Scope || snap.Pin() != b.Pin || snap.Validate() != nil || !sameBindings(snap.Secrets, b.Secrets) {
		return execution.Refusedf("the retained environment snapshot does not match the admission")
	}
	if cleanup {
		id := delivery.DeploymentID(b.Deployment)
		if id.Validate() != nil {
			return execution.Refusedf("the environment binding is not valid")
		}
		d, err := t.tx.Deployment(ctx, b.Scope, id)
		if err != nil {
			return txError(err, "the deployment record is unavailable")
		}
		if err := checkRetained(b.Scope, id, d, snap); err != nil {
			return err
		}
	}
	return nil
}

// txError classifies a delivery failure inside the transaction: refusals stay
// refusals, anything else is an outage and never access.
func txError(err error, unavailable string) error {
	switch {
	case errors.Is(err, delivery.ErrDenied):
		return execution.ErrDenied
	case refusedByDelivery(err):
		return fmt.Errorf("%w: the retained environment is not admissible", execution.ErrAdmissionRefused)
	}
	return execution.Unavailablef(err, "%s", unavailable)
}

// sameBindings reports whether two binding sets are identical: the same
// references, versions, names and delivery forms, regardless of order.
func sameBindings(a, b []runtimev1.SecretBinding) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]runtimev1.SecretBinding(nil), a...)
	y := append([]runtimev1.SecretBinding(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i].Name < x[j].Name })
	sort.Slice(y, func(i, j int) bool { return y[i].Name < y[j].Name })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
