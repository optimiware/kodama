package workspace

import (
	"context"
	"io"
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
// envelope and payload.
type EventKind string

const (
	EventStateChanged      EventKind = "workspace.state_changed"
	EventCheckpointCreated EventKind = "workspace.checkpoint_created"
)

// Event is a domain event appended atomically with the state change.
type Event struct {
	Kind  EventKind
	Actor Actor
	// state_changed
	From, To, Reason string
	// checkpoint_created
	PreviousCommit, NewCommit Commit
	PublishingAttempt         domain.AttemptID
	HumanSynchronization      bool
	// Repository names the repository of a checkpoint of an internally hosted
	// workspace; empty means the workspace's own (primary) repository.
	Repository RepositoryID
}

// Effects are the records a mutation writes with the aggregate.
type Effects struct {
	Audit  []Audit
	Events []Event
	// Receipt, when set, is inserted or updated.
	Receipt *Publication
	// Pins are inserted or updated.
	Pins []*RetainedRef
	// Associations and Checkpoints are the internal-hosting records: inserted,
	// or updated in their mutable fields (an association's state, accepted
	// commit and reason; a checkpoint's retained ref and its status).
	Associations []*RepositoryAssociation
	Checkpoints  []*InternalCheckpoint
}

// Aggregate is what a mutation sees: the locked workspace plus the optional
// records its Load asked for.
type Aggregate struct {
	Workspace *Workspace
	Receipt   *Publication
	Pins      []*RetainedRef
	// Associations and Checkpoints are set when Load asked for them, ordered by
	// repository (associations) and creation (checkpoints).
	Associations []*RepositoryAssociation
	Checkpoints  []*InternalCheckpoint
}

// Load names the optional records a mutation needs.
type Load struct {
	// Receipt loads the publication receipt of this attempt, if any.
	Receipt domain.AttemptID
	// Pins loads every retained reference of the workspace.
	Pins bool
	// Associations loads the repository associations of an internally hosted
	// workspace; Checkpoints its accepted push entries.
	Associations bool
	Checkpoints  bool
}

// Mutation changes a locked aggregate. It must be pure: no network or process
// calls, because it runs inside a database transaction. An error rolls back
// every effect.
type Mutation func(a *Aggregate) (Effects, error)

// Candidate is a workspace with durable work for the reconciler.
type Candidate struct {
	Scope domain.Scope
	ID    ID
}

// Store persists workspaces. Implementations scope every operation by
// tenant/project, lock the workspace row for Update, compare versions, and
// write audit and outbox records in the same transaction.
type Store interface {
	// Insert creates the workspace unless one exists for its work item. It
	// returns the persisted workspace and whether this call created it.
	Insert(ctx context.Context, w *Workspace, fx Effects) (*Workspace, bool, error)
	Get(ctx context.Context, scope domain.Scope, id ID) (*Workspace, error)
	GetByWorkItem(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (*Workspace, error)
	View(ctx context.Context, scope domain.Scope, id ID, load Load) (*Aggregate, error)
	Update(ctx context.Context, scope domain.Scope, id ID, load Load, fn Mutation) (*Aggregate, error)
	// Reconcilable lists workspaces of a tenant with unfinished durable work,
	// ordered by ID after the given ID.
	Reconcilable(ctx context.Context, tenant domain.TenantID, after ID, limit int) ([]Candidate, error)
	// ListProject lists workspace IDs of a project ordered by ID.
	ListProject(ctx context.Context, scope domain.Scope, after ID, limit int) ([]ID, error)
}

// TenantSource enumerates tenants for background reconciliation.
type TenantSource interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// ---- authority and peer modules (injected; missing ones fail closed) ----

// Action names an authorization decision.
type Action string

const (
	ActionProvision Action = "workspace.provision"
	ActionRead      Action = "workspace.read"
	ActionSync      Action = "workspace.sync"
	ActionArchive   Action = "workspace.archive"
	ActionRestore   Action = "workspace.restore"
	ActionPin       Action = "workspace.pin"
	ActionRemove    Action = "workspace.remove_ref"
	ActionFence     Action = "workspace.fence"
	ActionAdmit     Action = "workspace.reserve_writer"
	// ActionCheckpoint accepts an applied push as an internal checkpoint.
	ActionCheckpoint Action = "workspace.accept_checkpoint"
)

// Resource names the addressed resource of an authorization question.
type Resource struct {
	Kind string // "work_item" or "workspace"
	ID   string
}

// Authorizer decides, with current state on every call, whether the actor may
// perform the action in the scope. It returns nil to allow, ErrDenied or
// ErrNotFound to refuse; any other error is an outage and refuses too.
type Authorizer interface {
	Authorize(ctx context.Context, actor Actor, action Action, scope domain.Scope, res Resource) error
}

// WorkItems resolves work eligibility inside the scope: nil for an actionable
// item, ErrNotFound for a missing or foreign one, ErrNotActionable for an epic.
type WorkItems interface {
	Actionable(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error
}

// RepositoryConfig is the project's configured repository, resolved by the
// owning module. Secret references are opaque; the checkout and publisher
// references must differ.
type RepositoryConfig struct {
	ID                 RepositoryID
	URL                string
	CheckoutSecretRef  string
	PublisherSecretRef string
	// BaseRef is the branch new work branches start from.
	BaseRef string
}

type Repositories interface {
	Repository(ctx context.Context, scope domain.Scope) (RepositoryConfig, error)
}

// Access is the attempt's declared workspace access.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// WriterAuthority is the current authority of an attempt over a workspace.
type WriterAuthority struct {
	WorkspaceID ID
	Access      Access
	// Live is false once the attempt is cancelled, revoked, superseded or
	// finished.
	Live bool
}

// Attempts resolves attempt authority inside the scope (ErrNotFound for a
// foreign or unknown attempt).
type Attempts interface {
	Authority(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (WriterAuthority, error)
}

// Bundle describes a temporary bundle artifact and gives access to its bytes.
type Bundle struct {
	ID        domain.ArtifactID
	Scope     domain.Scope
	AttemptID domain.AttemptID // producing attempt
	Role      string           // "bundle"
	Complete  bool
	Digest    string
	Size      int64
	Body      io.ReadCloser
}

// Bundles gives scoped access to completed bundle artifacts.
type Bundles interface {
	Open(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (Bundle, error)
	// Settled tells the artifact owner the publication reached a final state
	// so the temporary bundle may be cleaned up. Best effort.
	Settled(ctx context.Context, scope domain.Scope, id domain.ArtifactID) error
}

// SecretNeedles returns the injected secret values of an attempt that must not
// appear in new source objects.
type SecretNeedles interface {
	Needles(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error)
}

// DeletionPolicy decides whether a released retained reference may be deleted.
// A nil policy denies every removal; Kodama never selects retention periods.
type DeletionPolicy interface {
	AllowDeletion(ctx context.Context, scope domain.Scope, pin *RetainedRef) (bool, error)
}

// RotationEvidence proves publisher credentials were rotated after a fence.
type RotationEvidence struct {
	RotationID string
	RotatedAt  time.Time
}

// PublisherRotation verifies rotation evidence for publication resumption.
type PublisherRotation interface {
	VerifyRotated(ctx context.Context, scope domain.Scope, e RotationEvidence) error
}

// ---- Git ----

// RepoRef addresses a remote repository and the credential references to use.
// Credentials are resolved inside the adapter: the reader adapter resolves only
// CheckoutSecretRef, the publisher adapter only PublisherSecretRef.
type RepoRef struct {
	Scope              domain.Scope
	URL                string
	CheckoutSecretRef  string
	PublisherSecretRef string
}

// BranchInfo is the observed state of a branch relative to a commit.
type BranchInfo struct {
	Tip      Commit
	Exists   bool
	Contains bool // Tip's history includes the queried commit (or equals it)
}

// RefReader reads remote state with read-only credentials.
type RefReader interface {
	// ResolveRef returns the current commit of ref.
	ResolveRef(ctx context.Context, repo RepoRef, ref string) (Commit, bool, error)
	// BranchContains fetches branch and reports whether commit is in its history.
	// A zero commit only resolves the tip.
	BranchContains(ctx context.Context, repo RepoRef, branch string, commit Commit) (BranchInfo, error)
}

// PrepareRequest verifies a bundle against pinned source in private scratch.
type PrepareRequest struct {
	Repo        RepoRef
	Branch      string
	Body        io.Reader
	Digest      string
	Size        int64
	Input       Commit
	ExpectedTip Commit
	Commit      Commit
	Needles     [][]byte
}

// Prepared is verified source in bounded private scratch, ready to publish.
type Prepared interface {
	// ObservedTip is the branch tip seen while preparing ("" if missing).
	ObservedTip() Commit
	// DescendsFrom reports whether the prepared commit descends from c.
	DescendsFrom(ctx context.Context, c Commit) (bool, error)
	Release()
}

// SourcePreparer validates bundles. Failures are *BundleError, ErrLimitExceeded,
// ErrSecretInSource or ErrUnavailable.
type SourcePreparer interface {
	Prepare(ctx context.Context, req PrepareRequest) (Prepared, error)
}

// PublishRequest is an explicit expected-old-ref update.
type PublishRequest struct {
	Branch   string
	Expected Commit
	Commit   Commit
	Source   Prepared
}

// RefPublisher is the only component holding write authority.
type RefPublisher interface {
	// Publish updates Branch from Expected to Commit as a compare-and-swap
	// (never an unconditional force). Any error leaves the outcome unknown
	// until read back from the remote.
	Publish(ctx context.Context, repo RepoRef, req PublishRequest) error
	// CreateRef creates ref at commit only if absent; an existing ref at the
	// same commit succeeds and one elsewhere is ErrBranchExists. from names a
	// branch whose history contains commit.
	CreateRef(ctx context.Context, repo RepoRef, ref string, commit Commit, from string) error
	// DeleteRef deletes ref if it still points at expected; an absent ref
	// succeeds.
	DeleteRef(ctx context.Context, repo RepoRef, ref string, expected Commit) error
}

// ---- correlation ----

type ctxKey int

const requestIDKey ctxKey = 1

// WithRequestID attaches the request/correlation identifier recorded in audit
// and events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the identifier set by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}
