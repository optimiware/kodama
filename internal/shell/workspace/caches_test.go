package workspace_test

import (
	"testing"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

func TestCacheScopeKeyIsolation(t *testing.T) {
	t1p1 := tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("proj-1")}
	t1p2 := tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("proj-2")}
	t2p1 := tenancy.Scope{TenantID: id.TenantID("tenant-2"), ProjectID: id.ProjectID("proj-1")}

	k1, err := workspace.CacheScopeKey(t1p1, "go-mod")
	if err != nil {
		t.Fatalf("CacheScopeKey(t1p1, go-mod): %v", err)
	}
	k1Again, err := workspace.CacheScopeKey(t1p1, "go-mod")
	if err != nil || k1Again != k1 {
		t.Fatalf("CacheScopeKey is not deterministic: %q vs %q (%v)", k1, k1Again, err)
	}
	k2, err := workspace.CacheScopeKey(t1p2, "go-mod")
	if err != nil {
		t.Fatalf("CacheScopeKey(t1p2, go-mod): %v", err)
	}
	k3, err := workspace.CacheScopeKey(t2p1, "go-mod")
	if err != nil {
		t.Fatalf("CacheScopeKey(t2p1, go-mod): %v", err)
	}
	k4, err := workspace.CacheScopeKey(t1p1, "npm")
	if err != nil {
		t.Fatalf("CacheScopeKey(t1p1, npm): %v", err)
	}

	seen := map[string]string{k1: "t1p1/go-mod"}
	for label, key := range map[string]string{
		"t1p2/go-mod": k2,
		"t2p1/go-mod": k3,
		"t1p1/npm":    k4,
	} {
		if prev, dup := seen[key]; dup {
			t.Fatalf("cache scope collision between %s and %s: %q", prev, label, key)
		}
		seen[key] = label
	}

	// Tenant-wide (non-project-scoped) scope must be refused.
	if _, err := workspace.CacheScopeKey(tenancy.Scope{TenantID: id.TenantID("tenant-1")}, "go-mod"); err == nil {
		t.Fatal("CacheScopeKey accepted tenant-wide scope without project_id")
	}

	// ValidateCacheMounts refuses unscoped declared caches.
	if err := workspace.ValidateCacheMounts([]string{"go-mod"}, nil); err == nil {
		t.Fatal("ValidateCacheMounts accepted declared caches without scoped CacheMounts")
	}
	mounts, err := workspace.BuildCacheMounts(t1p1, []string{"go-mod", "npm"})
	if err != nil {
		t.Fatalf("BuildCacheMounts: %v", err)
	}
	if err := workspace.ValidateCacheMounts([]string{"go-mod", "npm"}, mounts); err != nil {
		t.Fatalf("ValidateCacheMounts(valid): %v", err)
	}
	// Mismatched order/names rejected.
	if err := workspace.ValidateCacheMounts([]string{"go-mod"}, []engine.CacheMount{{Name: "npm", Key: k4}}); err == nil {
		t.Fatal("ValidateCacheMounts accepted mismatched cache name")
	}
}
