package podman_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	kr "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/runtime/podman"
)

// Fault injection between a removal and the writes that follow it. The network
// cleanup obligation is persisted with the removal intent, before the object can
// be made absent, so no interleaving of a crash or a failed write lets a
// restarted Observe confirm a removal whose cleanup is still owed. A directory
// made read-only makes every later write to that record fail.

func (f *kFix) recDir(name string) string { return filepath.Join(f.state, "kernel", name) }

// readOnly makes the record directory of name reject writes until restored; it
// is always restored at the end of the test.
func (f *kFix) readOnly(name string) (restore func()) {
	f.t.Helper()
	dir := f.recDir(name)
	if err := os.Chmod(dir, 0o500); err != nil {
		f.t.Fatal(err)
	}
	restore = func() { _ = os.Chmod(dir, 0o700) }
	f.t.Cleanup(restore)
	return restore
}

func Test_unit_podman_kernel_cleanup_obligation_ordering(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Run("a failed write of the obligation removes nothing", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("w1", 1)
		name := podman.ServiceObjectName(kScope, svc.Identity)
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		restore := f.readOnly(name)
		if rm, err := f.res.Remove(ctx, handle(svc)); err == nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove with the obligation unwritable: %+v, %v", rm, err)
		}
		if !exists("container", name) {
			t.Fatal("the object was removed although its obligation could not be recorded")
		}
		restore()
		f.open()
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.State != kr.ResourceReady {
			t.Fatalf("the service was disturbed: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("retry: %+v, %v", rm, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})

	t.Run("a crash after the service container is removed", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("w2", 1)
		name := podman.ServiceObjectName(kScope, svc.Identity)
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		f.flag("chmod-after-rm", f.recDir(name)) // every write after the container is gone fails
		if rm, err := f.res.Remove(ctx, handle(svc)); err == nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if exists("container", name) {
			t.Fatal("the container was not removed: the fault did not model the crash")
		}
		_ = os.Chmod(f.recDir(name), 0o700)
		f.open() // restart
		if m := f.record(name); m["netPending"] != true || m["removed"] == true {
			t.Fatalf("the obligation must be recorded before the object disappears: %v", m)
		}
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("a restarted Observe confirmed the removal: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("retry: %+v, %v", rm, err)
		}
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})

	t.Run("an unwritable network record after the parent is removed", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("w3", 1)
		netName := podman.NetworkObjectName(kScope, f.run)
		f.flag("swallow-network-create", "1") // the network create is unresolved
		if _, err := f.res.Ensure(ctx, svc); err == nil {
			t.Fatal("ensure")
		}
		restore := f.readOnly(netName) // the cleanup intent cannot be written
		if rm, err := f.res.Remove(ctx, handle(svc)); err == nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		restore()
		f.open()
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("a restarted Observe confirmed the removal with the network unresolved: %+v, %v", o, err)
		}
		f.plantOwnedNetwork(netName) // the delayed create lands
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("retry: %+v, %v", rm, err)
		}
		if exists("network", netName) || len(f.objects()) != 0 {
			t.Fatalf("left %v", f.objects())
		}
	})

	t.Run("a crash after the workload container is removed", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		svc := f.leanService("w4", 1)
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		b := f.binding("crashing", 1)
		mustStart(t, f, f.start(b, "true", func(r *kr.StartRequest) { r.Workspace = handle(ws); r.Services = []kr.ResourceHandle{handle(svc)} }))
		f.awaitPhase(ctx, b, kr.PhaseExited)
		name := podman.WorkloadObjectName(kScope, b.WorkloadID)

		f.flag("chmod-after-rm", f.recDir(name))
		if o, err := f.rt.Remove(ctx, b); err == nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
		if exists("container", name) || len(f.objects()) == 0 {
			t.Fatal("the fault did not model the crash")
		}
		_ = os.Chmod(f.recDir(name), 0o700)
		f.open()
		if m := f.record(name); m["netPending"] != true {
			t.Fatalf("the obligation must be recorded before the container disappears: %v", m)
		}
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("a restarted Observe confirmed the removal: %+v, %v", o, err)
		}
		// The service still uses the network, so the retry hands the cleanup over.
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("retry: %+v, %v", o, err)
		}
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		for _, h := range []kr.ResourceHandle{handle(svc), handle(ws)} {
			if rm, err := f.res.Remove(ctx, h); err != nil || !rm.ConfirmedRemoved() {
				t.Fatalf("remove %s: %+v, %v", h.Name, rm, err)
			}
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})
}
