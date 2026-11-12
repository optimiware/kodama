package domaincontract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// TestTenancyAdminCommitUnits pins the consumer-facing tenant/project/
// membership administration seam. Every mutation is one named
// commit unit carrying the owner scope, expected version, current-authority
// reads, receipt, audit, event intent and post-commit effects; the listing is a
// bounded query, not a repository.
func TestTenancyAdminCommitUnits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	receiptRecord := sampleReceipt()
	audit := sampleAudit(now)
	events := sampleEvents(now)
	authority := sampleAuthority()
	postCommit := samplePostCommit()

	tenantKey := token.TenantKey{
		TenantID:   id.TenantID("tenant-1"),
		Version:    1,
		Ciphertext: []byte("encrypted-derivation-key"),
		WrappedKey: []byte("wrapped-data-key"),
		WrapNonce:  []byte("wrap-nonce-1"),
		KeyRef:     "kms/tenant-key",
		KeyVersion: 1,
	}
	tenantUnit := tenancyadmin.TenantCreateUnit{
		Tenant:     tenancy.Tenant{ID: id.TenantID("tenant-1"), Name: "tenant", Version: 1},
		Key:        tenantKey,
		At:         now,
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	if err := tenantUnit.Validate(); err != nil {
		t.Fatalf("valid tenant create unit refused: %v", err)
	}
	var createKey token.TenantKey = tenantUnit.Key
	if createKey.Version != 1 {
		t.Fatalf("tenant create unit lost its initial derivation key: %+v", createKey)
	}
	if err := (tenancyadmin.TenantCreateUnit{Tenant: tenantUnit.Tenant, At: now}).Validate(); err == nil {
		t.Fatal("tenant create unit without a derivation key accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("tenant create missing-key class = %v; want invalid_input", class)
	}
	missingKeyMaterial := tenantUnit
	missingKeyMaterial.Key.Ciphertext = nil
	if err := missingKeyMaterial.Validate(); err == nil {
		t.Fatal("tenant create unit accepted a key without ciphertext")
	}
	mismatchedKeyTenant := tenantUnit
	mismatchedKeyTenant.Key.TenantID = id.TenantID("tenant-2")
	if err := mismatchedKeyTenant.Validate(); err == nil {
		t.Fatal("tenant create unit accepted a key for another tenant")
	}

	rotationKey := tenantKey
	rotationKey.Version = 2
	rotationKey.Ciphertext = []byte("rotated-encrypted-derivation-key")
	rotationKey.KeyVersion = 2
	rotationUnit := tenancyadmin.TenantKeyRotationUnit{
		Tenant:          tenancy.Tenant{ID: id.TenantID("tenant-1"), Name: "tenant", Version: 2},
		Key:             token.TenantKeyWrite{TenantID: id.TenantID("tenant-1"), ExpectedVersion: 1, Key: rotationKey},
		ExpectedVersion: 1,
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      []event.PostCommitEffect{{Kind: event.PostRefreshCredential, Target: "tenant-1"}},
	}
	if err := rotationUnit.Validate(); err != nil {
		t.Fatalf("valid tenant key rotation unit refused: %v", err)
	}
	var rotationWrite token.TenantKeyWrite = rotationUnit.Key
	if rotationWrite.ExpectedVersion != 1 || rotationWrite.Key.Version != 2 {
		t.Fatalf("tenant key rotation unit lost its versioned key write: %+v", rotationWrite)
	}
	staleTenantVersion := rotationUnit
	staleTenantVersion.ExpectedVersion = 2
	if err := staleTenantVersion.Validate(); err == nil {
		t.Fatal("tenant key rotation accepted a stale tenant version")
	}
	staleKeyVersion := rotationUnit
	staleKeyVersion.Key.ExpectedVersion = 2
	if err := staleKeyVersion.Validate(); err == nil {
		t.Fatal("tenant key rotation accepted a stale key version")
	}
	mismatchedRotationTenant := rotationUnit
	mismatchedRotationTenant.Key.TenantID = id.TenantID("tenant-2")
	if err := mismatchedRotationTenant.Validate(); err == nil {
		t.Fatal("tenant key rotation accepted a key for another tenant")
	}
	missingRotationKey := rotationUnit
	missingRotationKey.Key.Key.Ciphertext = nil
	if err := missingRotationKey.Validate(); err == nil {
		t.Fatal("tenant key rotation accepted an incomplete key")
	}
	rotationType := reflect.TypeOf(rotationUnit)
	for _, forbidden := range []string{"PrincipalID", "ExecutionID", "ServiceID"} {
		if _, ok := rotationType.FieldByName(forbidden); ok {
			t.Errorf("tenancyadmin.TenantKeyRotationUnit carries per-credential target field %q", forbidden)
		}
	}
	for _, field := range []string{"Tenant", "Key", "ExpectedVersion", "At", "Receipt", "Audit", "Events", "Authority", "PostCommit"} {
		if _, ok := rotationType.FieldByName(field); !ok {
			t.Errorf("tenancyadmin.TenantKeyRotationUnit is missing required field %q", field)
		}
	}
	projectUnit := tenancyadmin.ProjectCreateUnit{
		Project:    tenancy.Project{ID: id.ProjectID("project-1"), TenantID: id.TenantID("tenant-1"), Name: "project", Version: 1},
		At:         now,
		Receipt:    receiptRecord,
		Audit:      audit,
		Events:     events,
		Authority:  authority,
		PostCommit: postCommit,
	}
	updateUnit := tenancyadmin.ProjectUpdateUnit{
		Owner:           scope,
		ProjectID:       id.ProjectID("project-1"),
		ExpectedVersion: 1,
		Project:         tenancy.Project{ID: id.ProjectID("project-1"), TenantID: id.TenantID("tenant-1"), Name: "renamed", Version: 2},
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	grantUnit := tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: id.PrincipalID("principal-1"), Roles: []tenancy.Role{tenancy.RoleReviewer}, Version: 1},
		ExpectedVersion: 0,
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}
	revokeUnit := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     id.PrincipalID("principal-1"),
		ExpectedVersion: 1,
		At:              now,
		Receipt:         receiptRecord,
		Audit:           audit,
		Events:          events,
		Authority:       authority,
		PostCommit:      postCommit,
	}

	if tenantUnit.Tenant.Name != "tenant" || projectUnit.Project.Name != "project" {
		t.Fatalf("tenancy admin create units lost their values: %+v / %+v", tenantUnit, projectUnit)
	}
	if updateUnit.ExpectedVersion != 1 || grantUnit.Membership.Version != 1 || revokeUnit.ExpectedVersion != 1 {
		t.Fatalf("tenancy admin units lost expected versions: %+v / %+v / %+v", updateUnit, grantUnit, revokeUnit)
	}

	units := []struct {
		name   string
		value  any
		fields []string
	}{
		{
			name:  "tenancyadmin.TenantCreateUnit",
			value: tenantUnit,
			fields: []string{
				"Tenant", "Key", "At", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "tenancyadmin.TenantKeyRotationUnit",
			value: rotationUnit,
			fields: []string{
				"Tenant", "Key", "ExpectedVersion", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "tenancyadmin.ProjectCreateUnit",
			value: projectUnit,
			fields: []string{
				"Project", "At", "Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "tenancyadmin.ProjectUpdateUnit",
			value: updateUnit,
			fields: []string{
				"Owner", "ProjectID", "ExpectedVersion", "Project", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "tenancyadmin.MembershipGrantUnit",
			value: grantUnit,
			fields: []string{
				"Owner", "Membership", "ExpectedVersion", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
		{
			name:  "tenancyadmin.MembershipRevokeUnit",
			value: revokeUnit,
			fields: []string{
				"Owner", "PrincipalID", "ExpectedVersion", "At",
				"Receipt", "Audit", "Events", "Authority", "PostCommit",
			},
		},
	}
	for _, unit := range units {
		typ := reflect.TypeOf(unit.value)
		for _, field := range unit.fields {
			if _, ok := typ.FieldByName(field); !ok {
				t.Errorf("%s is missing required field %q", unit.name, field)
			}
		}
	}

	query := tenancyadmin.MembershipQuery{Scope: scope, Limit: 10}
	if err := query.Validate(); err != nil {
		t.Fatalf("valid membership query refused: %v", err)
	}
	if err := (tenancyadmin.MembershipQuery{Scope: scope, Limit: 0}).Validate(); err == nil {
		t.Fatal("membership query accepted a zero limit")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("membership query zero-limit class = %v; want invalid_input", class)
	}
	if err := (tenancyadmin.MembershipQuery{Scope: scope, Limit: tenancyadmin.MaxMembershipPageSize + 1}).Validate(); err == nil {
		t.Fatal("membership query accepted an oversized limit")
	}
	if err := (tenancyadmin.MembershipQuery{Scope: tenancy.Scope{}, Limit: 10}).Validate(); err == nil {
		t.Fatal("membership query accepted an empty scope")
	}
	if err := (tenancyadmin.MembershipQuery{Scope: scope, PrincipalID: "bad/id", Limit: 10}).Validate(); err == nil {
		t.Fatal("membership query accepted a malformed principal id")
	}

	page := tenancyadmin.MembershipPage{Memberships: []tenancy.Membership{grantUnit.Membership}, NextCursor: "cursor-1"}
	if len(page.Memberships) != 1 || page.Memberships[0].PrincipalID != "principal-1" {
		t.Fatalf("membership page lost its items: %+v", page)
	}
}

// TestTokenServiceAndTenantKeySeams pins the token read seams and the optional
// service/tenant-key companions on the credential mutation unit. The MAC
// golden vectors are pinned in TestTokenEncodingGoldenVectors.
func TestTokenServiceAndTenantKeySeams(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	enabled := service.StateEnabled
	credential := sampleCredential(now)

	change := token.ServiceChange{State: &enabled, Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleOperator}}
	if err := change.Validate(); err != nil {
		t.Fatalf("valid service change refused: %v", err)
	}
	adminChange := token.ServiceChange{Roles: []tenancy.Role{tenancy.RoleAdmin}}
	if err := adminChange.Validate(); err == nil {
		t.Fatal("service change accepted the Admin role")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("service Admin change class = %v; want invalid_input", class)
	}
	badState := service.State("suspended")
	if err := (token.ServiceChange{State: &badState}).Validate(); err == nil {
		t.Fatal("service change accepted an unknown state")
	}

	key := token.TenantKey{
		TenantID:   id.TenantID("tenant-1"),
		Version:    2,
		Ciphertext: []byte("encrypted-derivation-key"),
		WrappedKey: []byte("wrapped-data-key"),
		WrapNonce:  []byte("wrap-nonce-1"),
		KeyRef:     "kms/tenant-key",
		KeyVersion: 2,
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("valid tenant key refused: %v", err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("tenant key did not encode as JSON: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("tenant key is not a JSON object: %v", err)
	}
	for _, member := range []string{"tenant_id", "version", "ciphertext", "wrapped_key", "wrap_nonce", "key_ref", "key_version"} {
		if _, ok := object[member]; !ok {
			t.Errorf("tenant key JSON object is missing member %q; it must map to tenants.derivation_key jsonb", member)
		}
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 0, Ciphertext: []byte("x"), WrappedKey: []byte("y"), WrapNonce: []byte("z"), KeyRef: "kms", KeyVersion: 1}).Validate(); err == nil {
		t.Fatal("tenant key accepted a zero version")
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 1, WrappedKey: []byte("y"), WrapNonce: []byte("z"), KeyRef: "kms", KeyVersion: 1}).Validate(); err == nil {
		t.Fatal("tenant key accepted empty ciphertext")
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 1, Ciphertext: []byte("x"), WrapNonce: []byte("z"), KeyRef: "kms", KeyVersion: 1}).Validate(); err == nil {
		t.Fatal("tenant key accepted an empty wrapped key")
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 1, Ciphertext: []byte("x"), WrappedKey: []byte("y"), KeyRef: "kms", KeyVersion: 1}).Validate(); err == nil {
		t.Fatal("tenant key accepted an empty wrap nonce")
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 1, Ciphertext: []byte("x"), WrappedKey: []byte("y"), WrapNonce: []byte("z"), KeyVersion: 1}).Validate(); err == nil {
		t.Fatal("tenant key accepted an empty key reference")
	}
	if err := (token.TenantKey{TenantID: "tenant-1", Version: 1, Ciphertext: []byte("x"), WrappedKey: []byte("y"), WrapNonce: []byte("z"), KeyRef: "kms"}).Validate(); err == nil {
		t.Fatal("tenant key accepted a zero provider key version")
	}
	write := token.TenantKeyWrite{TenantID: id.TenantID("tenant-1"), ExpectedVersion: 1, Key: key}
	if err := write.Validate(); err != nil {
		t.Fatalf("valid tenant key write refused: %v", err)
	}
	mismatched := token.TenantKeyWrite{TenantID: id.TenantID("tenant-2"), ExpectedVersion: 1, Key: key}
	if err := mismatched.Validate(); err == nil {
		t.Fatal("tenant key write accepted mismatched tenant ids")
	}
	stale := token.TenantKeyWrite{TenantID: id.TenantID("tenant-1"), ExpectedVersion: 2, Key: key}
	if err := stale.Validate(); err == nil {
		t.Fatal("tenant key write accepted a key that does not advance the expected version")
	}

	mutation := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     id.PrincipalID("service-1"),
		ServiceID:       "service-1",
		Kind:            token.MutationRotate,
		ExpectedVersion: 3,
		Credential:      credential,
		Service:         &change,
		Receipt:         sampleReceipt(),
		Audit:           sampleAudit(now),
		Events:          sampleEvents(now),
		Authority:       sampleAuthority(),
		PostCommit:      samplePostCommit(),
	}
	if mutation.Service == nil || mutation.Service.State == nil || *mutation.Service.State != service.StateEnabled {
		t.Fatalf("token mutation unit lost the service change: %+v", mutation)
	}
	mutationType := reflect.TypeOf(mutation)
	for _, field := range []string{"Service", "Credential", "ExpectedVersion", "Authority"} {
		if _, ok := mutationType.FieldByName(field); !ok {
			t.Errorf("token.MutationUnit is missing required field %q", field)
		}
	}
	if _, ok := mutationType.FieldByName("TenantKey"); ok {
		t.Error("token.MutationUnit still carries an ambiguous tenant-key companion")
	}

	executionRequest := token.CredentialRequest{
		Scope:       scope,
		PrincipalID: id.PrincipalID("execution-1"),
		ExecutionID: id.ExecutionID("exec-1"),
	}
	if err := executionRequest.Validate(); err != nil {
		t.Fatalf("valid execution credential request refused: %v", err)
	}
	serviceRequest := token.CredentialRequest{
		Scope:       scope,
		PrincipalID: id.PrincipalID("service-1"),
		ServiceID:   "service-1",
	}
	if err := serviceRequest.Validate(); err != nil {
		t.Fatalf("valid service credential request refused: %v", err)
	}
	both := executionRequest
	both.ServiceID = "service-1"
	if err := both.Validate(); err == nil {
		t.Fatal("credential request accepted both an execution and a service")
	}
	neither := token.CredentialRequest{Scope: scope, PrincipalID: id.PrincipalID("principal-1")}
	if err := neither.Validate(); err == nil {
		t.Fatal("credential request accepted neither target")
	}

	fact := token.CredentialFact{
		Scope:       scope,
		PrincipalID: id.PrincipalID("service-1"),
		ServiceID:   "service-1",
		Enabled:     true,
		Version:     3,
		Credential:  credential,
	}
	if !fact.Enabled || !fact.Credential.Present() {
		t.Fatalf("credential fact lost enablement or credential: %+v", fact)
	}
	if err := (token.TenantKeyRequest{TenantID: id.TenantID("tenant-1")}).Validate(); err != nil {
		t.Fatalf("valid tenant key request refused: %v", err)
	}
	if err := (token.TenantKeyRequest{}).Validate(); err == nil {
		t.Fatal("tenant key request accepted an empty tenant")
	}
}

// TestAuthorizeAdmittedExecutionAndAuthorityContext pins the admitted execution
// context and the operation/required-grant context on authority reads.
func TestAuthorizeAdmittedExecutionAndAuthorityContext(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	executionPrincipal := identity.Principal{
		ID:          id.PrincipalID("execution-1"),
		Kind:        identity.KindExecution,
		Scope:       scope,
		Enabled:     true,
		Version:     1,
		ExecutionID: id.ExecutionID("exec-1"),
	}
	admitted := authorize.AdmittedExecution{
		Principal:   executionPrincipal,
		ExecutionID: id.ExecutionID("exec-1"),
		ActionID:    id.ActionID("action-1"),
		Permission:  authorize.ExecOwnOutputs,
		Deadline:    now.Add(time.Hour),
	}
	if err := admitted.Validate(); err != nil {
		t.Fatalf("valid admitted execution refused: %v", err)
	}
	if admitted.Permission != authorize.ExecOwnOutputs || admitted.Principal.Kind != identity.KindExecution {
		t.Fatalf("admitted execution lost permission or principal: %+v", admitted)
	}
	mismatched := admitted
	mismatched.ExecutionID = id.ExecutionID("exec-2")
	if err := mismatched.Validate(); err == nil {
		t.Fatal("admitted execution accepted a mismatched execution id")
	}
	unknown := admitted
	unknown.Permission = authorize.ExecutionPermission("admin")
	if err := unknown.Validate(); err == nil {
		t.Fatal("admitted execution accepted an unknown permission")
	}
	human := admitted
	human.Principal = identityPrincipal()
	if err := human.Validate(); err == nil {
		t.Fatal("admitted execution accepted a human principal")
	}

	read := authorize.AuthorityRead{
		Operation: "task.start",
		Required: authorize.Requires{
			HumanRoles: []tenancy.Role{tenancy.RoleEngineer},
			Execution:  authorize.ExecNone,
		},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: id.PrincipalID("human-1"),
		TargetID:    "task-1",
	}
	if read.Operation != "task.start" || read.Required.HumanRoles[0] != tenancy.RoleEngineer {
		t.Fatalf("authority read lost operation/required grant context: %+v", read)
	}
	readType := reflect.TypeOf(read)
	for _, field := range []string{"Operation", "Required", "Check", "Scope", "PrincipalID", "ExpectedVersion"} {
		if _, ok := readType.FieldByName(field); !ok {
			t.Errorf("authorize.AuthorityRead is missing required field %q", field)
		}
	}
}

// TestVerificationRecordAndEvidenceSeams pins the binding/evidence read
// seams and the durable verification-record creation unit. The forwarded actor
// credential remains absent from every value.
func TestVerificationRecordAndEvidenceSeams(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	record := verify.Record{
		ID:                verify.VerificationID("verification-1"),
		Scope:             scope,
		CallingService:    service.ServiceID("service-1"),
		BindingID:         resource.BindingID("binding-1"),
		Actor:             identityPrincipal(),
		ExecutionID:       id.ExecutionID("exec-1"),
		ActionID:          id.ActionID("action-1"),
		ResourceID:        resource.ResourceID("resource-1"),
		Operation:         "git.ref.update",
		Subject:           verify.Subject{},
		RequestedApproval: mustAssignment(t, "role/reviewer"),
		Result:            verify.ResultApproved,
		Approved:          true,
		At:                now,
	}
	if record.Scope != scope || record.BindingID != "binding-1" || record.ResourceID != "resource-1" {
		t.Fatalf("verification record lost scope/binding/resource linkage: %+v", record)
	}
	recordType := reflect.TypeOf(record)
	for _, field := range []string{"Scope", "BindingID", "ResourceID", "CallingService", "Actor", "Operation", "Subject", "RequestedApproval", "Result", "Approved", "At"} {
		if _, ok := recordType.FieldByName(field); !ok {
			t.Errorf("verify.Record is missing required field %q", field)
		}
	}
	for index := 0; index < recordType.NumField(); index++ {
		name := recordType.Field(index).Name
		if strings.Contains(strings.ToLower(name), "credential") || strings.Contains(strings.ToLower(name), "token") {
			t.Errorf("verify.Record field %q carries credential material", name)
		}
	}

	unit := verify.RecordUnit{
		Owner:      scope,
		Record:     record,
		Receipt:    sampleReceipt(),
		Audit:      sampleAudit(now),
		Events:     sampleEvents(now),
		Authority:  sampleAuthority(),
		PostCommit: samplePostCommit(),
	}
	if unit.Record.ID != "verification-1" || unit.Owner != scope {
		t.Fatalf("verification record unit lost owner or record: %+v", unit)
	}
	unitType := reflect.TypeOf(unit)
	for _, field := range []string{"Owner", "Record", "Receipt", "Audit", "Events", "Authority", "PostCommit"} {
		if _, ok := unitType.FieldByName(field); !ok {
			t.Errorf("verify.RecordUnit is missing required field %q", field)
		}
	}
	for index := 0; index < unitType.NumField(); index++ {
		name := strings.ToLower(unitType.Field(index).Name)
		if strings.Contains(name, "credential") || strings.Contains(name, "token") {
			t.Errorf("verify.RecordUnit field %q carries credential material", name)
		}
	}

	bindingQuery := verify.BindingQuery{Scope: scope, ResourceID: "resource-1", ServiceID: "service-1"}
	if err := bindingQuery.Validate(); err != nil {
		t.Fatalf("valid verification binding query refused: %v", err)
	}
	if err := (verify.BindingQuery{Scope: scope, ResourceID: "", ServiceID: "service-1"}).Validate(); err == nil {
		t.Fatal("verification binding query accepted an empty resource id")
	}
	if err := (verify.BindingQuery{Scope: scope, ResourceID: "resource-1", ServiceID: "service/one"}).Validate(); err == nil {
		t.Fatal("verification binding query accepted a malformed service id")
	}

	evidenceQuery := verify.EvidenceQuery{
		Scope:       scope,
		ExecutionID: id.ExecutionID("exec-1"),
		Assignment:  mustAssignment(t, "role/reviewer"),
		Operation:   "git.ref.update",
		ResourceID:  resource.ResourceID("resource-1"),
	}
	if err := evidenceQuery.Validate(); err != nil {
		t.Fatalf("valid evidence query refused: %v", err)
	}
	if err := (verify.EvidenceQuery{Scope: scope, ExecutionID: "exec-1", Operation: "git.ref.update", ResourceID: "resource-1"}).Validate(); err == nil {
		t.Fatal("evidence query accepted an invalid assignment")
	}
	if err := (verify.EvidenceQuery{Scope: scope, ExecutionID: "", Assignment: mustAssignment(t, "role/reviewer"), Operation: "git.ref.update", ResourceID: "resource-1"}).Validate(); err == nil {
		t.Fatal("evidence query accepted an empty execution id")
	}

	evidence := verify.Evidence{
		ExecutionID: id.ExecutionID("exec-1"),
		ActionID:    id.ActionID("action-1"),
		Assignment:  mustAssignment(t, "role/reviewer"),
		Operation:   "git.ref.update",
		ResourceID:  resource.ResourceID("resource-1"),
		Subject:     verify.Subject{},
		Decision:    request.DecisionPass,
		ResolvedAt:  now,
	}
	if evidence.Decision != request.DecisionPass || evidence.Assignment.Kind != request.AssignmentRole {
		t.Fatalf("evidence lost decision or assignment: %+v", evidence)
	}
}

// TestResourceBindingLinkageAndMetadataBounds pins the binding read/delete/
// query seam, the required resource-ID linkage and the metadata bounds.
func TestResourceBindingLinkageAndMetadataBounds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	binding := resource.Binding{
		ID:               resource.BindingID("binding-1"),
		ResourceID:       resource.ResourceID("resource-1"),
		Scope:            scope,
		EnforcingService: service.ServiceID("service-1"),
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          1,
		Metadata:         map[string]string{"kind": "repository"},
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("valid linked binding refused: %v", err)
	}
	missingResource := binding
	missingResource.ResourceID = ""
	if err := missingResource.Validate(); err == nil {
		t.Fatal("binding without a resource id accepted")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("binding missing resource id class = %v; want invalid_input", class)
	}
	if reflect.TypeOf(binding.ResourceID) == reflect.TypeOf(binding.ID) {
		t.Fatal("resource and binding ids collapsed into one type")
	}

	if err := resource.ValidateMetadata(nil); err != nil {
		t.Fatalf("nil metadata refused: %v", err)
	}
	tooMany := make(map[string]string, resource.MaxMetadataEntries+1)
	for index := 0; index <= resource.MaxMetadataEntries; index++ {
		tooMany[strings.Repeat("k", 1)+string(rune('a'+index))] = "v"
	}
	if err := resource.ValidateMetadata(tooMany); err == nil {
		t.Fatal("metadata accepted too many entries")
	}
	if err := resource.ValidateMetadata(map[string]string{"": "v"}); err == nil {
		t.Fatal("metadata accepted an empty key")
	}
	if err := resource.ValidateMetadata(map[string]string{strings.Repeat("k", resource.MaxMetadataKeyBytes+1): "v"}); err == nil {
		t.Fatal("metadata accepted an oversized key")
	}
	if err := resource.ValidateMetadata(map[string]string{"key": strings.Repeat("v", resource.MaxMetadataValueBytes+1)}); err == nil {
		t.Fatal("metadata accepted an oversized value")
	}

	read := resource.BindingReadRequest{Scope: scope, BindingID: "binding-1"}
	if err := read.Validate(); err != nil {
		t.Fatalf("valid binding read request refused: %v", err)
	}
	if err := (resource.BindingReadRequest{Scope: scope, BindingID: ""}).Validate(); err == nil {
		t.Fatal("binding read request accepted an empty binding id")
	}

	deleteUnit := resource.BindingDeleteUnit{
		Owner:           scope,
		BindingID:       "binding-1",
		ExpectedVersion: 1,
		At:              now,
		Receipt:         sampleReceipt(),
		Audit:           sampleAudit(now),
		Events:          sampleEvents(now),
		Authority:       sampleAuthority(),
		PostCommit:      samplePostCommit(),
	}
	deleteType := reflect.TypeOf(deleteUnit)
	for _, field := range []string{"Owner", "BindingID", "ExpectedVersion", "At", "Receipt", "Audit", "Events", "Authority", "PostCommit"} {
		if _, ok := deleteType.FieldByName(field); !ok {
			t.Errorf("resource.BindingDeleteUnit is missing required field %q", field)
		}
	}

	query := resource.BindingQuery{Scope: scope, Limit: resource.DefaultBindingPageSize}
	if err := query.Validate(); err != nil {
		t.Fatalf("valid binding query refused: %v", err)
	}
	if err := (resource.BindingQuery{Scope: scope, Limit: 0}).Validate(); err == nil {
		t.Fatal("binding query accepted a zero limit")
	}
	if err := (resource.BindingQuery{Scope: scope, Limit: resource.MaxBindingPageSize + 1}).Validate(); err == nil {
		t.Fatal("binding query accepted an oversized limit")
	}
	if err := (resource.BindingQuery{Scope: scope, ResourceID: "bad/id", Limit: 10}).Validate(); err == nil {
		t.Fatal("binding query accepted a malformed resource id")
	}

	page := resource.BindingPage{Bindings: []resource.Binding{binding}, NextCursor: "cursor-1"}
	if len(page.Bindings) != 1 || page.Bindings[0].ResourceID != "resource-1" {
		t.Fatalf("binding page lost its items: %+v", page)
	}
}

// TestTaskAdmissionAndActivationFacts pins the resolved-action/timeout
// fields on start, reply and request activation so the store can persist the
// complete admission/activation facts without re-deriving policy.
func TestTaskAdmissionAndActivationFacts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	scope := sampleScope()
	container := definition.Container{Image: "registry.example.com/image@sha256:abc", Commands: []string{"run"}}
	requestDefinition := definition.Request{Assignment: "role/reviewer", Subject: definition.Subject{}}

	containerAction := task.AdmittedAction{
		Index:     0,
		Name:      id.ActionName("build"),
		Kind:      definition.ActionKindContainer,
		Timeout:   time.Minute,
		Container: &container,
	}
	if err := containerAction.Validate(); err != nil {
		t.Fatalf("valid container action refused: %v", err)
	}
	requestAction := task.AdmittedAction{
		Index:   1,
		Name:    id.ActionName("review"),
		Kind:    definition.ActionKindRequest,
		Timeout: time.Hour,
		Request: &requestDefinition,
	}
	if err := requestAction.Validate(); err != nil {
		t.Fatalf("valid request action refused: %v", err)
	}
	if err := (task.AdmittedAction{Index: 0, Name: "build", Kind: definition.ActionKindContainer, Timeout: 0, Container: &container}).Validate(); err == nil {
		t.Fatal("admitted action accepted a zero timeout")
	}
	if err := (task.AdmittedAction{Index: 0, Name: "build", Kind: definition.ActionKindContainer, Timeout: time.Minute, Request: &requestDefinition}).Validate(); err == nil {
		t.Fatal("admitted container action accepted a request declaration")
	}
	if err := (task.AdmittedAction{Index: 0, Name: "", Kind: definition.ActionKindContainer, Timeout: time.Minute, Container: &container}).Validate(); err == nil {
		t.Fatal("admitted action accepted an empty name")
	}

	startUnit := task.StartUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		PipelineID:               pipelineIDForTest(),
		ExpectedLifecycleVersion: 3,
		ExpectedPipelineVersion:  2,
		Snapshot:                 sampleSnapshot(t),
		Actions:                  []task.AdmittedAction{containerAction, requestAction},
		Initiator:                id.PrincipalID("human-1"),
		ExecutionPrincipal:       id.PrincipalID("execution-1"),
		NewExecutionID:           id.ExecutionID("exec-1"),
		At:                       now,
		Receipt:                  sampleReceipt(),
		Audit:                    sampleAudit(now),
		Events:                   sampleEvents(now),
		Authority:                sampleAuthority(),
		PostCommit:               samplePostCommit(),
	}
	if startUnit.PipelineID == "" || len(startUnit.Actions) != 2 || startUnit.Actions[1].Timeout != time.Hour {
		t.Fatalf("start unit lost pipeline/actions/timeouts: %+v", startUnit)
	}
	if startUnit.ExecutionPrincipal != "execution-1" || startUnit.At != now {
		t.Fatalf("start unit lost execution principal or time: %+v", startUnit)
	}
	startType := reflect.TypeOf(startUnit)
	for _, field := range []string{"PipelineID", "Actions", "Initiator", "ExecutionPrincipal", "At", "Snapshot", "NewExecutionID", "ExpectedLifecycleVersion", "ExpectedPipelineVersion"} {
		if _, ok := startType.FieldByName(field); !ok {
			t.Errorf("task.StartUnit is missing required field %q", field)
		}
	}

	replyUnit := task.ReplyUnit{
		Owner:                    scope,
		TaskID:                   id.TaskID("task-1"),
		PipelineID:               pipelineIDForTest(),
		ExpectedLifecycleVersion: 4,
		QuestionID:               task.QuestionID("question-1"),
		ExpectedQuestionVersion:  1,
		Assignment:               mustAssignment(t, "role/reviewer"),
		Answer:                   "use abc123",
		Initiator:                id.PrincipalID("human-1"),
		ExpectedPipelineVersion:  2,
		Snapshot:                 sampleSnapshot(t),
		Actions:                  []task.AdmittedAction{containerAction},
		ExecutionPrincipal:       id.PrincipalID("execution-2"),
		NewExecutionID:           id.ExecutionID("exec-2"),
		At:                       now,
		Receipt:                  sampleReceipt(),
		Audit:                    sampleAudit(now),
		Events:                   sampleEvents(now),
		Authority:                sampleAuthority(),
		PostCommit:               samplePostCommit(),
	}
	if replyUnit.PipelineID == "" || len(replyUnit.Actions) != 1 || replyUnit.ExecutionPrincipal != "execution-2" {
		t.Fatalf("reply unit lost pipeline/actions/execution principal: %+v", replyUnit)
	}
	replyType := reflect.TypeOf(replyUnit)
	for _, field := range []string{"PipelineID", "Actions", "ExecutionPrincipal", "At", "Initiator", "NewExecutionID"} {
		if _, ok := replyType.FieldByName(field); !ok {
			t.Errorf("task.ReplyUnit is missing required field %q", field)
		}
	}

	activation := request.ActivationUnit{
		Owner:                    scope,
		ExecutionID:              id.ExecutionID("exec-1"),
		ActionID:                 id.ActionID("action-1"),
		ExpectedExecutionVersion: 1,
		ExpectedActionVersion:    1,
		Subject:                  definition.Subject{},
		Assignment:               mustAssignment(t, "role/reviewer"),
		Timeout:                  time.Hour,
		Deadline:                 now.Add(time.Hour),
		At:                       now,
		StopEvidence:             kernelStopEvidenceForTest(),
		Receipt:                  sampleReceipt(),
		Audit:                    sampleAudit(now),
		Events:                   sampleEvents(now),
		Authority:                sampleAuthority(),
		PostCommit:               samplePostCommit(),
	}
	if activation.Timeout != time.Hour || activation.At != now || !activation.StopEvidence.IsConfirmed() {
		t.Fatalf("activation unit lost timeout/time/stop evidence: %+v", activation)
	}
	activationType := reflect.TypeOf(activation)
	for _, field := range []string{"Commit", "Timeout", "Deadline", "At", "StopEvidence", "Subject", "Assignment"} {
		if _, ok := activationType.FieldByName(field); !ok {
			t.Errorf("request.ActivationUnit is missing required field %q", field)
		}
	}
}

// Compile-only proof that the read and record ports are implementable with
// no concrete store or transport. TestPortsExposeCompleteCommitUnits also
// checks every port takes context.Context first.
var (
	_ token.CredentialReader             = fakeCredentialReader{}
	_ token.TenantKeyReader              = fakeTenantKeyReader{}
	_ identity.PrincipalReader           = fakePrincipalReader{}
	_ service.ServiceReader              = fakeServiceReader{}
	_ resource.BindingDeletePort         = fakeBindingDelete{}
	_ resource.BindingReadPort           = fakeBindingRead{}
	_ resource.BindingQueryPort          = fakeBindingQuery{}
	_ verify.RecordPort                  = fakeVerifyRecord{}
	_ verify.BindingReader               = fakeBindingReader{}
	_ verify.EvidenceReader              = fakeEvidenceReader{}
	_ tenancyadmin.TenantCreatePort      = fakeTenantCreate{}
	_ tenancyadmin.TenantKeyRotationPort = fakeTenantKeyRotation{}
	_ tenancyadmin.ProjectCreatePort     = fakeProjectCreate{}
	_ tenancyadmin.ProjectUpdatePort     = fakeProjectUpdate{}
	_ tenancyadmin.MembershipGrantPort   = fakeMembershipGrant{}
	_ tenancyadmin.MembershipRevokePort  = fakeMembershipRevoke{}
	_ tenancyadmin.MembershipQueryPort   = fakeMembershipQuery{}
)

func pipelineIDForTest() pipeline.PipelineID {
	return pipeline.PipelineID("pipeline-1")
}

func kernelStopEvidenceForTest() kernel.StopEvidence {
	return kernel.ConfirmedStop("container exited")
}
