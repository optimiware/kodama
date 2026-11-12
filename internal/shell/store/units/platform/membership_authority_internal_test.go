package platform

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// The internal tests below pin the SHAPE of the membership-union locks the
// platform adapter derives: a project-scoped membership authority read pins both
// the exact-project row and the tenant-wide row by identity AND version, and a
// deliberately absent row becomes an absence guard on the memberships
// unique key. They also prove the store refuses a row that appears after the
// union read.

const (
	unionTenant    = id.TenantID("tenant-union")
	unionProject   = id.ProjectID("project-union")
	unionPrincipal = id.PrincipalID("principal-union")
	unionService   = id.PrincipalID("svc-union")
	unionNow       = "2026-02-03T04:05:06Z"
)

func unionScope() tenancy.Scope {
	return tenancy.Scope{TenantID: unionTenant, ProjectID: unionProject}
}

func unionStore(t *testing.T) *memory.Store {
	t.Helper()
	now, err := time.Parse(time.RFC3339, unionNow)
	if err != nil {
		t.Fatalf("parse fixed clock: %v", err)
	}
	return memory.NewWithClock(func() time.Time { return now })
}

func unionInsert(t *testing.T, s *memory.Store, scope tenancy.Scope, table, identity string, values map[string]any) {
	t.Helper()
	if _, err := s.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: table, ID: identity, Insert: true, Values: values}},
	}); err != nil {
		t.Fatalf("seed %s/%s: %v", table, identity, err)
	}
}

func unionMembership(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string, principal id.PrincipalID, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	unionInsert(t, s, scope, "memberships", identity, map[string]any{
		"principal_id": string(principal),
		"roles":        encoded,
		"version":      uint64(1),
		"created_at":   time.Unix(0, 0).UTC(),
	})
}

func unionServiceRow(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.PrincipalID, roles ...tenancy.Role) {
	t.Helper()
	unionServiceRowState(t, s, scope, identity, "enabled", roles...)
}

func unionDisabledServiceRow(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.PrincipalID, roles ...tenancy.Role) {
	t.Helper()
	unionServiceRowState(t, s, scope, identity, "disabled", roles...)
}

func unionServiceRowState(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.PrincipalID, state string, roles ...tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	unionInsert(t, s, scope, "services", string(identity), map[string]any{
		"name":       "git",
		"state":      state,
		"roles":      encoded,
		"version":    uint64(1),
		"created_at": time.Unix(0, 0).UTC(),
	})
}

// locksFor builds the locks one membership authority read contributes.
func locksFor(t *testing.T, s *memory.Store, scope tenancy.Scope, read authorize.AuthorityRead) []store.Lock {
	t.Helper()
	locks, err := authorityLocks(context.Background(), s, s, scope, []authorize.AuthorityRead{read}, authorityFacts{ActorKind: identity.KindHuman})
	if err != nil {
		t.Fatalf("authorityLocks: %v", err)
	}
	return locks
}

// membershipLocksFor is locksFor for the membership check.
func membershipLocksFor(t *testing.T, s *memory.Store, scope tenancy.Scope, principal id.PrincipalID) []store.Lock {
	t.Helper()
	return locksFor(t, s, scope, authorize.AuthorityRead{
		Operation:   "event.publish",
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleEngineer}},
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
	})
}

// TestMembershipUnionLocksPinBothRows asserts the exact lock set of a
// project-scoped union read: one present pin per row, each carrying its
// observed memberships.id and version, with no single shadowing lock.
func TestMembershipUnionLocksPinBothRows(t *testing.T) {
	s := unionStore(t)
	scope := unionScope()
	unionMembership(t, s, tenancy.Scope{TenantID: unionTenant}, "membership-tenant", unionPrincipal, tenancy.RoleEngineer)
	unionMembership(t, s, scope, "membership-project", unionPrincipal, tenancy.RoleReviewer)

	locks := membershipLocksFor(t, s, scope, unionPrincipal)
	if len(locks) != 2 {
		t.Fatalf("locks = %+v, want the exact-project and tenant-wide pins", locks)
	}
	byID := map[string]store.Lock{}
	for _, lock := range locks {
		if lock.Table != membershipsTable {
			t.Errorf("lock table = %q, want %q", lock.Table, membershipsTable)
		}
		if lock.ExpectAbsent {
			t.Errorf("present row pinned as absent: %+v", lock)
		}
		if len(lock.IDs) != 1 {
			t.Fatalf("lock = %+v, want exactly one row identity", lock)
		}
		byID[lock.IDs[0]] = lock
	}
	project, ok := byID["membership-project"]
	if !ok {
		t.Fatalf("locks = %+v, want the exact-project row", locks)
	}
	if project.ExpectVersion != 1 || project.TenantWide {
		t.Errorf("project pin = %+v, want version 1 and no tenant-wide flag", project)
	}
	tenant, ok := byID["membership-tenant"]
	if !ok {
		t.Fatalf("locks = %+v, want the tenant-wide row", locks)
	}
	if tenant.ExpectVersion != 1 || !tenant.TenantWide {
		t.Errorf("tenant-wide pin = %+v, want version 1 addressed as a tenant-wide row", tenant)
	}
}

// TestMembershipUnionPinsAbsentRowByUniqueKey asserts a row that was absent at
// confirmation becomes an absence guard naming the memberships unique key,
// with an explicit NULL project id for the tenant-wide row.
func TestMembershipUnionPinsAbsentRowByUniqueKey(t *testing.T) {
	t.Run("absent project row", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionMembership(t, s, tenancy.Scope{TenantID: unionTenant}, "membership-tenant", unionPrincipal, tenancy.RoleEngineer)
		locks := membershipLocksFor(t, s, scope, unionPrincipal)
		if len(locks) != 2 {
			t.Fatalf("locks = %+v, want a present tenant-wide pin and an absent project guard", locks)
		}
		guard, ok := absenceLock(locks)
		if !ok {
			t.Fatalf("locks = %+v, want an absence guard for the project row", locks)
		}
		want := map[string]any{
			tenantIDColumn:  string(unionTenant),
			projectIDColumn: string(unionProject),
			principalColumn: string(unionPrincipal),
		}
		assertKey(t, guard, want)
	})

	t.Run("absent tenant-wide row uses a null project key", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionMembership(t, s, scope, "membership-project", unionPrincipal, tenancy.RoleEngineer)
		locks := membershipLocksFor(t, s, scope, unionPrincipal)
		if len(locks) != 2 {
			t.Fatalf("locks = %+v, want a present project pin and an absent tenant-wide guard", locks)
		}
		guard, ok := absenceLock(locks)
		if !ok {
			t.Fatalf("locks = %+v, want an absence guard for the tenant-wide row", locks)
		}
		want := map[string]any{
			tenantIDColumn:  string(unionTenant),
			projectIDColumn: nil,
			principalColumn: string(unionPrincipal),
		}
		assertKey(t, guard, want)
	})
}

// TestMembershipUnionAbsenceGuardRefusesARowCreatedBeforeCommit proves the
// absence guard is more than a read: the store refuses the commit when the row
// that was absent at the union read exists by transaction time.
func TestMembershipUnionAbsenceGuardRefusesARowCreatedBeforeCommit(t *testing.T) {
	s := unionStore(t)
	scope := unionScope()
	unionMembership(t, s, tenancy.Scope{TenantID: unionTenant}, "membership-tenant", unionPrincipal, tenancy.RoleEngineer)
	locks := membershipLocksFor(t, s, scope, unionPrincipal)

	// The grant lands after the union read and before the unit commits.
	unionMembership(t, s, scope, "membership-project", unionPrincipal, tenancy.RoleEngineer)
	_, err := s.Execute(context.Background(), scope, store.Unit{Locks: locks})
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("execute error = %v (class %v), want version_conflict", err, class)
	}
}

// TestMembershipUnionTenantScopedReadPinsOnlyTenantWide asserts a tenant-scoped
// read never observes or pins a project row, so a project-only grant is
// forbidden.
func TestMembershipUnionTenantScopedReadPinsOnlyTenantWide(t *testing.T) {
	tenant := tenancy.Scope{TenantID: unionTenant}
	s := unionStore(t)
	unionMembership(t, s, tenant, "membership-tenant", unionPrincipal, tenancy.RoleEngineer)
	unionMembership(t, s, unionScope(), "membership-project", unionPrincipal, tenancy.RoleReviewer)

	locks := membershipLocksFor(t, s, tenant, unionPrincipal)
	if len(locks) != 1 {
		t.Fatalf("locks = %+v, want only the tenant-wide pin", locks)
	}
	if locks[0].IDs[0] != "membership-tenant" || locks[0].ExpectVersion != 1 {
		t.Errorf("tenant-scoped pin = %+v, want the tenant-wide row at version 1", locks[0])
	}

	projectOnly := unionStore(t)
	unionMembership(t, projectOnly, unionScope(), "membership-project", unionPrincipal, tenancy.RoleEngineer)
	_, err := authorityLocks(context.Background(), projectOnly, projectOnly, tenant, []authorize.AuthorityRead{{
		Operation:   "membership.list",
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleEngineer}},
		Check:       authorize.CheckMembership,
		Scope:       tenant,
		PrincipalID: unionPrincipal,
	}}, authorityFacts{ActorKind: identity.KindHuman})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("project-only tenant read error = %v (class %v), want forbidden", err, class)
	}
}

// TestServiceUnionLocksPinTheApplicableRow asserts the service-role union reads
// both service rows and pins the current one, carrying the enabled-state guard.
func TestServiceUnionLocksPinTheApplicableRow(t *testing.T) {
	serviceRead := func(scope tenancy.Scope) authorize.AuthorityRead {
		return authorize.AuthorityRead{
			Operation:   "actor.verify",
			Required:    authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}},
			Check:       authorize.CheckServiceEnabled,
			Scope:       scope,
			PrincipalID: unionService,
		}
	}

	t.Run("tenant-wide row", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionServiceRow(t, s, tenancy.Scope{TenantID: unionTenant}, unionService, tenancy.RoleEngineer)
		locks := locksFor(t, s, scope, serviceRead(scope))
		if len(locks) != 1 {
			t.Fatalf("locks = %+v, want one present service pin", locks)
		}
		if locks[0].Table != servicesTable || locks[0].IDs[0] != string(unionService) {
			t.Errorf("service pin = %+v, want the current service row", locks[0])
		}
		if !locks[0].TenantWide || locks[0].ExpectVersion != 1 {
			t.Errorf("service pin = %+v, want the tenant-wide row at version 1", locks[0])
		}
		if locks[0].Expect[stateColumn] != enabledServiceState {
			t.Errorf("service pin state guard = %v, want %q", locks[0].Expect[stateColumn], enabledServiceState)
		}
	})

	t.Run("project row", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionServiceRow(t, s, scope, unionService, tenancy.RoleEngineer)
		locks := locksFor(t, s, scope, serviceRead(scope))
		if len(locks) != 1 {
			t.Fatalf("locks = %+v, want one present service pin", locks)
		}
		if locks[0].TenantWide {
			t.Errorf("service pin = %+v, want the project row", locks[0])
		}
	})
}

// absenceLock returns the single absence guard in a lock set.
func absenceLock(locks []store.Lock) (store.Lock, bool) {
	for _, lock := range locks {
		if lock.ExpectAbsent {
			return lock, true
		}
	}
	return store.Lock{}, false
}

func assertKey(t *testing.T, lock store.Lock, want map[string]any) {
	t.Helper()
	if lock.Table != membershipsTable {
		t.Errorf("absence guard table = %q, want %q", lock.Table, membershipsTable)
	}
	if len(lock.Key) != len(want) {
		t.Fatalf("absence key = %+v, want %+v", lock.Key, want)
	}
	for column, value := range want {
		got, present := lock.Key[column]
		if !present {
			t.Errorf("absence key is missing %q: %+v", column, lock.Key)
			continue
		}
		if got != value {
			t.Errorf("absence key %q = %#v, want %#v", column, got, value)
		}
	}
}

// unionDeleteRow removes one committed row under its guarded version, so a test
// can re-create the SAME identity at the same version with different content.
func unionDeleteRow(t *testing.T, s *memory.Store, scope tenancy.Scope, table, identity string, version uint64) {
	t.Helper()
	if _, err := s.Execute(context.Background(), scope, store.Unit{
		Writes: []store.Write{{Table: table, ID: identity, Delete: true, ExpectVersion: version}},
	}); err != nil {
		t.Fatalf("delete %s/%s: %v", table, identity, err)
	}
}

// requireUnionConflict asserts the unit was refused with version_conflict.
func requireUnionConflict(t *testing.T, err error) {
	t.Helper()
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("execute error = %v (class %v), want version_conflict", err, class)
	}
}

// TestMembershipUnionRoleGuardRefusesSameVersionRewrite is the
// same-identity, same-version role-rewrite vector. The commit unit guards the
// exact roles bytes the union evaluated together with the row identity and
// version, so re-creating either row under the SAME id and version 1 with
// different roles is version_conflict even though identity and version match.
func TestMembershipUnionRoleGuardRefusesSameVersionRewrite(t *testing.T) {
	t.Run("project row", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionMembership(t, s, scope, "membership-project", unionPrincipal, tenancy.RoleEngineer)
		locks := membershipLocksFor(t, s, scope, unionPrincipal)

		unionDeleteRow(t, s, scope, "memberships", "membership-project", 1)
		unionMembership(t, s, scope, "membership-project", unionPrincipal, tenancy.RoleViewer)
		_, err := s.Execute(context.Background(), scope, store.Unit{Locks: locks})
		requireUnionConflict(t, err)
	})

	t.Run("tenant-wide row", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		tenant := tenancy.Scope{TenantID: unionTenant}
		unionMembership(t, s, tenant, "membership-tenant", unionPrincipal, tenancy.RoleEngineer)
		locks := membershipLocksFor(t, s, scope, unionPrincipal)

		unionDeleteRow(t, s, tenant, "memberships", "membership-tenant", 1)
		unionMembership(t, s, tenant, "membership-tenant", unionPrincipal, tenancy.RoleViewer)
		_, err := s.Execute(context.Background(), scope, store.Unit{Locks: locks})
		requireUnionConflict(t, err)
	})
}

// TestServiceUnionGuardRefusesSameVersionRewrite is the service variant: the
// service row pin guards the exact roles bytes and the enabled state alongside
// the identity and version, so a same-version role rewrite or enablement change
// is version_conflict.
func TestServiceUnionGuardRefusesSameVersionRewrite(t *testing.T) {
	serviceRead := authorize.AuthorityRead{
		Operation:   "actor.verify",
		Required:    authorize.Requires{ServiceRoles: []tenancy.Role{tenancy.RoleEngineer}},
		Check:       authorize.CheckServiceEnabled,
		Scope:       unionScope(),
		PrincipalID: unionService,
	}

	t.Run("roles", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionServiceRow(t, s, scope, unionService, tenancy.RoleEngineer)
		locks := locksFor(t, s, scope, serviceRead)

		unionDeleteRow(t, s, scope, "services", string(unionService), 1)
		unionServiceRow(t, s, scope, unionService, tenancy.RoleReviewer)
		_, err := s.Execute(context.Background(), scope, store.Unit{Locks: locks})
		requireUnionConflict(t, err)
	})

	t.Run("state", func(t *testing.T) {
		s := unionStore(t)
		scope := unionScope()
		unionServiceRow(t, s, scope, unionService, tenancy.RoleEngineer)
		locks := locksFor(t, s, scope, serviceRead)

		unionDeleteRow(t, s, scope, "services", string(unionService), 1)
		unionDisabledServiceRow(t, s, scope, unionService, tenancy.RoleEngineer)
		_, err := s.Execute(context.Background(), scope, store.Unit{Locks: locks})
		requireUnionConflict(t, err)
	})
}

// TestPushServiceAdminRefusesAnExecutionPrincipal proves the conditional Admin
// check is never "not applicable": an execution holds no membership role, so
// the read is a refusal instead of an empty lock set that lets the unit commit.
func TestPushServiceAdminRefusesAnExecutionPrincipal(t *testing.T) {
	s := unionStore(t)
	scope := unionScope()
	now, err := time.Parse(time.RFC3339, unionNow)
	if err != nil {
		t.Fatalf("parse fixed clock: %v", err)
	}
	unionInsert(t, s, scope, "executions", "execution-union", map[string]any{
		"principal_id": string(unionPrincipal),
		"state":        "running",
		"version":      uint64(1),
		"yaml_sha256":  []byte{1, 2, 3},
		"snapshot":     []byte(`{}`),
		"created_at":   now,
	})
	read := authorize.AuthorityRead{
		Operation:   "pipeline.update",
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleEngineer}, Execution: authorize.ExecOwnTaskContent},
		Check:       authorize.CheckPushServiceAdmin,
		Scope:       scope,
		PrincipalID: unionPrincipal,
	}
	_, err = authorityLocks(context.Background(), s, s, scope, []authorize.AuthorityRead{read}, authorityFacts{ActorKind: identity.KindHuman})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("push-admin read for an execution principal = %v; want forbidden", err)
	}

	// A plain membership read for the same execution stays not applicable.
	read.Check = authorize.CheckMembership
	locks, err := authorityLocks(context.Background(), s, s, scope, []authorize.AuthorityRead{read}, authorityFacts{ActorKind: identity.KindHuman})
	if err != nil || len(locks) != 0 {
		t.Fatalf("membership read for an execution principal = %v, %v; want no locks and no error", locks, err)
	}
}
