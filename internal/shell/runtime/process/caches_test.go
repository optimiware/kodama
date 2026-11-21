package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

func TestProcessAdapter_NamedCachesPersistAcrossExecutionsAndIsolateProjects(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)

	projA := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("proj-a")}
	projB := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("proj-b")}

	cachesA, err := workspace.BuildCacheMounts(projA, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projA): %v", err)
	}
	cachesB, err := workspace.BuildCacheMounts(projB, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projB): %v", err)
	}

	makeWorkspace := func(wsID id.WorkspaceID) string {
		t.Helper()
		dir, err := WorkspaceDir(fx.workspaceRoot, wsID)
		if err != nil {
			t.Fatalf("WorkspaceDir(%q): %v", wsID, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
		return dir
	}

	// Execution 1 in proj-a prepares and starts with cache "go-mod".
	ws1 := id.WorkspaceID("ws-cache-1")
	ws1Dir := makeWorkspace(ws1)
	req1 := fx.request
	req1.Identity = engine.EffectIdentity{ExecutionID: "exec-cache-1", ActionID: "act-1", WorkloadID: "wl-cache-1", IdempotencyKey: "key-1"}
	req1.WorkspaceID = ws1
	req1.Container.Caches = []string{"go-mod"}
	req1.Caches = cachesA

	if _, err := fx.adapter.Prepare(ctx, req1); err != nil {
		t.Fatalf("Prepare exec 1: %v", err)
	}
	if _, err := fx.adapter.Start(ctx, engine.StartRequest{Identity: req1.Identity, WorkspaceID: ws1}); err != nil {
		t.Fatalf("Start exec 1: %v", err)
	}
	if len(fx.executor.requests) != 1 || len(fx.executor.requests[0].Caches) != 1 {
		t.Fatalf("exec 1 spawns/caches = %+v; want 1 cache mount", fx.executor.requests)
	}
	spawn1Cache := fx.executor.requests[0].Caches[0]
	if spawn1Cache.MountPath != "/kodema/cache/go-mod" {
		t.Fatalf("cache mount path = %q; want /kodema/cache/go-mod", spawn1Cache.MountPath)
	}

	// Simulate Execution 1 writing a cached artifact to /kodema/cache/go-mod and
	// a disposable file to /workspace.
	cachedFile := filepath.Join(spawn1Cache.HostPath, "mod.zip")
	if err := fx.files.WriteFile(ctx, cachedFile, []byte("cached-bytes"), 0o644); err != nil {
		t.Fatalf("write cached file: %v", err)
	}
	if err := fx.files.WriteFile(ctx, filepath.Join(ws1Dir, "scratch.txt"), []byte("ephemeral"), 0o644); err != nil {
		t.Fatalf("write scratch file: %v", err)
	}

	// Clean up Execution 1 workload and release its workspace.
	group1, err := GroupName(req1.Identity.WorkloadID)
	if err != nil {
		t.Fatalf("GroupName: %v", err)
	}
	fx.observer.setStatus(group1, GroupStatus{Present: true, LeaderExited: true, ExitCode: 0, GroupGone: true})
	removal1, err := fx.adapter.Remove(ctx, engine.RemoveRequest{Identity: req1.Identity, WorkspaceID: ws1})
	if err != nil || !removal1.Removed {
		t.Fatalf("Remove exec 1 = %+v (%v); want removed", removal1, err)
	}
	if err := fx.adapter.Release(ctx, ws1); err != nil {
		t.Fatalf("Release ws1: %v", err)
	}

	// Execution 2 in the SAME project gets a fresh /workspace, while seeing the
	// persisted cache file from Execution 1!
	ws2 := id.WorkspaceID("ws-cache-2")
	ws2Dir := makeWorkspace(ws2)
	req2 := fx.request
	req2.Identity = engine.EffectIdentity{ExecutionID: "exec-cache-2", ActionID: "act-1", WorkloadID: "wl-cache-2", IdempotencyKey: "key-2"}
	req2.WorkspaceID = ws2
	req2.Container.Caches = []string{"go-mod"}
	req2.Caches = cachesA

	if _, err := fx.adapter.Prepare(ctx, req2); err != nil {
		t.Fatalf("Prepare exec 2: %v", err)
	}
	if _, err := fx.adapter.Start(ctx, engine.StartRequest{Identity: req2.Identity, WorkspaceID: ws2}); err != nil {
		t.Fatalf("Start exec 2: %v", err)
	}
	spawn2Cache := fx.executor.requests[1].Caches[0]
	if spawn2Cache.HostPath != spawn1Cache.HostPath {
		t.Fatalf("same-project cache path changed: %q vs %q", spawn2Cache.HostPath, spawn1Cache.HostPath)
	}
	gotCached, err := fx.files.ReadFile(ctx, filepath.Join(spawn2Cache.HostPath, "mod.zip"))
	if err != nil || string(gotCached) != "cached-bytes" {
		t.Fatalf("cache did not persist across executions: %q (%v)", string(gotCached), err)
	}
	if _, err := fx.files.Stat(ctx, filepath.Join(ws2Dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Fatalf("exec 2 /workspace was not fresh/isolated: stat err = %v", err)
	}

	// Execution 3 in proj-b gets an isolated cache path and cannot see proj-a's cache!
	ws3 := id.WorkspaceID("ws-cache-3")
	makeWorkspace(ws3)
	req3 := fx.request
	req3.Identity = engine.EffectIdentity{ExecutionID: "exec-cache-3", ActionID: "act-1", WorkloadID: "wl-cache-3", IdempotencyKey: "key-3"}
	req3.WorkspaceID = ws3
	req3.Container.Caches = []string{"go-mod"}
	req3.Caches = cachesB

	if _, err := fx.adapter.Prepare(ctx, req3); err != nil {
		t.Fatalf("Prepare exec 3 (proj-b): %v", err)
	}
	if _, err := fx.adapter.Start(ctx, engine.StartRequest{Identity: req3.Identity, WorkspaceID: ws3}); err != nil {
		t.Fatalf("Start exec 3 (proj-b): %v", err)
	}
	spawn3Cache := fx.executor.requests[2].Caches[0]
	if spawn3Cache.HostPath == spawn1Cache.HostPath {
		t.Fatalf("cross-project cache isolation violated: both used %q", spawn3Cache.HostPath)
	}
	if _, err := fx.files.Stat(ctx, filepath.Join(spawn3Cache.HostPath, "mod.zip")); !os.IsNotExist(err) {
		t.Fatalf("proj-b saw proj-a cached file!")
	}

	// Non-authoritative: evicting the cache directory before Prepare/Start starts empty and succeeds.
	if err := fx.files.RemoveAll(ctx, spawn1Cache.HostPath); err != nil {
		t.Fatalf("evict cache dir: %v", err)
	}
	ws4 := id.WorkspaceID("ws-cache-4")
	makeWorkspace(ws4)
	req4 := fx.request
	req4.Identity = engine.EffectIdentity{ExecutionID: "exec-cache-4", ActionID: "act-1", WorkloadID: "wl-cache-4", IdempotencyKey: "key-4"}
	req4.WorkspaceID = ws4
	req4.Container.Caches = []string{"go-mod"}
	req4.Caches = cachesA
	if _, err := fx.adapter.Prepare(ctx, req4); err != nil {
		t.Fatalf("Prepare after cache eviction failed (caches must be non-authoritative): %v", err)
	}
	if _, err := fx.files.Stat(ctx, spawn1Cache.HostPath); err != nil {
		t.Fatalf("evicted cache directory was not recreated empty: %v", err)
	}
}
