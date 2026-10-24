package identity

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. Adapters implement them;
// the composition root wires them. Nothing has an allow-all default.

// CredentialVerifier verifies a bearer access token and returns the verified
// (issuer, subject). ErrUnauthenticated means the credential is bad; any other
// error is an outage of the verification dependency (never access).
type CredentialVerifier interface {
	Verify(ctx context.Context, rawAccessToken string) (Principal, error)
}

// PrincipalMapper maps a verified principal to its stable user, creating the
// user on first login. It never creates a membership. Concurrent first logins
// yield one user.
type PrincipalMapper interface {
	MapPrincipal(ctx context.Context, p Principal) (UserID, error)
}

// User is the caller's own record.
type User struct {
	ID        UserID
	Issuer    string
	Subject   string
	CreatedAt time.Time
}

// TenantSummary is one tenant the caller holds a binding in. Roles lists the
// caller's distinct tenant-wide roles (project-only members have none).
type TenantSummary struct {
	ID          domain.TenantID
	Key         string
	DisplayName string
	Roles       []Role
}

// Directory is the principal-level read port. Both reads are filtered by the
// authenticated user's ID; neither accepts a caller-supplied selector.
type Directory interface {
	// User returns the caller's own record; ErrNotFound if the user is unknown.
	User(ctx context.Context, id UserID) (User, error)
	// Tenants returns the tenants where the user holds a binding, ordered by ID,
	// strictly after afterID, at most limit rows. An empty list is a successful
	// answer only; a failure is never an empty list.
	Tenants(ctx context.Context, id UserID, afterID string, limit int) ([]TenantSummary, error)
}

// BindingQuery selects bindings of one tenant. AllScopes reads every binding;
// otherwise only bindings of the listed projects. Ordered by ID, strictly after
// After.
type BindingQuery struct {
	AllScopes bool
	Projects  []domain.ProjectID
	Project   domain.ProjectID // optional narrowing filter
	User      UserID           // optional narrowing filter
	After     BindingID
	Limit     int
}

// ProjectQuery selects projects of one tenant. All reads every project;
// otherwise only the listed ones. Ordered by ID, strictly after After.
type ProjectQuery struct {
	All   bool
	Only  []domain.ProjectID
	After domain.ProjectID
	Limit int
}

// ChangeKind classifies a recorded identity change.
type ChangeKind string

const (
	ChangeBindingGranted     ChangeKind = "membership.granted"
	ChangeBindingRoleChanged ChangeKind = "membership.role_changed"
	ChangeBindingRevoked     ChangeKind = "membership.revoked"
	ChangeProjectCreated     ChangeKind = "project.created"
	ChangeProjectUpdated     ChangeKind = "project.updated"
)

// BindingOp is one binding transition inside a BindingChange.
type BindingOp struct {
	ID           BindingID
	Kind         ChangeKind // ChangeBindingGranted, ChangeBindingRoleChanged or ChangeBindingRevoked
	Role         Role       // resulting role; for a revocation the role removed
	PreviousRole Role       // role before a role change
	Version      domain.Version
}

// BindingChange describes one user's binding transitions inside a Change.
type BindingChange struct {
	User    UserID
	Project domain.ProjectID // scope of the bindings: empty for tenant-wide
	Ops     []BindingOp
	// EffectiveVersion is the user's authorization revision after the change.
	EffectiveVersion int64
}

// ProjectChange describes the project effect of a Change.
type ProjectChange struct {
	Scope   domain.Scope
	Version domain.Version
	Changed []string
}

// Change is what one successful transaction records, atomically with the state
// change, as audit rows and typed events. Duplicate or failed requests never
// reach Record.
type Change struct {
	ID         string // unique and stable; derives audit and event identifiers
	Kind       ChangeKind
	Actor      Actor
	RequestID  string
	OccurredAt time.Time
	Tenant     domain.TenantID
	Bindings   []BindingChange
	Project    *ProjectChange
}

// Tx is what a use case sees inside the repository's single tenant
// transaction. Every read observes current state.
//
// Order contract (enforced by the adapter): LockProject, then LockAuthorization,
// then reads/mutations, then Record last; no lock or mutation follows Record.
type Tx interface {
	// LockProject takes the project row lock and returns the locked project.
	LockProject(ctx context.Context, id domain.ProjectID) (Project, error)
	// LockAuthorization serializes with every reader/writer of the listed users'
	// authorization: exclusive for changed users, shared for observers (the
	// acting administrators). Locks are acquired in a deterministic order.
	LockAuthorization(ctx context.Context, changed []UserID, observed []UserID) error
	// Bindings returns the user's current bindings in the tenant.
	Bindings(ctx context.Context, user UserID) ([]Binding, error)
	Binding(ctx context.Context, id BindingID) (Binding, error)
	ListBindings(ctx context.Context, q BindingQuery) ([]Binding, error)
	InsertBinding(ctx context.Context, b Binding) error
	UpdateBinding(ctx context.Context, next Binding, expected domain.Version) error
	DeleteBinding(ctx context.Context, b Binding) error
	// BumpAuthorization advances the user's authorization revision and returns
	// it. The revision is retained after the last binding is removed.
	BumpAuthorization(ctx context.Context, user UserID) (int64, error)

	Project(ctx context.Context, id domain.ProjectID) (Project, error)
	ListProjects(ctx context.Context, q ProjectQuery) ([]Project, error)
	InsertProject(ctx context.Context, p Project) error
	UpdateProject(ctx context.Context, next Project, expected domain.Version) error

	// Record writes the audit rows and events of the change. It is last.
	Record(ctx context.Context, c Change) error
}

// Repository runs identity mutations and reads atomically. One Transact call is
// one tenant transaction: state, audit, authorization revisions and outbox
// events commit together or not at all. An unknown commit outcome is reported
// as ErrUnavailable and never replayed automatically.
type Repository interface {
	Transact(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx Tx) error) error
}
