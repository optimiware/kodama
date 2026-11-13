package authorize

import (
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// This file is the declared operation registry: 58 operations in 12 families.
// Schema names follow one convention so REST/MCP parity checks can enumerate
// the registry without a second table:
//
//	<operation>.input.v1
//	<operation>.result.v1
//
// Error-class sets are built from the same layered rule the specs state:
//
//   - every operation refuses invalid_input, unauthenticated, forbidden and
//     internal_failure;
//   - reads additionally surface not_found, dependency_unavailable and
//     internal_failure;
//   - mutations add version_conflict;
//   - receipt-bearing operations add changed_idempotency_content;
//   - admission adds unsupported_capability;
//   - request resolution adds deadline_expired;
//   - verification adds deadline_expired;
//   - model/tool invocation adds deadline_expired and uncertain_external_effect.

func readErrors() []fault.Class {
	return []fault.Class{
		fault.InvalidInput,
		fault.Unauthenticated,
		fault.Forbidden,
		fault.NotFound,
		fault.DependencyUnavailable,
		fault.InternalFailure,
	}
}

func withClasses(base []fault.Class, extra ...fault.Class) []fault.Class {
	out := make([]fault.Class, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

func mutationErrors() []fault.Class {
	return withClasses(readErrors(), fault.VersionConflict)
}

func receiptErrors() []fault.Class {
	return withClasses(mutationErrors(), fault.ChangedIdempotencyContent)
}

func admissionErrors() []fault.Class {
	return withClasses(receiptErrors(), fault.UnsupportedCapability)
}

func resolveErrors() []fault.Class {
	return withClasses(receiptErrors(), fault.DeadlineExpired)
}

func verifyErrors() []fault.Class {
	return withClasses(readErrors(), fault.DeadlineExpired)
}

func invokeErrors() []fault.Class {
	return withClasses(readErrors(), fault.DeadlineExpired, fault.UncertainExternalEffect)
}

// operation builds one registry row and derives its schema names.
func operation(
	name string,
	family Family,
	scope ScopeKind,
	human []tenancy.Role,
	service []tenancy.Role,
	execution ExecutionPermission,
	authority []AuthorityCheck,
	receipt ReceiptPolicy,
	errors []fault.Class,
) Operation {
	return Operation{
		Name:         OperationName(name),
		Family:       family,
		Scope:        scope,
		InputSchema:  name + ".input.v1",
		ResultSchema: name + ".result.v1",
		Requires: Requires{
			HumanRoles:   human,
			ServiceRoles: service,
			Execution:    execution,
		},
		Authority: authority,
		Receipt:   receipt,
		Errors:    errors,
	}
}

// registry is the declaration order. Families are grouped so the table reads
// like the public operation inventory.
var registry = []Operation{
	// Tenant, project and membership administration.
	operation("tenant.create", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckPrincipalEnabled}, ReceiptRequired, receiptErrors()),
	operation("tenant.rotate_key", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPrincipalEnabled}, ReceiptRequired, receiptErrors()),
	operation("project.create", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("project.update", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPrincipalEnabled}, ReceiptRequired, receiptErrors()),
	operation("project.list", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("membership.grant", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("membership.revoke", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("membership.list", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("principal.whoami", FamilyTenancy, ScopeTenant,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecOwnTaskContent,
		nil, ReceiptNone, readErrors()),

	// Resource bindings.
	operation("resource.binding.register", FamilyResource, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckResourceOwnership}, ReceiptRequired, receiptErrors()),
	operation("resource.binding.update", FamilyResource, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckBindingVersion}, ReceiptRequired, receiptErrors()),
	operation("resource.binding.delete", FamilyResource, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckBindingVersion}, ReceiptRequired, receiptErrors()),
	operation("resource.binding.read", FamilyResource, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("resource.binding.list", FamilyResource, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),

	// API-posted pipelines and immutable execution start.
	operation("pipeline.apply", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPipelineVersion, CheckPushServiceAdmin}, ReceiptRequired, receiptErrors()),
	operation("pipeline.read", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("pipeline.list", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("pipeline.update", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPipelineVersion, CheckPushServiceAdmin}, ReceiptRequired, receiptErrors()),
	operation("pipeline.delete", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPipelineVersion, CheckPushServiceAdmin}, ReceiptRequired, receiptErrors()),
	operation("pipeline.start", FamilyPipeline, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPipelineVersion}, ReceiptRequired, admissionErrors()),

	// Secret metadata and writes.
	operation("secret.metadata.read", FamilySecret, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("secret.metadata.list", FamilySecret, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("secret.write", FamilySecret, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("secret.delete", FamilySecret, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),

	// Service accounts and credential lifecycle.
	operation("service.create", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("service.list", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("service.update", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),
	operation("service.enable", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),
	operation("service.disable", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),
	operation("service.credential.issue", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),
	operation("service.credential.rotate", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),
	operation("service.credential.revoke", FamilyService, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin}, nil, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckServiceEnabled}, ReceiptRequired, receiptErrors()),

	// Tasks and conversation handoffs.
	operation("task.create", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecProjectTaskCreate,
		[]AuthorityCheck{CheckMembership}, ReceiptRequired, receiptErrors()),
	operation("task.read", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("task.list", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("task.edit", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion}, ReceiptRequired, receiptErrors()),
	operation("task.comment", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion}, ReceiptRequired, receiptErrors()),
	operation("task.select_pipeline", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion, CheckPipelineVersion}, ReceiptRequired, receiptErrors()),
	// task.start grants engineers an ordinary start. The operator role is
	// granted only as a failed-task retry: the conditional
	// failed_task_operator_start check holds only for an open task whose
	// current execution is failed, re-read atomically with the start commit.
	// The operator gets no pipeline selection beyond the task's existing
	// pipeline, and engineers are unchanged.
	operation("task.start", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion, CheckPipelineVersion, CheckFailedTaskOperatorStart}, ReceiptRequired, admissionErrors()),
	operation("task.create_and_start", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPipelineVersion}, ReceiptRequired, admissionErrors()),
	operation("task.ask", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecOwnTaskAsk,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion, CheckExecutionVersion}, ReceiptRequired, receiptErrors()),
	// A reply is answered by the exact outstanding assignee, so the grant stage
	// admits every human membership role and every enabled service. The
	// in-commit assignment check is the authority: a human must currently hold
	// the assigned role and a service must match the exact assigned service ID.
	operation("task.reply", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion, CheckPipelineVersion, CheckExecutionVersion}, ReceiptRequired, admissionErrors()),
	operation("task.cancel", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckTaskVersion, CheckExecutionVersion}, ReceiptRequired, receiptErrors()),
	operation("task.history", FamilyTask, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),

	// Inbox is a scoped query, not a mutable request entity.
	operation("task.inbox", FamilyInbox, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleReviewer, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPrincipalEnabled}, ReceiptNone, readErrors()),

	// Executions, outputs, request resolution and retained logs.
	operation("execution.inspect", FamilyExecution, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("execution.list", FamilyExecution, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("execution.cancel", FamilyExecution, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckExecutionVersion}, ReceiptRequired, receiptErrors()),
	operation("execution.action.outputs", FamilyExecution, ScopeProject,
		nil, nil, ExecOwnOutputs,
		[]AuthorityCheck{CheckExecutionRunning, CheckActionDeadline, CheckExecutionVersion}, ReceiptRequired, receiptErrors()),
	// Resolution is answered by the exact outstanding assignee, so the grant
	// stage admits every human membership role and every enabled service. The
	// in-commit assignment check is the authority: a human must currently hold
	// the assigned role and a service must match the exact assigned service ID.
	operation("execution.request.resolve", FamilyExecution, ScopeProject,
		[]tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckExecutionVersion, CheckActionDeadline}, ReceiptRequestScoped, resolveErrors()),
	operation("execution.logs.read", FamilyExecution, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecOwnTaskContent,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),

	// Verification and verified resource reports.
	operation("actor.verify", FamilyVerify, ScopeProject,
		nil,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckServiceEnabled, CheckBindingVersion, CheckPrincipalEnabled, CheckExecutionRunning, CheckActionDeadline},
		ReceiptNone, verifyErrors()),
	operation("resource.report", FamilyVerify, ScopeProject,
		nil,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckServiceEnabled, CheckBindingVersion, CheckResourceOwnership},
		ReceiptRequired, receiptErrors()),

	// Custom events and authorized event reads/subscriptions.
	operation("event.publish", FamilyEvent, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecOwnEvents,
		[]AuthorityCheck{CheckMembership, CheckExecutionRunning}, ReceiptRequired, receiptErrors()),
	operation("event.read", FamilyEvent, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership}, ReceiptNone, readErrors()),
	operation("event.subscribe", FamilyEvent, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer},
		[]tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator, tenancy.RoleViewer}, ExecNone,
		[]AuthorityCheck{CheckMembership, CheckPrincipalEnabled}, ReceiptNone, readErrors()),

	// Optional model/tool surfaces use the same authorizer.
	operation("model.invoke", FamilyModel, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecConfiguredModels,
		[]AuthorityCheck{CheckMembership, CheckExecutionRunning, CheckActionDeadline}, ReceiptNone, invokeErrors()),
	operation("tool.invoke", FamilyTool, ScopeProject,
		[]tenancy.Role{tenancy.RoleEngineer}, []tenancy.Role{tenancy.RoleEngineer}, ExecConfiguredTools,
		[]AuthorityCheck{CheckMembership, CheckExecutionRunning, CheckActionDeadline}, ReceiptNone, invokeErrors()),
}

var registryByName = func() map[OperationName]Operation {
	byName := make(map[OperationName]Operation, len(registry))
	for _, op := range registry {
		byName[op.Name] = op
	}
	return byName
}()

// Operations returns every registered operation in declaration order. Each
// returned value owns copies of its grant/check/error slices so a consumer
// cannot mutate the declared registry.
func Operations() []Operation {
	out := make([]Operation, len(registry))
	for index, op := range registry {
		out[index] = cloneOperation(op)
	}
	return out
}

// OperationNames returns every registered name in declaration order.
func OperationNames() []OperationName {
	out := make([]OperationName, len(registry))
	for index, op := range registry {
		out[index] = op.Name
	}
	return out
}

// LookupOperation returns the registered operation for name. Unknown and empty
// names are refused rather than defaulted.
func LookupOperation(name OperationName) (Operation, bool) {
	op, ok := registryByName[name]
	if !ok {
		return Operation{}, false
	}
	return cloneOperation(op), true
}

// Valid reports whether the name is registered.
func (name OperationName) Valid() bool {
	_, ok := registryByName[name]
	return ok
}

func cloneOperation(op Operation) Operation {
	op.Requires.HumanRoles = append([]tenancy.Role(nil), op.Requires.HumanRoles...)
	op.Requires.ServiceRoles = append([]tenancy.Role(nil), op.Requires.ServiceRoles...)
	op.Authority = append([]AuthorityCheck(nil), op.Authority...)
	op.Errors = append([]fault.Class(nil), op.Errors...)
	return op
}
