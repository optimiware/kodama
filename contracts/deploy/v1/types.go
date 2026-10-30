// Package deployv1 is the public, provider-neutral deployment driver contract.
// It is intentionally independent of the Kodama server package graph so an
// external image can implement the same protocol.
package deployv1

import (
	"context"
	"encoding/json"
	"time"
)

type ArtifactKind string

const (
	ArtifactFile     ArtifactKind = "file"
	ArtifactOCIImage ArtifactKind = "ociImage"
)

type Artifact struct {
	ID       string       `json:"id"`
	Digest   string       `json:"digest"`
	Kind     ArtifactKind `json:"kind"`
	Path     string       `json:"path,omitempty"`
	Registry string       `json:"registryRef,omitempty"`
}

type IdentityRequest struct {
	Recipe        string          `json:"recipe"`
	RecipeVersion int64           `json:"recipeVersion"`
	Configuration json.RawMessage `json:"configuration"`
}

type Identity struct {
	Driver        string         `json:"driver"`
	DriverVersion string         `json:"driverVersion"`
	ResourceKey   string         `json:"resourceKey"`
	ArtifactKinds []ArtifactKind `json:"artifactKinds"`
	Rollback      string         `json:"rollback"`
	Observations  []string       `json:"observations"`
	Preconditions []string       `json:"preconditions"`
}

type Request struct {
	Protocol       string                     `json:"protocol"`
	RequestID      string                     `json:"requestId"`
	Method         string                     `json:"method"`
	Deadline       time.Time                  `json:"deadline"`
	Identity       Identity                   `json:"identity"`
	OperationID    string                     `json:"operationId,omitempty"`
	DesiredRelease string                     `json:"desiredRelease,omitempty"`
	Configuration  json.RawMessage            `json:"configuration,omitempty"`
	Artifacts      []Artifact                 `json:"artifacts,omitempty"`
	Previous       json.RawMessage            `json:"previous,omitempty"`
	Expected       string                     `json:"expectedRevision,omitempty"`
	Checkpoints    map[string]json.RawMessage `json:"checkpoints,omitempty"`
}

type Resource struct {
	APIVersion      string `json:"apiVersion"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type Result struct {
	Effect          string          `json:"effect"`
	ProviderRelease string          `json:"providerRelease,omitempty"`
	ProviderVersion string          `json:"providerVersion,omitempty"`
	Resources       []Resource      `json:"resources,omitempty"`
	Detail          json.RawMessage `json:"detail,omitempty"`
}

type ObservationState string

const (
	Absent      ObservationState = "absent"
	Progressing ObservationState = "progressing"
	Ready       ObservationState = "ready"
	Unhealthy   ObservationState = "unhealthy"
	Conflict    ObservationState = "conflict"
	Unknown     ObservationState = "unknown"
)

type Observation struct {
	State           ObservationState `json:"state"`
	ObservedAt      time.Time        `json:"observedAt"`
	ObservedRelease string           `json:"observedRelease,omitempty"`
	ProviderRelease string           `json:"providerRelease,omitempty"`
	ProviderVersion string           `json:"providerVersion,omitempty"`
	Resources       []Resource       `json:"resources,omitempty"`
	Checks          []string         `json:"checks,omitempty"`
	Detail          json.RawMessage  `json:"detail,omitempty"`
}

type Checkpoint func(ctx context.Context, name string, data json.RawMessage) error
