package secrets

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ActorType identifies who caused a mutation.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorWorkload ActorType = "workload"
	ActorSystem   ActorType = "system"
	ActorOperator ActorType = "operator"
)

// Actor is the authenticated cause of a mutation.
type Actor struct {
	Type ActorType
	ID   string
}

// Validate checks the actor.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorWorkload, ActorSystem, ActorOperator:
	default:
		return invalidf("unsupported actor type")
	}
	if a.ID == "" || len(a.ID) > 256 {
		return invalidf("actor id must be 1..256 bytes")
	}
	return nil
}

// Mutation is the audit context of one write: who, when and why. It never
// carries values.
type Mutation struct {
	Actor     Actor
	RequestID string
	Reason    string
	Now       time.Time
}

// Validate checks the mutation context.
func (m Mutation) Validate() error {
	if err := m.Actor.Validate(); err != nil {
		return err
	}
	if m.Now.IsZero() {
		return invalidf("mutation time is required")
	}
	if len(m.RequestID) > 256 || len(m.Reason) > 2048 {
		return invalidf("request ID or reason too long")
	}
	return nil
}

// IDGenerator creates opaque identifiers.
type IDGenerator interface {
	NewID(prefix string) (string, error)
}

// Reader is the read side of persistence. Every method is tenant- and
// project-scoped: a resource of another scope is ErrNotFound.
type Reader interface {
	GetVariable(ctx context.Context, scope domain.Scope, id string) (Variable, error)
	// ListVariables returns up to limit non-deleted variables ordered by name,
	// after afterName ("" for the first page).
	ListVariables(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]Variable, error)

	// GetSecret returns the metadata, including a deleted tombstone.
	GetSecret(ctx context.Context, scope domain.Scope, id string) (Secret, error)
	// ListSecrets returns up to limit non-deleted secrets ordered by name.
	ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]Secret, error)
	GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (SecretVersion, error)
	// ListSecretVersions returns versions in ascending order after afterVersion.
	ListSecretVersions(ctx context.Context, scope domain.Scope, secretID string, afterVersion int64, limit int) ([]SecretVersion, error)

	// ActiveScopeKey returns the tenant's newest scope key generation with its
	// current wrap, or ErrNotFound if none exists.
	ActiveScopeKey(ctx context.Context, tenant domain.TenantID) (ScopeKey, error)
	// GetScopeKey returns one scope key by identity, or ErrNotFound.
	GetScopeKey(ctx context.Context, tenant domain.TenantID, id string) (ScopeKey, error)

	// RetentionReport counts pins and pending handling of a secret.
	RetentionReport(ctx context.Context, scope domain.Scope, secretID string) (RetentionReport, error)
	// PendingHandling lists unacknowledged effects ordered by ID, restart-safe.
	PendingHandling(ctx context.Context, scope domain.Scope, afterID string, limit int) ([]ConsumerHandling, error)
}

// Writer is the write side bound to one atomic unit (a database transaction in
// the PostgreSQL adapter, optionally shared with an idempotency record). Every
// method rechecks ownership and the expected version in the write itself,
// applies the entity transition, and records the safe audit entry and the
// configuration.changed event in the same unit. Nothing here calls a key
// provider: cryptographic preparation happens before the unit starts.
type Writer interface {
	Reader

	CreateVariable(ctx context.Context, m Mutation, v Variable) error
	// EditVariable applies the edit if expected is the current resource version.
	EditVariable(ctx context.Context, m Mutation, scope domain.Scope, id string, expected domain.Version, edit VariableEdit) (Variable, error)
	DeleteVariable(ctx context.Context, m Mutation, scope domain.Scope, id string, expected domain.Version) (Variable, error)

	// CreateSecret persists a new secret (already holding first as its current
	// version).
	CreateSecret(ctx context.Context, m Mutation, s Secret, first SecretVersion) error
	// AddSecretVersion appends v if the secret's resource version is expected
	// and v is the next value version.
	AddSecretVersion(ctx context.Context, m Mutation, scope domain.Scope, id string, expected domain.Version, v SecretVersion) (Secret, error)
	// RevokeSecretVersion revokes one version irreversibly and records the
	// consumer handling owed to every known consumer.
	RevokeSecretVersion(ctx context.Context, m Mutation, scope domain.Scope, id string, version int64) (RevocationResult, error)
	// DeleteSecret logically deletes the secret and reports what is retained.
	DeleteSecret(ctx context.Context, m Mutation, scope domain.Scope, id string, expected domain.Version) (Secret, RetentionReport, error)

	// RewrapScopeKey replaces the current wrap if expectedRevision is current.
	RewrapScopeKey(ctx context.Context, m Mutation, tenant domain.TenantID, keyID string, expectedRevision int64, w WrappedKey) (ScopeKey, error)

	// AcknowledgeHandling records the outcome of one external effect.
	AcknowledgeHandling(ctx context.Context, m Mutation, scope domain.Scope, id string, outcome HandlingState) (ConsumerHandling, error)
}

// Store is the persistence port of the management use cases.
type Store interface {
	Reader
	// EnsureScopeKey stores candidate as the tenant's scope key of its
	// generation unless one is already committed. It returns the committed key
	// and whether candidate won; the loser must discard its prepared material.
	EnsureScopeKey(ctx context.Context, m Mutation, candidate ScopeKey) (committed ScopeKey, created bool, err error)
	// Atomically runs fn in one unit of work. fn's error rolls everything back.
	Atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, w Writer) error) error
}
