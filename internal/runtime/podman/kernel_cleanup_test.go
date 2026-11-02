package podman_test

import (
	"context"
	"testing"
	"time"

	kr "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/runtime/podman"
)

// Regression tests for the third review of the kernel adapters: the network
// cleanup obligation, the network fence, and positive evidence of refusal. They
// run against the real engine (see kernel_contract_test.go).

func (f *kFix) leanService(name string, fence kr.Fence) kr.ResourceSpec {
	s := f.serviceSpec(name, fence, "exec sleep 120")
	s.Service.Ports = nil
	return s
}

func atFence(h kr.ResourceHandle, fence kr.Fence) kr.ResourceHandle { h.Fence = fence; return h }

// Test_unit_podman_kernel_network_cleanup_obligation proves a removal is not
// confirmed, by Remove or by a later Observe, until the run network's cleanup is
// met, and that the obligation survives a restart.
func Test_unit_podman_kernel_network_cleanup_obligation(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	netName := func(f *kFix) string { return podman.NetworkObjectName(kScope, f.run) }

	t.Run("an unresolved network create", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("o1", 1)
		f.flag("swallow-network-create", "1")
		if _, err := f.res.Ensure(ctx, svc); err == nil {
			t.Fatal("ensure")
		}
		for i, restart := range []bool{false, true, true} {
			if restart {
				f.open()
			}
			rm, err := f.res.Remove(ctx, handle(svc))
			if err != nil || rm.State != kr.ResourceUnknown {
				t.Fatalf("remove %d: %+v, %v", i, rm, err)
			}
			if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.State != kr.ResourceUnknown {
				t.Fatalf("observe %d after an unconfirmed removal: %+v, %v", i, o, err)
			}
		}
		f.plantOwnedNetwork(netName(f)) // the delayed create lands
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("a landed network is not yet cleaned: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		f.open()
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("observe after the cleanup: %+v, %v", o, err)
		}
		if exists("network", netName(f)) {
			t.Fatal("the network survived")
		}
	})

	t.Run("a failed network removal", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("o2", 1)
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		f.flag("fail-network-rm", "1")
		if rm, err := f.res.Remove(ctx, handle(svc)); err == nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		for _, restart := range []bool{false, true} {
			if restart {
				f.open()
			}
			if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.State != kr.ResourceUnknown {
				t.Fatalf("a removal whose network cleanup failed was confirmed by Observe: %+v, %v", o, err)
			}
		}
		f.unflag("fail-network-rm")
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("retry: %+v, %v", rm, err)
		}
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if exists("network", netName(f)) || len(f.objects()) != 0 {
			t.Fatalf("left %v", f.objects())
		}
	})

	t.Run("a failure when the last attached workload is removed", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		svc := f.leanService("o3", 1)
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		b := f.binding("last", 1)
		mustStart(t, f, f.start(b, "true", func(r *kr.StartRequest) { r.Workspace = handle(ws); r.Services = []kr.ResourceHandle{handle(svc)} }))
		f.awaitPhase(ctx, b, kr.PhaseExited)

		// The service goes first: the workload still uses the network, so the
		// workload owns the cleanup now, and the service itself is confirmed.
		if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove the service: %+v, %v", rm, err)
		}
		f.flag("fail-network-rm", "1")
		if o, err := f.rt.Remove(ctx, b); err == nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("the last holder's cleanup failure was discarded: %+v, %v", o, err)
		}
		if !exists("network", netName(f)) {
			t.Fatal("the network vanished")
		}
		for _, restart := range []bool{false, true} {
			if restart {
				f.open()
			}
			if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
				t.Fatalf("a workload whose network cleanup failed is %+v, %v", o, err)
			}
		}
		f.unflag("fail-network-rm")
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("retry: %+v, %v", o, err)
		}
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if exists("network", netName(f)) {
			t.Fatal("the network was orphaned")
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})
}

// Test_unit_podman_kernel_network_cleanup_fence proves network cleanup honours
// the durable network fence: a lower-fence handle of any resource cannot delete a
// network a higher-fence service advanced.
func Test_unit_podman_kernel_network_cleanup_fence(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := newKFix(t)
	netName := podman.NetworkObjectName(kScope, f.run)

	ws := f.workspaceSpec(1)
	if _, err := f.res.Ensure(ctx, ws); err != nil {
		t.Fatal(err)
	}
	a := f.leanService("fa", 1)
	if _, err := f.res.Ensure(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := f.leanService("fb", 2) // the takeover: advances the run network to fence 2
	if _, err := f.res.Ensure(ctx, b); err != nil {
		t.Fatal(err)
	}
	if m := f.record(netName); m["fence"] != float64(2) {
		t.Fatalf("the network fence is %v, want 2", m["fence"])
	}
	f.awaitState(ctx, handle(a), kr.ResourceReady)
	f.awaitState(ctx, handle(b), kr.ResourceReady)

	// Lower-fence cleanups, through a service and through the workspace, are
	// refused before any effect.
	for name, h := range map[string]kr.ResourceHandle{"service": handle(a), "workspace": handle(ws)} {
		if _, err := f.res.Remove(ctx, h); err == nil {
			t.Fatalf("a stale %s cleanup was accepted", name)
		} else {
			is(t, err, kr.ErrStaleFence)
		}
	}
	if !exists("network", netName) {
		t.Fatal("a lower-fence handle deleted the network")
	}
	if o, _ := f.res.Observe(ctx, handle(a)); o.State != kr.ResourceReady {
		t.Fatalf("the stale cleanup disturbed the service: %+v", o)
	}
	if m := f.record(netName); m["fence"] != float64(2) || m["cleaning"] == true || m["removed"] == true {
		t.Fatalf("the stale cleanup changed the network record: %v", m)
	}

	// The current fence is accepted: A goes (B still uses the network), then B,
	// whose cleanup intent is persisted before the removal that fails once.
	if rm, err := f.res.Remove(ctx, atFence(handle(a), 2)); err != nil || !rm.ConfirmedRemoved() {
		t.Fatalf("remove a: %+v, %v", rm, err)
	}
	if !exists("network", netName) {
		t.Fatal("the network in use was removed")
	}
	f.flag("fail-network-rm", "1")
	if _, err := f.res.Remove(ctx, handle(b)); err == nil {
		t.Fatal("remove b")
	}
	if m := f.record(netName); m["cleaning"] != true || m["fence"] != float64(2) {
		t.Fatalf("the accepted cleanup intent was not persisted before the removal: %v", m)
	}
	f.unflag("fail-network-rm")
	if rm, err := f.res.Remove(ctx, handle(b)); err != nil || !rm.ConfirmedRemoved() {
		t.Fatalf("retry: %+v, %v", rm, err)
	}
	if rm, err := f.res.Remove(ctx, atFence(handle(ws), 2)); err != nil || !rm.ConfirmedRemoved() {
		t.Fatalf("remove ws: %+v, %v", rm, err)
	}
	if exists("network", netName) || len(f.objects()) != 0 {
		t.Fatalf("left %v", f.objects())
	}
}

// Test_unit_podman_kernel_unrecognized_failure_stays_uncertain proves a CLI that
// is killed, or fails in a way the adapter does not recognize, never clears an
// outstanding create, while a positive refusal by the engine does.
func Test_unit_podman_kernel_unrecognized_failure_stays_uncertain(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Run("container", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("killed", 1)
		name := podman.WorkloadObjectName(kScope, "killed")
		f.flag("kill-container-create", "1") // a foreign object takes the name; the CLI dies with no answer
		_, err := f.rt.Start(ctx, f.start(b, "sleep 60"))
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("start: %v", err)
		}
		if m := f.record(name); m["unanswered"] != true {
			t.Fatalf("a killed CLI cleared the outstanding create: %v", m)
		}
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("removal confirmed with the create unresolved: %+v, %v", o, err)
		}

		// Control: the engine's own refusal of a taken name is positive evidence.
		c := f.binding("refused", 1)
		f.flag("collide-container-create", "1")
		_, err = f.rt.Start(ctx, f.start(c, "sleep 60"))
		is(t, err, kr.ErrSpecMismatch)
		if kr.ClassifyEffect(err) == kr.CommandConfirmed {
			t.Fatalf("start: %v", err)
		}
		if m := f.record(podman.WorkloadObjectName(kScope, "refused")); m["unanswered"] == true {
			t.Fatalf("an engine refusal left the create unresolved: %v", m)
		}
		if o, err := f.rt.Remove(ctx, c); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
	})

	t.Run("volume", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		name := podman.VolumeObjectName(kScope, ws.Identity)
		f.flag("kill-volume-create", "1")
		_, err := f.res.Ensure(ctx, ws)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		if m := f.record(name); m["unanswered"] != true {
			t.Fatalf("a killed CLI cleared the outstanding create: %v", m)
		}
		if o, err := f.res.Observe(ctx, handle(ws)); err != nil || o.State != kr.ResourceUnknown {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if o, err := f.res.Remove(ctx, handle(ws)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("removal confirmed with the create unresolved: %+v, %v", o, err)
		}

		g := newKFix(t) // control
		ws2 := g.workspaceSpec(1)
		g.flag("collide-volume-create", "1")
		_, err = g.res.Ensure(ctx, ws2)
		is(t, err, kr.ErrSpecMismatch)
		if m := g.record(podman.VolumeObjectName(kScope, ws2.Identity)); m["unanswered"] == true {
			t.Fatalf("an engine refusal left the create unresolved: %v", m)
		}
	})

	t.Run("network", func(t *testing.T) {
		f := newKFix(t)
		svc := f.leanService("k1", 1)
		name := podman.NetworkObjectName(kScope, f.run)
		f.flag("kill-network-create", "1")
		_, err := f.res.Ensure(ctx, svc)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		if m := f.record(name); m["unanswered"] != true {
			t.Fatalf("a killed CLI cleared the outstanding create: %v", m)
		}
		if o, err := f.res.Remove(ctx, handle(svc)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("cleanup confirmed with the network create unresolved: %+v, %v", o, err)
		}

		g := newKFix(t) // control
		svc2 := g.leanService("k2", 1)
		g.flag("collide-network-create", "1")
		_, err = g.res.Ensure(ctx, svc2)
		is(t, err, kr.ErrSpecMismatch)
		if m := g.record(podman.NetworkObjectName(kScope, g.run)); m["unanswered"] == true {
			t.Fatalf("an engine refusal left the create unresolved: %v", m)
		}
	})
}
