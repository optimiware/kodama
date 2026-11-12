package app

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestFirstAdminSeedIsIdempotentAndStoreOnly(t *testing.T) {
	clock := newFakeClock()
	memoryStore := memory.NewWithClock(clock.Now)
	cipher := newTestCipher(t)
	bootstrap := &Bootstrap{
		TenantID:       "tenant-1",
		TenantName:     "Tenant One",
		ProjectID:      "project-1",
		ProjectName:    "Project One",
		AdminPrincipal: "admin-1",
		AdminRoles:     []string{"admin"},
	}
	for attempt := range 2 {
		if err := seedFirstAdmin(context.Background(), memoryStore, memoryStore, bridges.NewSecureIDs(), cipher, "installation-1", secret.MinKeyVersion, bootstrap, clock); err != nil {
			t.Fatalf("seed attempt %d: %v", attempt, err)
		}
	}

	tenantScope := tenancy.Scope{TenantID: "tenant-1"}
	projectScope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	if _, err := memoryStore.Read(context.Background(), tenantScope, "tenants", "tenant-1"); err != nil {
		t.Fatalf("seeded tenant read: %v", err)
	}
	if _, err := memoryStore.Read(context.Background(), tenantScope, "projects", "project-1"); err != nil {
		t.Fatalf("seeded project read: %v", err)
	}
	tenantMemberships, err := memoryStore.Query(context.Background(), tenantScope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": "admin-1"},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("tenant membership query: %v", err)
	}
	if len(tenantMemberships.Rows) != 1 {
		t.Fatalf("tenant-wide admin memberships = %d; want exactly 1", len(tenantMemberships.Rows))
	}
	projectMemberships, err := memoryStore.Query(context.Background(), projectScope, store.ScanQuery{
		Table:   "memberships",
		Equals:  map[string]any{"principal_id": "admin-1"},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("project membership query: %v", err)
	}
	if len(projectMemberships.Rows) != 0 {
		t.Fatalf("seed created %d project memberships; want the service-owned mutation to remain absent", len(projectMemberships.Rows))
	}
}
