// Package authorize declares the one operation registry and the authorizer
// ports every Shell capability and public edge shares.
//
// The registry is the complete known-operation matrix: family, ownership scope,
// input/result schema names, human and service role grants, execution
// permission, current-authority checks, receipt policy and stable error
// classes. Unknown operations are refused. A grant is not durable permission:
// the checks listed for an operation are re-read at the atomic commit
// boundary, so a previously calculated allow never substitutes for current
// membership, enablement, execution state, deadline or version.
//
// Admin and Reviewer stay distinct. A service never receives Admin, and an
// execution never receives request resolution, assigned replies, or
// administration. This package imports only neutral values and identity; it
// never imports a capability implementation.
package authorize

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Family groups operations for routing and documentation. Every family has at
// least one registered operation.
type Family string

const (
	// FamilyTenancy covers tenants, projects and memberships.
	FamilyTenancy Family = "tenancy"
	// FamilyResource covers resource bindings.
	FamilyResource Family = "resource"
	// FamilyPipeline covers API-posted pipelines and immutable start.
	FamilyPipeline Family = "pipeline"
	// FamilySecret covers stored secret metadata and writes.
	FamilySecret Family = "secret"
	// FamilyService covers service accounts and credential lifecycle.
	FamilyService Family = "service"
	// FamilyTask covers tasks, conversation handoffs and history.
	FamilyTask Family = "task"
	// FamilyInbox covers the scoped role inbox query.
	FamilyInbox Family = "inbox"
	// FamilyExecution covers executions, outputs, request resolution and logs.
	FamilyExecution Family = "execution"
	// FamilyVerify covers actor verification and verified resource reports.
	FamilyVerify Family = "verify"
	// FamilyEvent covers custom publication and authorized event reads.
	FamilyEvent Family = "event"
	// FamilyModel covers the optional configured model surface.
	FamilyModel Family = "model"
	// FamilyTool covers the optional configured tool surface.
	FamilyTool Family = "tool"
)

var families = [...]Family{
	FamilyTenancy,
	FamilyResource,
	FamilyPipeline,
	FamilySecret,
	FamilyService,
	FamilyTask,
	FamilyInbox,
	FamilyExecution,
	FamilyVerify,
	FamilyEvent,
	FamilyModel,
	FamilyTool,
}

// Families returns every operation family in declaration order.
func Families() []Family {
	return append([]Family(nil), families[:]...)
}

// Valid reports whether the family is one of the declared families.
func (family Family) Valid() bool {
	for _, candidate := range families {
		if family == candidate {
			return true
		}
	}
	return false
}

// ScopeKind is the ownership scope an operation is authorized against.
type ScopeKind string

const (
	// ScopeTenant is a tenant-scoped administration operation.
	ScopeTenant ScopeKind = "tenant"
	// ScopeProject is a project-scoped operation.
	ScopeProject ScopeKind = "project"
)

var scopeKinds = [...]ScopeKind{ScopeTenant, ScopeProject}

// Valid reports whether the scope kind is one of the declared kinds.
func (kind ScopeKind) Valid() bool {
	for _, candidate := range scopeKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// ReceiptPolicy is the idempotency-receipt requirement of one operation.
type ReceiptPolicy string

const (
	// ReceiptNone is a read with no durable receipt.
	ReceiptNone ReceiptPolicy = "none"
	// ReceiptRequired is a mutation keyed by the scoped caller key.
	ReceiptRequired ReceiptPolicy = "required"
	// ReceiptRequestScoped is a request settlement whose retry identity is the
	// request action itself.
	ReceiptRequestScoped ReceiptPolicy = "request_scoped"
	// ReceiptPushScoped is an internal push selection keyed by
	// (pipeline id, event id) rather than a caller key.
	ReceiptPushScoped ReceiptPolicy = "push_scoped"
)

var receiptPolicies = [...]ReceiptPolicy{
	ReceiptNone,
	ReceiptRequired,
	ReceiptRequestScoped,
	ReceiptPushScoped,
}

// Valid reports whether the policy is one of the declared policies.
func (policy ReceiptPolicy) Valid() bool {
	for _, candidate := range receiptPolicies {
		if policy == candidate {
			return true
		}
	}
	return false
}

// AuthorityCheck names one current-authority fact the authorizer must read
// again at the atomic commit boundary.
type AuthorityCheck string

const (
	// CheckMembership reads the caller's current membership roles in scope.
	CheckMembership AuthorityCheck = "membership"
	// CheckPrincipalEnabled reads the caller's current enablement.
	CheckPrincipalEnabled AuthorityCheck = "principal_enabled"
	// CheckServiceEnabled reads the acting service's current enablement.
	CheckServiceEnabled AuthorityCheck = "service_enabled"
	// CheckExecutionRunning reads the execution's current state.
	CheckExecutionRunning AuthorityCheck = "execution_running"
	// CheckActionDeadline reads the active action's authoritative deadline.
	CheckActionDeadline AuthorityCheck = "action_deadline"
	// CheckBindingVersion reads the expected binding version.
	CheckBindingVersion AuthorityCheck = "binding_version"
	// CheckTaskVersion reads the expected task lifecycle version.
	CheckTaskVersion AuthorityCheck = "task_version"
	// CheckExecutionVersion reads the expected execution version.
	CheckExecutionVersion AuthorityCheck = "execution_version"
	// CheckPipelineVersion reads the expected pipeline version.
	CheckPipelineVersion AuthorityCheck = "pipeline_version"
	// CheckPushServiceAdmin reads whether the caller currently holds Admin in
	// scope. It is conditional: a pipeline mutation that binds, changes or
	// removes the automatic on.push service must satisfy it, while a mutation
	// that leaves the automatic principal untouched does not evaluate it.
	CheckPushServiceAdmin AuthorityCheck = "push_service_admin"
	// CheckFailedTaskOperatorStart reads the task's current lifecycle state and
	// its current execution state for an operator-only task.start. It is
	// conditional: it is evaluated only when the caller's task.start grant
	// comes from the operator role and not from the engineer grant. The
	// operator grant holds only while the task is open and its current
	// execution is failed; waiting, done, cancelled, a task with no execution
	// and a non-failed current execution all refuse forbidden. Engineers are
	// unaffected. The read is part of the start commit, so the operator
	// decision and the queued retry commit atomically.
	CheckFailedTaskOperatorStart AuthorityCheck = "failed_task_operator_start"
	// CheckResourceOwnership reads the resource's relational ownership scope.
	CheckResourceOwnership AuthorityCheck = "resource_ownership"
)

var authorityChecks = [...]AuthorityCheck{
	CheckMembership,
	CheckPrincipalEnabled,
	CheckServiceEnabled,
	CheckExecutionRunning,
	CheckActionDeadline,
	CheckBindingVersion,
	CheckTaskVersion,
	CheckExecutionVersion,
	CheckPipelineVersion,
	CheckPushServiceAdmin,
	CheckFailedTaskOperatorStart,
	CheckResourceOwnership,
}

// AuthorityChecks returns every authority check in declaration order.
func AuthorityChecks() []AuthorityCheck {
	return append([]AuthorityCheck(nil), authorityChecks[:]...)
}

// Valid reports whether the check is one of the declared checks.
func (check AuthorityCheck) Valid() bool {
	for _, candidate := range authorityChecks {
		if check == candidate {
			return true
		}
	}
	return false
}

// ExecutionPermission is the admitted authority of one execution principal.
// It is a closed vocabulary: there is no execution Admin bit and no execution
// permission for request resolution or administration.
type ExecutionPermission string

const (
	// ExecNone grants no execution authority.
	ExecNone ExecutionPermission = "none"
	// ExecOwnTaskContent grants content/metadata access to its own task.
	ExecOwnTaskContent ExecutionPermission = "own_task_content"
	// ExecProjectTaskCreate grants project task creation.
	ExecProjectTaskCreate ExecutionPermission = "project_task_create"
	// ExecOwnTaskAsk grants asking a question on its own task.
	ExecOwnTaskAsk ExecutionPermission = "own_task_ask"
	// ExecOwnOutputs grants submitting its own action outputs.
	ExecOwnOutputs ExecutionPermission = "own_outputs"
	// ExecOwnEvents grants publishing custom events for its own execution.
	ExecOwnEvents ExecutionPermission = "own_events"
	// ExecConfiguredModels grants invoking configured models.
	ExecConfiguredModels ExecutionPermission = "configured_models"
	// ExecConfiguredTools grants invoking configured tools.
	ExecConfiguredTools ExecutionPermission = "configured_tools"
)

var executionPermissions = [...]ExecutionPermission{
	ExecNone,
	ExecOwnTaskContent,
	ExecProjectTaskCreate,
	ExecOwnTaskAsk,
	ExecOwnOutputs,
	ExecOwnEvents,
	ExecConfiguredModels,
	ExecConfiguredTools,
}

// ExecutionPermissions returns every execution permission in declaration
// order.
func ExecutionPermissions() []ExecutionPermission {
	return append([]ExecutionPermission(nil), executionPermissions[:]...)
}

// Valid reports whether the permission is one of the declared permissions.
func (permission ExecutionPermission) Valid() bool {
	for _, candidate := range executionPermissions {
		if permission == candidate {
			return true
		}
	}
	return false
}

// OperationName is one registered operation name. Valid only for names in the
// registry; LookupOperation refuses everything else.
type OperationName string

// Requires is the principal grant set of one operation.
type Requires struct {
	HumanRoles   []tenancy.Role      `json:"human_roles"`
	ServiceRoles []tenancy.Role      `json:"service_roles"`
	Execution    ExecutionPermission `json:"execution"`
}

// Operation is one declared registry row.
type Operation struct {
	Name         OperationName    `json:"name"`
	Family       Family           `json:"family"`
	Scope        ScopeKind        `json:"scope"`
	InputSchema  string           `json:"input_schema"`
	ResultSchema string           `json:"result_schema"`
	Requires     Requires         `json:"requires"`
	Authority    []AuthorityCheck `json:"authority"`
	Receipt      ReceiptPolicy    `json:"receipt"`
	Errors       []fault.Class    `json:"errors"`
}

// Decision is the authorizer's answer for one principal/operation/scope.
type Decision string

const (
	// DecisionAllow means the principal currently holds the grant.
	DecisionAllow Decision = "allow"
	// DecisionDeny means the principal does not hold the grant.
	DecisionDeny Decision = "deny"
)

// Valid reports whether the decision is one of the declared decisions.
func (decision Decision) Valid() bool {
	return decision == DecisionAllow || decision == DecisionDeny
}

// Principal is the verified principal presented to the authorizer. It is the
// identity package value; the alias keeps one identity type in the contract
// while allowing the authorizer port to live with its consumer.
type Principal = identity.Principal

// AdmittedExecution is the verified execution principal together with the
// execution authority admitted with it: the current execution/action identity,
// the closed ExecutionPermission and the authoritative action deadline.
//
// It is a snapshot for one authorization or commit attempt, never durable
// permission. A caller must re-read the current execution/action state and
// deadline at the commit boundary.
type AdmittedExecution struct {
	Principal   Principal           `json:"principal"`
	ExecutionID id.ExecutionID      `json:"execution_id"`
	ActionID    id.ActionID         `json:"action_id,omitempty"`
	Permission  ExecutionPermission `json:"permission"`
	Deadline    time.Time           `json:"deadline,omitempty"`
}

// Validate checks that the context names an execution principal with its
// execution identity, a declared permission and a structurally valid principal.
func (admitted AdmittedExecution) Validate() error {
	if admitted.Principal.Kind != identity.KindExecution {
		return fault.New(fault.InvalidInput, "admitted execution principal is not an execution")
	}
	if err := admitted.Principal.Validate(); err != nil {
		return err
	}
	if admitted.ExecutionID == "" {
		return fault.New(fault.InvalidInput, "admitted execution context has no execution id")
	}
	if admitted.Principal.ExecutionID != admitted.ExecutionID {
		return fault.New(fault.InvalidInput, "admitted execution principal and context execution ids differ")
	}
	if !admitted.Permission.Valid() {
		return fault.New(fault.InvalidInput, "admitted execution permission is not frozen")
	}
	return nil
}

// AuthorityRead asks the authority reader for one current fact at the commit
// boundary. ExpectedVersion is zero when the check has no versioned target.
//
// Operation and Required carry the authority-read context: the registry
// operation being committed and the grant set it requires. An adapter can
// therefore re-evaluate the exact operation grant instead of guessing from a
// bare check name. They are context, never durable permission.
//
// Targets carries the membership rows a project-scoped membership read
// observed: the exact-project row and the tenant-wide row, each present at an
// observed row identity AND version or explicitly absent (see
// MembershipTargets). A consumer pins every target in the same commit unit, so
// a change to either row, including deleting and re-creating it with a new
// identity at the same version, is a version_conflict. Matching pins are still
// not durable permission: the commit unit must re-read both rows and
// re-evaluate the effective role union against the required role. Other checks
// leave Targets empty.
type AuthorityRead struct {
	Operation       OperationName     `json:"operation,omitempty"`
	Required        Requires          `json:"required,omitempty"`
	Check           AuthorityCheck    `json:"check"`
	Scope           tenancy.Scope     `json:"scope"`
	PrincipalID     id.PrincipalID    `json:"principal_id"`
	TargetID        string            `json:"target_id,omitempty"`
	ExpectedVersion uint64            `json:"expected_version,omitempty"`
	Targets         []AuthorityTarget `json:"targets,omitempty"`
}

// AuthorityFact is one current fact returned for an AuthorityRead. Allowed
// reports whether the fact held when it was read; Version is the observed
// version for versioned targets. Callers must not treat a fact as durable
// permission beyond the commit that requested it.
type AuthorityFact struct {
	Check       AuthorityCheck `json:"check"`
	Scope       tenancy.Scope  `json:"scope"`
	PrincipalID id.PrincipalID `json:"principal_id"`
	TargetID    string         `json:"target_id,omitempty"`
	Version     uint64         `json:"version,omitempty"`
	Allowed     bool           `json:"allowed"`
	Detail      string         `json:"detail,omitempty"`
}

type unassignedPrincipalContextKey struct{}

// WithUnassignedPrincipal marks a principal-resolution context so an
// authenticated human whose OIDC token verified cleanly is resolved even when
// they hold no committed membership rows yet. Only principal.whoami sets this
// marker; every other operation requires a committed principal in scope.
func WithUnassignedPrincipal(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, unassignedPrincipalContextKey{}, true)
}

// AllowsUnassignedPrincipal reports whether ctx carries the
// WithUnassignedPrincipal marker.
func AllowsUnassignedPrincipal(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	allowed, _ := ctx.Value(unassignedPrincipalContextKey{}).(bool)
	return allowed
}
