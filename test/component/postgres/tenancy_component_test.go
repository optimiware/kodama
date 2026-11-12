//go:build component

package postgrescomponent

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	tenancyadmin "github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

func tenantKey(tenant string) token.TenantKey {
	return token.TenantKey{
		TenantID: id.TenantID(tenant), Version: 1,
		Ciphertext: []byte("ciphertext"), WrappedKey: []byte("wrapped-key"),
		WrapNonce: []byte("wrap-nonce"), KeyRef: "kms/tenant-key", KeyVersion: 1,
	}
}

func adminContent(label string) receipt.Content {
	return receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"` + label + `"}`)}
}

// TestRealTenancyAdminOnPostgreSQL drives the real tenancy admin service through
// tenant create, project create/update, membership grant and revoke over the
// PostgreSQL identity adapter.
func TestRealTenancyAdminOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	tenant := pgtest.TenantScope("tenant-a")
	shard.SeedTenant(ctx, "tenant-a", 1)
	shard.SeedMembership(ctx, tenant, "membership-admin", "principal-admin", 1, string(tenancy.RoleAdmin))
	runtime := shard.Runtime(4)
	defer runtime.Close()
	scoped := storepg.New(runtime)
	adapter, err := identityunits.New(identityunits.Config{
		Executor: scoped, Scanner: scoped,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	service, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
		TenantCreate: adapter, TenantKeyRotation: adapter,
		ProjectCreate: adapter, ProjectUpdate: adapter,
		MembershipGrant: adapter, MembershipRevoke: adapter, MembershipQuery: adapter,
	})
	if err != nil {
		t.Fatalf("tenancyadmin.NewService: %v", err)
	}
	actor := humanAdmin(tenant, "principal-admin")
	tenantObserver := shard.ObserveUnit(t, "tenancy", "tenant.create")
	created, err := service.CreateTenant(ctx, tenancyadmin.TenantCreateCommand{
		Actor: actor, TenantID: "tenant-b", Name: "Tenant b", Key: tenantKey("tenant-b"),
		CallerKey: "tenant-create", Content: adminContent("tenant-create"),
	})
	if err != nil || created.Version != 1 {
		t.Fatalf("CreateTenant: %v (version=%d)", err, created.Version)
	}
	tenantName, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT name FROM tenants WHERE id = 'tenant-b'`)
	if err != nil || tenantName != "Tenant b" {
		t.Fatalf("durable tenant name = %q, err=%v; want Tenant b", tenantName, err)
	}
	tenantVersion, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT version FROM tenants WHERE id = 'tenant-b'`)
	if err != nil || tenantVersion != 1 {
		t.Fatalf("durable tenant version = %d, err=%v; want 1", tenantVersion, err)
	}
	derivationKey, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT derivation_key::text FROM tenants WHERE id = 'tenant-b'`)
	if err != nil || !jsonEqual(derivationKey, `{"tenant_id":"tenant-b","version":1,"ciphertext":"Y2lwaGVydGV4dA==","wrapped_key":"d3JhcHBlZC1rZXk=","wrap_nonce":"d3JhcC1ub25jZQ==","key_ref":"kms/tenant-key","key_version":1}`) {
		t.Fatalf("durable tenant derivation key = %q, err=%v; want the submitted key", derivationKey, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "tenants", map[string]any{"id": "tenant-b"}, map[string]any{
		"id": "tenant-b", "name": "Tenant b", "version": int64(1),
		"derivation_key": `{"tenant_id":"tenant-b","version":1,"ciphertext":"Y2lwaGVydGV4dA==","wrapped_key":"d3JhcHBlZC1rZXk=","wrap_nonce":"d3JhcC1ub25jZQ==","key_ref":"kms/tenant-key","key_version":1}`,
		"created_at":     pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-1"}, map[string]any{
		"id": "receipt-1", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "tenant.create", "caller_key": "tenant-create",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("tenant-create")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.TenantCreateResult{TenantID: "tenant-b", Version: 1}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "tenant.create", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "tenant-b", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	tenantObserver.Finish(map[string]int{"tenants": 1, "receipts": 1, "audit": 1, "events": 1})
	// The actor must be an admin in the new tenant's scope too; seed it directly.
	shard.SeedMembership(ctx, pgtest.TenantScope("tenant-b"), "membership-admin-b", "principal-admin", 1, string(tenancy.RoleAdmin))

	projectObserver := shard.ObserveUnit(t, "tenancy", "project.create")
	project, err := service.CreateProject(ctx, tenancyadmin.ProjectCreateCommand{
		Actor:     humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"),
		ProjectID: "project-b", Name: "Project b", CallerKey: "project-create",
		Content: adminContent("project-create"),
	})
	if err != nil || project.Version != 1 {
		t.Fatalf("CreateProject: %v (version=%d)", err, project.Version)
	}
	projectName, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT name FROM projects WHERE id = 'project-b'`)
	if err != nil || projectName != "Project b" {
		t.Fatalf("durable project name = %q, err=%v; want Project b", projectName, err)
	}
	projectVersion, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT version FROM projects WHERE id = 'project-b'`)
	if err != nil || projectVersion != 1 {
		t.Fatalf("durable project version = %d, err=%v; want 1", projectVersion, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "projects", map[string]any{"id": "project-b"}, map[string]any{
		"id": "project-b", "tenant_id": "tenant-b", "name": "Project b",
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-2"}, map[string]any{
		"id": "receipt-2", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "project.create", "caller_key": "project-create",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("project-create")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.ProjectCreateResult{ProjectID: "project-b", Version: 1}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-3"}, map[string]any{
		"id": "event-3", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "project.create", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-4"}, map[string]any{
		"id": "event-4", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "project-b", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	projectObserver.Finish(map[string]int{"projects": 1, "receipts": 1, "audit": 1, "events": 1})
	projectUpdateObserver := shard.ObserveUnit(t, "tenancy", "project.update")
	updated, err := service.UpdateProject(ctx, tenancyadmin.ProjectUpdateCommand{
		Actor:     humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"),
		ProjectID: "project-b", ExpectedVersion: 1, Name: "Project b renamed",
		CallerKey: "project-update", Content: adminContent("project-update"),
	})
	if err != nil || updated.Version != 2 {
		t.Fatalf("UpdateProject: %v (version=%d)", err, updated.Version)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "projects", map[string]any{"id": "project-b"}, map[string]any{
		"id": "project-b", "tenant_id": "tenant-b", "name": "Project b renamed",
		"version": int64(2), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-3"}, map[string]any{
		"id": "receipt-3", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "project.update", "caller_key": "project-update",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("project-update")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.ProjectUpdateResult{Version: 2}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-5"}, map[string]any{
		"id": "event-5", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "project.update", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-6"}, map[string]any{
		"id": "event-6", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "project-b", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	projectUpdateObserver.Finish(map[string]int{"projects": 1, "receipts": 1, "audit": 1, "events": 1})
	grant, err := service.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor: humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"),
		Membership: tenancy.Membership{
			Scope: pgtest.TenantScope("tenant-b"), PrincipalID: "principal-member",
			Roles: []tenancy.Role{tenancy.RoleEngineer}, Version: 1,
		},
		CallerKey: "membership-grant", Content: adminContent("membership-grant"),
	})
	if err != nil || grant.Version != 1 {
		t.Fatalf("GrantMembership: %v (version=%d)", err, grant.Version)
	}
	rawRoles, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT roles::text FROM memberships WHERE principal_id = 'principal-member'`)
	if err != nil || rawRoles != `["engineer"]` {
		t.Fatalf("durable membership roles = %q, err=%v; want [\"engineer\"]", rawRoles, err)
	}
	granted, err := adapter.ReadPrincipal(ctx, identity.PrincipalRequest{
		Scope: pgtest.TenantScope("tenant-b"), PrincipalID: "principal-member",
	})
	if err != nil || granted.Kind != identity.KindHuman || !granted.Enabled ||
		granted.Version != 1 || len(granted.Roles) != 1 || granted.Roles[0] != tenancy.RoleEngineer {
		t.Fatalf("granted principal = %+v, err=%v; want human/engineer/enabled/1", granted, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "memberships", map[string]any{"principal_id": "principal-member"}, map[string]any{
		"id": "row-1", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-member", "roles": `["engineer"]`,
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	grantExistingObserver := shard.ObserveUnit(t, "tenancy", "membership.grant.existing")
	grantExisting, err := service.GrantMembership(ctx, tenancyadmin.MembershipGrantCommand{
		Actor: humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"),
		Membership: tenancy.Membership{
			Scope: pgtest.TenantScope("tenant-b"), PrincipalID: "principal-member",
			Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}, Version: 2,
		},
		ExpectedVersion: 1,
		CallerKey:       "membership-grant-existing", Content: adminContent("membership-grant-existing"),
	})
	if err != nil {
		t.Fatalf("GrantMembership existing: %v", err)
	}
	if grantExisting != (tenancyadmin.MembershipGrantResult{Version: 2}) {
		t.Fatalf("GrantMembership existing result = %+v; want version 2, not replayed", grantExisting)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "memberships", map[string]any{"id": "row-1"}, map[string]any{
		"id": "row-1", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-member", "roles": `["engineer","viewer"]`,
		"version": int64(2), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-5"}, map[string]any{
		"id": "receipt-5", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "membership.grant", "caller_key": "membership-grant-existing",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("membership-grant-existing")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.MembershipGrantResult{Version: 2}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-9"}, map[string]any{
		"id": "event-9", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "membership.grant", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-10"}, map[string]any{
		"id": "event-10", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "principal-member", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	grantExistingObserver.Finish(map[string]int{"memberships": 1, "receipts": 1, "audit": 1, "events": 1})
	revokeObserver := shard.ObserveUnit(t, "tenancy", "membership.revoke")
	revoked, err := service.RevokeMembership(ctx, tenancyadmin.MembershipRevokeCommand{
		Actor: humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"),
		Scope: pgtest.TenantScope("tenant-b"), PrincipalID: "principal-member",
		ExpectedVersion: 2, CallerKey: "membership-revoke", Content: adminContent("membership-revoke"),
	})
	if err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if revoked != (tenancyadmin.MembershipRevokeResult{Version: 3}) {
		t.Fatalf("RevokeMembership result = %+v; want version 3, not replayed", revoked)
	}
	shard.RequireRowAbsent(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "memberships", map[string]any{"id": "row-1"})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-6"}, map[string]any{
		"id": "receipt-6", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "membership.revoke", "caller_key": "membership-revoke",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("membership-revoke")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.MembershipRevokeResult{Version: 3}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-11"}, map[string]any{
		"id": "event-11", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "membership.revoke", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-12"}, map[string]any{
		"id": "event-12", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "principal-member", "subject_version": int64(3),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	revokeObserver.Finish(map[string]int{"memberships": 1, "receipts": 1, "audit": 1, "events": 1})
	keyObserver := shard.ObserveUnit(t, "tenancy", "tenant.key.rotate")
	rotatedKey := tenantKey("tenant-b")
	rotatedKey.Version = 2
	rotation, err := service.RotateTenantKey(ctx, tenancyadmin.TenantKeyRotationCommand{
		Actor: humanAdmin(pgtest.TenantScope("tenant-b"), "principal-admin"), TenantID: "tenant-b",
		Name: "Tenant b", ExpectedVersion: 1, Key: rotatedKey,
		CallerKey: "tenant-key-rotate", Content: adminContent("tenant-key-rotate"),
	})
	if err != nil {
		t.Fatalf("RotateTenantKey: %v", err)
	}
	if rotation != (tenancyadmin.TenantKeyRotationResult{TenantVersion: 2, KeyVersion: 2}) {
		t.Fatalf("RotateTenantKey result = %+v; want tenant version 2 and key version 2, not replayed", rotation)
	}
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "tenants", map[string]any{"id": "tenant-b"}, map[string]any{
		"id": "tenant-b", "name": "Tenant b", "version": int64(2),
		"derivation_key": `{"tenant_id":"tenant-b","version":2,"ciphertext":"Y2lwaGVydGV4dA==","wrapped_key":"d3JhcHBlZC1rZXk=","wrap_nonce":"d3JhcC1ub25jZQ==","key_ref":"kms/tenant-key","key_version":1}`,
		"created_at":     pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "receipts", map[string]any{"id": "receipt-7"}, map[string]any{
		"id": "receipt-7", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"principal_id": "principal-admin", "operation": "tenant.rotate_key", "caller_key": "tenant-key-rotate",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("tenant-key-rotate")),
		"result":         pgtest.GoldenJSONOf(tenancyadmin.TenantKeyRotationResult{TenantVersion: 2, KeyVersion: 2}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "audit", map[string]any{"id": "event-13"}, map[string]any{
		"id": "event-13", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"actor": "principal-admin", "operation": "tenant.rotate_key", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, pgtest.TenantScope("tenant-b"), "events", map[string]any{"id": "event-14"}, map[string]any{
		"id": "event-14", "tenant_id": "tenant-b", "project_id": pgtest.NullValue,
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": "tenant-b", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	keyObserver.Finish(map[string]int{"tenants": 1, "receipts": 1, "audit": 1, "events": 1})
	for table, id := range map[string]string{
		"tenants": "tenant-b", "projects": "project-b", "memberships": "principal-member",
	} {
		column := "id"
		if table == "memberships" {
			column = "principal_id"
		}
		count, err := pgtest.ScalarInt(ctx, shard.Admin(), `SELECT count(*) FROM `+table+` WHERE `+column+` = $1`, id)
		if err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		want := int64(1)
		if table == "memberships" {
			want = 0 // revoked
		}
		if count != want {
			t.Fatalf("%s rows for %s = %d; want %d", table, id, count, want)
		}
	}
	name, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT name FROM projects WHERE id = 'project-b'`)
	if err != nil || name != "Project b renamed" {
		t.Fatalf("project name = %q, err=%v; want renamed", name, err)
	}
}
