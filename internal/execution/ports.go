package execution

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// The ports below are small and owned by this module. Adapters implement them;
// the composition root wires them. A missing implementation fails closed:
// nothing here has an allow-all default.

// Authority resolves a verified principal to an attributable actor and answers
// whether the actor currently holds a permission. Authorize returns nil to
// allow, ErrDenied or ErrHidden to refuse, and any other error for an outage
// (treated as unavailable, never as access). The transaction-bound variant used
// by dispatch is TxAuthority.
type Authority interface {
	ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (Actor, error)
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// TxAuthority is the permission check bound to one open tenant transaction. It
// reads current bindings through the transaction and holds the user's
// authorization revision until it ends, so a revocation committed while the
// transaction waited on a lock is seen.
type TxAuthority interface {
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
}

// Definitions resolves an immutable registered pipeline definition. The
// pipeline module's repository satisfies it.
type Definitions interface {
	Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error)
}

// ArtifactResolver resolves the verified descriptor of an admitted artifact
// input. It is a read-only admission seam; the artifact module owns ownership
// and content state.
type ArtifactResolver interface {
	ResolveArtifact(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (runtimev1.ArtifactRef, error)
}

// WorkSnapshot is the work item's content at one revision.
type WorkSnapshot struct {
	ID              domain.WorkItemID
	Actionable      bool
	Cancelled       bool
	ContentRevision domain.ContentRevision
	Spec            string
	Criteria        []runtimev1.AcceptanceCriterion
}

// WorkContent reads a work item's current content for pinning. It runs outside
// the admission transaction; the transaction re-verifies the revision under the
// item lock, so an edit racing admission is a conflict, never a stale pin.
type WorkContent interface {
	Snapshot(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (WorkSnapshot, error)
}

// InputDecl is one declared run input.
type InputDecl struct {
	Name     string
	Type     string
	Default  *runtimev1.Value
	Required bool
}

// SecretUse is one declared secret reference of a step.
type SecretUse struct {
	Step     string
	Name     string
	Ref      string
	Delivery runtimev1.SecretDelivery
}

// ConfigRequest asks for deterministic resolution of declared inputs and the
// exact secret versions to pin.
type ConfigRequest struct {
	Scope    domain.Scope
	Inputs   []InputDecl
	Explicit map[string]runtimev1.Value
	Secrets  []SecretUse
}

// ConfigResult is the immutable resolution: effective inputs with provenance
// and per-step pinned secret bindings. It carries no plaintext.
type ConfigResult struct {
	Snapshot Snapshot
}

// Configuration resolves inputs (precedence default, project variable, run
// input) and pins secret versions. Rejections are ErrAdmissionRefused.
type Configuration interface {
	Resolve(ctx context.Context, req ConfigRequest) (ConfigResult, error)
}

// Sources is the workspace seam of execution. All methods run outside SQL
// transactions.
type Sources interface {
	// Verify checks that commit belongs to the work item's retained workspace
	// history and returns where the source lives. A commit outside the history
	// is ErrAdmissionRefused.
	Verify(ctx context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (SourceInfo, error)
	// Describe returns where the workspace's source lives. With refresh set
	// (writer admission) it first observes the remote branch and returns the
	// freshly accepted tip; the dispatch transaction rechecks that tip under
	// the workspace lock.
	Describe(ctx context.Context, scope domain.Scope, workspaceID string, refresh bool) (SourceInfo, error)
	// Retain creates and confirms the run-bound retained source reference and
	// returns its identifier. It is idempotent: repeating it for the same run
	// and commit returns the same reference.
	Retain(ctx context.Context, scope domain.Scope, workspaceID string, run domain.RunID, commit string) (string, error)
}

// DeploymentCleanup is the internal, deployment-bound authorization of a
// cleanup run (M2, T-035). It confirms the recorded deployment exists in the
// scope and that cleanup is requested for it. It is unavailable in M1 wiring:
// without it no cleanup run can be admitted.
type DeploymentCleanup interface {
	AuthorizeCleanup(ctx context.Context, scope domain.Scope, deployment string) error
}

// DeploymentEnvironments resolves the retained configuration of a recorded
// deployment for its cleanup run (M2, T-077). It runs outside the admission
// transaction and returns the immutable snapshot the deployment pinned, never
// the environment's current settings; an unavailable or revoked retained
// snapshot is an error. Without it, a cleanup definition that declares an
// environment cannot be admitted. The admission transaction rechecks the result
// through TxEnvironments.
type DeploymentEnvironments interface {
	RetainedEnvironment(ctx context.Context, scope domain.Scope, deployment string) (EnvironmentSnapshot, error)
}

// Limits supplies the typed execution policy of a scope. There is no default:
// a missing or invalid limit denies dispatch.
type Limits interface {
	Limits(ctx context.Context, scope domain.Scope) (Limit, error)
}

// EffectQuery names the attempt whose external effects are uncertain.
type EffectQuery struct {
	Scope     domain.Scope
	RunID     domain.RunID
	StepID    string
	AttemptID domain.AttemptID
	ActionKey string
}

// EffectReconciler is the trusted adapter that can establish what an uncertain
// attempt did (a tool that honors the action key or exposes outcome lookup).
// General commands have none: without affirmative evidence the step stays
// blocked.
type EffectReconciler interface {
	Reconcile(ctx context.Context, q EffectQuery) (ReconciliationEvidence, error)
}

// Tenants enumerates tenants for background reconciliation.
type Tenants interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// WorkCancellation asks execution to cancel the current work runs of an item.
type WorkCancellation struct {
	Scope      domain.Scope
	WorkItemID domain.WorkItemID
	Version    domain.Version
}

// CancelledWork scans cancelled work items so a lost or reordered event cannot
// strand running work or permit dispatch.
type CancelledWork interface {
	CancelledItems(ctx context.Context, tenant domain.TenantID, after CancelCursor, limit int) ([]WorkCancellation, error)
}

// CancelCursor continues a cancelled-item scan.
type CancelCursor struct {
	ProjectID domain.ProjectID
	ID        domain.WorkItemID
}

// WorkAdmission is the locked, current eligibility of a work item.
type WorkAdmission struct {
	ContentRevision domain.ContentRevision
}

// WriterOutcome is how a writer attempt ended.
type WriterOutcome string

const (
	WriterSucceeded WriterOutcome = "succeeded"
	WriterFailed    WriterOutcome = "failed"
	WriterCancelled WriterOutcome = "cancelled"
)

// Env is the transaction-bound environment of a mutation. Every method acts
// through the open transaction, so authority, capacity, writer and secret state
// are consistent with the aggregate being changed. It makes no network call.
//
// Lock order (every execution transaction): quota scope rows (tenant, then
// project) -> work-item admission lock -> run, step and attempt rows ->
// authorization revision (read by the mutation's Authorize) -> runtime grants
// of attempts whose authority ended -> secret rows -> the workspace row ->
// event backlog and scope rows. Mutations therefore do their reads and pure decisions first
// and hand peer effects to Effects.After, which the store runs in this order.
type Env interface {
	Now() time.Time
	NewID(prefix string) string
	// Authorize checks the actor's current permission through the transaction.
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
	// Work is the work-item admission taken under UpdateOptions.LockWork: nil
	// error means actionable and not cancelled; ErrWorkCancelled, ErrNotFound
	// or ErrNotActionable otherwise.
	Work() (WorkAdmission, error)
	// CapacityAvailable reports ErrQuotaExceeded when the scope has no room.
	CapacityAvailable(ctx context.Context, scope domain.Scope, lim Limit) error
	// ReserveCapacity records the attempt's reservation once (idempotent).
	ReserveCapacity(ctx context.Context, at *Attempt) error
	// ReleaseCapacity releases the reservation exactly once (idempotent).
	ReleaseCapacity(ctx context.Context, at *Attempt) error
	// ReserveWriter reserves the workspace's single writer slot for the attempt.
	ReserveWriter(ctx context.Context, at *Attempt) error
	// SettleWriter ends the writer reservation; ErrBusy while the publication is
	// unsettled. The adapter performs it after the other queued effects (and
	// after every run of a batch has released its secrets), not at the call.
	SettleWriter(ctx context.Context, at *Attempt, outcome WriterOutcome, source string) error
	// ReleaseSecrets releases the run's secret-consumer pins.
	ReleaseSecrets(ctx context.Context, run *Run) error
}

// Effects are peer effects a mutation hands to the store.
type Effects struct {
	// After run once the execution rows are saved and the grants of attempts
	// revoked without an accepted result are ended, in order, before events. The
	// adapter runs SettleWriter (the workspace row) after every other effect
	// here, so entities may queue effects in any order without inverting the
	// transaction-wide lock order.
	After []func(ctx context.Context, env Env) error
	// Result is returned to the caller of Update.
	Result any
}

// Mutation changes the locked aggregate. It must be pure apart from Env: no
// network or process calls, because it runs inside a database transaction. An
// error rolls back every effect.
type Mutation func(ctx context.Context, env Env, agg *Aggregate) (Effects, error)

// UpdateOptions select the locks a mutation needs, in documented order.
type UpdateOptions struct {
	// Quota locks the tenant and project quota scopes first.
	Quota bool
	// LockWork takes the work-item admission lock before the run rows.
	LockWork bool
	// SkipLocked returns ErrBusy instead of waiting for a locked run row.
	SkipLocked bool
}

// RunRef addresses a run for scans.
type RunRef struct {
	Scope domain.Scope
	ID    domain.RunID
}

// Position is the continuation key of a list.
type Position struct {
	CreatedAt time.Time
	ID        domain.RunID
}

// ListQuery filters and pages runs ordered by (created_at, id).
type ListQuery struct {
	Statuses   []RunStatus
	WorkItemID domain.WorkItemID
	After      *Position
	Limit      int
}

// AdmissionPlan is a validated, fully pinned run ready to commit. Everything
// outside the transaction (definition, configuration, content, source
// verification, authorization) is already resolved.
type AdmissionPlan struct {
	Aggregate *Aggregate
	// Source is the workspace of a work run.
	Source SourceInfo
	// PinPrefix is the retained-ref namespace (workspace option).
	Actor     Actor
	RequestID string
	// WorkRevision is the content revision the pin was resolved at.
	WorkRevision domain.ContentRevision
	// Secrets lists the unique pinned versions to register as run pins.
	Secrets []runtimev1.SecretBinding
	// Environments lists the retained snapshots the run pins. A store must call
	// BindEnvironments with its transaction-bound TxEnvironments before it
	// saves the run; a plan with pins and no binding fails closed.
	Environments []EnvironmentBinding
}

// Store persists runs. One method call is one transaction: state, history,
// audit and outbox events commit together or not at all. Implementations scope
// every operation by tenant and project and apply the lock order documented on
// Env.
type Store interface {
	// Admit creates the run unless it exists. created is false for a replay of
	// the same run ID.
	Admit(ctx context.Context, p *AdmissionPlan) (run *Run, created bool, err error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*Aggregate, error)
	List(ctx context.Context, scope domain.Scope, q ListQuery) ([]*Run, error)
	Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts UpdateOptions, fn Mutation) (*Aggregate, any, error)
	// RunOf resolves the run of an attempt inside the scope (ErrNotFound for a
	// foreign or unknown attempt).
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	// Dispatchable lists nonterminal runs that may have a runnable step, oldest
	// first, after the given reference.
	Dispatchable(ctx context.Context, tenant domain.TenantID, after RunRef, limit int) ([]RunRef, error)
	// Active lists nonterminal runs for reconciliation.
	Active(ctx context.Context, tenant domain.TenantID, after RunRef, limit int) ([]RunRef, error)
	// RequestCancellation records the item-scoped intent and cancels the item's
	// nonterminal runs in one transaction. It is idempotent.
	RequestCancellation(ctx context.Context, c WorkCancellation) error
	// Removable lists settled attempts, of nonterminal and terminal runs alike,
	// whose workload removal is not yet recorded and whose run is pinned to
	// backend: oldest settlement first, after the cursor. Runs pinned to
	// another backend, or to none, are never listed.
	Removable(ctx context.Context, tenant domain.TenantID, backend string, after RemovalCursor, limit int) ([]Removal, error)
	// RecordRemoval records, once, that the settled attempt's workload is gone.
	RecordRemoval(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error
}

// EnvironmentStore is optionally implemented by a Store whose Admit calls
// BindEnvironments inside the admission transaction and registers the exact
// credential pins of every admitted run, cleanup runs included. Cleanup
// admission that declares an environment is refused (ErrNotConfigured) unless
// the configured store reports it.
type EnvironmentStore interface {
	AdmitsEnvironments() bool
}

func storeAdmitsEnvironments(s Store) bool {
	e, ok := s.(EnvironmentStore)
	return ok && e.AdmitsEnvironments()
}

// Removal is a settled attempt whose workload the runtime has not yet
// confirmed removed. Backend is the run's pinned runtime ("" when unbound).
type Removal struct {
	Workload  Workload
	Backend   string
	SettledAt time.Time
}

// Cursor returns the continuation key after r.
func (r Removal) Cursor() RemovalCursor {
	return RemovalCursor{SettledAt: r.SettledAt, ProjectID: r.Workload.Scope.ProjectID, AttemptID: r.Workload.AttemptID}
}

// RemovalCursor continues a Removable scan.
type RemovalCursor struct {
	SettledAt time.Time
	ProjectID domain.ProjectID
	AttemptID domain.AttemptID
}

// Dependencies are the collaborators of a Service. Required ports must be set;
// optional ones disable the behavior that needs them and never loosen a check.
type Dependencies struct {
	Store       Store
	Authority   Authority
	Definitions Definitions
	// Capabilities reports enabled step kinds for admission.
	Capabilities  pipeline.Capabilities
	Work          WorkContent
	Configuration Configuration
	Sources       Sources
	Artifacts     ArtifactResolver
	Limits        Limits
	Planner       Planner
	Hooks         Hooks
	Tenants       Tenants
	// Runtime runs attempt workloads. Without it runs are admitted unbound; an
	// unbound run is never dispatched, so admit with the runtime that will run it.
	Runtime Runtime
	// Requirements is the installation's admission policy (nil selects
	// DefaultRequirements).
	Requirements Requirements
	// Shared is the explicit opt-in of the run_shared model: the companion
	// resource port of the same backend plus the policy source. Nil (the
	// default) keeps new-format admission closed.
	Shared *SharedConfig
	// Optional.
	Cleanup DeploymentCleanup
	// CleanupEnvironments resolves a deployment's retained configuration for
	// cleanup admission; without it environment-declaring cleanup is refused.
	CleanupEnvironments DeploymentEnvironments
	EffectReconciler    EffectReconciler
	CancelledWork       CancelledWork
	Clock               func() time.Time
	NewID               func(prefix string) string
}
