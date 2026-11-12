package tenancyadmin_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

func TestCreateTenantBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.TenantCreateUnit
	calls := 0
	ports.createTenant = func(_ context.Context, unit tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
		calls++
		captured = unit
		return tenancyadmin.TenantCreateResult{TenantID: unit.Tenant.ID, Version: unit.Tenant.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("actor-tenant", "", "principal-admin")
	key := tenantKey("tenant-a", 1)
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Tenant A"}`)}
	result, err := service.CreateTenant(context.Background(), tenancyadmin.TenantCreateCommand{
		Actor:     actor,
		TenantID:  "tenant-a",
		Name:      "Tenant A",
		Key:       key,
		CallerKey: "caller-1",
		Content:   content,
	})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if calls != 1 {
		t.Fatalf("CreateTenant port calls = %d; want 1", calls)
	}
	if result.TenantID != "tenant-a" || result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want tenant-a version 1 not replayed", result)
	}
	if captured.Tenant.ID != "tenant-a" || captured.Tenant.Name != "Tenant A" || captured.Tenant.Version != 1 {
		t.Fatalf("tenant = %+v; want tenant-a version 1", captured.Tenant)
	}
	if !reflect.DeepEqual(captured.Key, key) {
		t.Fatalf("key = %+v; want %+v", captured.Key, key)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	if err := captured.Validate(); err != nil {
		t.Fatalf("tenant creation unit is not a valid atomic unit: %v", err)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "", "principal-admin", "tenant.create", "caller-1", content, testNow)
	requireAudit(t, captured.Audit, tenantScope("tenant-a"), actor.ID, "tenant.create", testNow)
	requireConfigEvent(t, captured.Events, tenantScope("tenant-a"), "tenant-a", 1, actor.ID, testNow)
	if captured.Audit[0].ID == captured.Events[0].ID {
		t.Fatal("audit and event intents reuse one identity")
	}
	requireAuthority(t, captured.Authority, "tenant.create", authorize.CheckPrincipalEnabled, tenantScope("tenant-a"), actor.ID)
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestCreateTenantRefusesProjectScopedActor(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.createTenant = func(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
		calls++
		return tenancyadmin.TenantCreateResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.CreateTenant(context.Background(), tenancyadmin.TenantCreateCommand{
		Actor:     adminActor("tenant-a", "project-a", "principal-admin"),
		TenantID:  "tenant-b",
		Name:      "Tenant B",
		Key:       tenantKey("tenant-b", 1),
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.Forbidden)
	if calls != 0 {
		t.Fatalf("CreateTenant port calls = %d; want 0 before refusal", calls)
	}
}

func TestCreateTenantRefusesMismatchedKeyTenant(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.createTenant = func(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
		calls++
		return tenancyadmin.TenantCreateResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.CreateTenant(context.Background(), tenancyadmin.TenantCreateCommand{
		Actor:     adminActor("actor-tenant", "", "principal-admin"),
		TenantID:  "tenant-a",
		Name:      "Tenant A",
		Key:       tenantKey("tenant-b", 1),
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("CreateTenant port calls = %d; want 0 before refusal", calls)
	}
}

func TestCreateTenantRefusesInvalidNameAndActor(t *testing.T) {
	tests := []struct {
		name   string
		actor  string
		tenant string
	}{
		{name: "empty name", actor: "principal-admin", tenant: "tenant-a"},
		{name: "invalid actor id", actor: "bad/id", tenant: "tenant-a"},
		{name: "invalid tenant id", actor: "principal-admin", tenant: "bad/id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.createTenant = func(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
				calls++
				return tenancyadmin.TenantCreateResult{}, nil
			}
			service := newAdminService(t, ports)
			name := "Tenant A"
			if test.name == "empty name" {
				name = ""
			}
			_, err := service.CreateTenant(context.Background(), tenancyadmin.TenantCreateCommand{
				Actor:     adminActor("actor-tenant", "", test.actor),
				TenantID:  id.TenantID(test.tenant),
				Name:      name,
				Key:       tenantKey(test.tenant, 1),
				CallerKey: "caller-1",
				Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if calls != 0 {
				t.Fatalf("CreateTenant port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestCreateTenantDefensiveCopiesKeyMaterial(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.TenantCreateUnit
	ports.createTenant = func(_ context.Context, unit tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
		captured = unit
		return tenancyadmin.TenantCreateResult{TenantID: unit.Tenant.ID, Version: unit.Tenant.Version}, nil
	}
	service := newAdminService(t, ports)

	key := tenantKey("tenant-a", 1)
	command := tenancyadmin.TenantCreateCommand{
		Actor:     adminActor("actor-tenant", "", "principal-admin"),
		TenantID:  "tenant-a",
		Name:      "Tenant A",
		Key:       key,
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	}
	if _, err := service.CreateTenant(context.Background(), command); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	command.Key.Ciphertext[0] = 'X'
	command.Key.WrappedKey[0] = 'Y'
	if captured.Key.Ciphertext[0] == 'X' || captured.Key.WrappedKey[0] == 'Y' {
		t.Fatal("committed tenant key aliases caller-owned key bytes")
	}
}

func TestRotateTenantKeyBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.TenantKeyRotationUnit
	ports.rotateTenantKey = func(_ context.Context, unit tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
		captured = unit
		return tenancyadmin.TenantKeyRotationResult{TenantVersion: unit.Tenant.Version, KeyVersion: unit.Key.Key.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"reason":"rotation"}`)}
	result, err := service.RotateTenantKey(context.Background(), tenancyadmin.TenantKeyRotationCommand{
		Actor:           actor,
		TenantID:        "tenant-a",
		Name:            "Tenant A",
		ExpectedVersion: 2,
		Key:             tenantKey("tenant-a", 3),
		CallerKey:       "caller-rotate",
		Content:         content,
	})
	if err != nil {
		t.Fatalf("RotateTenantKey: %v", err)
	}
	if result.TenantVersion != 3 || result.KeyVersion != 3 || result.Replayed {
		t.Fatalf("result = %+v; want tenant/key version 3 not replayed", result)
	}
	if captured.Tenant.Version != 3 || captured.ExpectedVersion != 2 {
		t.Fatalf("rotation = tenant %d expected %d; want tenant 3 expected 2", captured.Tenant.Version, captured.ExpectedVersion)
	}
	if captured.Key.TenantID != "tenant-a" || captured.Key.ExpectedVersion != 2 || captured.Key.Key.Version != 3 {
		t.Fatalf("key write = %+v; want tenant-a expected 2 next 3", captured.Key)
	}
	if err := captured.Validate(); err != nil {
		t.Fatalf("rotation unit is not a valid atomic unit: %v", err)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "", "principal-admin", "tenant.rotate_key", "caller-rotate", content, testNow)
	requireAudit(t, captured.Audit, tenantScope("tenant-a"), actor.ID, "tenant.rotate_key", testNow)
	requireConfigEvent(t, captured.Events, tenantScope("tenant-a"), "tenant-a", 3, actor.ID, testNow)
	requireAuthority(t, captured.Authority, "tenant.rotate_key", authorize.CheckMembership, tenantScope("tenant-a"), actor.ID)
	requireAuthority(t, captured.Authority, "tenant.rotate_key", authorize.CheckPrincipalEnabled, tenantScope("tenant-a"), actor.ID)
	if len(captured.PostCommit) != 2 {
		t.Fatalf("post-commit effects = %d; want publish and credential refresh", len(captured.PostCommit))
	}
	kinds := map[event.PostCommitKind]bool{}
	for _, effect := range captured.PostCommit {
		kinds[effect.Kind] = true
	}
	if !kinds[event.PostPublishEvent] || !kinds[event.PostRefreshCredential] {
		t.Fatalf("post-commit kinds = %v; want publish_event and refresh_credential", kinds)
	}
}

func TestRotateTenantKeyRefusesStaleOrMismatchedInput(t *testing.T) {
	tests := []struct {
		name       string
		expected   uint64
		keyTenant  string
		keyVersion uint64
	}{
		{name: "zero expected version", expected: 0, keyTenant: "tenant-a", keyVersion: 1},
		{name: "key does not advance", expected: 2, keyTenant: "tenant-a", keyVersion: 4},
		{name: "key tenant mismatch", expected: 2, keyTenant: "tenant-b", keyVersion: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.rotateTenantKey = func(context.Context, tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
				calls++
				return tenancyadmin.TenantKeyRotationResult{}, nil
			}
			service := newAdminService(t, ports)
			_, err := service.RotateTenantKey(context.Background(), tenancyadmin.TenantKeyRotationCommand{
				Actor:           adminActor("tenant-a", "", "principal-admin"),
				TenantID:        "tenant-a",
				Name:            "Tenant A",
				ExpectedVersion: test.expected,
				Key:             tenantKey(test.keyTenant, test.keyVersion),
				CallerKey:       "caller-rotate",
				Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if calls != 0 {
				t.Fatalf("RotateTenantKey port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestRotateTenantKeyRefusesCrossTenantActor(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.rotateTenantKey = func(context.Context, tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
		calls++
		return tenancyadmin.TenantKeyRotationResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.RotateTenantKey(context.Background(), tenancyadmin.TenantKeyRotationCommand{
		Actor:           adminActor("tenant-a", "", "principal-admin"),
		TenantID:        "tenant-b",
		Name:            "Tenant B",
		ExpectedVersion: 2,
		Key:             tenantKey("tenant-b", 3),
		CallerKey:       "caller-rotate",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.Forbidden)
	if calls != 0 {
		t.Fatalf("RotateTenantKey port calls = %d; want 0 before refusal", calls)
	}
}
