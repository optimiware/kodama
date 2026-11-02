package repository

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ---- persistence ----

// Audit is one audit record written in the transaction of the change.
// Metadata must hold only values safe for every audit reader.
type Audit struct {
	Action   string
	Actor    Actor
	Reason   string
	Metadata map[string]any
}

// EventKind selects an existing catalog event; the adapter maps it to its
// envelope and payload (contracts/events/v1).
type EventKind string

// Event kinds.
const (
	EventCreated      EventKind = "repository.created"
	EventStateChanged EventKind = "repository.state_changed"
)

// Event is a domain event appended atomically with the state change. Created
// carries the repository's name, default branch and hosting from the row;
// StateChanged carries From, To and Reason.
type Event struct {
	Kind             EventKind
	Actor            Actor
	From, To, Reason string
}

// Effects are the records a mutation writes with the aggregate.
type Effects struct {
	Audit  []Audit
	Events []Event
}

// Mutation changes a locked repository. It must be pure: no network or process
// calls, because it runs inside a database transaction. An error rolls back
// every effect. The store persists the repository only when the mutation
// changed it, always with the Effects.
type Mutation func(r *Repository) (Effects, error)

// Position is a list continuation: the (created_at, id) of the last row.
type Position struct {
	CreatedAt time.Time
	ID        ID
}

// ListQuery selects one page of a project's repositories, ordered by
// (created_at, id).
type ListQuery struct {
	After *Position
	Limit int
}

// Candidate is a repository whose provisioning is not complete.
type Candidate struct {
	Scope domain.Scope
	ID    ID
}

// Store persists repositories. Implementations scope every operation by
// tenant/project, lock the row for Update, write audit and outbox records in
// the same transaction as the change, and make no network call.
type Store interface {
	// Insert records a new repository unless one with the same
	// (tenant, project, name) exists; the existing row is then returned with
	// created false and nothing is written. A colliding identifier is an error,
	// never a replacement.
	Insert(ctx context.Context, r *Repository, fx Effects) (*Repository, bool, error)
	// Get returns the repository of the scope; a foreign or absent one is
	// ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, id ID) (*Repository, error)
	// GetInTenant is the worker-facing read: the tenant is the identity's and
	// the project is whatever owns the repository. A foreign or absent
	// repository is ErrNotFound.
	GetInTenant(ctx context.Context, tenant domain.TenantID, id ID) (*Repository, error)
	// GetByName returns the repository of the project with that name.
	GetByName(ctx context.Context, scope domain.Scope, name string) (*Repository, error)
	// List returns up to Limit repositories of the project after the position.
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]*Repository, error)
	// Update runs the mutation on the locked repository and persists its change
	// and effects atomically. A mutation that changed nothing writes nothing.
	Update(ctx context.Context, scope domain.Scope, id ID, fn Mutation) (*Repository, error)
	// Provisioning lists repositories of the tenant still provisioning, ordered
	// by identifier after the given one.
	Provisioning(ctx context.Context, tenant domain.TenantID, after ID, limit int) ([]Candidate, error)
}

// TenantSource enumerates tenants for background reconciliation.
type TenantSource interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// ---- authority and peer modules (injected; missing ones fail closed) ----

// Action names an authorization decision. The roles that hold them follow
// docs/specs/identity-tenancy-and-rbac.md: engineer manages repositories;
// viewer, analyst and reviewer read; admin sets policy but gains no implicit
// push. The composition maps these to the identity permissions.
type Action string

// Actions.
const (
	ActionCreate         Action = "repository.create"
	ActionRead           Action = "repository.read"
	ActionList           Action = "repository.list"
	ActionUpdate         Action = "repository.update"     // defaultBranch
	ActionSetPolicy      Action = "repository.set_policy" // policy
	ActionArchive        Action = "repository.archive"
	ActionRestore        Action = "repository.restore"
	ActionRegisterRemote Action = "repository.register_external" // read-only mapping of the project's configured repository
)

// Resource names the addressed resource of an authorization question.
type Resource struct {
	Kind string // "project" or "repository"
	ID   string
}

// Authorizer decides, with current state on every call, whether the actor may
// perform the action in the scope. It returns nil to allow, ErrDenied or
// ErrNotFound to refuse; any other error is an outage and refuses too.
type Authorizer interface {
	Authorize(ctx context.Context, actor Actor, action Action, scope domain.Scope, res Resource) error
}

// CloneURLs builds the credential-free clone URL of an internal repository,
// https://<git-host>/git/<tenant-key>/<project-key>/<name>.git. The tenant and
// project keys and the host are configuration the composition knows; this
// package never derives a URL from anything else.
type CloneURLs interface {
	CloneURL(ctx context.Context, scope domain.Scope, name string) (string, error)
}

// ExternalConfig is a project's configured external repository as the
// workspace module resolves it, without any secret reference. ID is the
// configured repository's identifier (workspace.RepositoryConfig.ID), URL its
// credential-free HTTPS URL and BaseRef the branch new work branches start from
// ("main" or "refs/heads/main").
type ExternalConfig struct {
	ID      string
	URL     string
	BaseRef string
}

// ExternalSource resolves the project's configured repository. It is
// implemented by an adapter over the workspace module's repository
// configuration; this package does not import it. ok is false for a project
// with no configured repository.
type ExternalSource interface {
	Configured(ctx context.Context, scope domain.Scope) (cfg ExternalConfig, ok bool, err error)
}

type requestIDKey struct{}

// WithRequestID attaches the request/correlation identifier recorded in audit
// and events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the identifier set by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
