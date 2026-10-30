package deliverytest

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

type genericDepRec struct {
	D delivery.GenericDeployment
}

func gdkey(project domain.ProjectID, id delivery.DeploymentID) string {
	return string(project) + "/" + string(id)
}

var genericDeploymentsT = memdb.NewTable("generic_deployments",
	func(r genericDepRec) string { return gdkey(r.D.Scope().ProjectID, r.D.ID()) },
	func(r genericDepRec) string {
		return string(r.D.Scope().ProjectID) + "|" + r.D.ActionKey()
	},
)

// GenericMemStore implements delivery.GenericStore over memdb for development
// and unit tests. Production refuses it via DevelopmentOnly().
type GenericMemStore struct {
	store *memdb.Store
}

var _ delivery.GenericStore = (*GenericMemStore)(nil)

// NewGenericMemStore returns a memdb-backed GenericStore.
func NewGenericMemStore(store *memdb.Store) (*GenericMemStore, error) {
	if store == nil {
		return nil, errors.New("deliverytest: a memdb store is required")
	}
	return &GenericMemStore{store: store}, nil
}

// DevelopmentOnly marks the adapter as development-only for app.Admit.
func (*GenericMemStore) DevelopmentOnly() bool { return true }

// SaveGeneric implements delivery.GenericStore.
func (s *GenericMemStore) SaveGeneric(ctx context.Context, dep delivery.GenericDeployment) (delivery.GenericDeployment, bool, error) {
	scope := dep.Scope()
	var out delivery.GenericDeployment
	var created bool
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if existing, ok := genericDeploymentsT.Get(tx, gdkey(scope.ProjectID, dep.ID())); ok {
			if existing.D.Scope() != scope {
				return delivery.ErrNotFound
			}
			if !existing.D.MatchesIntent(dep.Intent()) {
				return delivery.Fail(delivery.ErrConflict, nil, "conflicting deployment intent for action key")
			}
			out, created = existing.D, false
			return nil
		}
		if err := genericDeploymentsT.Put(tx, genericDepRec{D: dep}); err != nil {
			return delivery.Fail(delivery.ErrConflict, err, "conflicting deployment registration")
		}
		out, created = dep, true
		return nil
	})
	return out, created, err
}

// GetGeneric implements delivery.GenericStore.
func (s *GenericMemStore) GetGeneric(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.GenericDeployment, error) {
	var out delivery.GenericDeployment
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := genericDeploymentsT.Get(tx, gdkey(scope.ProjectID, id))
		if !ok || r.D.Scope() != scope {
			return delivery.ErrNotFound
		}
		out = r.D
		return nil
	})
	return out, err
}

// FindGenericByAction implements delivery.GenericStore.
func (s *GenericMemStore) FindGenericByAction(ctx context.Context, scope domain.Scope, actionKey string) (delivery.GenericDeployment, error) {
	return s.GetGeneric(ctx, scope, delivery.DeploymentIDFor(scope, actionKey))
}

// UpdateGeneric implements delivery.GenericStore.
func (s *GenericMemStore) UpdateGeneric(ctx context.Context, scope domain.Scope, id delivery.DeploymentID, fn func(*delivery.GenericDeployment) error) (delivery.GenericDeployment, error) {
	var out delivery.GenericDeployment
	err := s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		r, ok := genericDeploymentsT.Get(tx, gdkey(scope.ProjectID, id))
		if !ok || r.D.Scope() != scope {
			return delivery.ErrNotFound
		}
		d := r.D
		if err := fn(&d); err != nil {
			return err
		}
		if err := genericDeploymentsT.Put(tx, genericDepRec{D: d}); err != nil {
			return err
		}
		out = d
		return nil
	})
	return out, err
}
