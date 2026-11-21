package processdriver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/runtime/process"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

func TestDriver_CachesPersistAcrossExecutionsAndIsolateProjects(t *testing.T) {
	ctx := context.Background()
	driver := newTestDriver(t, 0)

	root := t.TempDir()
	wsRoot := t.TempDir()
	adapter, err := process.New(process.Config{
		Root:          root,
		WorkspaceRoot: wsRoot,
		KodemaURL:     "http://127.0.0.1:8080",
		Executor:      driver,
		Observer:      driver,
		Files:         driver,
		Cleanup:       process.CleanupPolicy{Attempts: 2},
	})
	if err != nil {
		t.Fatalf("process.New: %v", err)
	}

	projA := tenancy.Scope{TenantID: "t1", ProjectID: "proj-a"}
	projB := tenancy.Scope{TenantID: "t1", ProjectID: "proj-b"}
	cachesA, err := workspace.BuildCacheMounts(projA, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projA): %v", err)
	}
	cachesB, err := workspace.BuildCacheMounts(projB, []string{"go-mod"})
	if err != nil {
		t.Fatalf("BuildCacheMounts(projB): %v", err)
	}

	makeWS := func(wsID id.WorkspaceID) string {
		t.Helper()
		dir, err := process.WorkspaceDir(wsRoot, wsID)
		if err != nil {
			t.Fatalf("WorkspaceDir: %v", err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		return dir
	}

	runExec := func(execID id.ExecutionID, wlID id.WorkloadID, wsID id.WorkspaceID, caches []engine.CacheMount, commands []string) string {
		t.Helper()
		makeWS(wsID)
		ident := engine.EffectIdentity{
			ExecutionID:    execID,
			ActionID:       "act-1",
			WorkloadID:     wlID,
			IdempotencyKey: "key-" + string(wlID),
		}
		if _, err := adapter.Prepare(ctx, engine.PrepareRequest{
			Identity:    ident,
			WorkspaceID: wsID,
			ActionName:  "build",
			Credential:  "kodex." + string(execID) + ".tok",
			Container: definition.Container{
				Image:    "local/test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Commands: commands,
				Caches:   []string{"go-mod"},
			},
			Caches: caches,
		}); err != nil {
			t.Fatalf("Prepare(%s): %v", execID, err)
		}
		if _, err := adapter.Start(ctx, engine.StartRequest{Identity: ident, WorkspaceID: wsID}); err != nil {
			t.Fatalf("Start(%s): %v", execID, err)
		}
		group, err := process.GroupName(wlID)
		if err != nil {
			t.Fatalf("GroupName: %v", err)
		}
		status := waitStatus(t, driver, group, func(s process.GroupStatus) bool {
			return s.GroupGone && s.LeaderExited
		})
		logs, err := adapter.Logs(ctx, engine.LogsRequest{Identity: ident, Limit: process.MaxLogBytes})
		if err != nil {
			t.Fatalf("Logs(%s): %v", execID, err)
		}
		var out strings.Builder
		for _, chunk := range logs.Chunks {
			out.Write(chunk.Data)
		}
		if status.ExitCode != 0 {
			t.Fatalf("exec %s exited with %d; output=%q", execID, status.ExitCode, out.String())
		}
		removal, err := adapter.Remove(ctx, engine.RemoveRequest{Identity: ident, WorkspaceID: wsID})
		if err != nil || !removal.Removed {
			t.Fatalf("Remove(%s) = %+v (%v); want removed", execID, removal, err)
		}
		if err := adapter.Release(ctx, wsID); err != nil {
			t.Fatalf("Release(%s): %v", wsID, err)
		}
		return out.String()
	}

	// Execution 1 (proj-a): writes a file into $KODEMA_CACHE_DIR/go-mod and a scratch file into /workspace (cwd).
	out1 := runExec("exec-proc-cache-1", "wl-proc-cache-1", "ws-proc-cache-1", cachesA, []string{
		`test -d "$KODEMA_CACHE_DIR/go-mod"`,
		`test "$KODEMA_CACHE_GO_MOD" = "$KODEMA_CACHE_DIR/go-mod"`,
		`printf 'cached-from-exec-1' > "$KODEMA_CACHE_DIR/go-mod/mod.zip"`,
		`printf 'ephemeral-ws-1' > ./scratch.txt`,
		`echo "exec1-ok"`,
	})
	if !strings.Contains(out1, "exec1-ok") {
		t.Fatalf("exec 1 output = %q; want exec1-ok", out1)
	}

	// Execution 2 (same project proj-a): fresh /workspace (scratch.txt absent), cached file PERSISTS!
	out2 := runExec("exec-proc-cache-2", "wl-proc-cache-2", "ws-proc-cache-2", cachesA, []string{
		`test ! -e ./scratch.txt`,
		`cat "$KODEMA_CACHE_DIR/go-mod/mod.zip"`,
	})
	if strings.TrimSpace(out2) != "cached-from-exec-1" {
		t.Fatalf("exec 2 output = %q; want cached-from-exec-1", out2)
	}

	// Execution 3 (second project proj-b): cache is isolated (mod.zip must NOT exist)!
	out3 := runExec("exec-proc-cache-3", "wl-proc-cache-3", "ws-proc-cache-3", cachesB, []string{
		`test -d "$KODEMA_CACHE_DIR/go-mod"`,
		`test ! -e "$KODEMA_CACHE_DIR/go-mod/mod.zip"`,
		`echo "proj-b-isolated"`,
	})
	if !strings.Contains(out3, "proj-b-isolated") {
		t.Fatalf("exec 3 output = %q; want proj-b-isolated", out3)
	}
}

func TestDriver_SpawnRejectsInvalidCaches(t *testing.T) {
	driver := newTestDriver(t, 0)
	base := helperRequest(t, "group-bad-cache", "write")

	cases := []struct {
		name   string
		caches []process.CacheMount
	}{
		{
			name:   "invalid cache slug",
			caches: []process.CacheMount{{Name: "../escape", MountPath: "/kodema/cache/../escape", HostPath: filepath.Join(t.TempDir(), "c")}},
		},
		{
			name:   "relative host path",
			caches: []process.CacheMount{{Name: "go-mod", MountPath: "/kodema/cache/go-mod", HostPath: "relative/cache"}},
		},
		{
			name:   "mismatched mount path",
			caches: []process.CacheMount{{Name: "go-mod", MountPath: "/tmp/go-mod", HostPath: filepath.Join(t.TempDir(), "c")}},
		},
		{
			name: "duplicate cache name",
			caches: []process.CacheMount{
				{Name: "go-mod", MountPath: "/kodema/cache/go-mod", HostPath: filepath.Join(t.TempDir(), "c1")},
				{Name: "go-mod", MountPath: "/kodema/cache/go-mod", HostPath: filepath.Join(t.TempDir(), "c2")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.Group = process.GroupID("group-" + strings.ReplaceAll(tc.name, " ", "-"))
			req.Caches = tc.caches
			_, err := driver.Spawn(context.Background(), req)
			if err == nil {
				t.Fatalf("Spawn(%s) succeeded; want invalid_input", tc.name)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("Spawn(%s) fault class = %q (%v); want invalid_input (err=%v)", tc.name, class, ok, err)
			}
		})
	}
}
