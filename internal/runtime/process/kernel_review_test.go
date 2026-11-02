package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// savedExit reports whether the record of the workload durably carries the exit
// its supervisor collected.
func (e *kenv) savedExit(id string) bool {
	e.t.Helper()
	rec, found, err := e.k.h.st.load(typeWorkload, objectKey(kScope, typeWorkload, id))
	return err == nil && found && rec.ExitKnown
}

func pidFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	kAwait(t, "the child pid", func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	})
	return pid
}

// Test_process_kernel_group_termination_is_confirmed: the leader's status and
// the group's termination are separate facts. A workload whose leader ended
// while a descendant still runs is not finished, still holds its workspace, and
// is reported stopped or removed only once the whole group is verifiably gone.
func Test_process_kernel_group_termination_is_confirmed(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"stop", "remove"} {
		t.Run(op, func(t *testing.T) {
			e := newKenv(t)
			// Leave what the leader left behind: the supervisor's own kill must not
			// be what makes the group disappear.
			e.k.h.killOnExit = func(int) {}
			ws := e.workspace(e.run)
			wsDir := e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity))
			b := e.binding("survivor-"+op, 1)
			if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, `sleep 300 & echo $! > child.pid`)); err != nil {
				t.Fatal(err)
			}
			child := pidFile(t, filepath.Join(wsDir, "child.pid"))
			pgid := e.pgid(typeWorkload, b.WorkloadID)
			kAwait(t, "the leader's exit to be recorded", func() bool { return e.savedExit(b.WorkloadID) })
			time.Sleep(50 * time.Millisecond)

			if syscall.Kill(child, 0) != nil || groupGone(pgid) {
				t.Fatal("the descendant did not survive its leader")
			}
			if obs := e.observe(b); obs.Phase != kr.PhaseRunning {
				t.Fatalf("a workload whose group still runs is %s", obs.Phase)
			}
			if id, held, err := e.k.Resources.WorkspaceHolder(ctx, ws); err != nil || !held || id != b.WorkloadID {
				t.Fatalf("the workspace was released while a descendant runs: %q %v %v", id, held, err)
			}
			if _, err := e.k.Runtime.Start(ctx, e.request(e.binding("next", 1), ws, "true")); !errors.Is(err, ErrWorkspaceBusy) {
				t.Fatalf("a step started on a workspace a descendant holds: %v", err)
			}

			var obs kr.WorkloadObservation
			var err error
			if op == "stop" {
				obs, err = e.k.Runtime.Stop(ctx, b)
			} else {
				obs, err = e.k.Runtime.Remove(ctx, b)
			}
			if err != nil {
				t.Fatal(err)
			}
			if op == "stop" && (obs.Phase != kr.PhaseExited || obs.Result.Code != kr.ResultSucceeded) {
				t.Fatalf("stop: %+v", obs)
			}
			if op == "remove" && obs.Phase != kr.PhaseAbsent {
				t.Fatalf("remove: %+v", obs)
			}
			if !groupGone(pgid) {
				t.Fatalf("%s was confirmed while the group still runs", op)
			}
			if _, held, err := e.k.Resources.WorkspaceHolder(ctx, ws); err != nil || held {
				t.Fatalf("the workspace is still held after %s: %v %v", op, held, err)
			}
			if _, err := e.k.Runtime.Start(ctx, e.request(e.binding("after", 1), ws, "true")); err != nil {
				t.Fatalf("the released workspace refuses a step: %v", err)
			}
		})
	}
}

// Test_process_kernel_admitted_fence_is_persisted: a writer that was accepted
// supersedes every lower fence even when its call took a fast path (already
// stopped), found the workspace held, or failed to terminate; the fence
// survives reopening the adapter.
func Test_process_kernel_admitted_fence_is_persisted(t *testing.T) {
	ctx := context.Background()

	t.Run("a stopped workload", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("fast", 1)
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.k.Runtime.Stop(ctx, b); err != nil {
			t.Fatal(err)
		}
		b3 := b
		b3.Fence = 3
		if _, err := e.k.Runtime.Stop(ctx, b3); err != nil {
			t.Fatal(err)
		}
		e.restart()
		b2 := b
		b2.Fence = 2
		if _, err := e.k.Runtime.Remove(ctx, b2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a remove at an intermediate fence: %v", err)
		}
		if _, err := e.k.Runtime.Stop(ctx, b2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stop at an intermediate fence: %v", err)
		}
		if obs, err := e.k.Runtime.Remove(ctx, b3); err != nil || obs.Phase != kr.PhaseAbsent {
			t.Fatalf("the admitted writer removes: %+v %v", obs, err)
		}
	})

	t.Run("a stopped service", func(t *testing.T) {
		e := newKenv(t)
		spec := e.service("fast", 1, "exec sleep 300")
		obs, err := e.k.Resources.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		h := obs.Handle
		if _, err := e.k.Resources.Stop(ctx, h); err != nil {
			t.Fatal(err)
		}
		h3 := h
		h3.Fence = 3
		if o, err := e.k.Resources.Stop(ctx, h3); err != nil || o.State != kr.ResourceStopped {
			t.Fatalf("%+v %v", o, err)
		}
		e.restart()
		h2 := h
		h2.Fence = 2
		if _, err := e.k.Resources.Remove(ctx, h2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a remove at an intermediate fence: %v", err)
		}
		spec.Fence = 2
		if _, err := e.k.Resources.Ensure(ctx, spec); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("an ensure at an intermediate fence: %v", err)
		}
		if o, err := e.k.Resources.Remove(ctx, h3); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("%+v %v", o, err)
		}
	})

	t.Run("a stopped workspace", func(t *testing.T) {
		e := newKenv(t)
		h := e.workspace(e.run)
		if o, err := e.k.Resources.Stop(ctx, h); err != nil || o.State != kr.ResourceStopped {
			t.Fatalf("%+v %v", o, err)
		}
		h3 := h
		h3.Fence = 3
		if o, err := e.k.Resources.Stop(ctx, h3); err != nil || o.State != kr.ResourceStopped {
			t.Fatalf("%+v %v", o, err)
		}
		e.restart()
		h2 := h
		h2.Fence = 2
		if _, err := e.k.Resources.Remove(ctx, h2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a remove at an intermediate fence: %v", err)
		}
		if o, err := e.k.Resources.Remove(ctx, h3); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("%+v %v", o, err)
		}
	})

	t.Run("a held workspace", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		if _, err := e.k.Runtime.Start(ctx, e.request(e.binding("holder", 1), ws, "exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		for _, f := range []kr.Fence{1, 3} {
			h := ws
			h.Fence = f
			if o, err := e.k.Resources.Stop(ctx, h); err != nil || o.State != kr.ResourceUnknown {
				t.Fatalf("a held workspace at fence %d: %+v %v", f, o, err)
			}
		}
		e.restart()
		h2 := ws
		h2.Fence = 2
		if _, err := e.k.Resources.Stop(ctx, h2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stop at an intermediate fence: %v", err)
		}
		if _, err := e.k.Resources.Remove(ctx, h2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a remove at an intermediate fence: %v", err)
		}
	})

	t.Run("an uncertain termination", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("stubborn", 1)
		// It ignores SIGTERM, so a stop with a cancelled context cannot finish.
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "trap '' TERM; exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		for _, f := range []kr.Fence{1, 3} {
			bf := b
			bf.Fence = f
			if _, err := e.k.Runtime.Stop(cancelled, bf); !errors.Is(err, kr.ErrUncertain) {
				t.Fatalf("an interrupted stop at fence %d: %v", f, err)
			}
		}
		e.restart()
		b2 := b
		b2.Fence = 2
		if _, err := e.k.Runtime.Remove(ctx, b2); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a remove at an intermediate fence: %v", err)
		}
	})
}

// Test_process_kernel_backend_ownership: every runtime operation refuses a
// binding of another backend before it reads or creates any record.
func Test_process_kernel_backend_ownership(t *testing.T) {
	ctx := context.Background()
	e := newKenv(t)
	ws := e.workspace(e.run)
	mine := e.binding("owned", 1)
	if _, err := e.k.Runtime.Start(ctx, e.request(mine, ws, "exec sleep 300")); err != nil {
		t.Fatal(err)
	}
	unknown := e.binding("unknown", 1)

	for _, b := range []kr.RuntimeBinding{mine, unknown} {
		foreign := b
		foreign.Backend = "podman"
		ops := map[string]func() error{
			"observe": func() error { _, err := e.k.Runtime.Observe(ctx, foreign); return err },
			"logs":    func() error { _, err := e.k.Runtime.Logs(ctx, foreign, kr.LogOptions{}); return err },
			"stop":    func() error { _, err := e.k.Runtime.Stop(ctx, foreign); return err },
			"remove":  func() error { _, err := e.k.Runtime.Remove(ctx, foreign); return err },
		}
		for name, op := range ops {
			err := op()
			if !errors.Is(err, domain.ErrInvalidValue) {
				t.Fatalf("%s of %s with a foreign backend: %v", name, b.WorkloadID, err)
			}
			if (name == "stop" || name == "remove") && !errors.Is(err, kr.ErrNotReached) {
				t.Fatalf("%s of %s with a foreign backend is not proven unreached: %v", name, b.WorkloadID, err)
			}
		}
	}
	if obs := e.observe(mine); obs.Phase != kr.PhaseRunning {
		t.Fatalf("a foreign backend's stop or remove reached the workload: %s", obs.Phase)
	}
	if _, found, err := e.k.h.st.load(typeWorkload, objectKey(kScope, typeWorkload, unknown.WorkloadID)); err != nil || found {
		t.Fatalf("a foreign backend's stop left a record: found=%v err=%v", found, err)
	}
	if _, err := e.k.Runtime.Start(ctx, e.request(unknown, ws, "true")); err == nil {
		// The identity is free: the workspace is held, so only exclusion refuses it.
		t.Fatal("the workspace is held by the first workload")
	} else if errors.Is(err, ErrFenced) {
		t.Fatalf("a foreign backend's stop fenced the identity: %v", err)
	}
}

// Test_process_kernel_reconcile_respects_takeover: reconciliation acts with the
// authority of the record it selected. An orphan an engine took over after the
// selection (its fence advanced) is skipped, never stopped.
func Test_process_kernel_reconcile_respects_takeover(t *testing.T) {
	ctx := context.Background()
	e := newKenv(t)
	orphan := domain.RunID("run_orphan")
	ws := e.workspace(orphan)
	b := e.binding("adoptee", 1)
	b.RunID = orphan
	req := e.request(b, ws, "exec sleep 300")
	if _, err := e.k.Runtime.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	pgid := e.pgid(typeWorkload, b.WorkloadID)

	tookOver := false
	owned := func(_ domain.Scope, run domain.RunID) bool {
		if run != orphan {
			return true
		}
		if !tookOver {
			// Between the selection of the orphan and its removal, a new engine adopts it.
			tookOver = true
			takeover := req
			takeover.Binding.Fence = 2
			if _, err := e.k.Runtime.Start(ctx, takeover); err != nil {
				t.Errorf("takeover: %v", err)
			}
		}
		return false
	}
	rep, err := e.k.Reconcile(ctx, owned)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(rep.Skipped, b.WorkloadID) || containsAll(rep.Removed, b.WorkloadID) {
		t.Fatalf("report %+v", rep)
	}
	if groupGone(pgid) {
		t.Fatal("reconciliation killed a workload that was taken over")
	}
	obs := e.observe(func() kr.RuntimeBinding { x := b; x.Fence = 2; return x }())
	if obs.Phase != kr.PhaseRunning {
		t.Fatalf("the adopted workload is %s", obs.Phase)
	}
	rec, _, err := e.k.h.st.load(typeWorkload, objectKey(kScope, typeWorkload, b.WorkloadID))
	if err != nil || rec.Fence != 2 || rec.Stopping || rec.Removed {
		t.Fatalf("the adopted record was changed: %+v %v", rec, err)
	}
}

// Test_process_kernel_exit_evidence_survives_failed_save: a transient storage
// failure while recording the exit must not turn the result into a lost
// workload or a cancellation.
func Test_process_kernel_exit_evidence_survives_failed_save(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not stop root")
	}
	ctx := context.Background()
	for _, op := range []string{"observe", "stop"} {
		t.Run(op, func(t *testing.T) {
			e := newKenv(t)
			ws := e.workspace(e.run)
			b := e.binding("evidence-"+op, 1)
			records := filepath.Join(e.dir, kRecordsDir)
			if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "sleep 0.3; exit 3")); err != nil {
				t.Fatal(err)
			}
			// From here on nothing can be saved.
			if err := os.Chmod(records, 0o500); err != nil {
				t.Fatal(err)
			}
			restore := func() { _ = os.Chmod(records, 0o700) }
			t.Cleanup(restore)
			kAwait(t, "the leader to end", func() bool {
				p := e.k.h.proc(typeWorkload, objectKey(kScope, typeWorkload, b.WorkloadID))
				return p == nil || p.ended()
			})
			if !e.k.Idle(10 * time.Second) {
				t.Fatal("the supervisor did not finish")
			}
			if e.savedExit(b.WorkloadID) {
				t.Fatal("the exit was saved although storage failed")
			}

			// The failed call reports the failure; it does not invent an outcome.
			if op == "observe" {
				if obs, err := e.k.Runtime.Observe(ctx, b); err == nil {
					t.Fatalf("observe with failing storage: %+v", obs)
				}
			} else if _, err := e.k.Runtime.Stop(ctx, b); err == nil {
				t.Fatal("stop with failing storage")
			}
			restore()

			want := kr.Result{Code: kr.ResultFailed, ExitCode: 3, Reason: kr.ReasonCommandFailed}
			var obs kr.WorkloadObservation
			var err error
			if op == "observe" {
				obs, err = e.k.Runtime.Observe(ctx, b)
			} else {
				obs, err = e.k.Runtime.Stop(ctx, b)
			}
			if err != nil || obs.Phase != kr.PhaseExited || obs.Result != want {
				t.Fatalf("after the storage recovered: %+v %v, want %+v", obs, err, want)
			}
			if again := e.observe(b); again.Result != want {
				t.Fatalf("a later observe: %+v", again)
			}
			if stopped, err := e.k.Runtime.Stop(ctx, b); err != nil || stopped.Result != want {
				t.Fatalf("a later stop: %+v %v", stopped, err)
			}
		})
	}
}
