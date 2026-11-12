package platform_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// Membership union at the platform commit boundary.
//
// A project-scoped membership authority read observes BOTH the exact-project
// row and the tenant-wide row, and the effective grant is their union: a
// project row only ADDS roles and never shadows a tenant-wide grant. The commit
// unit pins both rows by their observed memberships.id identity AND version (or
// a deliberate absence guard), then re-evaluates the effective union on the
// current rows. A concurrent grant, revoke, role change or re-create of either
// row between confirmation and commit is a version_conflict and commits
// nothing. A service principal uses the service-role union, so Admin never
// enters its effective set.

// membershipTargets builds the exact pin set the capability recorded at
// confirmation: the exact-project target first and the tenant-wide target
// second, each present at its observed memberships.id identity and version or
// explicitly absent.
func membershipTargets(scope tenancy.Scope, projectRowID string, projectVersion uint64, tenantRowID string, tenantVersion uint64) []authorize.AuthorityTarget {
	return []authorize.AuthorityTarget{
		{
			Scope:           scope,
			RowID:           projectRowID,
			ExpectedVersion: projectVersion,
			Absent:          projectRowID == "",
		},
		{
			Scope:           tenancy.Scope{TenantID: scope.TenantID},
			RowID:           tenantRowID,
			ExpectedVersion: tenantVersion,
			Absent:          tenantRowID == "",
		},
	}
}

// membershipRead returns the unit's membership authority read. It fails the
// test when the unit carries none, so a renamed check cannot silently pass.
func membershipRead(t *testing.T, reads []authorize.AuthorityRead) *authorize.AuthorityRead {
	t.Helper()
	for index := range reads {
		if reads[index].Check == authorize.CheckMembership {
			return &reads[index]
		}
	}
	t.Fatal("the unit carries no membership authority read")
	return nil
}

// serviceEnabledRead returns the unit's service_enabled authority read.
func serviceEnabledRead(t *testing.T, reads []authorize.AuthorityRead) *authorize.AuthorityRead {
	t.Helper()
	for index := range reads {
		if reads[index].Check == authorize.CheckServiceEnabled {
			return &reads[index]
		}
	}
	t.Fatal("the unit carries no service_enabled authority read")
	return nil
}

// TestIngressMembershipUnionGrantsEitherRow is the shadowing regression: a
// project-scoped membership check grants the union of the tenant-wide row and
// the exact-project row, so a project row can only add roles. A project row
// that replaced the tenant-wide grant would refuse the first vector.
func TestIngressMembershipUnionGrantsEitherRow(t *testing.T) {
	cases := []struct {
		name         string
		tenantRoles  []tenancy.Role
		projectRoles []tenancy.Role
	}{
		{
			name:         "a project reviewer row does not shadow the tenant-wide engineer grant",
			tenantRoles:  []tenancy.Role{tenancy.RoleEngineer},
			projectRoles: []tenancy.Role{tenancy.RoleReviewer},
		},
		{
			name:         "a project engineer row adds to a tenant-wide viewer grant",
			tenantRoles:  []tenancy.Role{tenancy.RoleViewer},
			projectRoles: []tenancy.Role{tenancy.RoleEngineer},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedMembershipRoles(t, s, tenancy.Scope{TenantID: testTenant}, "membership-tenant", testActor, testCase.tenantRoles)
			seedMembershipRoles(t, s, testScope(), "membership-project", testActor, testCase.projectRoles)
			adapter, err := platform.NewIngressAdapter(s, s)
			if err != nil {
				t.Fatalf("NewIngressAdapter: %v", err)
			}
			unit := ingressUnit(t, event.SourceShell, testActor, "")
			if _, err := adapter.Ingest(context.Background(), unit); err != nil {
				t.Fatalf("Ingest with the membership union: %v", err)
			}
			if events := s.Events(testScope()); len(events) != 1 {
				t.Errorf("events = %d, want 1", len(events))
			}
		})
	}
}

// tenantScopedIngressUnit builds the same custom observation at the tenant
// scope.
func tenantScopedIngressUnit(t *testing.T, principal id.PrincipalID) event.IngressUnit {
	t.Helper()
	unit := ingressUnit(t, event.SourceShell, principal, "")
	tenant := tenancy.Scope{TenantID: testTenant}
	unit.Owner = tenant
	unit.Event.Scope = tenant
	unit.Receipt.Key.ProjectID = ""
	unit.Audit[0].Scope = tenant
	unit.Authority = ingressAuthorityReads(t, tenant, event.SourceShell, principal, "")
	return unit
}

// TestIngressRefusesProjectOnlyMembershipAtTenantScope pins the tenant-scoped
// half of the union: a tenant-scoped read observes only the tenant-wide row, so
// a project-only membership grants no tenant-wide right and commits nothing.
func TestIngressRefusesProjectOnlyMembershipAtTenantScope(t *testing.T) {
	s := newStore()
	seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleEngineer})
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := tenantScopedIngressUnit(t, testActor)
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	assertIngressRefusedCommittedNothing(t, s, unit)
}

// assertIngressRefusedCommittedNothing asserts a refused ingest left no durable
// footprint in the unit scope.
func assertIngressRefusedCommittedNothing(t *testing.T, s *memoryStoreAlias, unit event.IngressUnit) {
	t.Helper()
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused ingress committed its receipt")
	}
	if audits := s.Audit(unit.Owner); len(audits) != 0 {
		t.Errorf("audit rows = %d after refusal, want 0", len(audits))
	}
	if events := s.Events(unit.Owner); len(events) != 0 {
		t.Errorf("events = %d after refusal, want 0", len(events))
	}
}

// TestIngressRefusesMembershipPinChangeBetweenConfirmationAndCommit pins the
// both-row guard for the human source path. The capability confirmed a
// two-target pin set; any change of EITHER row inside the commit window is
// refused version_conflict, and identity is checked together with the version
// so a revoked-and-recreated row at a restarted version cannot pass.
func TestIngressRefusesMembershipPinChangeBetweenConfirmationAndCommit(t *testing.T) {
	seedTenant := func(t *testing.T, s *memoryStoreAlias, _ tenancy.Scope) {
		seedMembershipRoles(t, s, tenancy.Scope{TenantID: testTenant}, "membership-tenant", testActor, []tenancy.Role{tenancy.RoleEngineer})
	}
	seedProject := func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope) {
		seedMembershipRoles(t, s, scope, "membership-project", testActor, []tenancy.Role{tenancy.RoleReviewer})
	}
	seedBoth := func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope) {
		seedTenant(t, s, scope)
		seedProject(t, s, scope)
	}
	cases := []struct {
		name   string
		seed   func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope)
		pins   func(scope tenancy.Scope) []authorize.AuthorityTarget
		mutate func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope)
	}{
		{
			name: "project row created after an absent pin",
			seed: seedTenant,
			pins: func(scope tenancy.Scope) []authorize.AuthorityTarget {
				return membershipTargets(scope, "", 0, "membership-tenant", 1)
			},
			mutate: seedProject,
		},
		{
			name: "project row revoked after confirmation",
			seed: seedBoth,
			pins: func(scope tenancy.Scope) []authorize.AuthorityTarget {
				return membershipTargets(scope, "membership-project", 1, "membership-tenant", 1)
			},
			mutate: func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope) {
				deleteRow(t, s, scope, "memberships", "membership-project", 1)
			},
		},
		{
			name: "project row re-created with a new identity at the same version",
			seed: seedBoth,
			pins: func(scope tenancy.Scope) []authorize.AuthorityTarget {
				return membershipTargets(scope, "membership-project", 1, "membership-tenant", 1)
			},
			mutate: func(t *testing.T, s *memoryStoreAlias, scope tenancy.Scope) {
				deleteRow(t, s, scope, "memberships", "membership-project", 1)
				seedMembershipRoles(t, s, scope, "membership-project-2", testActor, []tenancy.Role{tenancy.RoleEngineer})
			},
		},
		{
			name: "tenant-wide row bumped after confirmation",
			seed: seedBoth,
			pins: func(scope tenancy.Scope) []authorize.AuthorityTarget {
				return membershipTargets(scope, "membership-project", 1, "membership-tenant", 1)
			},
			mutate: func(t *testing.T, s *memoryStoreAlias, _ tenancy.Scope) {
				updateVersion(t, s, tenancy.Scope{TenantID: testTenant}, "memberships", "membership-tenant", 1)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			testCase.seed(t, s, testScope())
			adapter, err := platform.NewIngressAdapter(s, s)
			if err != nil {
				t.Fatalf("NewIngressAdapter: %v", err)
			}
			unit := ingressUnit(t, event.SourceShell, testActor, "")
			membershipRead(t, unit.Authority).Targets = testCase.pins(testScope())
			testCase.mutate(t, s, testScope())
			_, err = adapter.Ingest(context.Background(), unit)
			requireClass(t, err, fault.VersionConflict)
			assertIngressRefusedCommittedNothing(t, s, unit)
		})
	}
}

// TestRecordServiceUnionAcceptsEitherScope asserts the service-role union reads
// both service rows: a current tenant-wide grant and a current project grant
// each satisfy the operation, never only the exact-project row.
func TestRecordServiceUnionAcceptsEitherScope(t *testing.T) {
	cases := []struct {
		name  string
		scope tenancy.Scope
	}{
		{name: "tenant-wide service row", scope: tenancy.Scope{TenantID: testTenant}},
		{name: "project service row", scope: testScope()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newStore()
			seedServiceRoles(t, s, testCase.scope, testService, "enabled", []tenancy.Role{tenancy.RoleEngineer})
			seedBinding(t, s, testScope(), testBinding, 7)
			seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
			seedAction(t, s, testScope(), testAction, testExecution, "running", "")
			adapter, err := platform.NewRecordAdapter(s, s)
			if err != nil {
				t.Fatalf("NewRecordAdapter: %v", err)
			}
			if _, err := adapter.Record(context.Background(), recordUnit(t)); err != nil {
				t.Fatalf("Record with the %s: %v", testCase.name, err)
			}
			if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err != nil {
				t.Errorf("verification row was not committed: %v", err)
			}
		})
	}
}

// TestRecordServiceNeverGainsAdmin pins the service variant of the union: Admin
// is never service-assignable, so even a corrupt row that carries it cannot
// satisfy a service requirement naming Admin.
func TestRecordServiceNeverGainsAdmin(t *testing.T) {
	// A corrupt service row carries Admin, which is never service-assignable.
	s := newStore()
	seedServiceRoles(t, s, testScope(), testService, "enabled", []tenancy.Role{tenancy.RoleAdmin})
	seedBinding(t, s, testScope(), testBinding, 7)
	seedExecution(t, s, testScope(), testExecution, testPrincipal, "running")
	seedAction(t, s, testScope(), testAction, testExecution, "running", "")
	adapter, err := platform.NewRecordAdapter(s, s)
	if err != nil {
		t.Fatalf("NewRecordAdapter: %v", err)
	}
	unit := recordUnit(t)
	serviceEnabledRead(t, unit.Authority).Required.ServiceRoles = []tenancy.Role{tenancy.RoleAdmin}
	_, err = adapter.Record(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	assertRecordRefusedCommittedNothing(t, s, unit)
}

// TestRecordRefusesServicePinChangeBetweenConfirmationAndCommit pins the
// service row by identity and version: a bump or a re-create at the same
// version between confirmation and commit is version_conflict.
func TestRecordRefusesServicePinChangeBetweenConfirmationAndCommit(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, s *memoryStoreAlias)
	}{
		{
			name: "service version bumped after confirmation",
			mutate: func(t *testing.T, s *memoryStoreAlias) {
				updateVersion(t, s, testScope(), "services", string(testService), 1)
			},
		},
		{
			name: "service row re-created with a new identity at the same version",
			mutate: func(t *testing.T, s *memoryStoreAlias) {
				deleteRow(t, s, testScope(), "services", string(testService), 1)
				seedServiceRoles(t, s, testScope(), "svc-git-2", "enabled", []tenancy.Role{tenancy.RoleEngineer})
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			s := seedVerificationFixtures(t)
			adapter, err := platform.NewRecordAdapter(s, s)
			if err != nil {
				t.Fatalf("NewRecordAdapter: %v", err)
			}
			unit := recordUnit(t)
			serviceEnabledRead(t, unit.Authority).Targets = membershipTargets(testScope(), string(testService), 1, "", 0)
			testCase.mutate(t, s)
			_, err = adapter.Record(context.Background(), unit)
			requireClass(t, err, fault.VersionConflict)
			assertRecordRefusedCommittedNothing(t, s, unit)
		})
	}
}

// assertRecordRefusedCommittedNothing asserts a refused actor.verify unit left
// no verification row, receipt, audit row or event.
func assertRecordRefusedCommittedNothing(t *testing.T, s *memoryStoreAlias, unit verify.RecordUnit) {
	t.Helper()
	if _, err := s.Read(context.Background(), testScope(), "verifications", testVerification); err == nil {
		t.Error("a refused record persisted its verification row")
	}
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused record committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after refusal, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after refusal, want 0", len(events))
	}
}

// TestReportRefusesServicePinChangeBetweenConfirmationAndCommit asserts the
// verified-report path applies the same service union pin, and that a refusal
// leaves the receipt, audit trail and accepted event untouched.
func TestReportRefusesServicePinChangeBetweenConfirmationAndCommit(t *testing.T) {
	s := newStore()
	seedService(t, s, testScope(), testService, "enabled")
	seedBinding(t, s, testScope(), testBinding, 7)
	seedVerification(t, s, testScope(), testVerification)
	mustInsert(t, s, testScope(), "resources", testResource, map[string]any{
		"kind": "git", "version": uint64(1), "metadata": []byte(`{}`), "created_at": fixedNow,
	})
	adapter, err := platform.NewReportAdapter(s, s)
	if err != nil {
		t.Fatalf("NewReportAdapter: %v", err)
	}
	unit := reportUnit(t)
	serviceEnabledRead(t, unit.Authority).Targets = membershipTargets(testScope(), string(testService), 1, "", 0)
	// The service row moves after the capability confirmed version 1.
	updateVersion(t, s, testScope(), "services", string(testService), 1)
	_, err = adapter.Report(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	if _, found := s.Receipt(unit.Receipt.Key); found {
		t.Error("a refused report committed its receipt")
	}
	if audits := s.Audit(testScope()); len(audits) != 0 {
		t.Errorf("audit rows = %d after refusal, want 0", len(audits))
	}
	if events := s.Events(testScope()); len(events) != 0 {
		t.Errorf("events = %d after refusal, want 0", len(events))
	}
}

// TestIngressMatchingPinsWithInsufficientUnionIsForbidden is the
// forbidden-after-matching-pins vector: the confirmed pins still match the
// current rows, so the refusal cannot come from a pin change; the union on the
// CURRENT rows no longer grants the required role and the commit is forbidden,
// leaving no receipt, audit row or event.
func TestIngressMatchingPinsWithInsufficientUnionIsForbidden(t *testing.T) {
	s := newStore()
	seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleViewer})
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceShell, testActor, "")
	membershipRead(t, unit.Authority).Targets = membershipTargets(testScope(), testMembership, 1, "", 0)
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.Forbidden)
	assertIngressRefusedCommittedNothing(t, s, unit)
}

// TestIngressPinMismatchOutranksInsufficientUnion pins the refusal order: when
// the confirmed pins no longer match AND the current union no longer grants the
// role, the pin change is version_conflict, never forbidden.
func TestIngressPinMismatchOutranksInsufficientUnion(t *testing.T) {
	s := newStore()
	seedMembershipRoles(t, s, testScope(), testMembership, testActor, []tenancy.Role{tenancy.RoleViewer})
	adapter, err := platform.NewIngressAdapter(s, s)
	if err != nil {
		t.Fatalf("NewIngressAdapter: %v", err)
	}
	unit := ingressUnit(t, event.SourceShell, testActor, "")
	// The confirmed project pin names a version the current row no longer has.
	membershipRead(t, unit.Authority).Targets = membershipTargets(testScope(), testMembership, 2, "", 0)
	_, err = adapter.Ingest(context.Background(), unit)
	requireClass(t, err, fault.VersionConflict)
	assertIngressRefusedCommittedNothing(t, s, unit)
}
