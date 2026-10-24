package adapters

import (
	"context"
	"errors"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
)

// DeploymentReader reads deployment records. delivery.Store implements it.
type DeploymentReader interface {
	GetDeployment(ctx context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error)
}

// Deployments implements qa.Deployments over the deployment module's store. The
// answer is the deployment row of exactly the given scope (a deployment of
// another project or tenant is not found), never a value from a caller.
type Deployments struct{ store DeploymentReader }

var _ qa.Deployments = (*Deployments)(nil)

// NewDeployments requires the deployment reader.
func NewDeployments(s DeploymentReader) (*Deployments, error) {
	if s == nil {
		return nil, errors.New("qa/adapters: a deployment store is required")
	}
	return &Deployments{store: s}, nil
}

// Deployment implements qa.Deployments.
func (d *Deployments) Deployment(ctx context.Context, scope domain.Scope, id string) (qa.DeploymentFacts, error) {
	did := delivery.DeploymentID(id)
	if did.Validate() != nil {
		return qa.DeploymentFacts{}, qa.Failf(qa.ErrNotFound, "deployment not found")
	}
	dep, err := d.store.GetDeployment(ctx, scope, did)
	if err != nil {
		if errors.Is(err, delivery.ErrNotFound) {
			return qa.DeploymentFacts{}, qa.Failf(qa.ErrNotFound, "deployment not found")
		}
		return qa.DeploymentFacts{}, qa.Unavailablef(err, "the deployment store could not answer")
	}
	rec := dep.Record()
	return qa.DeploymentFacts{
		ID: string(rec.ID), Scope: rec.Scope, Environment: rec.Environment, ConfigDigest: rec.ConfigDigest,
		Candidate: runtimev1.GitCommit(rec.Source.Commit), Image: rec.Image.Digest,
		Succeeded: rec.Status == delivery.StatusSucceeded, DeletedAt: rec.DeletedAt,
	}, nil
}
