package secret_test

// Commit-boundary membership resolution against the row shape the identity
// adapter commits.
//
// The identity adapter allocates a membership row identity as
// membership-<random>: the memberships primary key is global, so it is never
// the principal id, and a tenant-wide grant stores a NULL project id. A secret
// mutation therefore cannot read a membership by id == principal id. These
// proofs drive the real tenancyadmin.Service grant path and apply its commit
// unit with the identity adapter's exact row shape through the neutral store
// write, because a Shell capability test may not import a concrete adapter
// (internal/archtest classifies this package as a capability consumer, and a
// capability may not import an adapter even from a test).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

// membershipRowIDs allocates the identity adapter's membership row identity
// shape: a global membership-<hex> primary key that never equals the principal
// id.
type membershipRowIDs struct {
	next int
}

func (ids *membershipRowIDs) NewRowID() (string, error) {
	ids.next++
	return fmt.Sprintf("membership-%032x", ids.next), nil
}

// adminPorts applies the tenancy administration commit ports to the replay
// store. Only the membership grant and revoke paths are exercised; every other
// port refuses an unexpected call instead of silently succeeding.
type adminPorts struct {
	store *replayStore
	ids   *membershipRowIDs
}

func (ports *adminPorts) GrantMembership(ctx context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	roles, err := json.Marshal(unit.Membership.Roles)
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, fault.Wrap(fault.InternalFailure, "membership roles could not be encoded", err)
	}
	if unit.ExpectedVersion == 0 {
		rowID, err := ports.ids.NewRowID()
		if err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		_, err = ports.store.Execute(ctx, unit.Owner, store.Unit{Writes: []store.Write{{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(unit.Membership.PrincipalID),
				"roles":        roles,
				"version":      int64(unit.Membership.Version),
				"created_at":   unit.At,
			},
		}}})
		if err != nil {
			return tenancyadmin.MembershipGrantResult{}, err
		}
		return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
	}
	rowID, err := ports.membershipRow(ctx, unit.Owner, unit.Membership.PrincipalID)
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	_, err = ports.store.Execute(ctx, unit.Owner, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            rowID,
		ExpectVersion: unit.ExpectedVersion,
		Values:        map[string]any{"roles": roles},
	}}})
	if err != nil {
		return tenancyadmin.MembershipGrantResult{}, err
	}
	return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
}

func (ports *adminPorts) RevokeMembership(ctx context.Context, unit tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
	rowID, err := ports.membershipRow(ctx, unit.Owner, unit.PrincipalID)
	if err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	_, err = ports.store.Execute(ctx, unit.Owner, store.Unit{Writes: []store.Write{{
		Table:         "memberships",
		ID:            rowID,
		Delete:        true,
		ExpectVersion: unit.ExpectedVersion,
	}}})
	if err != nil {
		return tenancyadmin.MembershipRevokeResult{}, err
	}
	return tenancyadmin.MembershipRevokeResult{Version: unit.ExpectedVersion + 1}, nil
}

// membershipRow finds the one scoped membership row of a principal by scanning
// principal_id, never by assuming the row id is the principal id.
func (ports *adminPorts) membershipRow(ctx context.Context, scope tenancy.Scope, principal id.PrincipalID) (string, error) {
	page, err := ports.store.Query(ctx, scope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": string(principal)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   2,
	})
	if err != nil {
		return "", err
	}
	if len(page.Rows) != 1 {
		return "", fault.New(fault.VersionConflict, "the membership to change is missing or duplicated")
	}
	rowID, ok := page.Rows[0].Values["id"].(string)
	if !ok || rowID == "" {
		return "", fault.New(fault.InternalFailure, "the membership row has no usable identity")
	}
	return rowID, nil
}

func (ports *adminPorts) CreateTenant(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
	return tenancyadmin.TenantCreateResult{}, fault.New(fault.InternalFailure, "unexpected tenant create")
}

func (ports *adminPorts) RotateTenantKey(context.Context, tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
	return tenancyadmin.TenantKeyRotationResult{}, fault.New(fault.InternalFailure, "unexpected tenant key rotation")
}

func (ports *adminPorts) CreateProject(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
	return tenancyadmin.ProjectCreateResult{}, fault.New(fault.InternalFailure, "unexpected project create")
}

func (ports *adminPorts) UpdateProject(context.Context, tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
	return tenancyadmin.ProjectUpdateResult{}, fault.New(fault.InternalFailure, "unexpected project update")
}

func (ports *adminPorts) ListMemberships(context.Context, tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
	return tenancyadmin.MembershipPage{}, fault.New(fault.InternalFailure, "unexpected membership list")
}

// newGrantService builds the real tenancy administration service over the
// replay store ports.
func newGrantService(t *testing.T, executor *replayStore, ids *membershipRowIDs) *tenancyadmin.Service {
	t.Helper()
	ports := &adminPorts{store: executor, ids: ids}
	service, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             &fakeClock{now: testNow},
		IDs:               &fakeIDs{},
		TenantCreate:      ports,
		TenantKeyRotation: ports,
		ProjectCreate:     ports,
		ProjectUpdate:     ports,
		MembershipGrant:   ports,
		MembershipRevoke:  ports,
		MembershipQuery:   ports,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

// grantMembership commits one membership through the real tenancy
// administration service. The actor is a tenant Admin, so the grant itself is
// admitted and its unit carries the registry's current-authority read.
func grantMembership(t *testing.T, service *tenancyadmin.Service, owner tenancy.Scope, principal id.PrincipalID, roles ...tenancy.Role) {
	t.Helper()
	actor := authorize.Principal{
		ID:      id.PrincipalID("principal-admin"),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: owner.TenantID},
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
	_, err := service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
		Actor:           actor,
		Membership:      tenancy.Membership{Scope: owner, PrincipalID: principal, Roles: roles},
		ExpectedVersion: 0,
		CallerKey:       "grant-" + string(principal),
		Content: receipt.Content{
			MediaType: "application/json",
			Bytes:     []byte(`{"principal":"` + string(principal) + `"}`),
		},
	})
	if err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
}

// onlyMembershipRowID returns the single committed membership row identity in
// one scope.
func onlyMembershipRowID(t *testing.T, executor *replayStore, scope tenancy.Scope) string {
	t.Helper()
	page, err := executor.Query(context.Background(), scope, store.ScanQuery{
		Table:   "memberships",
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("membership scan: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("membership rows = %d; want 1", len(page.Rows))
	}
	rowID, ok := page.Rows[0].Values["id"].(string)
	if !ok || rowID == "" {
		t.Fatalf("membership row identity = %#v; want a usable id", page.Rows[0].Values["id"])
	}
	return rowID
}

// requireObservedMembershipLock asserts the guard pins the membership row that
// was actually observed (its real row identity, principal equality, observed
// version and tenant-wide flag) plus the ExpectAbsent pin on the unobserved
// sibling scope.
func requireObservedMembershipLock(t *testing.T, unit store.Unit, rowID string, actor id.PrincipalID, version uint64, tenantWide bool) {
	t.Helper()
	if len(unit.Locks) != 2 {
		t.Fatalf("commit unit locks = %+v; want 2 membership union guards", unit.Locks)
	}
	var lock, absent store.Lock
	var wantAbsentProject any
	if tenantWide {
		absent = unit.Locks[0]
		lock = unit.Locks[1]
		wantAbsentProject = "project-a"
	} else {
		lock = unit.Locks[0]
		absent = unit.Locks[1]
		wantAbsentProject = nil
	}
	if lock.Table != "memberships" {
		t.Fatalf("guard table = %q; want memberships", lock.Table)
	}
	if len(lock.IDs) != 1 || lock.IDs[0] != rowID {
		t.Fatalf("guard ids = %v; want the observed membership row %q", lock.IDs, rowID)
	}
	if lock.TenantWide != tenantWide {
		t.Fatalf("guard tenant-wide = %t; want %t", lock.TenantWide, tenantWide)
	}
	if lock.ExpectVersion != version {
		t.Fatalf("guard expected version = %d; want %d", lock.ExpectVersion, version)
	}
	principal, ok := lock.Expect["principal_id"].(string)
	if !ok || principal != string(actor) {
		t.Fatalf("guard expected principal = %#v; want %q", lock.Expect["principal_id"], actor)
	}
	if absent.Table != "memberships" || !absent.ExpectAbsent {
		t.Fatalf("absence guard = %+v; want memberships ExpectAbsent", absent)
	}
	if absent.Key["tenant_id"] != "tenant-a" || absent.Key["project_id"] != wantAbsentProject || absent.Key["principal_id"] != string(actor) {
		t.Fatalf("absence guard key = %#v; want tenant-a / %#v / %s", absent.Key, wantAbsentProject, actor)
	}
}

func TestManagerAuthorizesIdentityAdapterMembershipRow(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	grantMembership(t, newGrantService(t, executor, &membershipRowIDs{}), scope, "principal-bob", tenancy.RoleEngineer)
	rowID := onlyMembershipRowID(t, executor, scope)
	if rowID == "principal-bob" || !strings.HasPrefix(rowID, "membership-") {
		t.Fatalf("membership row id = %q; want the identity adapter's membership-<hex> shape", rowID)
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "first", 0, "identity-insert")
	put.Actor = "principal-bob"
	inserted, err := manager.Put(context.Background(), put)
	if err != nil {
		t.Fatalf("Put with an identity-allocated membership row: %v", err)
	}
	requireObservedMembershipLock(t, executor.lastUnit(t), rowID, "principal-bob", 1, false)

	remove := replayDeleteCommand(scope, "DB_PASSWORD", 1, "identity-delete")
	remove.Actor = "principal-bob"
	if err := manager.Delete(context.Background(), remove); err != nil {
		t.Fatalf("Delete with an identity-allocated membership row: %v", err)
	}
	requireObservedMembershipLock(t, executor.lastUnit(t), rowID, "principal-bob", 1, false)
	if executor.hasRow("secrets", string(inserted.ID)) {
		t.Fatal("Delete left the secret row behind")
	}
}

func TestManagerAuthorizesTenantWideIdentityAdapterMembershipRow(t *testing.T) {
	tenant := tenantScope("tenant-a")
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	grantMembership(t, newGrantService(t, executor, &membershipRowIDs{}), tenant, "principal-bob", tenancy.RoleAdmin)
	rowID := onlyMembershipRowID(t, executor, tenant)
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "first", 0, "tenant-wide-identity-insert")
	put.Actor = "principal-bob"
	if _, err := manager.Put(context.Background(), put); err != nil {
		t.Fatalf("Put with a tenant-wide identity-allocated membership row: %v", err)
	}
	requireObservedMembershipLock(t, executor.lastUnit(t), rowID, "principal-bob", 1, true)
}

func TestManagerRefusesViewerOnlyIdentityAdapterMembership(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	grantMembership(t, newGrantService(t, executor, &membershipRowIDs{}), scope, "principal-bob", tenancy.RoleViewer)
	baseline := executor.executions
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "identity-viewer")
	put.Actor = "principal-bob"
	_, err := manager.Put(context.Background(), put)
	requireClass(t, err, fault.Forbidden)
	if executor.executions != baseline {
		t.Fatal("a viewer-only identity membership reached the commit")
	}
}

func TestManagerRefusesRevokedIdentityAdapterMembershipAtCommit(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	grantMembership(t, newGrantService(t, executor, &membershipRowIDs{}), scope, "principal-bob", tenancy.RoleEngineer)
	rowID := onlyMembershipRowID(t, executor, scope)
	executor.beforeExecute = func() {
		executor.revokeMembershipRow(rowID)
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "identity-revoked")
	put.Actor = "principal-bob"
	_, err := manager.Put(context.Background(), put)
	requireClass(t, err, fault.VersionConflict)
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a revoked identity membership landed the secret row")
	}
	if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
		t.Fatalf("revoked commit persisted receipt=%d audit=%d event=%d; want none",
			executor.receiptCount(), executor.auditCount(), executor.eventCount())
	}
}

func TestManagerRefusesIdentityAdapterMembershipVersionBumpAtCommit(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	grantMembership(t, newGrantService(t, executor, &membershipRowIDs{}), scope, "principal-bob", tenancy.RoleEngineer)
	rowID := onlyMembershipRowID(t, executor, scope)
	executor.beforeExecute = func() {
		executor.setMembershipVersionRow(rowID, 2)
	}
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "identity-version-bump")
	put.Actor = "principal-bob"
	_, err := manager.Put(context.Background(), put)
	requireClass(t, err, fault.VersionConflict)
	if executor.hasRow("secrets", "secret-1") {
		t.Fatal("a changed identity membership landed the secret row")
	}
	if executor.receiptCount() != 0 || executor.auditCount() != 0 || executor.eventCount() != 0 {
		t.Fatalf("changed commit persisted receipt=%d audit=%d event=%d; want none",
			executor.receiptCount(), executor.auditCount(), executor.eventCount())
	}
}

func TestManagerFailsClosedOnDuplicateMembershipRowsInOneScope(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.seedIdentityMembership(scope, "membership-aaaa", "principal-bob", 1, "engineer")
	executor.seedIdentityMembership(scope, "membership-bbbb", "principal-bob", 1, "engineer")
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "duplicate-membership")
	put.Actor = "principal-bob"
	_, err := manager.Put(context.Background(), put)
	requireClass(t, err, fault.InternalFailure)
	if executor.executions != 0 {
		t.Fatal("a duplicated membership still reached the commit")
	}
}

func TestManagerFailsClosedOnMembershipScanFailure(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	executor := newReplayStore()
	executor.scanErr = fault.New(fault.DependencyUnavailable, "membership scan unavailable")
	manager := newReplayManager(t, &fakeClock{now: testNow}, &fakeIDs{}, executor, &fakeCipher{}, executor)

	put := replayPutCommand(scope, "DB_PASSWORD", "value", 0, "scan-failure")
	put.Actor = "principal-bob"
	_, err := manager.Put(context.Background(), put)
	requireClass(t, err, fault.DependencyUnavailable)
	if executor.executions != 0 {
		t.Fatal("a failed membership scan still reached the commit")
	}
}

func TestNewManagerRequiresScanner(t *testing.T) {
	manager, err := secret.NewManager(secret.ManagerConfig{
		InstallationID: "install-1",
		KeyVersion:     secret.KeyVersion(1),
		Clock:          &fakeClock{now: testNow},
		IDs:            &fakeIDs{},
		Executor:       newReplayStore(),
		Cipher:         &fakeCipher{},
		Lookup:         &fakeLookup{rows: map[string]secret.Stored{}},
	})
	requireClass(t, err, fault.InvalidInput)
	if manager != nil {
		t.Fatal("NewManager returned a partially wired manager without a scanner")
	}
}
