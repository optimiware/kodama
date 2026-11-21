package kube

import (
	"context"
	"testing"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

func TestKubeAdapter_NamedCacheMountsEmptyDirPVCAndEvictionDegradation(t *testing.T) {
	ctx := context.Background()
	projA := tenancy.Scope{TenantID: testTenant(), ProjectID: id.ProjectID("proj-a")}
	cachesA, err := workspace.BuildCacheMounts(projA, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts: %v", err)
	}

	t.Run("default emptyDir mount at /kodema/cache/<name>", func(t *testing.T) {
		cluster := newFakeCluster()
		addTestNamespace(cluster)
		addTestClaim(t, cluster, testWorkspaceID())
		adapter := newTestAdapter(t, cluster, newFakeClock())

		req := testPrepareRequest()
		req.Container.Caches = []string{"go-mod"}
		req.Caches = cachesA

		if _, err := adapter.Prepare(ctx, req); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if _, err := adapter.Start(ctx, engine.StartRequest{Identity: req.Identity, WorkspaceID: req.WorkspaceID}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		job := cacheTestOnlyJob(t, cluster)
		found := false
		for _, v := range job.Spec.Volumes {
			if v.MountPath == "/kodema/cache/go-mod" {
				if !v.EmptyDir || v.ReadOnly || v.ClaimName != "" {
					t.Fatalf("unexpected default cache volume: %+v", v)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("missing /kodema/cache/go-mod volume in job spec: %+v", job.Spec.Volumes)
		}
	})

	t.Run("configured shared cache PVC mounts with tenant/project subPath", func(t *testing.T) {
		cluster := newFakeCluster()
		addTestNamespace(cluster)
		addTestClaim(t, cluster, testWorkspaceID())
		cluster.addClaim(Claim{
			Name:      "kodema-shared-cache",
			Namespace: testNamespace,
		})
		cfg := testConfig(cluster, newFakeClock())
		cfg.CacheClaimName = "kodema-shared-cache"
		adapter, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		req := testPrepareRequest()
		req.Container.Caches = []string{"go-mod"}
		req.Caches = cachesA

		if _, err := adapter.Prepare(ctx, req); err != nil {
			t.Fatalf("Prepare with shared cache PVC: %v", err)
		}
		if _, err := adapter.Start(ctx, engine.StartRequest{Identity: req.Identity, WorkspaceID: req.WorkspaceID}); err != nil {
			t.Fatalf("Start with shared cache PVC: %v", err)
		}
		job := cacheTestOnlyJob(t, cluster)
		found := false
		for _, v := range job.Spec.Volumes {
			if v.MountPath == "/kodema/cache/go-mod" {
				if v.ClaimName != "kodema-shared-cache" || v.SubPath != cachesA[0].Key || v.ReadOnly || v.EmptyDir {
					t.Fatalf("unexpected shared PVC cache volume: %+v", v)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("missing shared PVC cache volume in job spec: %+v", job.Spec.Volumes)
		}
	})

	t.Run("missing/evicted cache PVC degrades cleanly to emptyDir without failing", func(t *testing.T) {
		cluster := newFakeCluster()
		addTestNamespace(cluster)
		addTestClaim(t, cluster, testWorkspaceID())
		// Deliberately do NOT seed kodema-evicted-cache.
		cfg := testConfig(cluster, newFakeClock())
		cfg.CacheClaimName = "kodema-evicted-cache"
		adapter, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		req := testPrepareRequest()
		req.Container.Caches = []string{"go-mod"}
		req.Caches = cachesA

		if _, err := adapter.Prepare(ctx, req); err != nil {
			t.Fatalf("Prepare failed on missing non-authoritative cache PVC: %v", err)
		}
		if _, err := adapter.Start(ctx, engine.StartRequest{Identity: req.Identity, WorkspaceID: req.WorkspaceID}); err != nil {
			t.Fatalf("Start failed on missing non-authoritative cache PVC: %v", err)
		}
		job := cacheTestOnlyJob(t, cluster)
		found := false
		for _, v := range job.Spec.Volumes {
			if v.MountPath == "/kodema/cache/go-mod" {
				if !v.EmptyDir || v.ClaimName != "" || v.ReadOnly {
					t.Fatalf("expected emptyDir fallback for missing cache PVC, got: %+v", v)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("missing fallback cache volume in job spec: %+v", job.Spec.Volumes)
		}
	})
}

func cacheTestOnlyJob(t *testing.T, cluster *fakeCluster) Job {
	t.Helper()
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if len(cluster.jobs) != 1 {
		t.Fatalf("jobs count = %d; want 1", len(cluster.jobs))
	}
	for _, job := range cluster.jobs {
		return job
	}
	t.Fatal("unreachable")
	return Job{}
}
