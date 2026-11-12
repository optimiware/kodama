package secret_test

// Commit-boundary membership proofs for secret writes and deletes.
//
// A previously calculated authorization is not durable permission. Every
// secret mutation therefore carries a membership row lock in its store.Unit:
// the acting principal's current scoped membership row must still exist, name
// that principal and be at the version observed before the commit. A revoke or
// role change between authorization and commit refuses the whole unit, so no
// secret row, receipt, audit row or event publication intent lands.

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testMembershipsTable    = "memberships"
	testMembershipPrincipal = "principal_id"
)

// requireMembershipLock asserts the exact commit-boundary guards every
// project-scoped secret mutation by a project-only human must carry: a present
// lock on the project membership row plus an ExpectAbsent pin on the
// tenant-wide membership slot.
func requireMembershipLock(t *testing.T, unit store.Unit, actor id.PrincipalID, version uint64) {
	t.Helper()
	requireMembershipLockShape(t, unit, actor, version, false)
}

// requireTenantWideMembershipLock asserts the tenant-wide membership guards on
// a project-scoped mutation: an ExpectAbsent pin on the project membership slot
// first and a TenantWide present lock on the NULL-project row second.
func requireTenantWideMembershipLock(t *testing.T, unit store.Unit, actor id.PrincipalID, version uint64) {
	t.Helper()
	requireMembershipLockShape(t, unit, actor, version, true)
}

func requireMembershipLockShape(t *testing.T, unit store.Unit, actor id.PrincipalID, version uint64, tenantWide bool) {
	t.Helper()
	if len(unit.Locks) != 2 {
		t.Fatalf("commit unit locks = %+v; want 2 membership union guards (project + tenant-wide)", unit.Locks)
	}
	var presentLock, absentLock store.Lock
	var wantAbsentProject any
	if tenantWide {
		absentLock = unit.Locks[0]
		presentLock = unit.Locks[1]
		wantAbsentProject = "project-a"
	} else {
		presentLock = unit.Locks[0]
		absentLock = unit.Locks[1]
		wantAbsentProject = nil
	}
	if presentLock.Table != testMembershipsTable {
		t.Fatalf("guard table = %q; want %q", presentLock.Table, testMembershipsTable)
	}
	if presentLock.ExpectAbsent {
		t.Fatalf("present guard unexpectedly set ExpectAbsent: %+v", presentLock)
	}
	if len(presentLock.IDs) != 1 || presentLock.IDs[0] != string(actor) {
		t.Fatalf("guard ids = %v; want the acting principal %q", presentLock.IDs, actor)
	}
	if presentLock.TenantWide != tenantWide {
		t.Fatalf("guard tenant-wide = %t; want %t", presentLock.TenantWide, tenantWide)
	}
	if presentLock.ExpectVersion != version {
		t.Fatalf("guard expected version = %d; want %d", presentLock.ExpectVersion, version)
	}
	principal, ok := presentLock.Expect[testMembershipPrincipal].(string)
	if !ok || principal != string(actor) {
		t.Fatalf("guard expected principal = %#v; want %q", presentLock.Expect[testMembershipPrincipal], actor)
	}
	if absentLock.Table != testMembershipsTable || !absentLock.ExpectAbsent {
		t.Fatalf("absence guard = %+v; want memberships ExpectAbsent", absentLock)
	}
	if absentLock.Key["tenant_id"] != "tenant-a" || absentLock.Key["project_id"] != wantAbsentProject || absentLock.Key["principal_id"] != string(actor) {
		t.Fatalf("absence guard key = %#v; want tenant-a / %#v / %s", absentLock.Key, wantAbsentProject, actor)
	}
}

// requireServiceLock asserts the commit-boundary guard for a service actor: one
// guarded lock on the service-account row with its observed version and enabled
// state.
func requireServiceLock(t *testing.T, unit store.Unit, serviceID id.PrincipalID, version uint64, tenantWide bool) {
	t.Helper()
	if len(unit.Locks) != 1 {
		t.Fatalf("commit unit locks = %+v; want exactly one service guard", unit.Locks)
	}
	lock := unit.Locks[0]
	if lock.Table != "services" {
		t.Fatalf("guard table = %q; want services", lock.Table)
	}
	if len(lock.IDs) != 1 || lock.IDs[0] != string(serviceID) {
		t.Fatalf("guard ids = %v; want the acting service %q", lock.IDs, serviceID)
	}
	if lock.TenantWide != tenantWide {
		t.Fatalf("guard tenant-wide = %t; want %t", lock.TenantWide, tenantWide)
	}
	if lock.ExpectVersion != version {
		t.Fatalf("guard expected version = %d; want %d", lock.ExpectVersion, version)
	}
	state, ok := lock.Expect["state"].(string)
	if !ok || state != "enabled" {
		t.Fatalf("guard expected state = %#v; want enabled", lock.Expect["state"])
	}
}

func TestManagerPutInsertCarriesMembershipCommitGuard(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 7)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "guard-insert")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	requireMembershipLock(t, executor.lastUnit(t), "principal-admin", 7)
}

func TestManagerPutReplaceCarriesMembershipCommitGuard(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 3)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "guard-replace-insert")); err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "second", 1, "guard-replace")); err != nil {
		t.Fatalf("replace Put: %v", err)
	}
	requireMembershipLock(t, executor.lastUnit(t), "principal-admin", 3)
}

func TestManagerDeleteCarriesMembershipCommitGuard(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 2)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	if _, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "guard-delete-insert")); err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	if err := manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "guard-delete")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	requireMembershipLock(t, executor.lastUnit(t), "principal-admin", 2)
}

func TestManagerPutRefusesRevokedMembershipAtCommit(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	executor.beforeExecute = func() {
		executor.revokeMembership("principal-admin")
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "revoked-insert"))
	requireClass(t, err, fault.VersionConflict)
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a revoked membership landed the secret row")
	}
	if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
		t.Fatalf("revoked commit persisted receipt=%d audit=%d event=%d; want none",
			executor.receiptCount(), executor.auditCount(), executor.eventCount())
	}
}

func TestManagerDeleteRefusesRevokedMembershipAtCommit(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "revoked-delete-insert"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	receipts, audits, events := executor.receiptCount(), executor.auditCount(), executor.eventCount()
	executor.beforeExecute = func() {
		executor.revokeMembership("principal-admin")
	}

	err = manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "revoked-delete"))
	requireClass(t, err, fault.VersionConflict)
	if !executor.hasRow("secrets", string(inserted.ID)) {
		t.Fatal("a revoked membership landed the secret delete")
	}
	if executor.receiptCount() != receipts || executor.auditCount() != audits || executor.eventCount() != events {
		t.Fatalf("revoked delete persisted receipt=%d audit=%d event=%d; want %d/%d/%d",
			executor.receiptCount(), executor.auditCount(), executor.eventCount(), receipts, audits, events)
	}
}

func TestManagerPutRefusesChangedMembershipVersionAtCommit(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	executor.beforeExecute = func() {
		executor.setMembershipVersion("principal-admin", 2)
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "changed-role-insert"))
	requireClass(t, err, fault.VersionConflict)
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a changed membership version landed the secret row")
	}
	if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
		t.Fatalf("changed membership persisted receipt=%d audit=%d event=%d; want none",
			executor.receiptCount(), executor.auditCount(), executor.eventCount())
	}
}

func TestManagerPutRefusesMembershipRowForAnotherPrincipal(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.rows[testMembershipsTable+"/principal-admin"] = map[string]any{
		"id":           "principal-admin",
		"tenant_id":    string(scope.TenantID),
		"project_id":   string(scope.ProjectID),
		"principal_id": "principal-other",
		"roles":        []byte(`["engineer"]`),
		"version":      int64(1),
		"created_at":   testNow,
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "other-principal-insert"))
	requireClass(t, err, fault.Forbidden)
	if executor.executions != 0 {
		t.Fatal("a mismatched membership row reached the commit")
	}
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a mismatched membership row landed the secret row")
	}
}

func TestManagerPutRefusesMembershipWithoutSecretRole(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	executor.rows[testMembershipsTable+"/principal-admin"]["roles"] = []byte(`["viewer"]`)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "viewer-insert"))
	requireClass(t, err, fault.Forbidden)
	if executor.executions != 0 {
		t.Fatal("a membership without the secret role reached the commit")
	}
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a membership without the secret role landed the secret row")
	}
}

func TestManagerDeleteRefusesMembershipWithoutSecretRole(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 1)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "viewer-delete-insert"))
	if err != nil {
		t.Fatalf("insert Put: %v", err)
	}
	receipts, audits, events := executor.receiptCount(), executor.auditCount(), executor.eventCount()
	executor.rows[testMembershipsTable+"/principal-admin"]["roles"] = []byte(`["viewer"]`)

	err = manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "viewer-delete"))
	requireClass(t, err, fault.Forbidden)
	if !executor.hasRow("secrets", string(inserted.ID)) {
		t.Fatal("a membership without the secret role landed the secret delete")
	}
	if executor.receiptCount() != receipts || executor.auditCount() != audits || executor.eventCount() != events {
		t.Fatalf("forbidden delete persisted receipt=%d audit=%d event=%d; want %d/%d/%d",
			executor.receiptCount(), executor.auditCount(), executor.eventCount(), receipts, audits, events)
	}
}

func TestManagerPutFailsClosedOnMembershipReadFailure(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := &fakeExecutor{membershipErr: fault.New(fault.DependencyUnavailable, "membership store unavailable")}
	manager := newManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, &fakeLookup{rows: map[string]secret.Stored{}})

	_, err := manager.Put(context.Background(), testPutCommand(scope, "DB_PASSWORD", "value", 0))
	requireClass(t, err, fault.DependencyUnavailable)
	if len(executor.callSnapshot()) != 0 {
		t.Fatal("a failed membership read still reached the commit")
	}
}

func TestManagerPutReplayKeepsCurrentMembershipGuard(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedMembership(scope, "principal-admin", 4)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)
	command := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "guard-replay")

	first, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	requireMembershipLock(t, executor.lastUnit(t), "principal-admin", 4)

	retry, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("retried Put: %v", err)
	}
	requireMetadataEqual(t, retry, first)
	requireMembershipLock(t, executor.lastUnit(t), "principal-admin", 4)
	if executor.replays != 1 {
		t.Fatalf("receipt replays = %d; want 1", executor.replays)
	}
	if _, ok := executor.snapshot(scope, "secret-2"); ok {
		t.Fatal("retried Put committed a second secret row")
	}
}

// TestManagerTenantWideMembershipGrantsProjectSecretWrites proves a
// tenant-wide-only human grant is not permanently refused: the guard falls
// back to the NULL-project membership row and pins it with TenantWide instead
// of requiring a project row.
func TestManagerTenantWideMembershipGrantsProjectSecretWrites(t *testing.T) {
	cases := []struct {
		name    string
		roles   string
		version uint64
	}{
		{name: "engineer", roles: `["engineer"]`, version: 5},
		{name: "admin", roles: `["admin"]`, version: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := projectScope("tenant-a", "project-a")
			executor := newReplayStore()
			executor.seedTenantMembership(scope, "principal-admin", tc.version, tc.roles)
			manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

			inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "first", 0, "tenant-wide-insert"))
			if err != nil {
				t.Fatalf("tenant-wide Put: %v", err)
			}
			requireTenantWideMembershipLock(t, executor.lastUnit(t), "principal-admin", tc.version)

			if err := manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "tenant-wide-delete")); err != nil {
				t.Fatalf("tenant-wide Delete: %v", err)
			}
			requireTenantWideMembershipLock(t, executor.lastUnit(t), "principal-admin", tc.version)
			if executor.hasRow("secrets", string(inserted.ID)) {
				t.Fatal("tenant-wide delete left the secret row behind")
			}
		})
	}
}

// TestManagerRefusesChangedTenantWideMembershipAtCommit proves the fallback row
// is guarded, not just read: a revoke or version change between the guard read
// and the commit refuses the whole unit with no secret, receipt, audit or event.
func TestManagerRefusesChangedTenantWideMembershipAtCommit(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*replayStore, id.PrincipalID)
	}{
		{name: "revoked", mutate: func(executor *replayStore, actor id.PrincipalID) { executor.revokeMembership(actor) }},
		{name: "changed version", mutate: func(executor *replayStore, actor id.PrincipalID) { executor.setMembershipVersion(actor, 7) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := projectScope("tenant-a", "project-a")
			executor := newReplayStore()
			executor.seedTenantMembership(scope, "principal-admin", 6, `["engineer"]`)
			executor.beforeExecute = func() { tc.mutate(executor, "principal-admin") }
			manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

			_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "tenant-wide-changed"))
			requireClass(t, err, fault.VersionConflict)
			if executor.hasRow("secrets", "secret-1") {
				t.Fatal("a changed tenant-wide membership landed the secret row")
			}
			if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
				t.Fatalf("changed tenant-wide membership persisted receipt=%d audit=%d event=%d; want none",
					executor.receiptCount(), executor.auditCount(), executor.eventCount())
			}
		})
	}
}

// TestManagerFailsClosedWithoutGrantingMembership proves an actor with neither
// a project nor a tenant-wide membership is refused before any commit.
func TestManagerFailsClosedWithoutGrantingMembership(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "no-membership"))
	requireClass(t, err, fault.Forbidden)
	if executor.executions != 0 {
		t.Fatal("an unresolvable membership still reached the commit")
	}
}

// TestManagerServiceActorGuardsServiceRow proves a registry-permitted service
// actor is guarded by its enabled service row and its observed version instead
// of a membership row it never has.
func TestManagerServiceActorGuardsServiceRow(t *testing.T) {
	cases := []struct {
		name       string
		scope      func() tenancy.Scope
		tenantWide bool
		version    uint64
	}{
		{name: "project service", scope: func() tenancy.Scope { return projectScope("tenant-a", "project-a") }, version: 4},
		{name: "tenant-wide service", scope: func() tenancy.Scope { return tenantScope("tenant-a") }, tenantWide: true, version: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.scope()
			executor := newReplayStore()
			executor.seedService(scope, "service-writer", tc.version, "enabled", `["engineer"]`)
			manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

			put := replayPutCommand(projectScope("tenant-a", "project-a"), "DB_PASSWORD", "first", 0, "service-insert")
			put.Actor = "service-writer"
			put.ActorKind = identity.KindService
			inserted, err := manager.Put(context.Background(), put)
			if err != nil {
				t.Fatalf("service Put: %v", err)
			}
			requireServiceLock(t, executor.lastUnit(t), "service-writer", tc.version, tc.tenantWide)

			remove := replayDeleteCommand(projectScope("tenant-a", "project-a"), "DB_PASSWORD", 1, "service-delete")
			remove.Actor = "service-writer"
			remove.ActorKind = identity.KindService
			if err := manager.Delete(context.Background(), remove); err != nil {
				t.Fatalf("service Delete: %v", err)
			}
			requireServiceLock(t, executor.lastUnit(t), "service-writer", tc.version, tc.tenantWide)
			if executor.hasRow("secrets", string(inserted.ID)) {
				t.Fatal("service delete left the secret row behind")
			}
		})
	}
}

// TestManagerRefusesChangedServiceAuthorityAtCommit proves a service disabled
// or role-narrowed between the guard read and the commit refuses the whole unit
// with no secret, receipt, audit or event.
func TestManagerRefusesChangedServiceAuthorityAtCommit(t *testing.T) {
	cases := []struct {
		name    string
		state   string
		roles   string
		version uint64
	}{
		{name: "disabled", state: "disabled", roles: `["engineer"]`, version: 5},
		{name: "role narrowed", state: "enabled", roles: `["viewer"]`, version: 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := projectScope("tenant-a", "project-a")
			executor := newReplayStore()
			executor.seedService(scope, "service-writer", 4, "enabled", `["engineer"]`)
			executor.beforeExecute = func() {
				executor.setServiceAuthority("service-writer", tc.state, tc.roles, tc.version)
			}
			manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

			put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "service-changed")
			put.Actor = "service-writer"
			put.ActorKind = identity.KindService
			_, err := manager.Put(context.Background(), put)
			requireClass(t, err, fault.VersionConflict)
			if executor.hasRow("secrets", "secret-1") {
				t.Fatal("a changed service authority landed the secret row")
			}
			if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
				t.Fatalf("changed service authority persisted receipt=%d audit=%d event=%d; want none",
					executor.receiptCount(), executor.auditCount(), executor.eventCount())
			}
		})
	}
}

// TestManagerRefusesWrongKindGrants proves the commit boundary re-evaluates the
// actor kind's exact role set: a service cannot borrow a human-only role and a
// viewer grant of either kind cannot write secrets.
func TestManagerRefusesWrongKindGrants(t *testing.T) {
	t.Run("service viewer", func(t *testing.T) {
		scope := projectScope("tenant-a", "project-a")
		executor := newReplayStore()
		executor.seedService(scope, "service-writer", 2, "enabled", `["viewer"]`)
		manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

		put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "service-viewer")
		put.Actor = "service-writer"
		put.ActorKind = identity.KindService
		_, err := manager.Put(context.Background(), put)
		requireClass(t, err, fault.Forbidden)
		if executor.executions != 0 {
			t.Fatal("a viewer service reached the commit")
		}
	})

	t.Run("service human-only admin", func(t *testing.T) {
		scope := projectScope("tenant-a", "project-a")
		executor := newReplayStore()
		// A service row carrying Admin is refused by the exact service role set;
		// unioning the human roles would incorrectly allow it.
		executor.seedService(scope, "service-writer", 2, "enabled", `["admin"]`)
		manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

		put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "service-admin")
		put.Actor = "service-writer"
		put.ActorKind = identity.KindService
		_, err := manager.Put(context.Background(), put)
		requireClass(t, err, fault.Forbidden)
		if executor.executions != 0 {
			t.Fatal("a service with only a human role reached the commit")
		}
	})

	t.Run("human viewer", func(t *testing.T) {
		scope := projectScope("tenant-a", "project-a")
		executor := newReplayStore()
		executor.seedMembership(scope, "principal-admin", 1)
		executor.rows[testMembershipsTable+"/principal-admin"]["roles"] = []byte(`["viewer"]`)
		manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

		_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "human-viewer"))
		requireClass(t, err, fault.Forbidden)
		if executor.executions != 0 {
			t.Fatal("a viewer human reached the commit")
		}
	})

	t.Run("execution actor kind", func(t *testing.T) {
		scope := projectScope("tenant-a", "project-a")
		executor := newReplayStore()
		manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

		put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "execution-kind")
		put.ActorKind = identity.KindExecution
		_, err := manager.Put(context.Background(), put)
		requireClass(t, err, fault.InvalidInput)
		if executor.executions != 0 {
			t.Fatal("an execution actor reached the commit")
		}
	})
}

// TestManagerServicePutReplayStaysReceiptFirst proves a service actor keeps the
// receipt-first retry behavior: an equal retry replays the committed metadata
// and never commits a second secret row or bypasses the service guard.
func TestManagerServicePutReplayStaysReceiptFirst(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedService(scope, "service-writer", 3, "enabled", `["engineer"]`)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)
	command := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "service-replay")
	command.Actor = "service-writer"
	command.ActorKind = identity.KindService

	first, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	requireServiceLock(t, executor.lastUnit(t), "service-writer", 3, false)

	retry, err := manager.Put(context.Background(), command)
	if err != nil {
		t.Fatalf("retried Put: %v", err)
	}
	requireMetadataEqual(t, retry, first)
	requireServiceLock(t, executor.lastUnit(t), "service-writer", 3, false)
	if executor.replays != 1 {
		t.Fatalf("receipt replays = %d; want 1", executor.replays)
	}
	if _, ok := executor.snapshot(scope, "secret-2"); ok {
		t.Fatal("retried service Put committed a second secret row")
	}
}

// TestManagerTenantAdminPlusProjectViewerUnionGrantsSecretMutations proves the
// membership union: a project Viewer row never shadows a tenant-wide Admin row,
// and the commit unit locks both observed membership rows.
func TestManagerTenantAdminPlusProjectViewerUnionGrantsSecretMutations(t *testing.T) {
	tenant := tenantScope("tenant-a")
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedIdentityMembership(tenant, "membership-tenant-admin", "principal-admin", 3, "admin")
	executor.seedIdentityMembership(scope, "membership-project-viewer", "principal-admin", 5, "viewer")
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	inserted, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "union-value", 0, "union-insert"))
	if err != nil {
		t.Fatalf("union Put: %v", err)
	}
	unit := executor.lastUnit(t)
	if len(unit.Locks) != 2 {
		t.Fatalf("union locks = %+v; want 2 present locks", unit.Locks)
	}
	if len(unit.Locks[0].IDs) != 1 || unit.Locks[0].IDs[0] != "membership-project-viewer" || unit.Locks[0].ExpectVersion != 5 || unit.Locks[0].TenantWide {
		t.Fatalf("project lock = %+v; want membership-project-viewer v5 project-scoped", unit.Locks[0])
	}
	if len(unit.Locks[1].IDs) != 1 || unit.Locks[1].IDs[0] != "membership-tenant-admin" || unit.Locks[1].ExpectVersion != 3 || !unit.Locks[1].TenantWide {
		t.Fatalf("tenant-wide lock = %+v; want membership-tenant-admin v3 tenant-wide", unit.Locks[1])
	}

	if err := manager.Delete(context.Background(), replayDeleteCommand(scope, "DB_PASSWORD", 1, "union-delete")); err != nil {
		t.Fatalf("union Delete: %v", err)
	}
	if executor.hasRow("secrets", string(inserted.ID)) {
		t.Fatal("union Delete left the secret row behind")
	}
}

// TestManagerRefusesConcurrentSiblingMembershipGrantOnAbsentPin proves the
// ExpectAbsent guard: if the unobserved sibling membership slot is filled
// between guard evaluation and commit, the commit fails with version_conflict.
func TestManagerRefusesConcurrentSiblingMembershipGrantOnAbsentPin(t *testing.T) {
	tenant := tenantScope("tenant-a")
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedIdentityMembership(tenant, "membership-tenant-admin", "principal-admin", 1, "admin")
	executor.beforeExecute = func() {
		executor.seedIdentityMembership(scope, "membership-concurrent-project", "principal-admin", 1, "viewer")
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	_, err := manager.Put(context.Background(), replayPutCommand(scope, "DB_PASSWORD", "value", 0, "absent-pin-race"))
	requireClass(t, err, fault.VersionConflict)
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("concurrent sibling membership grant landed the secret row")
	}
}
