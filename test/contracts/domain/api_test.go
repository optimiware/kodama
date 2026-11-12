// Package domaincontract pins the Shell domain contract.
//
// This file is the human-readable API pin for the declaration-only Shell
// domain packages. Tests in this package are compile-time consumers plus
// deterministic table/vector checks; they never start a process, container,
// database, broker, network call or real identity provider.
//
// # Pinned package graph
//
// The Shell consumer contract is split by capability, never by a universal
// "contracts" or "repository" package:
//
//	internal/shell/tenancy    Scope, Role, Membership, Tenant, Project
//	internal/shell/identity   Principal, PrincipalKind
//	internal/shell/service    ServiceID, Service, credential fields
//	internal/shell/authorize  operation registry, Principal, Authorizer,
//	                          CurrentPrincipal, AuthorityReader
//	internal/shell/token      credential claims, canonical MAC input, mutation
//	internal/shell/verify     verification record, typed subject matching, report
//	internal/shell/pipeline   admission input/snapshot, exact YAML hash, limits
//	internal/shell/execution  execution values and transition commit unit
//	internal/shell/request    assignment grammar and activation/resolve units
//	internal/shell/task       content vs lifecycle versions and task commit units
//	internal/shell/inbox      scoped inbox query/page
//	internal/shell/event      envelope/audit/post-commit effect records
//	internal/shell/receipt    scoped receipt identity and idempotency content
//	internal/shell/trigger    push selection/result commit unit
//	internal/shell/secret     secret metadata and write bounds
//	internal/shell/log        retained/live log chunk bounds
//	internal/shell/gateway    optional model surface
//	internal/shell/mcpproxy   optional tool surface
//
// Import direction is one-way: tenancy/id/fault are neutral; authorize imports
// identity; capability packages import the contracts they consume. No package
// imports a concrete store, runtime, transport, Shell app or Userland package.
//
// # Pinned value rules
//
// These pure value rules are part of the contract, not a capability
// implementation. Capability providers implement behavior; the contract only
// fixes the shapes and the value rules needed for every provider to agree.
//
//   - tenancy.Scope: TenantID required; ProjectID empty means tenant-scoped.
//   - tenancy.Roles/HumanRoles: admin, engineer, reviewer, operator, viewer.
//     ServiceRoles is the same list without admin.
//   - service.Service.Validate: Admin is not a service role; a nonce and its
//     expiry are both present or both absent; a present nonce is >= 32 bytes.
//   - authorize.Operations is the complete known-operation registry. Every
//     operation declares family, scope kind, input/result schema names,
//     human roles, service roles, execution permission, current-authority
//     checks, receipt policy and stable error classes. Unknown names refuse.
//   - resource.Operation is a separate vocabulary for effects a binding's
//     enforcing service verifies (git.ref.update). Binding.Validate refuses
//     empty or unknown resource operations; a public operation name is not a
//     resource operation and git.ref.update is not a public operation.
//   - pipeline.NewSnapshot hashes the exact YAML bytes (SHA-256, no
//     canonicalization), copies all inputs and never accepts empty YAML.
//     pipeline.ValidateInputs enforces declared keys, required values and
//     string/number/boolean kinds.
//   - request.ParseAssignment accepts exactly "role/<role>" or
//     "service/<id>" with no trimming, whitespace or extra segments.
//     request.ResponseSchema.Validate enforces declared response keys and
//     required response fields.
//   - token.ExecutionMACInput/ServiceMACInput return the canonical binary MAC
//     input: length-prefixed domain, tenant, principal, nonce, then big-endian
//     uint64 Unix seconds expiry. Golden vectors live in testdata/tokens.json.
//     token.Credential.State derives absent/current/expired from the stored
//     nonce and expiry; there is no durable token plaintext or token hash.
//   - receipt.Normalize/Fingerprint: JSON media type is canonicalized with
//     sorted object keys and literal numbers; other media types are exact
//     bytes. Absent and null remain distinct.
//   - verify.SubjectMatches: every required field matches by name, typed kind
//     and value; recorded fields the enforcing service did not supply as
//     required are display context and are ignored; a required field is never
//     ignorable.
//   - Every transaction payload carries owner scope, expected versions,
//     current-authority reads, state/credential/receipt/audit/event writes and
//     post-commit effects. Ports expose complete commit units; no Save,
//     Update, SetState, Persist, Store or generic Repository method exists.
package domaincontract

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// TestFaultClassification pins the one public error vocabulary every
// operation must use. Operation-specific classes are checked in
// operations_test.go; this test proves the shared table cannot drift.
func TestFaultClassification(t *testing.T) {
	classes := fault.Classes()
	codes := fault.Codes()
	if len(classes) != 11 || len(codes) != 11 {
		t.Fatalf("fault table has %d classes and %d codes; want 11 and 11", len(classes), len(codes))
	}
	want := []fault.Class{
		fault.InvalidInput,
		fault.Unauthenticated,
		fault.Forbidden,
		fault.NotFound,
		fault.VersionConflict,
		fault.ChangedIdempotencyContent,
		fault.DeadlineExpired,
		fault.UnsupportedCapability,
		fault.DependencyUnavailable,
		fault.UncertainExternalEffect,
		fault.InternalFailure,
	}
	if !reflect.DeepEqual(classes, want) {
		t.Fatalf("classes = %v; want %v", classes, want)
	}
	for index, class := range classes {
		code, ok := fault.CodeFor(class)
		if !ok {
			t.Fatalf("class %q has no public code", class)
		}
		if code != codes[index] {
			t.Fatalf("class %q maps to %q; table position is %q", class, code, codes[index])
		}
		if !class.Valid() || !code.Valid() {
			t.Fatalf("class/code %q/%q is not valid", class, code)
		}
	}
}

// TestTypedIdentityNames proves the contract introduces distinct named types
// rather than reusing a generic string for resource, binding, service,
// pipeline, verification, question and secret identities.
func TestTypedIdentityNames(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{id.ExecutionID("exec-1"), "id.ExecutionID"},
		{id.TaskID("task-1"), "id.TaskID"},
		{id.TenantID("tenant-1"), "id.TenantID"},
		{id.ProjectID("project-1"), "id.ProjectID"},
		{id.PrincipalID("principal-1"), "id.PrincipalID"},
		{resource.ResourceID("resource-1"), "resource.ResourceID"},
		{resource.BindingID("binding-1"), "resource.BindingID"},
		{service.ServiceID("service-1"), "service.ServiceID"},
		{pipeline.PipelineID("pipeline-1"), "pipeline.PipelineID"},
		{verify.VerificationID("verification-1"), "verify.VerificationID"},
		{task.QuestionID("question-1"), "task.QuestionID"},
		{secret.SecretID("secret-1"), "secret.SecretID"},
	}
	for _, tc := range cases {
		if got := reflect.TypeOf(tc.value).String(); got != tc.want {
			t.Errorf("identity type = %q; want %q", got, tc.want)
		}
	}

	// Parsing is the admission path for every new identity. Invalid payloads
	// use the invalid_input class, not an untyped error.
	if _, err := resource.ParseResourceID("../escape"); err == nil {
		t.Error("resource.ParseResourceID accepted a path escape")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Errorf("resource.ParseResourceID class = %v; want invalid_input", class)
	}
	if _, err := resource.ParseBindingID(""); err == nil {
		t.Error("resource.ParseBindingID accepted an empty id")
	}
	if _, err := service.ParseServiceID("service/one"); err == nil {
		t.Error("service.ParseServiceID accepted a slash")
	}
	if _, err := pipeline.ParsePipelineID("pipeline one"); err == nil {
		t.Error("pipeline.ParsePipelineID accepted a space")
	}
	if _, err := verify.ParseVerificationID("verification.one"); err == nil {
		t.Error("verify.ParseVerificationID accepted a dot")
	}
	if _, err := secret.ParseSecretID("secret-1"); err != nil {
		t.Errorf("secret.ParseSecretID refused a valid id: %v", err)
	}
}

func TestScopeAndVersions(t *testing.T) {
	tenantScope := tenancy.Scope{TenantID: id.TenantID("tenant-1")}
	if !tenantScope.TenantScoped() || tenantScope.ProjectScoped() {
		t.Fatalf("tenant scope predicates = tenant:%v project:%v", tenantScope.TenantScoped(), tenantScope.ProjectScoped())
	}
	if err := tenantScope.Validate(); err != nil {
		t.Fatalf("tenant scope refused: %v", err)
	}

	projectScope := tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")}
	if !projectScope.ProjectScoped() || projectScope.TenantScoped() {
		t.Fatal("project scope must report project-scoped")
	}
	if err := projectScope.Validate(); err != nil {
		t.Fatalf("project scope refused: %v", err)
	}

	if err := (tenancy.Scope{}).Validate(); err == nil {
		t.Fatal("empty scope accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("empty scope class = %v; want invalid_input", class)
	}

	// Optimistic versions are explicit unsigned counters on every versioned
	// entity. Constructing them directly proves the field names and types.
	tenant := tenancy.Tenant{ID: id.TenantID("tenant-1"), Name: "tenant", Version: 3}
	project := tenancy.Project{ID: id.ProjectID("project-1"), TenantID: id.TenantID("tenant-1"), Name: "project", Version: 4}
	membership := tenancy.Membership{
		Scope:       projectScope,
		PrincipalID: id.PrincipalID("principal-1"),
		Roles:       []tenancy.Role{tenancy.RoleEngineer},
		Version:     5,
	}
	if tenant.Version != 3 || project.Version != 4 || membership.Version != 5 {
		t.Fatalf("version fields = %d/%d/%d; want 3/4/5", tenant.Version, project.Version, membership.Version)
	}
	if !membership.Has(tenancy.RoleEngineer) || membership.Has(tenancy.RoleAdmin) {
		t.Fatalf("membership.Has did not reflect roles %v", membership.Roles)
	}
}

func TestRoles(t *testing.T) {
	want := []tenancy.Role{
		tenancy.RoleAdmin,
		tenancy.RoleEngineer,
		tenancy.RoleReviewer,
		tenancy.RoleOperator,
		tenancy.RoleViewer,
	}
	if got := tenancy.Roles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Roles() = %v; want %v", got, want)
	}
	if got := tenancy.HumanRoles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("HumanRoles() = %v; want %v", got, want)
	}
	serviceRoles := tenancy.ServiceRoles()
	wantService := []tenancy.Role{
		tenancy.RoleEngineer,
		tenancy.RoleReviewer,
		tenancy.RoleOperator,
		tenancy.RoleViewer,
	}
	if !reflect.DeepEqual(serviceRoles, wantService) {
		t.Fatalf("ServiceRoles() = %v; want %v", serviceRoles, wantService)
	}
	for _, role := range serviceRoles {
		if role == tenancy.RoleAdmin {
			t.Fatal("Admin must not be a service-assignable role")
		}
		if !role.ServiceAssignable() {
			t.Fatalf("%q must be service-assignable", role)
		}
	}
	if tenancy.RoleAdmin.ServiceAssignable() {
		t.Fatal("Admin reported service-assignable")
	}
	if tenancy.Role("owner").Valid() {
		t.Fatal("unknown role reported valid")
	}
	// Admin and Reviewer are distinct authorities. This is the S05
	// boundary that must never be collapsed into one "admin" bit.
	if tenancy.RoleAdmin == tenancy.RoleReviewer {
		t.Fatal("Admin and Reviewer collapsed")
	}
}

func TestPrincipalKindsAndValidation(t *testing.T) {
	want := []identity.PrincipalKind{
		identity.KindHuman,
		identity.KindService,
		identity.KindExecution,
	}
	if got := identity.PrincipalKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PrincipalKinds() = %v; want %v", got, want)
	}
	if identity.PrincipalKind("robot").Valid() {
		t.Fatal("unknown principal kind reported valid")
	}

	scope := tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")}
	human := identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
	if err := human.Validate(); err != nil {
		t.Fatalf("human principal refused: %v", err)
	}
	if !human.HasRole(tenancy.RoleAdmin) || human.HasRole(tenancy.RoleReviewer) {
		t.Fatalf("human HasRole did not reflect roles %v", human.Roles)
	}

	servicePrincipal := human
	servicePrincipal.ID = id.PrincipalID("service-1")
	servicePrincipal.Kind = identity.KindService
	servicePrincipal.Roles = []tenancy.Role{tenancy.RoleAdmin}
	if err := servicePrincipal.Validate(); err == nil {
		t.Fatal("service principal accepted the Admin role")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("service Admin class = %v; want invalid_input", class)
	}

	executionPrincipal := identity.Principal{
		ID:          id.PrincipalID("execution-1"),
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: id.ExecutionID("exec-1"),
	}
	if err := executionPrincipal.Validate(); err != nil {
		t.Fatalf("execution principal refused: %v", err)
	}
	executionPrincipal.Roles = []tenancy.Role{tenancy.RoleEngineer}
	if err := executionPrincipal.Validate(); err == nil {
		t.Fatal("execution principal accepted a human role")
	}
}

func TestServiceValidation(t *testing.T) {
	expiry := time.Unix(1_700_000_000, 0).UTC()
	valid := service.Service{
		ID:          service.ServiceID("service-1"),
		Scope:       tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		State:       service.StateEnabled,
		Roles:       []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator},
		Version:     7,
		TokenNonce:  make([]byte, 32),
		TokenExpiry: &expiry,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid service refused: %v", err)
	}
	if !valid.Active() {
		t.Fatal("enabled service is not active")
	}

	admin := valid
	admin.Roles = []tenancy.Role{tenancy.RoleAdmin}
	if err := admin.Validate(); err == nil {
		t.Fatal("service with Admin role accepted")
	}

	halfPair := valid
	halfPair.TokenExpiry = nil
	if err := halfPair.Validate(); err == nil {
		t.Fatal("service with nonce but no expiry accepted")
	}

	shortNonce := valid
	shortNonce.TokenNonce = make([]byte, 31)
	if err := shortNonce.Validate(); err == nil {
		t.Fatal("service with a 31-byte nonce accepted")
	}

	disabled := valid
	disabled.State = service.StateDisabled
	disabled.TokenNonce = nil
	disabled.TokenExpiry = nil
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled service without credential refused: %v", err)
	}
	if disabled.Active() {
		t.Fatal("disabled service reported active")
	}

	if got := service.States(); !reflect.DeepEqual(got, []service.State{service.StateEnabled, service.StateDisabled}) {
		t.Fatalf("States() = %v", got)
	}
	if service.State("suspended").Valid() {
		t.Fatal("unknown service state reported valid")
	}
}

func TestStateVocabularies(t *testing.T) {
	if got := task.States(); !reflect.DeepEqual(got, []task.State{task.StateOpen, task.StateWaiting, task.StateDone, task.StateCancelled}) {
		t.Fatalf("task.States() = %v", got)
	}
	for _, state := range task.States() {
		if !state.Valid() {
			t.Fatalf("task state %q reported invalid", state)
		}
	}
	if task.State("running").Valid() {
		t.Fatal("task state 'running' accepted; execution progress is derived")
	}

	if got := []request.Decision{request.DecisionPass, request.DecisionFail}; len(got) != 2 || !got[0].Valid() || !got[1].Valid() {
		t.Fatalf("request decision vocabulary = %v", got)
	}
	if request.Decision("maybe").Valid() {
		t.Fatal("unknown request decision accepted")
	}

	if got := []verify.ValueKind{verify.KindString, verify.KindNumber, verify.KindBoolean, verify.KindNull}; len(got) != 4 {
		t.Fatalf("verify value kinds = %v", got)
	}
	for _, kind := range []verify.ValueKind{verify.KindString, verify.KindNumber, verify.KindBoolean, verify.KindNull} {
		if !kind.Valid() {
			t.Fatalf("verify kind %q reported invalid", kind)
		}
	}
	if verify.ValueKind("array").Valid() {
		t.Fatal("unknown verify value kind accepted")
	}

	// Execution and action states are Kernel vocabulary; Shell must reuse it
	// rather than invent a second state machine.
	if !kernel.ExecutionRunning.Valid() || !kernel.ActionPending.Valid() {
		t.Fatal("kernel state vocabulary is not valid")
	}
}

func TestAuthorityVocabulary(t *testing.T) {
	families := authorize.Families()
	wantFamilies := []authorize.Family{
		authorize.FamilyTenancy,
		authorize.FamilyResource,
		authorize.FamilyPipeline,
		authorize.FamilySecret,
		authorize.FamilyService,
		authorize.FamilyTask,
		authorize.FamilyInbox,
		authorize.FamilyExecution,
		authorize.FamilyVerify,
		authorize.FamilyEvent,
		authorize.FamilyModel,
		authorize.FamilyTool,
	}
	if !reflect.DeepEqual(families, wantFamilies) {
		t.Fatalf("Families() = %v; want %v", families, wantFamilies)
	}
	for _, family := range families {
		if !family.Valid() {
			t.Fatalf("family %q reported invalid", family)
		}
	}
	if authorize.Family("magic").Valid() {
		t.Fatal("unknown family reported valid")
	}

	if !authorize.ScopeTenant.Valid() || !authorize.ScopeProject.Valid() {
		t.Fatal("scope kinds are not valid")
	}
	if authorize.ScopeKind("global").Valid() {
		t.Fatal("unknown scope kind reported valid")
	}

	for _, policy := range []authorize.ReceiptPolicy{
		authorize.ReceiptNone,
		authorize.ReceiptRequired,
		authorize.ReceiptRequestScoped,
		authorize.ReceiptPushScoped,
	} {
		if !policy.Valid() {
			t.Fatalf("receipt policy %q reported invalid", policy)
		}
	}
	if authorize.ReceiptPolicy("sometimes").Valid() {
		t.Fatal("unknown receipt policy reported valid")
	}

	for _, check := range []authorize.AuthorityCheck{
		authorize.CheckMembership,
		authorize.CheckPrincipalEnabled,
		authorize.CheckServiceEnabled,
		authorize.CheckExecutionRunning,
		authorize.CheckActionDeadline,
		authorize.CheckBindingVersion,
		authorize.CheckTaskVersion,
		authorize.CheckExecutionVersion,
		authorize.CheckPipelineVersion,
		authorize.CheckPushServiceAdmin,
		authorize.CheckFailedTaskOperatorStart,
		authorize.CheckResourceOwnership,
	} {
		if !check.Valid() {
			t.Fatalf("authority check %q reported invalid", check)
		}
	}
	if authorize.AuthorityCheck("trust_me").Valid() {
		t.Fatal("unknown authority check reported valid")
	}

	for _, permission := range []authorize.ExecutionPermission{
		authorize.ExecNone,
		authorize.ExecOwnTaskContent,
		authorize.ExecProjectTaskCreate,
		authorize.ExecOwnTaskAsk,
		authorize.ExecOwnOutputs,
		authorize.ExecOwnEvents,
		authorize.ExecConfiguredModels,
		authorize.ExecConfiguredTools,
	} {
		if !permission.Valid() {
			t.Fatalf("execution permission %q reported invalid", permission)
		}
	}
	if authorize.ExecutionPermission("admin").Valid() {
		t.Fatal("execution Admin permission accepted")
	}

	if authorize.OperationName("task.start").Valid() == false {
		t.Fatal("registered operation reported invalid")
	}
	if authorize.OperationName("task.teleport").Valid() {
		t.Fatal("unknown operation reported valid")
	}
}

// TestResourceOperationVocabulary pins the separate resource-operation
// vocabulary. A binding declares the effects its enforcing service verifies
// (for example git.ref.update), never a public router operation. The two
// vocabularies must stay disjoint rather than silently sharing names.
func TestResourceOperationVocabulary(t *testing.T) {
	operations := resource.Operations()
	want := []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	if len(operations) != len(want) {
		t.Fatalf("resource.Operations() = %v; want %v", operations, want)
	}
	for index := range want {
		if operations[index] != want[index] {
			t.Fatalf("resource operation %d = %q; want %q", index, operations[index], want[index])
		}
	}

	// Mutating the returned slice must not corrupt the declared vocabulary.
	operations[0] = resource.Operation("task.start")
	if got := resource.Operations(); len(got) != len(want) || got[0] != resource.OperationGitRefUpdate || got[1] != resource.OperationGitRead {
		t.Fatalf("resource.Operations() exposed mutable state: %v", got)
	}

	if !resource.OperationGitRefUpdate.Valid() || !resource.OperationGitRead.Valid() {
		t.Fatal("a declared resource operation reported invalid")
	}
	for _, name := range []string{"", "git.ref.read", "git.ref.delete", "resource.report", "task.start"} {
		if resource.Operation(name).Valid() {
			t.Errorf("resource.Operation(%q) reported valid", name)
		}
		if _, err := resource.ParseOperation(name); err == nil {
			t.Errorf("resource.ParseOperation(%q) accepted an invalid name", name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("resource.ParseOperation(%q) class = %v; want invalid_input", name, class)
		}
	}
	if parsed, err := resource.ParseOperation("git.ref.update"); err != nil || parsed != resource.OperationGitRefUpdate {
		t.Fatalf("resource.ParseOperation(git.ref.update) = %q, %v", parsed, err)
	}
	if parsed, err := resource.ParseOperation("git.read"); err != nil || parsed != resource.OperationGitRead {
		t.Fatalf("resource.ParseOperation(git.read) = %q, %v", parsed, err)
	}

	// The vocabularies stay disjoint: the public registry did not gain
	// git.ref.update or git.read and a public operation is never a resource
	// operation.
	for _, operation := range want {
		if authorize.OperationName(operation).Valid() {
			t.Fatalf("resource operation %q leaked into the public operation registry", operation)
		}
	}
	if authorize.OperationName(resource.OperationGitRefUpdate).Valid() {
		t.Fatal("resource operation accepted by the public registry")
	}

	binding := resource.Binding{
		ID:               resource.BindingID("binding-1"),
		ResourceID:       resource.ResourceID("resource-1"),
		Scope:            tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		EnforcingService: service.ServiceID("service-1"),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          3,
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("binding with git.ref.update and git.read refused: %v", err)
	}

	for _, name := range []resource.Operation{"", "git.ref.read", "git.ref.delete", "resource.report", "task.start"} {
		invalid := binding
		invalid.Operations = []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead, name}
		if err := invalid.Validate(); err == nil {
			t.Errorf("binding accepted invalid operation %q", name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("binding invalid operation %q class = %v; want invalid_input", name, class)
		}
	}
}

func TestPostCommitVocabulary(t *testing.T) {
	kinds := []event.PostCommitKind{
		event.PostPublishEvent,
		event.PostDeliverCredential,
		event.PostRefreshCredential,
		event.PostDispatch,
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			t.Fatalf("post-commit kind %q reported invalid", kind)
		}
	}
	if event.PostCommitKind("restart_everything").Valid() {
		t.Fatal("unknown post-commit kind reported valid")
	}
}

// Compile-only consumers prove the consumer-owned ports are implementable
// without any concrete store, transport or identity provider.
var (
	_ authorize.CurrentPrincipal         = fakeCurrentPrincipal{}
	_ authorize.Authorizer               = fakeAuthorizer{}
	_ authorize.AuthorityReader          = fakeAuthorityReader{}
	_ execution.TransitionPort           = fakeTransition{}
	_ request.ActivationPort             = fakeActivation{}
	_ request.ResolvePort                = fakeResolve{}
	_ task.StartPort                     = fakeTaskStart{}
	_ task.AskPort                       = fakeTaskAsk{}
	_ task.ReplyPort                     = fakeTaskReply{}
	_ task.CancelPort                    = fakeTaskCancel{}
	_ token.MutationPort                 = fakeTokenMutation{}
	_ token.CredentialReader             = fakeCredentialReader{}
	_ token.TenantKeyReader              = fakeTenantKeyReader{}
	_ identity.PrincipalReader           = fakePrincipalReader{}
	_ service.ServiceReader              = fakeServiceReader{}
	_ resource.BindingPort               = fakeBinding{}
	_ resource.BindingDeletePort         = fakeBindingDelete{}
	_ resource.BindingReadPort           = fakeBindingRead{}
	_ resource.BindingQueryPort          = fakeBindingQuery{}
	_ verify.ReportPort                  = fakeReport{}
	_ verify.RecordPort                  = fakeVerifyRecord{}
	_ verify.BindingReader               = fakeBindingReader{}
	_ verify.EvidenceReader              = fakeEvidenceReader{}
	_ trigger.SelectPort                 = fakeTrigger{}
	_ inbox.QueryPort                    = fakeInbox{}
	_ tenancyadmin.TenantCreatePort      = fakeTenantCreate{}
	_ tenancyadmin.TenantKeyRotationPort = fakeTenantKeyRotation{}
	_ tenancyadmin.ProjectCreatePort     = fakeProjectCreate{}
	_ tenancyadmin.ProjectUpdatePort     = fakeProjectUpdate{}
	_ tenancyadmin.MembershipGrantPort   = fakeMembershipGrant{}
	_ tenancyadmin.MembershipRevokePort  = fakeMembershipRevoke{}
	_ tenancyadmin.MembershipQueryPort   = fakeMembershipQuery{}
	_                                    = context.Background
	_                                    = time.Time{}
)

type fakeCurrentPrincipal struct{}

func (fakeCurrentPrincipal) Resolve(context.Context, string) (authorize.Principal, error) {
	return authorize.Principal{}, nil
}

type fakeAuthorizer struct{}

func (fakeAuthorizer) Authorize(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
	return authorize.DecisionDeny, nil
}

type fakeAuthorityReader struct{}

func (fakeAuthorityReader) Read(context.Context, []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	return nil, nil
}

type fakeTransition struct{}

func (fakeTransition) Transition(context.Context, execution.TransitionUnit) (execution.TransitionResult, error) {
	return execution.TransitionResult{}, nil
}

type fakeActivation struct{}

func (fakeActivation) Activate(context.Context, request.ActivationUnit) (request.ActivationResult, error) {
	return request.ActivationResult{}, nil
}

type fakeResolve struct{}

func (fakeResolve) Resolve(context.Context, request.ResolveUnit) (request.ResolveResult, error) {
	return request.ResolveResult{}, nil
}

type fakeTaskStart struct{}

func (fakeTaskStart) Start(context.Context, task.StartUnit) (task.StartResult, error) {
	return task.StartResult{}, nil
}

type fakeTaskAsk struct{}

func (fakeTaskAsk) Ask(context.Context, task.AskUnit) (task.AskResult, error) {
	return task.AskResult{}, nil
}

type fakeTaskReply struct{}

func (fakeTaskReply) Reply(context.Context, task.ReplyUnit) (task.ReplyResult, error) {
	return task.ReplyResult{}, nil
}

type fakeTaskCancel struct{}

func (fakeTaskCancel) Cancel(context.Context, task.CancelUnit) (task.CancelResult, error) {
	return task.CancelResult{}, nil
}

type fakeTokenMutation struct{}

func (fakeTokenMutation) Mutate(context.Context, token.MutationUnit) (token.MutationResult, error) {
	return token.MutationResult{}, nil
}

type fakeBinding struct{}

func (fakeBinding) Apply(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
	return resource.BindingResult{}, nil
}

type fakeReport struct{}

func (fakeReport) Report(context.Context, verify.ReportUnit) (verify.ReportResult, error) {
	return verify.ReportResult{}, nil
}

type fakeTrigger struct{}

func (fakeTrigger) Select(context.Context, trigger.SelectUnit) (trigger.Result, error) {
	return trigger.Result{}, nil
}

type fakeInbox struct{}

func (fakeInbox) Query(context.Context, inbox.Query) (inbox.Page, error) {
	return inbox.Page{}, nil
}

// TestPortsExposeCompleteCommitUnits rejects a generic repository/save split.
// Every consumer-owned port is a single named commit unit and takes
// context.Context first.
func TestPortsExposeCompleteCommitUnits(t *testing.T) {
	ports := []reflect.Type{
		reflect.TypeOf((*authorize.CurrentPrincipal)(nil)).Elem(),
		reflect.TypeOf((*authorize.Authorizer)(nil)).Elem(),
		reflect.TypeOf((*authorize.AuthorityReader)(nil)).Elem(),
		reflect.TypeOf((*execution.TransitionPort)(nil)).Elem(),
		reflect.TypeOf((*request.ActivationPort)(nil)).Elem(),
		reflect.TypeOf((*request.ResolvePort)(nil)).Elem(),
		reflect.TypeOf((*task.StartPort)(nil)).Elem(),
		reflect.TypeOf((*task.AskPort)(nil)).Elem(),
		reflect.TypeOf((*task.ReplyPort)(nil)).Elem(),
		reflect.TypeOf((*task.CancelPort)(nil)).Elem(),
		reflect.TypeOf((*token.MutationPort)(nil)).Elem(),
		reflect.TypeOf((*token.CredentialReader)(nil)).Elem(),
		reflect.TypeOf((*token.TenantKeyReader)(nil)).Elem(),
		reflect.TypeOf((*identity.PrincipalReader)(nil)).Elem(),
		reflect.TypeOf((*service.ServiceReader)(nil)).Elem(),
		reflect.TypeOf((*resource.BindingPort)(nil)).Elem(),
		reflect.TypeOf((*resource.BindingDeletePort)(nil)).Elem(),
		reflect.TypeOf((*resource.BindingReadPort)(nil)).Elem(),
		reflect.TypeOf((*resource.BindingQueryPort)(nil)).Elem(),
		reflect.TypeOf((*verify.ReportPort)(nil)).Elem(),
		reflect.TypeOf((*verify.RecordPort)(nil)).Elem(),
		reflect.TypeOf((*verify.BindingReader)(nil)).Elem(),
		reflect.TypeOf((*verify.EvidenceReader)(nil)).Elem(),
		reflect.TypeOf((*trigger.SelectPort)(nil)).Elem(),
		reflect.TypeOf((*inbox.QueryPort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.TenantCreatePort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.TenantKeyRotationPort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.ProjectCreatePort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.ProjectUpdatePort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.MembershipGrantPort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.MembershipRevokePort)(nil)).Elem(),
		reflect.TypeOf((*tenancyadmin.MembershipQueryPort)(nil)).Elem(),
	}
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	forbidden := map[string]bool{
		"Save":       true,
		"Update":     true,
		"SetState":   true,
		"Persist":    true,
		"Store":      true,
		"Repository": true,
	}
	for _, port := range ports {
		if port.NumMethod() == 0 {
			t.Errorf("%s has no commit method", port)
			continue
		}
		for index := 0; index < port.NumMethod(); index++ {
			method := port.Method(index)
			if forbidden[method.Name] {
				t.Errorf("%s.%s is a generic persistence split", port, method.Name)
			}
			inputs := method.Type
			if inputs.NumIn() == 0 || inputs.In(0) != contextType {
				t.Errorf("%s.%s must take context.Context first", port, method.Name)
			}
		}
	}
}

type fakeCredentialReader struct{}

func (fakeCredentialReader) ReadCredential(context.Context, token.CredentialRequest) (token.CredentialFact, error) {
	return token.CredentialFact{}, nil
}

type fakeTenantKeyReader struct{}

func (fakeTenantKeyReader) ReadTenantKey(context.Context, token.TenantKeyRequest) (token.TenantKey, error) {
	return token.TenantKey{}, nil
}

type fakePrincipalReader struct{}

func (fakePrincipalReader) ReadPrincipal(context.Context, identity.PrincipalRequest) (identity.Principal, error) {
	return identity.Principal{}, nil
}

type fakeServiceReader struct{}

func (fakeServiceReader) ReadService(context.Context, service.ServiceReadRequest) (service.Service, error) {
	return service.Service{}, nil
}

type fakeBindingDelete struct{}

func (fakeBindingDelete) DeleteBinding(context.Context, resource.BindingDeleteUnit) (resource.BindingDeleteResult, error) {
	return resource.BindingDeleteResult{}, nil
}

type fakeBindingRead struct{}

func (fakeBindingRead) ReadBinding(context.Context, resource.BindingReadRequest) (resource.Binding, error) {
	return resource.Binding{}, nil
}

type fakeBindingQuery struct{}

func (fakeBindingQuery) QueryBindings(context.Context, resource.BindingQuery) (resource.BindingPage, error) {
	return resource.BindingPage{}, nil
}

type fakeVerifyRecord struct{}

func (fakeVerifyRecord) Record(context.Context, verify.RecordUnit) (verify.RecordResult, error) {
	return verify.RecordResult{}, nil
}

type fakeBindingReader struct{}

func (fakeBindingReader) ReadBinding(context.Context, verify.BindingQuery) (resource.Binding, error) {
	return resource.Binding{}, nil
}

type fakeEvidenceReader struct{}

func (fakeEvidenceReader) ReadEvidence(context.Context, verify.EvidenceQuery) ([]verify.Evidence, error) {
	return nil, nil
}

type fakeTenantCreate struct{}

func (fakeTenantCreate) CreateTenant(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
	return tenancyadmin.TenantCreateResult{}, nil
}

type fakeTenantKeyRotation struct{}

func (fakeTenantKeyRotation) RotateTenantKey(context.Context, tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
	return tenancyadmin.TenantKeyRotationResult{}, nil
}

type fakeProjectCreate struct{}

func (fakeProjectCreate) CreateProject(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
	return tenancyadmin.ProjectCreateResult{}, nil
}

type fakeProjectUpdate struct{}

func (fakeProjectUpdate) UpdateProject(context.Context, tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
	return tenancyadmin.ProjectUpdateResult{}, nil
}

type fakeMembershipGrant struct{}

func (fakeMembershipGrant) GrantMembership(context.Context, tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
	return tenancyadmin.MembershipGrantResult{}, nil
}

type fakeMembershipRevoke struct{}

func (fakeMembershipRevoke) RevokeMembership(context.Context, tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
	return tenancyadmin.MembershipRevokeResult{}, nil
}

type fakeMembershipQuery struct{}

func (fakeMembershipQuery) ListMemberships(context.Context, tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
	return tenancyadmin.MembershipPage{}, nil
}
