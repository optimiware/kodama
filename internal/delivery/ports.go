package delivery

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are consumer-owned: this module states what it needs from
// identity, secrets, execution, artifacts and runtime authority, and the
// packages under adapters/ satisfy them. A missing port fails closed; there is
// no allow-all default.

// ActorType identifies who caused a change.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorWorkload ActorType = "workload"
	ActorSystem   ActorType = "system"
)

// Actor is an attributable principal.
type Actor struct {
	Type ActorType
	ID   string
}

// Validate checks the actor can be attributed.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorWorkload, ActorSystem:
	default:
		return invalidf("unknown actor type")
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return invalidf("an actor identifier is 1 to 256 bytes")
	}
	return nil
}

// Caller is the verified cause of a request.
type Caller struct {
	Actor     Actor
	RequestID string
}

// Mutation is the audit context of one write. Runtime marks a workload
// mutation: the store then requires a bound runtime session and repeats the
// session check inside its transaction.
type Mutation struct {
	Actor     Actor
	RequestID string
	Reason    string
	Now       time.Time
	Runtime   bool
}

// Validate checks the mutation context.
func (m Mutation) Validate() error {
	if err := m.Actor.Validate(); err != nil {
		return err
	}
	if m.Now.IsZero() || len(m.RequestID) > 256 || len(m.Reason) > 2048 {
		return invalidf("a mutation needs a time and bounded request and reason")
	}
	return nil
}

// Permission names an authorization question; the values equal the identity
// module's permission names.
type Permission string

const (
	PermRunRead          Permission = "run:read"
	PermDeploymentManage Permission = "deployment:manage"
)

// Access answers whether an actor currently holds a permission in a scope. It
// reads current bindings on every call; nil allows, ErrDenied or ErrNotFound
// refuse, and any other error is an outage that never grants access.
type Access interface {
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// KindKubeconfig is the secret kind of a deployment credential.
const KindKubeconfig = "kubeconfig"

// CredentialInfo is the secret owner's metadata about a credential reference:
// its kind and its current non-revoked value version. It never carries a value.
type CredentialInfo struct {
	Ref     string
	Kind    string
	Version int64
}

// Credentials resolves credential references through the secrets owner. A
// reference that is missing, foreign, deleted or whose current version is
// revoked is ErrCredentialUnavailable.
type Credentials interface {
	Current(ctx context.Context, scope domain.Scope, ref string) (CredentialInfo, error)
}

// PinnedContexts returns the immutable pinned context of an attempt, exactly as
// execution persisted it at admission and dispatch. It is required for every
// protected callback: there is no fallback to the environment's current
// settings.
type PinnedContexts interface {
	Pinned(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev1.Context, error)
}

// Producers reads the execution owner's persisted attempts and results.
type Producers interface {
	// Produced describes the attempt that produced the artifact. It reports
	// Accepted only for a succeeded attempt whose accepted result names the
	// artifact as a declared OCI output.
	Produced(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, artifact domain.ArtifactID) (ProducedOutput, error)
}

// Artifacts is the artifact owner's scoped read and retention surface.
type Artifacts interface {
	// Evidence returns the artifact of the scope or ErrNotFound.
	Evidence(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (ImageEvidence, error)
	// Protect pins the artifact for owner (idempotent) so retention cannot
	// remove it. The artifact owner's API opens its own transaction.
	Protect(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner DeploymentID) error
	// Release removes owner's pin; releasing an absent pin succeeds.
	Release(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner DeploymentID) error
}

// CleanupBindings proves which recorded deployment a cleanup run was admitted
// for. Environment equality alone is not proof.
type CleanupBindings interface {
	// Binding returns the deployment the run is bound to, or found=false.
	Binding(ctx context.Context, scope domain.Scope, run domain.RunID) (deployment DeploymentID, found bool, err error)
}

// Principal is the verified authority of a workload for the deployment.call
// action. Every identifier in it comes from the durable grant.
type Principal struct {
	Scope   domain.Scope
	Run     domain.RunID
	Attempt domain.AttemptID
	// Proof is opaque to this module: the authority adapter reads it back in
	// Bind to repeat the session check inside the protected transaction.
	Proof any
}

// Authority verifies attempt-scoped bearer tokens for deployment.call. The
// decision of Authenticate is a first check, not a permit: Bind attaches the
// session so the store repeats it in the mutation's transaction after the
// execution locks.
type Authority interface {
	Authenticate(ctx context.Context, attempt domain.AttemptID, bearer string) (Principal, error)
	Bind(ctx context.Context, p Principal) (context.Context, error)
}

// HistoryEntry is one append-only record of a deployment's history: a durable
// receipt of what was recorded, never rewritten.
type HistoryEntry struct {
	Seq        int64
	Kind       string
	Version    domain.Version
	FromStatus Status
	ToStatus   Status
	Reason     string
	Actor      Actor
	Attempt    domain.AttemptID
	Detail     map[string]any
	At         time.Time
}

// Transition is what a deployment mutation reports for history and events.
type Transition struct {
	// Kind names the history entry: registered, apply_started, outcome_unknown,
	// succeeded, failed, cleanup_requested, cleanup_done, cleanup_failed,
	// cleanup_unknown.
	Kind   string
	Change Change
	// Detail holds identifiers, counts and reason codes only.
	Detail map[string]any
	// Attempt is the attempt that caused the change, when any.
	Attempt domain.AttemptID
}

// RegisterCommand is the atomic creation of a pre-apply record.
type RegisterCommand struct {
	Mutation   Mutation
	Deployment Deployment
	// Caller attempt (may differ from the originating attempt on a retry).
	Attempt domain.AttemptID
}

// RegisterResult is the created or the already recorded deployment.
type RegisterResult struct {
	Deployment Deployment
	// Created is false when the logical action already had a deployment.
	Created bool
}

// DeploymentFilter narrows a deployment listing.
type DeploymentFilter struct {
	Environment domain.EnvironmentID
	Status      Status
}

// Store is the persistence port. Every method is tenant- and project-scoped: a
// resource of another scope is ErrNotFound. Mutations commit state, history,
// audit and the outbox event together, or none of them.
type Store interface {
	CreateEnvironment(ctx context.Context, m Mutation, env Environment) error
	GetEnvironment(ctx context.Context, scope domain.Scope, id domain.EnvironmentID) (Environment, error)
	// ListEnvironments returns up to limit environments ordered by name after
	// afterName ("" for the first page).
	ListEnvironments(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]Environment, error)
	// UpdateEnvironment applies edit if expected is the current version, inside
	// the write itself.
	UpdateEnvironment(ctx context.Context, m Mutation, scope domain.Scope, id domain.EnvironmentID, expected domain.Version, edit EnvironmentEdit) (Environment, error)

	// RetainSnapshot stores the immutable snapshot, or returns the identical one
	// already retained under its digest.
	RetainSnapshot(ctx context.Context, snap ConfigurationSnapshot) (ConfigurationSnapshot, error)
	GetSnapshot(ctx context.Context, scope domain.Scope, env domain.EnvironmentID, digest runtimev1.Digest) (ConfigurationSnapshot, error)

	// Register creates the deployment of a logical action once. An identical
	// request returns the original with Created=false; a changed immutable input
	// is ErrConflict. The mutation's session check runs in the transaction.
	Register(ctx context.Context, cmd RegisterCommand) (RegisterResult, error)
	GetDeployment(ctx context.Context, scope domain.Scope, id DeploymentID) (Deployment, error)
	FindByAction(ctx context.Context, scope domain.Scope, actionKey string) (Deployment, error)
	// ListDeployments returns up to limit deployments ordered by ID after afterID.
	ListDeployments(ctx context.Context, scope domain.Scope, f DeploymentFilter, afterID DeploymentID, limit int) ([]Deployment, error)
	// Update locks the deployment, applies fn (a pure entity transition) and, when
	// it changed the deployment, commits it with history, audit and event.
	Update(ctx context.Context, scope domain.Scope, id DeploymentID, m Mutation, fn func(*Deployment) (Transition, error)) (Deployment, Transition, error)
	// Expired lists deployments past their effective expiry that were not
	// deleted, oldest expiry first.
	Expired(ctx context.Context, scope domain.Scope, now time.Time, limit int) ([]Deployment, error)
	History(ctx context.Context, scope domain.Scope, id DeploymentID) ([]HistoryEntry, error)
}
