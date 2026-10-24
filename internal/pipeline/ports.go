package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. The composition root
// wires adapters; a nil or missing port fails closed, and nothing here has an
// allow-all default.

// ActorType classifies who performed an action. The type comes from the
// identity authority, never from request content.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is an attributable, authority-resolved identity.
type Actor struct {
	ID   string    `json:"id"`
	Type ActorType `json:"type"`
}

// Validate checks that the actor is usable for attribution.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return fmt.Errorf("%w: unknown actor type", domain.ErrInvalidValue)
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return fmt.Errorf("%w: actor id must be 1..256 bytes", domain.ErrInvalidValue)
	}
	return nil
}

// Permission names an authorization question put to the Authority. The values
// double as the proposed route action identifiers.
type Permission string

const (
	PermList     Permission = "pipeline_definition.list"
	PermRegister Permission = "pipeline_definition.register"
	PermRead     Permission = "pipeline_definition.read"
)

// Authority resolves a verified principal to an attributable actor in a scope
// and answers whether the actor currently holds a permission. Authorize returns
// nil to allow, ErrDenied or ErrHidden to refuse, and any other error for an
// outage (treated as unavailable, never as access).
type Authority interface {
	ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (Actor, error)
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// Capabilities reports which step kinds the installation executes. M1 wiring
// enables containers only; the schema still represents agent and gate steps.
type Capabilities interface {
	StepKindEnabled(ctx context.Context, kind StepKind) (bool, error)
}

// EnabledKinds is a static Capabilities. Selecting it is a deliberate wiring
// decision, for example EnabledKinds(KindContainer) for M1.
type EnabledKinds []StepKind

// StepKindEnabled implements Capabilities.
func (e EnabledKinds) StepKindEnabled(_ context.Context, kind StepKind) (bool, error) {
	for _, k := range e {
		if k == kind {
			return true, nil
		}
	}
	return false, nil
}

// ProfileInfo is what validation needs to know about an agent profile.
type ProfileInfo struct {
	// Immutable reports that the name resolves to one immutable, versioned
	// profile. A moving alias must report false.
	Immutable bool
}

// ProfileLookup resolves an agent profile reference as written in the document.
// This module does not define the profile-version syntax: the profile owner
// decides what a reference means. It returns ErrReferenceNotFound for an
// unknown or out-of-scope profile and any other error for an outage.
type ProfileLookup interface {
	LookupProfile(ctx context.Context, scope domain.Scope, name string) (ProfileInfo, error)
}

// SecretUse is how a document consumes a secret reference.
type SecretUse string

const (
	// SecretUseEnv exposes the secret as an environment variable, which only
	// installations and policies that explicitly permit it allow.
	SecretUseEnv SecretUse = "env"
	// SecretUseFile mounts the secret under /kodama/secrets.
	SecretUseFile SecretUse = "file"
)

// ReferencePolicy answers whether the actor may reference a secret or project
// environment from a definition in the scope. It never retrieves secret values.
// It returns nil to allow, ErrReferenceNotFound or ErrReferenceDenied to refuse
// (both are reported identically), and any other error for an outage.
type ReferencePolicy interface {
	CheckSecret(ctx context.Context, scope domain.Scope, actor Actor, name string, use SecretUse) error
	CheckEnvironment(ctx context.Context, scope domain.Scope, actor Actor, id domain.EnvironmentID) error
}

// Candidate is a validated document proposed for registration. It carries the
// identity the use case generated for it; the repository decides whether it
// becomes a new version or resolves to an existing identical one.
type Candidate struct {
	Scope     domain.Scope
	ID        domain.DefinitionID
	Content   Validated
	CreatedBy Actor
	CreatedAt time.Time
	RequestID string
}

// Position is the continuation key of a list: the creation time and ID of the
// last returned row.
type Position struct {
	CreatedAt time.Time
	ID        domain.DefinitionID
}

// ListQuery filters and pages a list. Ordering is always (created_at, id).
type ListQuery struct {
	// Name optionally restricts the list to one pipeline name.
	Name  string
	After *Position
	// Limit is the number of rows to read (callers ask for page size + 1).
	Limit int
}

// Repository is the persistence port. RegisterOrGet is one atomic operation:
// the definition and its audit record commit together or not at all. Identical
// content (same scoped digest) returns the existing definition with
// created=false and writes nothing. An unknown commit outcome is reported as
// ErrUnavailable wrapping ErrOutcomeUnknown and is never retried internally.
type Repository interface {
	RegisterOrGet(ctx context.Context, c Candidate) (def Definition, created bool, err error)
	Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (Definition, error)
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]Definition, error)
}
