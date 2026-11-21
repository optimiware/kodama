package podman

import (
	"context"
	"testing"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

func TestPodmanAdapter_NamedCacheMountsAndIsolation(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)

	projA := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("proj-a")}
	projB := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("proj-b")}

	cachesA, err := workspace.BuildCacheMounts(projA, []string{"go-mod", "npm"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projA): %v", err)
	}
	cachesB, err := workspace.BuildCacheMounts(projB, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projB): %v", err)
	}

	// Prepare succeeds even though cache volumes do not exist yet (non-authoritative).
	fx.request.Container.Caches = []string{"go-mod", "npm"}
	fx.request.Caches = cachesA
	if _, err := fx.adapter.Prepare(ctx, fx.request); err != nil {
		t.Fatalf("Prepare with declared caches: %v", err)
	}
	if _, err := fx.adapter.Start(ctx, engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err != nil {
		t.Fatalf("Start with declared caches: %v", err)
	}

	if len(fx.libpod.createRequests) != 1 {
		t.Fatalf("createRequests count = %d; want 1", len(fx.libpod.createRequests))
	}
	specA := fx.libpod.createRequests[0]

	wantGoModVol, _ := CacheVolumeName(cachesA[0].Key)
	wantNpmVol, _ := CacheVolumeName(cachesA[1].Key)

	foundGoMod, foundNpm := false, false
	for _, m := range specA.Mounts {
		if m.Destination == "/kodema/cache/go-mod" {
			if m.Type != "volume" || m.ReadOnly || m.Source != wantGoModVol {
				t.Fatalf("unexpected go-mod cache mount: %+v (want writable volume %q)", m, wantGoModVol)
			}
			foundGoMod = true
		}
		if m.Destination == "/kodema/cache/npm" {
			if m.Type != "volume" || m.ReadOnly || m.Source != wantNpmVol {
				t.Fatalf("unexpected npm cache mount: %+v (want writable volume %q)", m, wantNpmVol)
			}
			foundNpm = true
		}
	}
	if !foundGoMod || !foundNpm {
		t.Fatalf("missing cache mounts in specA.Mounts: %+v", specA.Mounts)
	}

	// A different project receives a distinct cache volume name.
	wantProjBVol, _ := CacheVolumeName(cachesB[0].Key)
	if wantProjBVol == wantGoModVol {
		t.Fatalf("cross-project Podman cache volume collision: %q", wantProjBVol)
	}

	// Unscoped caches (missing Key) must be refused.
	badReq := fx.request
	badReq.Identity.WorkloadID = id.WorkloadID("wl-bad-cache")
	badReq.Caches = nil
	if _, err := fx.adapter.Prepare(ctx, badReq); err == nil {
		t.Fatal("Prepare accepted Container.Caches without scoped CacheMounts")
	}
}
