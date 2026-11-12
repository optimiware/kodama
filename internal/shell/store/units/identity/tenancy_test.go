package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

func testTenantKey(tenant string, version uint64) token.TenantKey {
	return token.TenantKey{
		TenantID:   id.TenantID(tenant),
		Version:    version,
		Ciphertext: []byte("ciphertext"),
		WrappedKey: []byte("wrapped-key"),
		WrapNonce:  []byte("wrap-nonce"),
		KeyRef:     "kms/tenant-key",
		KeyVersion: 1,
	}
}

func rolesJSON(t *testing.T, roles ...tenancy.Role) []byte {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal roles: %v", err)
	}
	return encoded
}

func principalEnabledRead(scope tenancy.Scope, principal id.PrincipalID) authorize.AuthorityRead {
	return authorize.AuthorityRead{Check: authorize.CheckPrincipalEnabled, Scope: scope, PrincipalID: principal}
}

func membershipRead(scope tenancy.Scope, principal id.PrincipalID) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Check:       authorize.CheckMembership,
		Scope:       scope,
		PrincipalID: principal,
		Required:    authorize.Requires{HumanRoles: []tenancy.Role{tenancy.RoleAdmin}},
	}
}

func (h *harness) seedMembership(scope tenancy.Scope, rowID, principal string, version uint64, roles ...tenancy.Role) {
	h.t.Helper()
	h.seed(scope, "memberships", rowID, map[string]any{
		"principal_id": principal,
		"roles":        rolesJSON(h.t, roles...),
		"version":      int64(version),
		"created_at":   testNow,
	})
}

func (h *harness) tenantCreateUnit(tenant, actor string) tenancyadmin.TenantCreateUnit {
	scope := tenantScope(tenant)
	return tenancyadmin.TenantCreateUnit{
		Tenant:    tenancy.Tenant{ID: id.TenantID(tenant), Name: "Tenant " + tenant, Version: 1},
		Key:       testTenantKey(tenant, 1),
		At:        testNow,
		Receipt:   h.receipt(scope, id.PrincipalID(actor), "tenant.create", "caller-"+tenant, tenancyadmin.TenantCreateResult{TenantID: id.TenantID(tenant), Version: 1}),
		Audit:     []event.Audit{h.audit(scope, id.PrincipalID(actor), "tenant.create")},
		Events:    []event.Envelope{h.envelope(scope, id.PrincipalID(actor), tenant, 1)},
		Authority: []authorize.AuthorityRead{principalEnabledRead(scope, id.PrincipalID(actor))},
	}
}

func TestCreateTenantCommitsKeyAndPublications(t *testing.T) {
	h := newHarness(t)
	unit := h.tenantCreateUnit("tenant-a", "principal-admin")
	result, err := h.adapter.CreateTenant(context.Background(), unit)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if result.Replayed || result.TenantID != "tenant-a" || result.Version != 1 {
		t.Fatalf("result = %+v; want tenant-a version 1 not replayed", result)
	}
	row, found := h.snapshot(tenantScope("tenant-a"), "tenants", "tenant-a")
	if !found {
		t.Fatal("the tenant row was not committed")
	}
	stored := testTenantKey("tenant-a", 1)
	if err := json.Unmarshal(mustBytes(t, row, "derivation_key"), &stored); err != nil {
		t.Fatalf("derivation_key is not the structured key: %v", err)
	}
	if stored.KeyRef != "kms/tenant-key" || stored.Version != 1 {
		t.Fatalf("stored derivation key = %+v; want the committed key", stored)
	}
	if got := mustInt(t, row, "version"); got != 1 {
		t.Fatalf("tenant version = %d; want 1", got)
	}
	key, err := h.adapter.ReadTenantKey(context.Background(), token.TenantKeyRequest{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("ReadTenantKey: %v", err)
	}
	if key.KeyRef != stored.KeyRef || key.Version != stored.Version {
		t.Fatalf("read key = %+v; want the committed key", key)
	}
	if _, found := h.store.Receipt(unit.Receipt.Key); !found {
		t.Fatal("the receipt was not committed with the tenant")
	}
	if got := len(h.store.Audit(tenantScope("tenant-a"))); got != 1 {
		t.Fatalf("audit rows = %d; want 1", got)
	}
	if got := len(h.store.Events(tenantScope("tenant-a"))); got != 1 {
		t.Fatalf("event rows = %d; want 1", got)
	}
}

func TestCreateTenantReplayReturnsOriginalResult(t *testing.T) {
	h := newHarness(t)
	unit := h.tenantCreateUnit("tenant-a", "principal-admin")
	first, err := h.adapter.CreateTenant(context.Background(), unit)
	if err != nil {
		t.Fatalf("first CreateTenant: %v", err)
	}
	second, err := h.adapter.CreateTenant(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed CreateTenant: %v", err)
	}
	if !second.Replayed {
		t.Fatal("the equal receipt did not replay")
	}
	first.Replayed = true
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replayed result = %+v; want %+v", second, first)
	}
	if got := len(h.store.Events(tenantScope("tenant-a"))); got != 1 {
		t.Fatalf("event rows after replay = %d; want 1", got)
	}
	if got := len(h.store.Audit(tenantScope("tenant-a"))); got != 1 {
		t.Fatalf("audit rows after replay = %d; want 1", got)
	}
}

func TestCreateTenantChangedContentRefused(t *testing.T) {
	h := newHarness(t)
	unit := h.tenantCreateUnit("tenant-a", "principal-admin")
	if _, err := h.adapter.CreateTenant(context.Background(), unit); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	changed := unit
	changed.Receipt = h.receipt(tenantScope("tenant-a"), "principal-admin", "tenant.create", "caller-tenant-a", tenancyadmin.TenantCreateResult{TenantID: "tenant-a", Version: 1})
	changed.Receipt.ContentHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	changed.Receipt.Key = unit.Receipt.Key
	_, err := h.adapter.CreateTenant(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
}

func TestCreateTenantRollbackLeavesNothing(t *testing.T) {
	h := newHarness(t)
	unit := h.tenantCreateUnit("tenant-a", "principal-admin")
	h.store.FailNextCommit(errors.New("injected commit failure"))
	if _, err := h.adapter.CreateTenant(context.Background(), unit); err == nil {
		t.Fatal("CreateTenant succeeded despite the injected commit failure")
	}
	h.requireNoRows(tenantScope("tenant-a"), "tenants", "tenant-a")
	h.requireNoPublications(tenantScope("tenant-a"))
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the failed unit left a receipt")
	}
}

func TestRotateTenantKeyAdvancesBothVersions(t *testing.T) {
	h := newHarness(t)
	h.seed(tenantScope("tenant-a"), "tenants", "tenant-a", map[string]any{
		"name":           "Tenant a",
		"version":        int64(1),
		"derivation_key": rolesJSON(t, tenancy.RoleAdmin),
		"created_at":     testNow,
	})
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	scope := tenantScope("tenant-a")
	next := testTenantKey("tenant-a", 2)
	unit := tenancyadmin.TenantKeyRotationUnit{
		Tenant:          tenancy.Tenant{ID: "tenant-a", Name: "Tenant a", Version: 2},
		Key:             token.TenantKeyWrite{TenantID: "tenant-a", ExpectedVersion: 1, Key: next},
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "tenant.rotate_key", "caller-rotate", tenancyadmin.TenantKeyRotationResult{TenantVersion: 2, KeyVersion: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "tenant.rotate_key")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "tenant-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin"), principalEnabledRead(scope, "principal-admin")},
	}
	result, err := h.adapter.RotateTenantKey(context.Background(), unit)
	if err != nil {
		t.Fatalf("RotateTenantKey: %v", err)
	}
	if result.TenantVersion != 2 || result.KeyVersion != 2 || result.Replayed {
		t.Fatalf("result = %+v; want tenant 2 key 2 not replayed", result)
	}
	row, found := h.snapshot(scope, "tenants", "tenant-a")
	if !found {
		t.Fatal("tenant row is missing after rotation")
	}
	if got := mustInt(t, row, "version"); got != 2 {
		t.Fatalf("tenant version = %d; want 2", got)
	}
	key, err := h.adapter.ReadTenantKey(context.Background(), token.TenantKeyRequest{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("ReadTenantKey: %v", err)
	}
	if key.Version != 2 {
		t.Fatalf("key version = %d; want 2", key.Version)
	}
}

func TestRotateTenantKeyStaleVersionLeavesNoPartialState(t *testing.T) {
	h := newHarness(t)
	h.seed(tenantScope("tenant-a"), "tenants", "tenant-a", map[string]any{
		"name":           "Tenant a",
		"version":        int64(1),
		"derivation_key": rolesJSON(t, tenancy.RoleAdmin),
		"created_at":     testNow,
	})
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	scope := tenantScope("tenant-a")
	unit := tenancyadmin.TenantKeyRotationUnit{
		Tenant:          tenancy.Tenant{ID: "tenant-a", Name: "Tenant a", Version: 3},
		Key:             token.TenantKeyWrite{TenantID: "tenant-a", ExpectedVersion: 2, Key: testTenantKey("tenant-a", 3)},
		ExpectedVersion: 2,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "tenant.rotate_key", "caller-rotate", tenancyadmin.TenantKeyRotationResult{TenantVersion: 3, KeyVersion: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "tenant.rotate_key")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "tenant-a", 3)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.RotateTenantKey(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	row, found := h.snapshot(scope, "tenants", "tenant-a")
	if !found {
		t.Fatal("tenant row disappeared")
	}
	if got := mustInt(t, row, "version"); got != 1 {
		t.Fatalf("tenant version = %d; want the untouched 1", got)
	}
	h.requireNoPublications(scope)
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the refused rotation left a receipt")
	}
}

func TestCreateProjectAndUpdateProject(t *testing.T) {
	h := newHarness(t)
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	scope := tenantScope("tenant-a")
	create := tenancyadmin.ProjectCreateUnit{
		Project:   tenancy.Project{ID: "project-a", TenantID: "tenant-a", Name: "Project a", Version: 1},
		At:        testNow,
		Receipt:   h.receipt(scope, "principal-admin", "project.create", "caller-create", tenancyadmin.ProjectCreateResult{ProjectID: "project-a", Version: 1}),
		Audit:     []event.Audit{h.audit(scope, "principal-admin", "project.create")},
		Events:    []event.Envelope{h.envelope(scope, "principal-admin", "project-a", 1)},
		Authority: []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	if _, err := h.adapter.CreateProject(context.Background(), create); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	update := tenancyadmin.ProjectUpdateUnit{
		Owner:           scope,
		ProjectID:       "project-a",
		ExpectedVersion: 1,
		Project:         tenancy.Project{ID: "project-a", TenantID: "tenant-a", Name: "Renamed", Version: 2},
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "project.update", "caller-update", tenancyadmin.ProjectUpdateResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "project.update")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "project-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.UpdateProject(context.Background(), update)
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if result.Version != 2 || result.Replayed {
		t.Fatalf("update result = %+v; want version 2", result)
	}
	row, found := h.snapshot(scope, "projects", "project-a")
	if !found {
		t.Fatal("project row missing")
	}
	if got := mustString(t, row, "name"); got != "Renamed" {
		t.Fatalf("project name = %q; want Renamed", got)
	}
	if got := mustInt(t, row, "version"); got != 2 {
		t.Fatalf("project version = %d; want 2", got)
	}
	stale := update
	stale.Receipt = h.receipt(scope, "principal-admin", "project.update", "caller-stale", tenancyadmin.ProjectUpdateResult{Version: 2})
	_, err = h.adapter.UpdateProject(context.Background(), stale)
	requireFaultClass(t, err, fault.VersionConflict)
}

func TestGrantMembershipInsertAndUpdate(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	grant := tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleViewer}, Version: 1},
		ExpectedVersion: 0,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.grant", "caller-grant-1", tenancyadmin.MembershipGrantResult{Version: 1}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 1)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.GrantMembership(context.Background(), grant)
	if err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if result.Version != 1 || result.Replayed {
		t.Fatalf("first grant result = %+v; want version 1", result)
	}
	page, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(page.Memberships) != 1 || page.Memberships[0].PrincipalID != "principal-b" {
		t.Fatalf("memberships = %+v; want principal-b", page.Memberships)
	}
	update := grant
	update.Membership = tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}, Version: 2}
	update.ExpectedVersion = 1
	update.Receipt = h.receipt(scope, "principal-admin", "membership.grant", "caller-grant-2", tenancyadmin.MembershipGrantResult{Version: 2})
	update.Audit = []event.Audit{h.audit(scope, "principal-admin", "membership.grant")}
	update.Events = []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 2)}
	result, err = h.adapter.GrantMembership(context.Background(), update)
	if err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if result.Version != 2 {
		t.Fatalf("second grant version = %d; want 2", result.Version)
	}
	page, err = h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 10})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if !reflect.DeepEqual(page.Memberships[0].Roles, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}) {
		t.Fatalf("roles = %v; want engineer and reviewer", page.Memberships[0].Roles)
	}
}

func TestGrantMembershipStaleVersionRefused(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seed(scope, "memberships", "membership-b", map[string]any{"principal_id": "principal-b", "roles": rolesJSON(t, tenancy.RoleViewer), "version": int64(2), "created_at": testNow})
	grant := tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleAdmin}, Version: 2},
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.grant", "caller-stale", tenancyadmin.MembershipGrantResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.GrantMembership(context.Background(), grant)
	requireFaultClass(t, err, fault.VersionConflict)
	row, found := h.snapshot(scope, "memberships", "membership-b")
	if !found {
		t.Fatal("membership row disappeared")
	}
	if got := mustInt(t, row, "version"); got != 2 {
		t.Fatalf("membership version = %d; want untouched 2", got)
	}
	h.requireNoPublications(scope)
}

func TestRevokeMembershipDeletesUnderVersion(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-b", "principal-b", 3, tenancy.RoleViewer)
	revoke := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     "principal-b",
		ExpectedVersion: 3,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.revoke", "caller-revoke", tenancyadmin.MembershipRevokeResult{Version: 4}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 4)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	result, err := h.adapter.RevokeMembership(context.Background(), revoke)
	if err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if result.Version != 4 || result.Replayed {
		t.Fatalf("revoke result = %+v; want version 4", result)
	}
	h.requireNoRows(scope, "memberships", "membership-b")
	if got := len(h.store.Audit(scope)); got != 1 {
		t.Fatalf("audit rows = %d; want 1", got)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want 1", got)
	}
}

func TestRevokeMembershipStaleVersionKeepsRow(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-b", "principal-b", 2, tenancy.RoleViewer)
	revoke := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     "principal-b",
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.revoke", "caller-stale", tenancyadmin.MembershipRevokeResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.RevokeMembership(context.Background(), revoke)
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := h.snapshot(scope, "memberships", "membership-b"); !found {
		t.Fatal("the stale revocation removed the row")
	}
	h.requireNoPublications(scope)
}

// TestRevokeMembershipReplayAfterDelete proves a revoke retry converges on the
// original result even though the successful first attempt removed the row the
// adapter used to resolve its identity: the equal receipt replays without a
// second audit row, event or write.
func TestRevokeMembershipReplayAfterDelete(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-b", "principal-b", 3, tenancy.RoleViewer)
	revoke := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     "principal-b",
		ExpectedVersion: 3,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.revoke", "caller-replay", tenancyadmin.MembershipRevokeResult{Version: 4}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 4)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	first, err := h.adapter.RevokeMembership(context.Background(), revoke)
	if err != nil {
		t.Fatalf("first RevokeMembership: %v", err)
	}
	if first.Version != 4 || first.Replayed {
		t.Fatalf("first result = %+v; want version 4", first)
	}
	second, err := h.adapter.RevokeMembership(context.Background(), revoke)
	if err != nil {
		t.Fatalf("replayed RevokeMembership: %v", err)
	}
	if !second.Replayed || second.Version != 4 {
		t.Fatalf("replayed result = %+v; want version 4 replayed", second)
	}
	if got := len(h.store.Audit(scope)); got != 1 {
		t.Fatalf("audit rows = %d; want only the first revoke", got)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want only the first revoke", got)
	}
	h.requireNoRows(scope, "memberships", "membership-b")
}

// TestRevokeMembershipChangedContentAfterDelete proves a retry after the row is
// gone still refuses changed request content with changed_idempotency_content
// instead of reporting a missing membership.
func TestRevokeMembershipChangedContentAfterDelete(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-b", "principal-b", 3, tenancy.RoleViewer)
	revoke := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     "principal-b",
		ExpectedVersion: 3,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.revoke", "caller-changed", tenancyadmin.MembershipRevokeResult{Version: 4}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 4)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	if _, err := h.adapter.RevokeMembership(context.Background(), revoke); err != nil {
		t.Fatalf("first RevokeMembership: %v", err)
	}
	changed := revoke
	changed.Receipt.ContentHash = changedHash
	_, err := h.adapter.RevokeMembership(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows = %d; want only the first revoke", got)
	}
}

// TestRevokeMembershipMissingRowRefused proves a genuinely missing membership
// without a committed receipt is still a version_conflict from the guard, not a
// silent success.
func TestRevokeMembershipMissingRowRefused(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	revoke := tenancyadmin.MembershipRevokeUnit{
		Owner:           scope,
		PrincipalID:     "principal-missing",
		ExpectedVersion: 2,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.revoke", "caller-missing", tenancyadmin.MembershipRevokeResult{Version: 3}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.revoke")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-missing", 3)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.RevokeMembership(context.Background(), revoke)
	requireFaultClass(t, err, fault.VersionConflict)
	if _, found := h.store.Receipt(revoke.Receipt.Key); found {
		t.Fatal("the refused revoke left a receipt")
	}
	h.requireNoPublications(scope)
}

func TestListMembershipsPagesAreStable(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	for index, rowID := range []string{"membership-1", "membership-2", "membership-3"} {
		h.seedMembership(scope, rowID, "principal-"+rowID, uint64(index+1), tenancy.RoleViewer)
	}
	first, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Memberships) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v; want two rows and a cursor", first)
	}
	second, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: scope, Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Memberships) != 1 {
		t.Fatalf("second page rows = %d; want 1", len(second.Memberships))
	}
	seen := map[string]bool{}
	for _, membership := range append(first.Memberships, second.Memberships...) {
		key := string(membership.PrincipalID)
		if seen[key] {
			t.Fatalf("membership %q was returned twice", key)
		}
		seen[key] = true
	}
}

func TestListMembershipsScopeIsolation(t *testing.T) {
	h := newHarness(t)
	h.seedMembership(tenantScope("tenant-a"), "membership-a", "principal-a", 1, tenancy.RoleViewer)
	h.seedMembership(tenantScope("tenant-b"), "membership-b", "principal-b", 1, tenancy.RoleViewer)
	page, err := h.adapter.ListMemberships(context.Background(), tenancyadmin.MembershipQuery{Scope: tenantScope("tenant-a"), Limit: 10})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(page.Memberships) != 1 || page.Memberships[0].PrincipalID != "principal-a" {
		t.Fatalf("tenant-a memberships = %+v; want only principal-a", page.Memberships)
	}
}

func TestConcurrentMembershipGrantOneWinner(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedMembership(scope, "membership-b", "principal-b", 1, tenancy.RoleViewer)
	base := tenancyadmin.MembershipGrantUnit{
		Owner:           scope,
		Membership:      tenancy.Membership{Scope: scope, PrincipalID: "principal-b", Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: 2},
		ExpectedVersion: 1,
		At:              testNow,
		Receipt:         h.receipt(scope, "principal-admin", "membership.grant", "caller-a", tenancyadmin.MembershipGrantResult{Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "membership.grant")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	other := base
	other.Membership.Roles = []tenancy.Role{tenancy.RoleReviewer}
	other.Receipt = h.receipt(scope, "principal-admin", "membership.grant", "caller-b", tenancyadmin.MembershipGrantResult{Version: 2})
	other.Audit = []event.Audit{h.audit(scope, "principal-admin", "membership.grant")}
	other.Events = []event.Envelope{h.envelope(scope, "principal-admin", "principal-b", 2)}

	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	for index, unit := range []tenancyadmin.MembershipGrantUnit{base, other} {
		go func(index int, unit tenancyadmin.MembershipGrantUnit) {
			defer wait.Done()
			_, results[index] = h.adapter.GrantMembership(context.Background(), unit)
		}(index, unit)
	}
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		requireFaultClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly one", winners)
	}
	row, found := h.snapshot(scope, "memberships", "membership-b")
	if !found {
		t.Fatal("membership row missing after the race")
	}
	if got := mustInt(t, row, "version"); got != 2 {
		t.Fatalf("membership version = %d; want 2", got)
	}
}

// mustString reads one committed text column.
func mustString(t *testing.T, row store.Row, name string) string {
	t.Helper()
	value, ok := row.Values[name].(string)
	if !ok {
		t.Fatalf("%s.%s is not text: %T", row.Table, name, row.Values[name])
	}
	return value
}

// mustInt reads one committed integer column.
func mustInt(t *testing.T, row store.Row, name string) int64 {
	t.Helper()
	value, ok := row.Values[name].(int64)
	if !ok {
		t.Fatalf("%s.%s is not an integer: %T", row.Table, name, row.Values[name])
	}
	return value
}

// mustBytes reads one committed bytea/jsonb column.
func mustBytes(t *testing.T, row store.Row, name string) []byte {
	t.Helper()
	value, ok := row.Values[name].([]byte)
	if !ok {
		t.Fatalf("%s.%s is not bytes: %T", row.Table, name, row.Values[name])
	}
	return value
}
