package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

func requiredRoles(human, service []tenancy.Role) authorize.Requires {
	return authorize.Requires{HumanRoles: human, ServiceRoles: service}
}

func TestAuthorityReaderReturnsOneFactPerReadInOrder(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-human", 2, tenancy.RoleAdmin)
	reads := []authorize.AuthorityRead{
		{Check: authorize.CheckMembership, Scope: scope, PrincipalID: "principal-human", Required: requiredRoles([]tenancy.Role{tenancy.RoleAdmin}, nil)},
		{Check: authorize.CheckPrincipalEnabled, Scope: scope, PrincipalID: "principal-human"},
	}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %d; want one per read", len(facts))
	}
	if facts[0].Check != authorize.CheckMembership || facts[1].Check != authorize.CheckPrincipalEnabled {
		t.Fatalf("fact order = %v; want the read order", facts)
	}
	if !facts[0].Allowed || !facts[1].Allowed {
		t.Fatalf("facts = %+v; want both allowed", facts)
	}
	if facts[0].Version != 2 {
		t.Fatalf("membership version = %d; want 2", facts[0].Version)
	}
}

func TestAuthorityReaderMembershipRoles(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-viewer", "principal-human", 1, tenancy.RoleViewer)
	allowed, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{{
		Check: authorize.CheckMembership, Scope: scope, PrincipalID: "principal-human",
		Required: requiredRoles([]tenancy.Role{tenancy.RoleViewer}, nil),
	}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !allowed[0].Allowed {
		t.Fatalf("viewer read = %+v; want allowed for the viewer grant", allowed[0])
	}
	denied, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{{
		Check: authorize.CheckMembership, Scope: scope, PrincipalID: "principal-human",
		Required: requiredRoles([]tenancy.Role{tenancy.RoleAdmin}, nil),
	}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if denied[0].Allowed {
		t.Fatalf("admin read = %+v; want denied for a viewer membership", denied[0])
	}
}

func TestAuthorityReaderServiceEnabledAndExecutionRunning(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedService(scope, "service-a", 1, "enabled", nil, nil)
	h.seedService(scope, "service-b", 1, "disabled", nil, nil)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	h.seedExecution(scope, "execution-b", "principal-exec-2", 1, "succeeded", nil, nil)
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin", TargetID: "service-a"},
		{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin", TargetID: "service-b"},
		{Check: authorize.CheckExecutionRunning, Scope: scope, PrincipalID: "principal-admin", TargetID: "execution-a"},
		{Check: authorize.CheckExecutionRunning, Scope: scope, PrincipalID: "principal-admin", TargetID: "execution-b"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed || facts[1].Allowed {
		t.Fatalf("service facts = %+v; want enabled allowed and disabled denied", facts[:2])
	}
	if !facts[2].Allowed || facts[3].Allowed {
		t.Fatalf("execution facts = %+v; want running allowed and terminal denied", facts[2:])
	}
}

func TestAuthorityReaderVersionedChecks(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seed(scope, "resource_bindings", "binding-a", map[string]any{
		"resource_id": "resource-a", "service_id": "service-a", "origins": []byte(`[]`),
		"operations": []byte(`[]`), "reportable_events": []byte(`[]`), "metadata": []byte(`{}`),
		"version": int64(3), "created_at": testNow,
	})
	h.seed(scope, "tasks", "task-a", map[string]any{
		"content_version": int64(1), "lifecycle_version": int64(4), "state": "open",
		"initiator_id": "principal-human", "title": "Task a", "created_at": testNow,
	})
	h.seed(scope, "pipelines", "pipeline-a", map[string]any{
		"name": "pipeline-a", "yaml": []byte("steps: []"), "yaml_sha256": []byte("sha256"),
		"version": int64(2), "created_at": testNow,
	})
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a", ExpectedVersion: 3},
		{Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a", ExpectedVersion: 2},
		{Check: authorize.CheckTaskVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "task-a", ExpectedVersion: 4},
		{Check: authorize.CheckPipelineVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "pipeline-a", ExpectedVersion: 2},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed || facts[1].Allowed {
		t.Fatalf("binding facts = %+v; want the exact version allowed and the stale version denied", facts[:2])
	}
	if !facts[2].Allowed || !facts[3].Allowed {
		t.Fatalf("task/pipeline facts = %+v; want allowed", facts[2:])
	}
	if facts[2].Version != 4 || facts[3].Version != 2 {
		t.Fatalf("observed versions = %d,%d; want 4,2", facts[2].Version, facts[3].Version)
	}
}

func TestAuthorityReaderActionDeadline(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	seedAction := func(id string, deadline any) {
		h.seed(scope, "actions", id, map[string]any{
			"execution_id": "execution-" + id, "index": int64(0), "name": id, "kind": "container",
			"state": "running", "version": int64(1), "timeout_ms": int64(1000),
			"staged_outputs_set": false, "deadline": deadline,
		})
	}
	seedAction("action-live", testNow.Add(time.Minute))
	seedAction("action-past", testNow.Add(-time.Minute))
	seedAction("action-none", nil)
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckActionDeadline, Scope: scope, PrincipalID: "principal-admin", TargetID: "action-live"},
		{Check: authorize.CheckActionDeadline, Scope: scope, PrincipalID: "principal-admin", TargetID: "action-past"},
		{Check: authorize.CheckActionDeadline, Scope: scope, PrincipalID: "principal-admin", TargetID: "action-none"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed || facts[1].Allowed || facts[2].Allowed {
		t.Fatalf("deadline facts = %+v; want only the live deadline allowed", facts)
	}
}

func TestAuthorityReaderPushServiceAdmin(t *testing.T) {
	h := newHarness(t)
	scope := tenantScope("tenant-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-viewer", "principal-viewer", 1, tenancy.RoleViewer)
	facts, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckPushServiceAdmin, Scope: scope, PrincipalID: "principal-admin"},
		{Check: authorize.CheckPushServiceAdmin, Scope: scope, PrincipalID: "principal-viewer"},
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed || facts[1].Allowed {
		t.Fatalf("push admin facts = %+v; want only admin allowed", facts)
	}
}

func TestAuthorityReaderMissingTargetIsNotFound(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	_, err := h.adapter.Read(context.Background(), []authorize.AuthorityRead{
		{Check: authorize.CheckResourceOwnership, Scope: scope, PrincipalID: "principal-admin", TargetID: "resource-missing"},
	})
	requireFaultClass(t, err, fault.NotFound)
}

func TestAuthorityGuardRefusesRevokedMembershipAtCommit(t *testing.T) {
	h := newHarness(t)
	scope := tenantScope("tenant-a")
	h.seed(scope, "tenants", "tenant-a", map[string]any{
		"name": "Tenant a", "version": int64(1),
		"derivation_key": []byte(`{"tenant_id":"tenant-a"}`), "created_at": testNow,
	})
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	unit := tenancyadmin.TenantKeyRotationUnit{
		Tenant:          tenancy.Tenant{ID: "tenant-a", Name: "Tenant a", Version: 2},
		Key:             token.TenantKeyWrite{TenantID: "tenant-a", ExpectedVersion: 1, Key: testTenantKey("tenant-a", 2)},
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "tenant.rotate_key", "caller-revoked", tenancyadmin.TenantKeyRotationResult{TenantVersion: 2, KeyVersion: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "tenant.rotate_key")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "tenant-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table: "memberships", ID: "membership-admin", Delete: true, ExpectVersion: 1,
	}}}); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
	_, err := h.adapter.RotateTenantKey(context.Background(), unit)
	requireFaultClass(t, err, fault.Forbidden)
	row, found := h.snapshot(scope, "tenants", "tenant-a")
	if !found {
		t.Fatal("tenant row disappeared")
	}
	if got := mustInt(t, row, "version"); got != 1 {
		t.Fatalf("tenant version = %d; want the untouched 1", got)
	}
	h.requireNoPublications(scope)
}

func TestAuthorityGuardRefusesDisabledServiceAtCommit(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, "enabled", nil, nil)
	unit := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "service-a",
		ServiceID:       "service-a",
		Kind:            token.MutationIssue,
		ExpectedVersion: 2,
		Credential:      token.Credential{Nonce: testNonce(0x44), Expiry: testExpiry},
		Receipt:         h.receipt(scope, "principal-admin", "service.credential.issue", "caller-disabled", token.MutationResult{State: token.StateCurrent, Version: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.credential.issue")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 3)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin", TargetID: "service-a"},
		},
	}
	if _, err := h.store.Execute(context.Background(), scope, store.Unit{Writes: []store.Write{{
		Table: "services", ID: "service-a", ExpectVersion: 1, Values: map[string]any{"state": "disabled"},
	}}}); err != nil {
		t.Fatalf("disable service: %v", err)
	}
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.TokenNonce != nil {
		t.Fatalf("service = %+v; a disabled service received a credential", read)
	}
}

func TestAuthorityGuardRequiresVersionedTargets(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedResource(scope, "resource-a")
	unit := h.registerUnit(scope, "principal-admin", "binding-a", "resource-a")
	unit.Authority = append(unit.Authority, authorize.AuthorityRead{
		Check: authorize.CheckBindingVersion, Scope: scope, PrincipalID: "principal-admin", TargetID: "binding-a",
	})
	_, err := h.adapter.Apply(context.Background(), unit)
	requireFaultClass(t, err, fault.InvalidInput)
	h.requireNoRows(scope, "resource_bindings", "binding-a")
	h.requireNoPublications(scope)
}

// TestAuthorityGuardDeadlineExpiredAtCommit proves the transaction-time
// deadline guard: a unit whose authority read names an action whose deadline is
// not strictly after the transaction clock is refused with deadline_expired and
// applies nothing.
func TestAuthorityGuardDeadlineExpiredAtCommit(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seed(scope, "actions", "action-late", map[string]any{
		"execution_id": "execution-a", "index": int64(0), "name": "action-late", "kind": "container",
		"state": "running", "version": int64(1), "timeout_ms": int64(1000),
		"staged_outputs_set": false, "deadline": testNow.Add(-time.Minute),
	})
	unit := h.membershipGrantWithAuthority(scope, "caller-late", authorize.AuthorityRead{
		Check: authorize.CheckActionDeadline, Scope: scope, PrincipalID: "principal-admin", TargetID: "action-late",
	})
	_, err := h.adapter.GrantMembership(context.Background(), unit)
	requireFaultClass(t, err, fault.DeadlineExpired)
	page, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	for _, membership := range page.Memberships {
		if membership.PrincipalID == "principal-b" {
			t.Fatalf("memberships = %+v; want nothing committed for principal-b", page.Memberships)
		}
	}
	if got := len(h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want 0", got)
	}
}

// TestAuthorityGuardLiveDeadlineCommits proves a live deadline guard still
// commits the unit normally.
func TestAuthorityGuardLiveDeadlineCommits(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seed(scope, "actions", "action-live", map[string]any{
		"execution_id": "execution-a", "index": int64(0), "name": "action-live", "kind": "container",
		"state": "running", "version": int64(1), "timeout_ms": int64(1000),
		"staged_outputs_set": false, "deadline": testNow.Add(time.Minute),
	})
	unit := h.membershipGrantWithAuthority(scope, "caller-live", authorize.AuthorityRead{
		Check: authorize.CheckActionDeadline, Scope: scope, PrincipalID: "principal-admin", TargetID: "action-live",
	})
	result, err := h.adapter.GrantMembership(context.Background(), unit)
	if err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	if result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want version 1", result)
	}
}

// TestAuthorityGuardRefusesDemotedMembershipAtCommit proves the commit guard
// re-evaluates the required role the caller recorded instead of only locking
// the membership row it happens to find: a membership demoted from Admin to
// Viewer after the capability's Confirm cannot authorize the commit, and the
// refusal persists nothing.
func TestAuthorityGuardRefusesDemotedMembershipAtCommit(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 2, tenancy.RoleViewer)
	unit := h.membershipGrantWithAuthority(scope, "caller-demoted", authorize.AuthorityRead{
		Check: authorize.CheckPrincipalEnabled, Scope: scope, PrincipalID: "principal-admin",
	})
	unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "principal-admin")}
	_, err := h.adapter.GrantMembership(context.Background(), unit)
	requireFaultClass(t, err, fault.Forbidden)
	page, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	for _, membership := range page.Memberships {
		if membership.PrincipalID == "principal-b" {
			t.Fatalf("memberships = %+v; want nothing committed for principal-b", page.Memberships)
		}
	}
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the refused grant left a receipt")
	}
	h.requireNoPublications(scope)
}

// TestAuthorityGuardMembershipVersionBumpConflicts proves the membership guard
// locks the exact version it evaluated: a membership change between that
// evaluation and the transaction is a version_conflict for both a membership
// write port and a service write port, and nothing is persisted.
func TestAuthorityGuardMembershipVersionBumpConflicts(t *testing.T) {
	t.Run("membership write port", func(t *testing.T) {
		h := newBumpingHarness(t, "memberships", "membership-admin", 1, map[string]any{
			"roles": rolesJSON(t, tenancy.RoleAdmin),
		})
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		unit := h.membershipGrantWithAuthority(scope, "caller-bump-grant", authorize.AuthorityRead{
			Check: authorize.CheckPrincipalEnabled, Scope: scope, PrincipalID: "principal-admin",
		})
		unit.Authority = []authorize.AuthorityRead{membershipRead(scope, "principal-admin")}
		_, err := h.adapter.GrantMembership(context.Background(), unit)
		requireFaultClass(t, err, fault.VersionConflict)
		if _, found := h.store.Receipt(unit.Receipt.Key); found {
			t.Fatal("the refused grant left a receipt")
		}
		h.requireNoPublications(scope)
	})
	t.Run("service write port", func(t *testing.T) {
		h := newBumpingHarness(t, "memberships", "membership-admin", 1, map[string]any{
			"roles": rolesJSON(t, tenancy.RoleAdmin),
		})
		scope := projectScope("tenant-a", "project-a")
		h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
		h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
		unit := service.StatusUnit{
			Owner:           scope,
			ServiceID:       "service-a",
			ExpectedVersion: 1,
			State:           service.StateDisabled,
			RevokeAll:       &service.RevokeAll{},
			Actor:           "principal-admin",
			At:              testNow,
			Receipt:         h.receipt(scope, "principal-admin", "service.disable", "caller-bump-status", service.StatusResult{State: service.StateDisabled, Version: 2}),
			Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.disable")},
			Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
			Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
		}
		_, err := h.adapter.SetStatus(context.Background(), unit)
		requireFaultClass(t, err, fault.VersionConflict)
		read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
		if err != nil {
			t.Fatalf("ReadService: %v", err)
		}
		if read.State != service.StateEnabled || read.Version != 1 {
			t.Fatalf("service = %+v; want the untouched enabled version 1", read)
		}
		if _, found := h.store.Receipt(unit.Receipt.Key); found {
			t.Fatal("the refused status change left a receipt")
		}
		h.requireNoPublications(scope)
	})
}

// TestServiceEnabledActingPrincipal covers the service_enabled semantics: with
// no explicit target the check applies to the acting principal. A human actor
// is not applicable (the membership check still gates it), an enabled service
// is allowed, a disabled service is denied, and a principal that resolves to
// nothing is refused.
func TestServiceEnabledActingPrincipal(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-disabled", 1, string(service.StateDisabled), nil, nil)
	reads := []authorize.AuthorityRead{
		{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin"},
		{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "service-disabled"},
		{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-missing"},
	}
	facts, err := h.adapter.Read(context.Background(), reads)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !facts[0].Allowed {
		t.Fatalf("human fact = %+v; want allowed (not applicable)", facts[0])
	}
	if facts[1].Allowed {
		t.Fatalf("disabled service fact = %+v; want denied", facts[1])
	}
	if facts[2].Allowed {
		t.Fatalf("missing principal fact = %+v; want denied", facts[2])
	}

	refused := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "service-disabled",
		ServiceID:       "service-disabled",
		Kind:            token.MutationRevoke,
		ExpectedVersion: 1,
		Receipt:         h.receipt(scope, "service-disabled", "service.credential.revoke", "caller-disabled-actor", token.MutationResult{State: token.StateAbsent, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "service-disabled", "service.credential.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "service-disabled", "service-disabled", 2)},
		Authority:       []authorize.AuthorityRead{reads[1]},
	}
	_, err = h.adapter.Mutate(context.Background(), refused)
	requireFaultClass(t, err, fault.Forbidden)
	if _, found := h.store.Receipt(refused.Receipt.Key); found {
		t.Fatal("the disabled service actor committed a credential change")
	}

	missing := refused
	missing.Authority = []authorize.AuthorityRead{reads[2]}
	missing.Receipt = h.receipt(scope, "service-disabled", "service.credential.revoke", "caller-missing-actor", token.MutationResult{State: token.StateAbsent, Version: 2})
	_, err = h.adapter.Mutate(context.Background(), missing)
	requireFaultClass(t, err, fault.Forbidden)
}

// TestServiceEnabledActorCommits proves an enabled service principal satisfies
// both the membership read (through its service roles) and the acting
// service_enabled read, and commits the guarded service row.
func TestServiceEnabledActorCommits(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	unit := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "service-a",
		ServiceID:       "service-a",
		Kind:            token.MutationIssue,
		ExpectedVersion: 1,
		Credential:      token.Credential{Nonce: testNonce(0x77), Expiry: testExpiry},
		Receipt:         h.receipt(scope, "service-a", "service.credential.issue", "caller-service-actor", token.MutationResult{State: token.StateCurrent, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "service-a", "service.credential.issue")},
		Events:          []event.Envelope{h.envelope(scope, "service-a", "service-a", 2)},
		Authority: []authorize.AuthorityRead{
			{Check: authorize.CheckMembership, Scope: scope, PrincipalID: "service-a", Required: authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}}},
			{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "service-a"},
		},
	}
	result, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if result.Version != 2 || result.Replayed {
		t.Fatalf("result = %+v; want version 2", result)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.Version != 2 || read.TokenNonce == nil {
		t.Fatalf("service = %+v; want version 2 with the issued pair", read)
	}
}

// TestServiceAdminUnitsCommitWithHumanActor proves the capability-emitted
// [membership, service_enabled] reads with a human Admin actor commit the
// service administration and credential units in the real composition.
func TestServiceAdminUnitsCommitWithHumanActor(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	humanReads := func() []authorize.AuthorityRead {
		return []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin"},
		}
	}
	update := service.UpdateUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 1,
		ExpectedState:   service.StateEnabled,
		Service:         service.Service{ID: "service-a", Scope: scope, State: service.StateEnabled, Roles: []tenancy.Role{tenancy.RoleReviewer}, Version: 2},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.update", "caller-human-update", service.UpdateResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.update")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       humanReads(),
	}
	if _, err := h.adapter.Update(context.Background(), update); err != nil {
		t.Fatalf("service update with a human actor: %v", err)
	}
	disable := service.StatusUnit{
		Owner:           scope,
		ServiceID:       "service-a",
		ExpectedVersion: 2,
		State:           service.StateDisabled,
		RevokeAll:       &service.RevokeAll{},
		Actor:           "principal-admin",
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "service.disable", "caller-human-disable", service.StatusResult{State: service.StateDisabled, Version: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.disable")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 3)},
		Authority:       humanReads(),
	}
	if _, err := h.adapter.SetStatus(context.Background(), disable); err != nil {
		t.Fatalf("service disable with a human actor: %v", err)
	}
	enable := disable
	enable.ExpectedVersion = 3
	enable.State = service.StateEnabled
	enable.RevokeAll = nil
	enable.Receipt = h.receipt(scope, "principal-admin", "service.enable", "caller-human-enable", service.StatusResult{State: service.StateEnabled, Version: 4})
	enable.Audit = []event.Audit{h.audit(scope, "principal-admin", "service.enable")}
	enable.Events = []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 4)}
	if _, err := h.adapter.SetStatus(context.Background(), enable); err != nil {
		t.Fatalf("service enable with a human actor: %v", err)
	}
	issue := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "service-a",
		ServiceID:       "service-a",
		Kind:            token.MutationIssue,
		ExpectedVersion: 4,
		Credential:      token.Credential{Nonce: testNonce(0x66), Expiry: testExpiry},
		Receipt:         h.receipt(scope, "principal-admin", "service.credential.issue", "caller-human-issue", token.MutationResult{State: token.StateCurrent, Version: 5}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.credential.issue")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 5)},
		Authority:       humanReads(),
	}
	if _, err := h.adapter.Mutate(context.Background(), issue); err != nil {
		t.Fatalf("credential issue with a human actor: %v", err)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateEnabled || read.Version != 5 || read.TokenNonce == nil {
		t.Fatalf("service = %+v; want enabled version 5 with the issued pair", read)
	}
}

// membershipGrantWithAuthority builds one project-scoped first grant whose
// current-authority reads include the supplied extra check.
func (h *harness) membershipGrantWithAuthority(scope tenancy.Scope, callerKey string, extra authorize.AuthorityRead) tenancyadmin.MembershipGrantUnit {
	h.t.Helper()
	return tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 1},
		ExpectedVersion: 0,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.grant", callerKey, tenancyadmin.MembershipGrantResult{Version: 1}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 1)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin"), extra},
	}
}
