package domaincontract

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// The operation matrix is the S08 operation inventory. Every family the
// router may expose has an explicit schema, scope, grant set, current-authority
// reads, receipt policy and stable error classes. Unknown operations refuse.
//
// Schema names follow one convention so REST/MCP parity checks can enumerate
// the registry without a second table: <operation>.input.v1 and
// <operation>.result.v1.

type expectedOperation struct {
	name      authorize.OperationName
	family    authorize.Family
	scope     authorize.ScopeKind
	human     []tenancy.Role
	service   []tenancy.Role
	execution authorize.ExecutionPermission
	authority []authorize.AuthorityCheck
	receipt   authorize.ReceiptPolicy
	errors    []fault.Class
}

func roles(rs ...tenancy.Role) []tenancy.Role { return rs }

func checks(cs ...authorize.AuthorityCheck) []authorize.AuthorityCheck { return cs }

func classes(cs ...fault.Class) []fault.Class { return cs }

func readErrors() []fault.Class {
	return classes(
		fault.InvalidInput,
		fault.Unauthenticated,
		fault.Forbidden,
		fault.NotFound,
		fault.DependencyUnavailable,
		fault.InternalFailure,
	)
}

func mutationErrors() []fault.Class {
	return append(readErrors(), fault.VersionConflict)
}

func receiptErrors() []fault.Class {
	return append(mutationErrors(), fault.ChangedIdempotencyContent)
}

func admissionErrors() []fault.Class {
	return append(receiptErrors(), fault.UnsupportedCapability)
}

func resolveErrors() []fault.Class {
	return append(receiptErrors(), fault.DeadlineExpired)
}

func verifyErrors() []fault.Class {
	return append(readErrors(), fault.DeadlineExpired)
}

func invokeErrors() []fault.Class {
	return append(readErrors(), fault.DeadlineExpired, fault.UncertainExternalEffect)
}

func eop(
	name string,
	family authorize.Family,
	scope authorize.ScopeKind,
	human []tenancy.Role,
	service []tenancy.Role,
	execution authorize.ExecutionPermission,
	authority []authorize.AuthorityCheck,
	receipt authorize.ReceiptPolicy,
	errs []fault.Class,
) expectedOperation {
	return expectedOperation{
		name:      authorize.OperationName(name),
		family:    family,
		scope:     scope,
		human:     human,
		service:   service,
		execution: execution,
		authority: authority,
		receipt:   receipt,
		errors:    errs,
	}
}

func expectedOperations() []expectedOperation {
	engineer := tenancy.RoleEngineer
	reviewer := tenancy.RoleReviewer
	operator := tenancy.RoleOperator
	viewer := tenancy.RoleViewer
	admin := tenancy.RoleAdmin

	return []expectedOperation{
		// Tenant/project/membership administration.
		eop("tenant.create", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckPrincipalEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("tenant.rotate_key", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPrincipalEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("project.create", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("project.update", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPrincipalEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("membership.grant", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("membership.revoke", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("membership.list", authorize.FamilyTenancy, authorize.ScopeTenant,
			roles(admin, viewer), roles(viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),

		// Resource bindings.
		eop("resource.binding.register", authorize.FamilyResource, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckResourceOwnership), authorize.ReceiptRequired, receiptErrors()),
		eop("resource.binding.update", authorize.FamilyResource, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckBindingVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("resource.binding.delete", authorize.FamilyResource, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckBindingVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("resource.binding.read", authorize.FamilyResource, authorize.ScopeProject,
			roles(admin, viewer), roles(viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),

		// API-posted pipelines and immutable execution start.
		eop("pipeline.apply", authorize.FamilyPipeline, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPipelineVersion, authorize.CheckPushServiceAdmin), authorize.ReceiptRequired, receiptErrors()),
		eop("pipeline.read", authorize.FamilyPipeline, authorize.ScopeProject,
			roles(engineer, viewer), roles(engineer, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("pipeline.update", authorize.FamilyPipeline, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPipelineVersion, authorize.CheckPushServiceAdmin), authorize.ReceiptRequired, receiptErrors()),
		eop("pipeline.delete", authorize.FamilyPipeline, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPipelineVersion, authorize.CheckPushServiceAdmin), authorize.ReceiptRequired, receiptErrors()),
		eop("pipeline.start", authorize.FamilyPipeline, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPipelineVersion), authorize.ReceiptRequired, admissionErrors()),

		// Secret metadata and writes.
		eop("secret.metadata.read", authorize.FamilySecret, authorize.ScopeProject,
			roles(admin, engineer, viewer), roles(engineer, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("secret.write", authorize.FamilySecret, authorize.ScopeProject,
			roles(admin, engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("secret.delete", authorize.FamilySecret, authorize.ScopeProject,
			roles(admin, engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),

		// Service accounts and credential lifecycle.
		eop("service.create", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("service.update", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("service.enable", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("service.disable", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("service.credential.issue", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("service.credential.rotate", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),
		eop("service.credential.revoke", authorize.FamilyService, authorize.ScopeProject,
			roles(admin), roles(), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckServiceEnabled), authorize.ReceiptRequired, receiptErrors()),

		// Tasks and conversation handoffs.
		eop("task.create", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecProjectTaskCreate,
			checks(authorize.CheckMembership), authorize.ReceiptRequired, receiptErrors()),
		eop("task.read", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer, viewer), roles(engineer, viewer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("task.edit", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("task.comment", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("task.select_pipeline", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion, authorize.CheckPipelineVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("task.start", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer, operator), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion, authorize.CheckPipelineVersion, authorize.CheckFailedTaskOperatorStart), authorize.ReceiptRequired, admissionErrors()),
		eop("task.create_and_start", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPipelineVersion), authorize.ReceiptRequired, admissionErrors()),
		eop("task.ask", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecOwnTaskAsk,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion, authorize.CheckExecutionVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("task.reply", authorize.FamilyTask, authorize.ScopeProject,
			roles(admin, engineer, reviewer, operator, viewer),
			roles(engineer, reviewer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion, authorize.CheckPipelineVersion, authorize.CheckExecutionVersion), authorize.ReceiptRequired, admissionErrors()),
		eop("task.cancel", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer, operator), roles(engineer, operator), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckTaskVersion, authorize.CheckExecutionVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("task.history", authorize.FamilyTask, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),

		// Inbox is a scoped query, not a mutable request entity.
		eop("task.inbox", authorize.FamilyInbox, authorize.ScopeProject,
			roles(engineer, operator, reviewer, viewer), roles(engineer, operator, reviewer, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPrincipalEnabled), authorize.ReceiptNone, readErrors()),

		// Executions, outputs, request resolution and retained logs.
		eop("execution.inspect", authorize.FamilyExecution, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("execution.list", authorize.FamilyExecution, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("execution.cancel", authorize.FamilyExecution, authorize.ScopeProject,
			roles(engineer, operator), roles(engineer, operator), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckExecutionVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("execution.action.outputs", authorize.FamilyExecution, authorize.ScopeProject,
			roles(), roles(), authorize.ExecOwnOutputs,
			checks(authorize.CheckExecutionRunning, authorize.CheckActionDeadline, authorize.CheckExecutionVersion), authorize.ReceiptRequired, receiptErrors()),
		eop("execution.request.resolve", authorize.FamilyExecution, authorize.ScopeProject,
			roles(admin, engineer, reviewer, operator, viewer),
			roles(engineer, reviewer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckExecutionVersion, authorize.CheckActionDeadline), authorize.ReceiptRequestScoped, resolveErrors()),
		eop("execution.logs.read", authorize.FamilyExecution, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecOwnTaskContent,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),

		// Verification and verified resource reports.
		eop("actor.verify", authorize.FamilyVerify, authorize.ScopeProject,
			roles(), roles(engineer, reviewer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckServiceEnabled, authorize.CheckBindingVersion, authorize.CheckPrincipalEnabled, authorize.CheckExecutionRunning, authorize.CheckActionDeadline), authorize.ReceiptNone, verifyErrors()),
		eop("resource.report", authorize.FamilyVerify, authorize.ScopeProject,
			roles(), roles(engineer, reviewer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckServiceEnabled, authorize.CheckBindingVersion, authorize.CheckResourceOwnership), authorize.ReceiptRequired, receiptErrors()),

		// Custom events and authorized event reads/subscriptions.
		eop("event.publish", authorize.FamilyEvent, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecOwnEvents,
			checks(authorize.CheckMembership, authorize.CheckExecutionRunning), authorize.ReceiptRequired, receiptErrors()),
		eop("event.read", authorize.FamilyEvent, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership), authorize.ReceiptNone, readErrors()),
		eop("event.subscribe", authorize.FamilyEvent, authorize.ScopeProject,
			roles(engineer, operator, viewer), roles(engineer, operator, viewer), authorize.ExecNone,
			checks(authorize.CheckMembership, authorize.CheckPrincipalEnabled), authorize.ReceiptNone, readErrors()),

		// Optional model/tool surfaces use the same authorizer.
		eop("model.invoke", authorize.FamilyModel, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecConfiguredModels,
			checks(authorize.CheckMembership, authorize.CheckExecutionRunning, authorize.CheckActionDeadline), authorize.ReceiptNone, invokeErrors()),
		eop("tool.invoke", authorize.FamilyTool, authorize.ScopeProject,
			roles(engineer), roles(engineer), authorize.ExecConfiguredTools,
			checks(authorize.CheckMembership, authorize.CheckExecutionRunning, authorize.CheckActionDeadline), authorize.ReceiptNone, invokeErrors()),
	}
}

func TestOperationRegistryMatrix(t *testing.T) {
	expected := expectedOperations()
	wantByName := make(map[authorize.OperationName]expectedOperation, len(expected))
	for _, op := range expected {
		if _, duplicate := wantByName[op.name]; duplicate {
			t.Fatalf("test matrix duplicates %q", op.name)
		}
		wantByName[op.name] = op
	}

	got := authorize.Operations()
	if len(got) != len(expected) {
		t.Fatalf("registry has %d operations; want %d", len(got), len(expected))
	}
	seen := make(map[authorize.OperationName]bool, len(got))
	for _, op := range got {
		if seen[op.Name] {
			t.Errorf("registry duplicates %q", op.Name)
			continue
		}
		seen[op.Name] = true
		want, ok := wantByName[op.Name]
		if !ok {
			t.Errorf("registry contains unknown operation %q", op.Name)
			continue
		}
		if op.Family != want.family {
			t.Errorf("%s family = %q; want %q", op.Name, op.Family, want.family)
		}
		if op.Scope != want.scope {
			t.Errorf("%s scope = %q; want %q", op.Name, op.Scope, want.scope)
		}
		if wantInput := string(op.Name) + ".input.v1"; op.InputSchema != wantInput {
			t.Errorf("%s input schema = %q; want %q", op.Name, op.InputSchema, wantInput)
		}
		if wantResult := string(op.Name) + ".result.v1"; op.ResultSchema != wantResult {
			t.Errorf("%s result schema = %q; want %q", op.Name, op.ResultSchema, wantResult)
		}
		if !sameRoles(op.Requires.HumanRoles, want.human) {
			t.Errorf("%s human roles = %v; want %v", op.Name, op.Requires.HumanRoles, want.human)
		}
		if !sameRoles(op.Requires.ServiceRoles, want.service) {
			t.Errorf("%s service roles = %v; want %v", op.Name, op.Requires.ServiceRoles, want.service)
		}
		if op.Requires.Execution != want.execution {
			t.Errorf("%s execution permission = %q; want %q", op.Name, op.Requires.Execution, want.execution)
		}
		if !sameChecks(op.Authority, want.authority) {
			t.Errorf("%s authority = %v; want %v", op.Name, op.Authority, want.authority)
		}
		if op.Receipt != want.receipt {
			t.Errorf("%s receipt = %q; want %q", op.Name, op.Receipt, want.receipt)
		}
		if !sameClasses(op.Errors, want.errors) {
			t.Errorf("%s errors = %v; want %v", op.Name, op.Errors, want.errors)
		}
	}
	for _, want := range expected {
		if !seen[want.name] {
			t.Errorf("registry is missing operation %q", want.name)
		}
	}

	names := authorize.OperationNames()
	if len(names) != len(expected) {
		t.Fatalf("OperationNames has %d names; want %d", len(names), len(expected))
	}
	sortedNames := append([]authorize.OperationName(nil), names...)
	sort.Slice(sortedNames, func(i, j int) bool { return sortedNames[i] < sortedNames[j] })
	for index, name := range sortedNames {
		if index > 0 && sortedNames[index-1] == name {
			t.Fatalf("OperationNames duplicates %q", name)
		}
		if _, ok := wantByName[name]; !ok {
			t.Fatalf("OperationNames contains unknown %q", name)
		}
	}
}

func TestOperationRegistryInvariants(t *testing.T) {
	families := make(map[authorize.Family]bool)
	usedChecks := make(map[authorize.AuthorityCheck]bool)
	for _, op := range authorize.Operations() {
		families[op.Family] = true

		if op.Name == "" {
			t.Error("operation has an empty name")
		}
		if !op.Family.Valid() {
			t.Errorf("%s has invalid family %q", op.Name, op.Family)
		}
		if !op.Scope.Valid() {
			t.Errorf("%s has invalid scope %q", op.Name, op.Scope)
		}
		if op.InputSchema == "" || op.ResultSchema == "" {
			t.Errorf("%s has an empty schema name", op.Name)
		}
		if !op.Receipt.Valid() {
			t.Errorf("%s has invalid receipt policy %q", op.Name, op.Receipt)
		}
		for _, check := range op.Authority {
			usedChecks[check] = true
			if !check.Valid() {
				t.Errorf("%s has invalid authority check %q", op.Name, check)
			}
		}
		for _, role := range op.Requires.HumanRoles {
			if !role.Valid() {
				t.Errorf("%s has invalid human role %q", op.Name, role)
			}
		}
		for _, role := range op.Requires.ServiceRoles {
			if !role.Valid() || role == tenancy.RoleAdmin {
				t.Errorf("%s has invalid service role %q", op.Name, role)
			}
		}
		if !op.Requires.Execution.Valid() {
			t.Errorf("%s has invalid execution permission %q", op.Name, op.Requires.Execution)
		}

		// Every operation refuses the shared base classes. Mutations add
		// version conflicts; receipt-bearing operations add changed content.
		required := []fault.Class{
			fault.InvalidInput,
			fault.Unauthenticated,
			fault.Forbidden,
			fault.InternalFailure,
		}
		for _, class := range required {
			if !containsClass(op.Errors, class) {
				t.Errorf("%s errors %v omit %q", op.Name, op.Errors, class)
			}
		}
		for _, class := range op.Errors {
			if !class.Valid() {
				t.Errorf("%s has invalid error class %q", op.Name, class)
			}
		}
		if op.Receipt != authorize.ReceiptNone {
			if !containsClass(op.Errors, fault.VersionConflict) {
				t.Errorf("%s is a mutation but omits version_conflict", op.Name)
			}
		}
		if op.Receipt == authorize.ReceiptRequired ||
			op.Receipt == authorize.ReceiptRequestScoped ||
			op.Receipt == authorize.ReceiptPushScoped {
			if !containsClass(op.Errors, fault.ChangedIdempotencyContent) {
				t.Errorf("%s requires a receipt but omits changed_idempotency_content", op.Name)
			}
		}

		// A grant must exist: human role, service role, or an explicit
		// execution permission. An operation with none of them is unreachable.
		if len(op.Requires.HumanRoles) == 0 && len(op.Requires.ServiceRoles) == 0 && op.Requires.Execution == authorize.ExecNone {
			t.Errorf("%s has no principal grant", op.Name)
		}
	}

	for _, family := range authorize.Families() {
		if !families[family] {
			t.Errorf("family %q has no registered operation", family)
		}
	}

	// The authority-check vocabulary is complete: every declared check is used by
	// at least one operation, so a new conditional check cannot be declared
	// without a registry consumer.
	for _, check := range authorize.AuthorityChecks() {
		if !usedChecks[check] {
			t.Errorf("authority check %q is not used by any operation", check)
		}
	}
}

// TestPushServiceAdminConditionalCheck pins S03 §4: binding or changing the
// automatic on.push service requires Admin authority in addition to pipeline
// management. The grant stage still admits an engineer; the commit-time
// conditional check refuses an engineer that does not also hold Admin.
func TestPushServiceAdminConditionalCheck(t *testing.T) {
	ops := []authorize.OperationName{"pipeline.apply", "pipeline.update", "pipeline.delete"}
	for _, name := range ops {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if !hasCheck(op.Authority, authorize.CheckPushServiceAdmin) {
			t.Errorf("%s does not require the conditional push_service_admin check", name)
		}
	}

	ctx := context.Background()
	scope := sampleScope()
	matrix := authorize.NewMatrix()
	now := time.Unix(1_700_000_000, 0).UTC()
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"push-ci"}`)}
	pushYAML := []byte(`repository: repo-1
service: git-service
on:
  push:
    - refs/heads/main
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`)
	noPushYAML := []byte(`inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
`)

	actorWithRoles := func(userRoles ...tenancy.Role) identity.Principal {
		return identity.Principal{
			ID:      id.PrincipalID("human-1"),
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   userRoles,
			Enabled: true,
			Version: 1,
		}
	}
	engineerOnly := actorWithRoles(tenancy.RoleEngineer)
	engineerAdmin := actorWithRoles(tenancy.RoleEngineer, tenancy.RoleAdmin)

	runMutation := func(opName authorize.OperationName, actor identity.Principal, adminAllowed bool) ([]authorize.AuthorityRead, []authorize.AuthorityFact, *pushAdminChangePort, error) {
		authority := &pushAdminAuthorityReader{adminAllowed: adminAllowed}
		changes := &pushAdminChangePort{}
		reader := &pushAdminPipelineReader{
			byName: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
				return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline not found")
			},
			get: func(_ context.Context, _ tenancy.Scope, pipelineID pipeline.PipelineID) (pipeline.Definition, error) {
				yamlBytes := noPushYAML
				if opName == "pipeline.delete" {
					yamlBytes = pushYAML
				}
				return pipeline.Definition{
					ID:      pipelineID,
					Name:    "push-ci",
					YAML:    yamlBytes,
					Version: 1,
				}, nil
			},
		}
		management, err := pipeline.NewManagement(pipeline.ManagementConfig{
			Clock:      pushAdminClock{now: now},
			IDs:        &pushAdminIDs{},
			Authorizer: matrix,
			Authority:  authority,
			Changes:    changes,
			Reader:     reader,
		})
		if err != nil {
			t.Fatalf("NewManagement(%s): %v", opName, err)
		}
		switch opName {
		case "pipeline.apply":
			_, err = management.Apply(ctx, pipeline.ApplyCommand{
				Actor:     actor,
				Scope:     scope,
				Name:      "push-ci",
				YAML:      pushYAML,
				CallerKey: "caller-apply",
				Content:   content,
			})
		case "pipeline.update":
			_, err = management.Update(ctx, pipeline.UpdateCommand{
				Actor:           actor,
				Scope:           scope,
				PipelineID:      "pipeline-1",
				Name:            "push-ci",
				YAML:            pushYAML,
				ExpectedVersion: 1,
				CallerKey:       "caller-update",
				Content:         content,
			})
		case "pipeline.delete":
			_, err = management.Delete(ctx, pipeline.DeleteCommand{
				Actor:           actor,
				Scope:           scope,
				PipelineID:      "pipeline-1",
				ExpectedVersion: 1,
				CallerKey:       "caller-delete",
				Content:         content,
			})
		}
		return authority.reads, authority.facts, changes, err
	}

	for _, name := range ops {
		for _, actor := range []identity.Principal{engineerOnly, engineerAdmin} {
			decision, err := matrix.Authorize(ctx, actor, name, scope)
			if err != nil || decision != authorize.DecisionAllow {
				t.Fatalf("%s grant for roles=%v = (%s, %v); want allow", name, actor.Roles, decision, err)
			}
		}

		reads, facts, changes, err := runMutation(name, engineerOnly, false)
		if !hasAuthorityRead(reads, name, scope, engineerOnly.ID, authorize.CheckPushServiceAdmin) {
			t.Fatalf("%s did not emit push_service_admin authority read: %+v", name, reads)
		}
		confirmErr := authorize.Confirm(reads, facts)
		if class, ok := fault.ClassOf(confirmErr); !ok || class != fault.Forbidden {
			t.Fatalf("%s Confirm(Allowed=false) = %v; want forbidden", name, confirmErr)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("%s without Admin = %v; want forbidden", name, err)
		}
		if changes.calls != 0 {
			t.Fatalf("%s committed %d times when Admin was absent; want 0", name, changes.calls)
		}

		reads, facts, changes, err = runMutation(name, engineerAdmin, true)
		if !hasAuthorityRead(reads, name, scope, engineerAdmin.ID, authorize.CheckPushServiceAdmin) {
			t.Fatalf("%s with Admin did not emit push_service_admin authority read: %+v", name, reads)
		}
		if confirmErr := authorize.Confirm(reads, facts); confirmErr != nil {
			t.Fatalf("%s Confirm(Allowed=true) = %v; want nil", name, confirmErr)
		}
		if err != nil {
			t.Fatalf("%s with Engineer+Admin failed: %v", name, err)
		}
		if changes.calls != 1 {
			t.Fatalf("%s with Engineer+Admin committed %d times; want 1", name, changes.calls)
		}
		if !hasAuthorityRead(changes.lastUnit.Authority, name, scope, engineerAdmin.ID, authorize.CheckPushServiceAdmin) {
			t.Fatalf("%s ChangeUnit omitted push_service_admin read: %+v", name, changes.lastUnit.Authority)
		}
	}
}

type pushAdminClock struct{ now time.Time }

func (c pushAdminClock) Now() time.Time { return c.now }

type pushAdminIDs struct{ next int }

func (ids *pushAdminIDs) NewEventID() (id.EventID, error) {
	ids.next++
	return id.EventID(fmt.Sprintf("event-%d", ids.next)), nil
}

func (ids *pushAdminIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.next)), nil
}

type pushAdminAuthorityReader struct {
	adminAllowed bool
	reads        []authorize.AuthorityRead
	facts        []authorize.AuthorityFact
}

func (r *pushAdminAuthorityReader) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	r.reads = append([]authorize.AuthorityRead(nil), reads...)
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		allowed := true
		if read.Check == authorize.CheckPushServiceAdmin {
			allowed = r.adminAllowed
		}
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     allowed,
			Version:     read.ExpectedVersion,
		}
	}
	r.facts = append([]authorize.AuthorityFact(nil), facts...)
	return facts, nil
}

type pushAdminChangePort struct {
	calls    int
	lastUnit pipeline.ChangeUnit
}

func (p *pushAdminChangePort) Change(_ context.Context, unit pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
	p.calls++
	p.lastUnit = unit
	pipelineID := unit.PipelineID
	if pipelineID == "" {
		pipelineID = "pipeline-1"
	}
	return pipeline.ChangeResult{PipelineID: pipelineID, Version: unit.ExpectedVersion + 1}, nil
}

type pushAdminPipelineReader struct {
	get    func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error)
	byName func(context.Context, tenancy.Scope, string) (pipeline.Definition, error)
}

func (r *pushAdminPipelineReader) Get(ctx context.Context, scope tenancy.Scope, id pipeline.PipelineID) (pipeline.Definition, error) {
	return r.get(ctx, scope, id)
}

func (r *pushAdminPipelineReader) ByName(ctx context.Context, scope tenancy.Scope, name string) (pipeline.Definition, error) {
	return r.byName(ctx, scope, name)
}

func (r *pushAdminPipelineReader) List(context.Context, tenancy.Scope) ([]pipeline.Definition, error) {
	return nil, nil
}

func hasAuthorityRead(reads []authorize.AuthorityRead, op authorize.OperationName, scope tenancy.Scope, principal id.PrincipalID, want authorize.AuthorityCheck) bool {
	for _, read := range reads {
		if read.Operation == op && read.Scope == scope && read.PrincipalID == principal && read.Check == want {
			return true
		}
	}
	return false
}

func hasCheck(list []authorize.AuthorityCheck, want authorize.AuthorityCheck) bool {
	for _, check := range list {
		if check == want {
			return true
		}
	}
	return false
}

func TestUnknownOperationRefused(t *testing.T) {
	if _, ok := authorize.LookupOperation(authorize.OperationName("task.teleport")); ok {
		t.Fatal("LookupOperation accepted an unknown operation")
	}
	if _, ok := authorize.LookupOperation(""); ok {
		t.Fatal("LookupOperation accepted an empty operation")
	}
}

// TestAdminReviewerDistinct pins the two authorities S05 separates: Admin
// configures, Reviewer decides. Neither substitutes for the other.
//
// Assigned-respondent operations (task.reply, execution.request.resolve) admit
// every human membership role at the grant stage because the assignment grammar
// accepts role/<any role>; the in-commit assignment check is the authority. An
// Admin therefore gains no implicit reviewer authority: it may answer only an
// assignment naming the role it currently holds.
func TestAdminReviewerDistinct(t *testing.T) {
	membership, ok := authorize.LookupOperation("membership.grant")
	if !ok {
		t.Fatal("membership.grant missing")
	}
	if !hasRole(membership.Requires.HumanRoles, tenancy.RoleAdmin) {
		t.Fatal("membership.grant does not grant Admin")
	}
	if hasRole(membership.Requires.HumanRoles, tenancy.RoleReviewer) {
		t.Fatal("membership.grant wrongly grants Reviewer")
	}

	for _, name := range []authorize.OperationName{"execution.request.resolve", "task.reply"} {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		for _, role := range tenancy.HumanRoles() {
			if !hasRole(op.Requires.HumanRoles, role) {
				t.Errorf("%s does not admit human role %q at the grant stage", name, role)
			}
		}
		for _, role := range tenancy.ServiceRoles() {
			if !hasRole(op.Requires.ServiceRoles, role) {
				t.Errorf("%s does not admit service role %q at the grant stage", name, role)
			}
		}
		if hasRole(op.Requires.ServiceRoles, tenancy.RoleAdmin) {
			t.Errorf("%s grants Admin to services", name)
		}
		if op.Requires.Execution != authorize.ExecNone {
			t.Errorf("%s grants execution authority %q", name, op.Requires.Execution)
		}
	}

	// Services never receive Admin, and execution authority never includes
	// request resolution, assigned replies or administration.
	for _, op := range authorize.Operations() {
		if hasRole(op.Requires.ServiceRoles, tenancy.RoleAdmin) {
			t.Errorf("%s grants Admin to services", op.Name)
		}
		switch op.Name {
		case "execution.request.resolve", "task.reply", "membership.grant", "pipeline.apply":
			if op.Requires.Execution != authorize.ExecNone {
				t.Errorf("%s grants execution authority %q", op.Name, op.Requires.Execution)
			}
		}
	}
}

// TestAssignedRespondentAuthority proves the split between the operation grant
// and the in-commit assignment check. Every human role and every enabled service
// with a single non-Admin role passes the grant stage; execution principals are
// denied; the exact assignment still refuses an Admin that does not hold the
// assigned reviewer/operator/engineer/viewer role.
func TestAssignedRespondentAuthority(t *testing.T) {
	for _, name := range []authorize.OperationName{"task.reply", "execution.request.resolve"} {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}

		for _, role := range tenancy.HumanRoles() {
			human := identity.Principal{
				ID:      "human-1",
				Kind:    identity.KindHuman,
				Roles:   []tenancy.Role{role},
				Enabled: true,
			}
			if got := decide(op, human.Kind, human.Roles, authorize.ExecNone); got != authorize.DecisionAllow {
				t.Errorf("%s human role %q grant = %s; want allow", name, role, got)
			}
		}

		for _, role := range tenancy.ServiceRoles() {
			svc := identity.Principal{
				ID:      "service-1",
				Kind:    identity.KindService,
				Roles:   []tenancy.Role{role},
				Enabled: true,
			}
			if got := decide(op, svc.Kind, svc.Roles, authorize.ExecNone); got != authorize.DecisionAllow {
				t.Errorf("%s service role %q grant = %s; want allow", name, role, got)
			}
		}

		if got := decide(op, identity.KindExecution, nil, authorize.ExecOwnOutputs); got != authorize.DecisionDeny {
			t.Errorf("%s execution grant = %s; want deny", name, got)
		}

		admin := identity.Principal{
			ID:      "admin-1",
			Kind:    identity.KindHuman,
			Roles:   []tenancy.Role{tenancy.RoleAdmin},
			Enabled: true,
		}
		if assignmentAllows(mustAssignment(t, "role/reviewer"), admin) {
			t.Errorf("%s admitted Admin to an unheld role/reviewer assignment", name)
		}
		if !assignmentAllows(mustAssignment(t, "role/admin"), admin) {
			t.Errorf("%s refused Admin for its own role/admin assignment", name)
		}

		for _, role := range tenancy.HumanRoles() {
			holder := identity.Principal{
				ID:      "human-1",
				Kind:    identity.KindHuman,
				Roles:   []tenancy.Role{role},
				Enabled: true,
			}
			if !assignmentAllows(mustAssignment(t, "role/"+string(role)), holder) {
				t.Errorf("%s refused the human currently holding assigned role %q", name, role)
			}
		}

		svc := identity.Principal{
			ID:      "service-1",
			Kind:    identity.KindService,
			Roles:   []tenancy.Role{tenancy.RoleViewer},
			Enabled: true,
		}
		if !assignmentAllows(mustAssignment(t, "service/service-1"), svc) {
			t.Errorf("%s refused the exact assigned service", name)
		}
		if assignmentAllows(mustAssignment(t, "service/service-2"), svc) {
			t.Errorf("%s admitted a different service to an assignment", name)
		}
		svc.Enabled = false
		if assignmentAllows(mustAssignment(t, "service/service-1"), svc) {
			t.Errorf("%s admitted a disabled assigned service", name)
		}
	}
}

// assignmentAllows is the test-side oracle for the in-commit assignment check:
// a role assignment requires a current enabled human membership holding exactly
// that role; a service assignment requires exactly that enabled service.
func assignmentAllows(assignment request.Assignment, principal identity.Principal) bool {
	if !principal.Enabled {
		return false
	}
	switch assignment.Kind {
	case request.AssignmentRole:
		if principal.Kind != identity.KindHuman {
			return false
		}
		for _, role := range principal.Roles {
			if role == assignment.Role {
				return true
			}
		}
	case request.AssignmentService:
		return principal.Kind == identity.KindService && string(principal.ID) == string(assignment.ServiceID)
	}
	return false
}

func TestRoleDecisionVectors(t *testing.T) {
	type vector struct {
		operation string
		kind      identity.PrincipalKind
		roles     []tenancy.Role
		execution authorize.ExecutionPermission
		want      authorize.Decision
	}
	vectors := []vector{
		{"membership.grant", identity.KindHuman, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionAllow},
		{"membership.grant", identity.KindHuman, roles(tenancy.RoleReviewer), authorize.ExecNone, authorize.DecisionDeny},
		// Assigned-respondent grants admit every human role at the grant stage;
		// the in-commit assignment check decides the exact respondent.
		{"execution.request.resolve", identity.KindHuman, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindHuman, roles(tenancy.RoleReviewer), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindHuman, roles(tenancy.RoleEngineer), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindHuman, roles(tenancy.RoleOperator), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindHuman, roles(tenancy.RoleViewer), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindService, roles(tenancy.RoleReviewer), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindService, roles(tenancy.RoleViewer), authorize.ExecNone, authorize.DecisionAllow},
		{"execution.request.resolve", identity.KindService, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionDeny},
		{"task.reply", identity.KindHuman, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindHuman, roles(tenancy.RoleReviewer), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindHuman, roles(tenancy.RoleEngineer), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindHuman, roles(tenancy.RoleOperator), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindHuman, roles(tenancy.RoleViewer), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindService, roles(tenancy.RoleOperator), authorize.ExecNone, authorize.DecisionAllow},
		{"task.reply", identity.KindService, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionDeny},
		{"task.create", identity.KindHuman, roles(tenancy.RoleEngineer), authorize.ExecNone, authorize.DecisionAllow},
		{"task.create", identity.KindHuman, roles(tenancy.RoleViewer), authorize.ExecNone, authorize.DecisionDeny},
		{"pipeline.apply", identity.KindService, roles(tenancy.RoleEngineer), authorize.ExecNone, authorize.DecisionAllow},
		{"pipeline.apply", identity.KindService, roles(tenancy.RoleViewer), authorize.ExecNone, authorize.DecisionDeny},
		{"pipeline.apply", identity.KindService, roles(tenancy.RoleAdmin), authorize.ExecNone, authorize.DecisionDeny},
		{"execution.action.outputs", identity.KindExecution, roles(), authorize.ExecOwnOutputs, authorize.DecisionAllow},
		{"execution.action.outputs", identity.KindExecution, roles(), authorize.ExecOwnTaskContent, authorize.DecisionDeny},
		{"execution.request.resolve", identity.KindExecution, roles(), authorize.ExecOwnOutputs, authorize.DecisionDeny},
		{"task.read", identity.KindExecution, roles(), authorize.ExecNone, authorize.DecisionDeny},
	}
	for _, tc := range vectors {
		op, ok := authorize.LookupOperation(authorize.OperationName(tc.operation))
		if !ok {
			t.Fatalf("operation %q missing", tc.operation)
		}
		if got := decide(op, tc.kind, tc.roles, tc.execution); got != tc.want {
			t.Errorf("%s for %s roles=%v exec=%q = %s; want %s",
				tc.operation, tc.kind, tc.roles, tc.execution, got, tc.want)
		}
	}

	// Unknown operations never allow, regardless of role or permission.
	if _, ok := authorize.LookupOperation("task.teleport"); ok {
		t.Fatal("unknown operation unexpectedly registered")
	}
}

// decide is the test-side oracle for the declared grant matrix. It is not a
// production authorizer: production also rechecks current credentials, scope
// and versions, which is why the registry carries Authority checks separately.
func decide(op authorize.Operation, kind identity.PrincipalKind, userRoles []tenancy.Role, permission authorize.ExecutionPermission) authorize.Decision {
	switch kind {
	case identity.KindHuman:
		for _, role := range userRoles {
			if hasRole(op.Requires.HumanRoles, role) {
				return authorize.DecisionAllow
			}
		}
	case identity.KindService:
		for _, role := range userRoles {
			if hasRole(op.Requires.ServiceRoles, role) {
				return authorize.DecisionAllow
			}
		}
	case identity.KindExecution:
		if op.Requires.Execution != authorize.ExecNone && op.Requires.Execution == permission {
			return authorize.DecisionAllow
		}
	}
	return authorize.DecisionDeny
}

func sameRoles(left, right []tenancy.Role) bool {
	if len(left) != len(right) {
		return false
	}
	present := make(map[tenancy.Role]bool, len(left))
	for _, role := range left {
		present[role] = true
	}
	for _, role := range right {
		if !present[role] {
			return false
		}
	}
	return true
}

func sameChecks(left, right []authorize.AuthorityCheck) bool {
	if len(left) != len(right) {
		return false
	}
	present := make(map[authorize.AuthorityCheck]bool, len(left))
	for _, check := range left {
		present[check] = true
	}
	for _, check := range right {
		if !present[check] {
			return false
		}
	}
	return true
}

func sameClasses(left, right []fault.Class) bool {
	if len(left) != len(right) {
		return false
	}
	present := make(map[fault.Class]bool, len(left))
	for _, class := range left {
		present[class] = true
	}
	for _, class := range right {
		if !present[class] {
			return false
		}
	}
	return true
}

func hasRole(list []tenancy.Role, want tenancy.Role) bool {
	for _, role := range list {
		if role == want {
			return true
		}
	}
	return false
}

func containsClass(list []fault.Class, want fault.Class) bool {
	for _, class := range list {
		if class == want {
			return true
		}
	}
	return false
}
