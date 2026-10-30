package productmcp

import (
	"context"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Caller is the authenticated MCP principal. It is the only identity the use
// cases ever see: the package holds no service identity, so a tool can never do
// more than its caller may do over HTTP or the CLI (AC-15).
//
// A person (Actor.Type user) acts with that person's current roles. A
// provisioning attempt (Actor.Type workload) is bound to the project of its run:
// every scope outside Project is not_found, and it may not create projects,
// request seeds or start runs (PX-25: its seed authority comes from the runtime
// route, T-150, and only for repositories its run created).
//
// Only a provisioning attempt may be a workload caller: the Authenticator
// mints one for a live attempt of a run whose origin is provision, in that
// run's admitted project, and for nothing else (a work item's, push's or manual
// run's attempt is refused at authentication). Validate refuses a workload
// whose RunOrigin is not OriginProvision, so a mis-built authenticator fails
// closed. Liveness changes after authentication, so every port re-checks that
// the attempt is still live and the run still provisioning, like any other
// current-authority check.
type Caller struct {
	Actor gitv1.Actor
	// Tenant is the authenticated principal's tenant. It is never taken from a
	// tool argument.
	Tenant domain.TenantID
	// Project is the project of a workload's run; empty for a person.
	Project domain.ProjectID
	// RunOrigin is the origin kind of a workload's run (execution's origin
	// wire value); empty for a person. It must be OriginProvision.
	RunOrigin string
	// RequestID is the correlation identifier recorded in audit and events.
	RequestID string
}

// OriginProvision is the run origin of a provisioning run (PX-25), the only
// origin whose attempts may call the product tools.
const OriginProvision = "provision"

// IsWorkload reports whether the caller is a run's attempt.
func (c Caller) IsWorkload() bool { return c.Actor.Type == gitv1.ActorWorkload }

// Validate checks the principal's shape.
func (c Caller) Validate() error {
	if err := c.Tenant.Validate(); err != nil {
		return fmt.Errorf("%w: caller tenant", ErrUnauthenticated)
	}
	// The contract's own actor rules: a valid type, a well-formed identity and,
	// when present, well-formed run and attempt identifiers (an empty or odd one
	// would collapse every workload onto one idempotency principal).
	if err := c.Actor.Validate(); err != nil {
		return fmt.Errorf("%w: caller identity", ErrUnauthenticated)
	}
	switch c.Actor.Type {
	case gitv1.ActorUser:
		if c.Project != "" || c.RunOrigin != "" || c.Actor.RunID != nil || c.Actor.AttemptID != nil {
			return fmt.Errorf("%w: a person carries no run binding", ErrUnauthenticated)
		}
	case gitv1.ActorWorkload:
		if c.Project.Validate() != nil || c.Actor.RunID == nil || c.Actor.AttemptID == nil {
			return fmt.Errorf("%w: a workload is bound to its run and project", ErrUnauthenticated)
		}
		if c.RunOrigin != OriginProvision {
			return fmt.Errorf("%w: only a provisioning attempt may call these tools", ErrUnauthenticated)
		}
	default:
		return fmt.Errorf("%w: caller type", ErrUnauthenticated)
	}
	return nil
}

// ---- consumer-owned ports ----------------------------------------------------
//
// Every method receives the Caller and must authorize it against current state
// before acting, exactly as the HTTP handler's use case does, and fail closed
// (an authority outage is ErrUnavailable, never access). Adapters (wired by
// T-132) call the same authorized use cases as the HTTP routes with the
// caller's identity, and translate their failures to this package's classes.

// Repositories is the product MCP server's use of the repository use cases
// (contracts/git/README.md, S10). It adds no authority.
type Repositories interface {
	// Create is the repository create use case; idempotencyKey makes the
	// reservation exactly-once for IdempotencyPrincipal(caller). When the caller
	// is a run's attempt it MUST record the creating run (caller.Actor.RunID and
	// AttemptID) with the repository, durably and in the reservation's own
	// transaction: T-150 allows a run's seed grant only for "a repository this
	// run created" and reads that record. A replay under the same key returns
	// the same repository and keeps the original creating run.
	Create(ctx context.Context, caller Caller, scope domain.Scope, idempotencyKey string, req gitv1.CreateRepositoryRequest) (gitv1.Repository, error)
	Get(ctx context.Context, caller Caller, scope domain.Scope, repositoryID string) (gitv1.Repository, error)
	List(ctx context.Context, caller Caller, scope domain.Scope, cursor *string) (gitv1.RepositoryList, error)
	// RequestSeed is gitauth.Credentials.IssueSeed for a person with engineer on
	// the project. The tool layer never calls it for a workload.
	RequestSeed(ctx context.Context, caller Caller, scope domain.Scope, repositoryID string) (gitv1.CredentialResponse, error)
}

// Project is the tool-visible view of a project.
type Project struct {
	ID             domain.ProjectID
	Key            string
	DisplayName    string
	ActiveWIPLimit int
	Version        int64
}

// ProjectList is one page of projects the caller may read.
type ProjectList struct {
	Items      []Project
	NextCursor *string
}

// CreateProjectRequest creates a project in the caller's tenant.
type CreateProjectRequest struct {
	Key            string
	DisplayName    string
	ActiveWIPLimit int
}

// Projects is the identity module's project use cases for a person.
type Projects interface {
	// Create is exactly-once for IdempotencyPrincipal(caller) and
	// idempotencyKey, keyed like WorkItems.Create (only a person calls it).
	Create(ctx context.Context, caller Caller, idempotencyKey string, req CreateProjectRequest) (Project, error)
	Get(ctx context.Context, caller Caller, projectID domain.ProjectID) (Project, error)
	List(ctx context.Context, caller Caller, cursor *string) (ProjectList, error)
}

// Criterion is one acceptance criterion of a work item.
type Criterion struct {
	ID   string
	Text string
}

// CreateWorkItemRequest creates a work item in backlog.
type CreateWorkItemRequest struct {
	Kind     string // epic, story, task or defect
	Title    string
	Spec     string
	Criteria []Criterion
}

// WorkItem is the tool-visible view of a work item.
type WorkItem struct {
	ID              domain.WorkItemID
	Kind            string
	Title           string
	State           string
	Version         int64
	ContentRevision int64
	CreatedAt       time.Time
}

// WorkItemQuery filters and pages a list; the adapter binds the cursor to the
// scope and filter (AC-04).
type WorkItemQuery struct {
	States []string
	Kinds  []string
	Cursor *string
	Limit  int
}

// WorkItemList is one page of work items.
type WorkItemList struct {
	Items      []WorkItem
	NextCursor *string
}

// WorkItems is the work module's create/get/list use cases.
type WorkItems interface {
	// Create makes the create exactly-once for IdempotencyPrincipal(caller) and
	// idempotencyKey: the adapter keys its idempotency record by that principal
	// (a run, not an attempt, for a workload) and returns the same item for a
	// retry, in the creating transaction.
	Create(ctx context.Context, caller Caller, scope domain.Scope, idempotencyKey string, req CreateWorkItemRequest) (WorkItem, error)
	Get(ctx context.Context, caller Caller, scope domain.Scope, id domain.WorkItemID) (WorkItem, error)
	List(ctx context.Context, caller Caller, scope domain.Scope, q WorkItemQuery) (WorkItemList, error)
}

// StartProvisioning is the bounded request to start a provisioning run (origin
// provision, PX-25). It carries no credential, no seed grant and no origin: the
// port always admits origin provision, and T-150 issues the run's seed grant
// through the runtime route for repositories the run itself creates.
type StartProvisioning struct {
	// RunID is derived from the caller's idempotency identity, so a retry is a
	// replay of the stored run and another declaration under it is a conflict.
	RunID        domain.RunID
	DefinitionID domain.DefinitionID
	// Repository optionally names the repository (by name) of the run's implicit
	// source; empty for a source-free run.
	Repository string
	Inputs     map[string]string
	// MaxActive bounds the PROJECT's active provisioning runs, whoever created
	// them (a project-wide cap: N engineers share it, they do not each get one).
	// "Active" is: a run of origin provision in this scope's project whose
	// status is not terminal. Start's contract, in this order:
	//
	//  1. A run with RunID already exists in the scope. It is a replay only if
	//     it was created by this caller (the same principal) with origin
	//     provision: then compare the WHOLE declaration (DefinitionID,
	//     Repository and Inputs; the tool layer's request hash is not
	//     available, because the Results record may have been lost). An
	//     identical declaration returns the stored run unchanged, BEFORE and
	//     regardless of the budget; a different one is ErrIdempotencyConflict
	//     (the same code the tool layer gives its own mismatch). A run of
	//     another creator or origin under that RunID is ErrNotFound (the
	//     existence of another principal's run is not disclosed) and is never
	//     returned, replayed or modified. A RunID of another tenant or project
	//     does not exist in the scope at all.
	//  2. Otherwise authorize, then count the project's active runs and admit
	//     the new one as ONE atomic step (a transaction that locks the
	//     project's admission row, or one conditional insert): when the count is
	//     already MaxActive nothing is admitted and the answer is ErrBudget. A
	//     count read in one step and an insert in another would let concurrent
	//     starts exceed the cap.
	//
	// The tool layer performs no pre-count; this is the only enforcement.
	MaxActive int
}

// ProvisioningRun is the tool-visible view of a provisioning run.
type ProvisioningRun struct {
	ID           domain.RunID
	DefinitionID domain.DefinitionID
	Status       string
	Reason       string
}

// Provisioning is the execution module's run admission for origin provision,
// acting with the caller's run:manage authority (PrepareSourceRun authorizes
// the caller as it does for the HTTP route).
type Provisioning interface {
	// Start admits the run as StartProvisioning specifies, acting with the
	// caller's run:manage authority in the scope.
	Start(ctx context.Context, caller Caller, scope domain.Scope, req StartProvisioning) (ProvisioningRun, error)
	// Get returns the provisioning run of the scope under the caller's current
	// authority. An unknown id, a foreign tenant's or project's run and a run of
	// another origin are all ErrNotFound, indistinguishable.
	Get(ctx context.Context, caller Caller, scope domain.Scope, id domain.RunID) (ProvisioningRun, error)
}

// IdempotencyPrincipal is the principal an idempotent create is keyed by: a
// person by identity, and a workload by its RUN, not its attempt, so a
// replacement attempt of the same run that retries a create converges on the
// original resource instead of colliding with it. Adapters key the use case's
// idempotency record by this value; the tool layer keys Results by it.
func IdempotencyPrincipal(c Caller) string {
	if c.IsWorkload() && c.Actor.RunID != nil {
		return "run:" + *c.Actor.RunID
	}
	return string(c.Actor.Type) + ":" + c.Actor.ID
}

// ReplayKey identifies one idempotent create: the principal, the tenant, the
// project (empty for tenant-level tools), the tool and the client's key. The
// request hash is a value, never part of the lookup key.
type ReplayKey struct {
	Tenant    domain.TenantID
	Principal string // IdempotencyPrincipal
	Project   domain.ProjectID
	Tool      string
	Key       string
}

// Recorded is the stored outcome of an idempotent create. It holds a resource
// identifier only: a replay rereads the resource through the caller's
// authority, so revoked access is never answered from storage, and no
// credential is ever stored.
type Recorded struct {
	RequestHash string
	ResourceID  string
}

// Results records idempotent creates. Implementations are tenant-scoped,
// retain entries for at least the idempotency retention (24 hours, AC-02) and
// make no network call outside their store.
type Results interface {
	// Lookup returns the record of the key.
	Lookup(ctx context.Context, k ReplayKey) (Recorded, bool, error)
	// Record stores r unless the key already has a record and returns the
	// record that holds (put-if-absent), so concurrent creators converge.
	Record(ctx context.Context, k ReplayKey, r Recorded) (Recorded, error)
}

// Authenticator authenticates one MCP request: it verifies the bearer and
// resolves the caller with its current tenant and, for an attempt, its run's
// project. Every request is authenticated; there are no sessions.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Caller, error)
}
