// Package driver defines the Go view of the public provider contract. Drivers
// own provider work; the engine owns state, reservations, history and recovery.
package driver

import (
	"context"

	deployv1 "github.com/optimiware/kodama/contracts/deploy/v1"
)

type (
	ArtifactKind     = deployv1.ArtifactKind
	Artifact         = deployv1.Artifact
	IdentityRequest  = deployv1.IdentityRequest
	Identity         = deployv1.Identity
	Request          = deployv1.Request
	Resource         = deployv1.Resource
	Result           = deployv1.Result
	ObservationState = deployv1.ObservationState
	Observation      = deployv1.Observation
	Checkpoint       = deployv1.Checkpoint
)

const (
	ArtifactFile     = deployv1.ArtifactFile
	ArtifactOCIImage = deployv1.ArtifactOCIImage
	Absent           = deployv1.Absent
	Progressing      = deployv1.Progressing
	Ready            = deployv1.Ready
	Unhealthy        = deployv1.Unhealthy
	Conflict         = deployv1.Conflict
	Unknown          = deployv1.Unknown
)

// Driver is the narrow provider boundary. The engine calls Identify once
// before reserving an operation and then Apply/Observe or Destroy/Observe.
type Driver interface {
	Identify(ctx context.Context, req IdentityRequest) (Identity, error)
	Apply(ctx context.Context, req Request, checkpoint Checkpoint) (Result, error)
	Observe(ctx context.Context, req Request) (Observation, error)
	Destroy(ctx context.Context, req Request, checkpoint Checkpoint) (Result, error)
	Rollback(ctx context.Context, req Request, checkpoint Checkpoint) (Result, error)
}

// Config is the common provider-neutral part of a driver configuration. Each
// driver decodes its own typed configuration from Request.Configuration.
type Config struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
}
