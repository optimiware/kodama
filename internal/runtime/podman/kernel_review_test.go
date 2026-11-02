package podman_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/runtime/podman"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// Regression tests for the review of the kernel adapters, against the real
// engine (see kernel_contract_test.go for the engine and skip conventions).

// record reads the durable record of the object at name.
func (f *kFix) record(name string) map[string]any {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.state, "kernel", name, "record.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *kFix) labelsOf(name, class string) []string {
	f.t.Helper()
	m := f.record(name)
	return []string{
		"--label", podman.LabelManagedBy + "=" + podman.ValueKernelManagedBy,
		"--label", podman.LabelTenant + "=" + string(kScope.TenantID),
		"--label", podman.LabelProject + "=" + string(kScope.ProjectID),
		"--label", podman.LabelClass + "=" + class,
		"--label", podman.LabelUID + "=" + m["uid"].(string),
		"--label", podman.LabelSpecDigest + "=" + m["specDigest"].(string),
		"--label", podman.LabelRun + "=" + string(f.run),
	}
}

// plantOwned creates, without starting it, the container the adapter's create
// request would have made: it lands "late", after the adapter lost its answer.
func (f *kFix) plantOwned(name, class string) {
	f.t.Helper()
	args := append([]string{"create", "--name", name}, f.labelsOf(name, class)...)
	sh(f.t, "podman", append(args, podmantest.Image, "sleep", "60")...)
}

// plantOwnedNetwork creates the run network the adapter's create request would
// have made.
func (f *kFix) plantOwnedNetwork(name string) {
	f.t.Helper()
	args := append([]string{"network", "create", "--internal"}, f.labelsOf(name, "network")...)
	sh(f.t, "podman", append(args, name)...)
}

// plantForeign runs a container that is not the adapter's at name.
func (f *kFix) plantForeign(name string) {
	f.t.Helper()
	sh(f.t, "podman", "run", "-d", "--name", name, "--label", podman.LabelRun+"="+string(f.run), podmantest.Image, "sleep", "60")
}

func exists(kind, name string) bool { return exec.Command("podman", kind, "exists", name).Run() == nil }

func (f *kFix) awaitCalls(prefix string, n int) {
	f.t.Helper()
	kWait(f.t, "a call to "+prefix, func() (int, bool) { c := f.calls(prefix); return c, c >= n })
}

// Test_unit_podman_kernel_workspace_launch_fence proves a workload launch and the
// release of its workspace are serialized, and that a holder whose create is
// unresolved blocks release and removal.
func Test_unit_podman_kernel_workspace_launch_fence(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Run("a stop of the workspace waits for a launch in flight", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		f.flag("delay-create", "3") // the launch is slow at the engine
		b := f.binding("racing", 1)
		type outcome struct {
			o   kr.WorkloadObservation
			err error
		}
		started := make(chan outcome, 1)
		go func() {
			o, err := f.rt.Start(ctx, f.start(b, "sleep 60", func(r *kr.StartRequest) { r.Workspace = handle(ws) }))
			started <- outcome{o, err}
		}()
		f.awaitCalls("create", 1)

		t0 := time.Now()
		st, err := f.res.Stop(ctx, handle(ws))
		waited := time.Since(t0)
		if err != nil {
			t.Fatal(err)
		}
		if got := <-started; got.err != nil {
			t.Fatalf("start: %v", got.err)
		}
		if waited < time.Second {
			t.Fatalf("the stop of the workspace did not wait for the launch (%s)", waited)
		}
		// The launched workload holds the workspace: it is fenced but not released.
		if st.State == kr.ResourceStopped {
			t.Fatalf("a workspace with a running holder was reported stopped: %+v", st)
		}
		if o, _ := f.rt.Observe(ctx, b); o.Phase != kr.PhaseRunning {
			t.Fatalf("the workload: %+v", o)
		}
		// The stop fenced the workspace: nothing new launches on it.
		other := f.binding("late", 1)
		_, err = f.rt.Start(ctx, f.start(other, "true", func(r *kr.StartRequest) { r.Workspace = handle(ws) }))
		is(t, err, kr.ErrStaleFence)
		// Once the holder is gone the workspace is released.
		if _, err := f.rt.Remove(ctx, b); err != nil {
			t.Fatal(err)
		}
		if st, err := f.res.Stop(ctx, handle(ws)); err != nil || st.State != kr.ResourceStopped {
			t.Fatalf("stop: %+v, %v", st, err)
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
	})

	t.Run("a created container is not started after the workspace was stopped", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		b := f.binding("created", 1)
		req := f.start(b, "sleep 60", func(r *kr.StartRequest) { r.Workspace = handle(ws) })
		f.flag("lose-create", "1") // created, never started, response lost
		if _, err := f.rt.Start(ctx, req); err == nil {
			t.Fatal("a lost response was reported as success")
		}
		if st, err := f.res.Stop(ctx, handle(ws)); err != nil || st.State != kr.ResourceStopped {
			t.Fatalf("stop: %+v, %v", st, err)
		}
		// Adopting the existing container is a start: refused, and it never runs.
		_, err := f.rt.Start(ctx, req)
		is(t, err, kr.ErrStaleFence)
		if got := sh(t, "podman", "ps", "-a", "--filter", "label="+podman.LabelRun+"="+string(f.run), "--format", "{{.State}}"); got != "created" {
			t.Fatalf("the container is %q, want created", got)
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
	})

	t.Run("an unresolved create blocks the release and removal of the workspace", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		b := f.binding("unresolved", 1)
		req := f.start(b, "sleep 60", func(r *kr.StartRequest) { r.Workspace = handle(ws) })
		f.flag("swallow-create", "1") // the answer is lost and the engine holds nothing to scan
		if _, err := f.rt.Start(ctx, req); err == nil {
			t.Fatal("start")
		}
		if got := f.objects(); len(got) != 1 || !strings.HasPrefix(got[0], "volume:") {
			t.Fatalf("the engine holds %v: a scan of it cannot see the holder", got)
		}
		st, err := f.res.Stop(ctx, handle(ws))
		if err != nil || st.State != kr.ResourceUnknown {
			t.Fatalf("a workspace with an unresolved holder was released: %+v, %v", st, err)
		}
		_, err = f.res.Remove(ctx, handle(ws))
		is(t, err, kr.ErrUncertain)
		if !exists("volume", podman.VolumeObjectName(kScope, ws.Identity)) {
			t.Fatal("the volume was removed under an unresolved holder")
		}
		if o, err := f.res.Observe(ctx, handle(ws)); err != nil || o.State == kr.ResourceRemoved || o.State == kr.ResourceStopped {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		// The delayed create lands; resolving it through the owned instance releases the workspace.
		f.plantOwned(podman.WorkloadObjectName(kScope, "unresolved"), "workload")
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove the workload: %+v, %v", o, err)
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})
}

// Test_unit_podman_kernel_foreign_name_keeps_unresolved_create proves a foreign
// object at the name never clears the evidence of an outstanding create.
func Test_unit_podman_kernel_foreign_name_keeps_unresolved_create(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Run("a collision appearing and disappearing during an uncertain create", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("collide", 1)
		req := f.start(b, "sleep 60")
		name := podman.WorkloadObjectName(kScope, "collide")
		f.flag("swallow-create", "1")
		if _, err := f.rt.Start(ctx, req); err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("start: %v", err)
		}
		f.plantForeign(name)
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("a foreign object at the name resolved the create: %+v, %v", o, err)
		}
		// Not replayed, and not treated as nothing-to-wait-for.
		_, err := f.rt.Start(ctx, req)
		is(t, err, kr.ErrSpecMismatch)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates", n)
		}
		if o, err := f.rt.Stop(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("stop: %+v, %v", o, err)
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("removal was confirmed over a foreign object: %+v, %v", o, err)
		}
		if !exists("container", name) {
			t.Fatal("the foreign container was removed")
		}
		if m := f.record(name); m["unanswered"] != true {
			t.Fatalf("the unresolved create was cleared: %v", m)
		}

		sh(t, "podman", "rm", "--force", "--time", "0", name) // the collision disappears
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("a vanished collision resolved the create: %+v, %v", o, err)
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("removal was confirmed with the create unresolved: %+v, %v", o, err)
		}

		f.plantOwned(name, "workload") // the create lands: resolved through the owned instance
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
		if exists("container", name) {
			t.Fatal("the landed container survived a confirmed removal")
		}
		if m := f.record(name); m["unanswered"] == true || m["objectId"] == "" || m["objectId"] == nil {
			t.Fatalf("the create was not resolved through the owned instance: %v", m)
		}
	})

	t.Run("a collision without an outstanding create is never adopted or removed", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("squatter", 1)
		name := podman.WorkloadObjectName(kScope, "squatter")
		f.plantForeign(name)
		_, err := f.rt.Start(ctx, f.start(b, "sleep 60"))
		is(t, err, kr.ErrSpecMismatch)
		if f.calls("create") != 0 {
			t.Fatal("a create was issued over a foreign object")
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
		if !exists("container", name) {
			t.Fatal("the foreign container was removed")
		}
	})

	t.Run("a foreign volume does not resolve an unresolved volume create", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		f.flag("swallow-volume-create", "1") // nothing is created, the answer is lost
		if _, err := f.res.Ensure(ctx, ws); err == nil {
			t.Fatal("ensure")
		}
		name := podman.VolumeObjectName(kScope, ws.Identity)
		sh(t, "podman", "volume", "create", "--label", podman.LabelRun+"="+string(f.run), name)
		if o, err := f.res.Observe(ctx, handle(ws)); err != nil || o.State != kr.ResourceUnknown {
			t.Fatalf("a foreign volume resolved the create: %+v, %v", o, err)
		}
		if o, err := f.res.Remove(ctx, handle(ws)); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("removal confirmed over a foreign volume: %+v, %v", o, err)
		}
		if !exists("volume", name) {
			t.Fatal("the foreign volume was removed")
		}
	})
}

// Test_unit_podman_kernel_logs_do_not_roll_back_records proves reading logs
// cannot roll back a newer fence: the record is reloaded under the lock.
func Test_unit_podman_kernel_logs_do_not_roll_back_records(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	interleave := func(t *testing.T, f *kFix, observe, takeover, logs func()) {
		t.Helper()
		f.flag("delay-container-exists", "3") // the first engine lookup is slow: the lock is held
		var wg sync.WaitGroup
		run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
		run(observe)
		time.Sleep(700 * time.Millisecond)
		run(takeover) // queues for the lock behind the observation
		time.Sleep(300 * time.Millisecond)
		run(logs) // loads (in the old code) a record that the takeover is about to supersede
		wg.Wait()
	}

	t.Run("workload", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("logroll", 1)
		f.flag("lose-create", "1") // a created, unbound container
		if _, err := f.rt.Start(ctx, f.start(b, "echo hi; sleep 30")); err == nil {
			t.Fatal("start")
		}
		f.open()
		b2 := b
		b2.Fence = 2
		interleave(t, f,
			func() { _, _ = f.rt.Observe(ctx, b) },
			func() {
				if _, err := f.rt.Start(ctx, f.start(b2, "echo hi; sleep 30")); err != nil {
					t.Errorf("takeover start: %v", err)
				}
			},
			func() {
				rc, err := f.rt.Logs(ctx, b, kr.LogOptions{})
				if err != nil {
					t.Errorf("logs: %v", err)
					return
				}
				rc.Close()
			})
		_, err := f.rt.Start(ctx, f.start(b, "echo hi; sleep 30"))
		is(t, err, kr.ErrStaleFence) // fence 2 was not rolled back to 1
		_, err = f.rt.Stop(ctx, b)
		is(t, err, kr.ErrStaleFence)
		if o, _ := f.rt.Observe(ctx, b2); o.Phase != kr.PhaseRunning {
			t.Fatalf("the workload: %+v", o)
		}
		// The stop intent is not rolled back either.
		if _, err := f.rt.Stop(ctx, b2); err != nil {
			t.Fatal(err)
		}
		rc, err := f.rt.Logs(ctx, b2, kr.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
		_, err = f.rt.Start(ctx, f.start(b2, "echo hi; sleep 30"))
		is(t, err, kr.ErrStaleFence)
	})

	t.Run("service", func(t *testing.T) {
		f := newKFix(t)
		svc := f.serviceSpec("logsvc", 1, "exec sleep 120")
		svc.Service.Ports = nil
		f.flag("lose-create", "1")
		if _, err := f.res.Ensure(ctx, svc); err == nil {
			t.Fatal("ensure")
		}
		f.open()
		svc2 := svc
		svc2.Fence = 2
		interleave(t, f,
			func() { _, _ = f.res.Observe(ctx, handle(svc)) },
			func() {
				if _, err := f.res.Ensure(ctx, svc2); err != nil {
					t.Errorf("takeover ensure: %v", err)
				}
			},
			func() {
				rc, err := f.res.Logs(ctx, handle(svc), kr.LogOptions{})
				if err != nil {
					t.Errorf("logs: %v", err)
					return
				}
				rc.Close()
			})
		_, err := f.res.Ensure(ctx, svc)
		is(t, err, kr.ErrStaleFence)
		_, err = f.res.Stop(ctx, handle(svc))
		is(t, err, kr.ErrStaleFence)
		if _, err := f.res.Stop(ctx, handle(svc2)); err != nil {
			t.Fatal(err)
		}
		rc, err := f.res.Logs(ctx, handle(svc2), kr.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
		_, err = f.res.Ensure(ctx, svc2)
		is(t, err, kr.ErrStaleFence)
	})
}

// Test_unit_podman_kernel_lost_volume_create_then_remove proves a removal after a
// lost volume-create response adopts the verified volume and resolves the create
// before removing it, so the resource is not left unknown.
func Test_unit_podman_kernel_lost_volume_create_then_remove(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, restart := range []bool{false, true} {
		name := "same process"
		if restart {
			name = "after a restart"
		}
		t.Run(name, func(t *testing.T) {
			f := newKFix(t)
			ws := f.workspaceSpec(1)
			f.flag("lose-volume-create", "1")
			if _, err := f.res.Ensure(ctx, ws); err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
				t.Fatalf("ensure: %v", err)
			}
			vol := podman.VolumeObjectName(kScope, ws.Identity)
			if !exists("volume", vol) {
				t.Fatal("the fault did not model a lost response")
			}
			if restart {
				f.open()
			}
			rm, err := f.res.Remove(ctx, handle(ws))
			if err != nil || !rm.ConfirmedRemoved() {
				t.Fatalf("remove: %+v, %v", rm, err)
			}
			if exists("volume", vol) {
				t.Fatal("the volume survived a confirmed removal")
			}
			if m := f.record(vol); m["unanswered"] == true || m["removed"] != true || m["objectId"] == nil {
				t.Fatalf("the create was not resolved and recorded: %v", m)
			}
			if o, err := f.res.Observe(ctx, handle(ws)); err != nil || !o.ConfirmedRemoved() {
				t.Fatalf("observe: %+v, %v", o, err)
			}
		})
	}

	t.Run("a stop adopts it too", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		f.flag("lose-volume-create", "1")
		if _, err := f.res.Ensure(ctx, ws); err == nil {
			t.Fatal("ensure")
		}
		f.open()
		st, err := f.res.Stop(ctx, handle(ws))
		if err != nil || st.State != kr.ResourceStopped {
			t.Fatalf("stop: %+v, %v", st, err)
		}
		if m := f.record(podman.VolumeObjectName(kScope, ws.Identity)); m["unanswered"] == true || m["objectId"] == nil {
			t.Fatalf("not adopted: %v", m)
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
	})
}

// Test_unit_podman_kernel_run_network_tracking proves the run network is tracked
// like every other object: a durable identity, an unresolved-create record, a
// fence and a confirmed removal that is part of resource settlement.
func Test_unit_podman_kernel_run_network_tracking(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	svcSpec := func(f *kFix, name string) kr.ResourceSpec {
		s := f.serviceSpec(name, 1, "exec sleep 120")
		s.Service.Ports = nil
		return s
	}
	netName := func(f *kFix) string { return podman.NetworkObjectName(kScope, f.run) }

	t.Run("a lost create response is adopted and removed", func(t *testing.T) {
		f := newKFix(t)
		svc := svcSpec(f, "n1")
		f.flag("lose-network-create", "1")
		if _, err := f.res.Ensure(ctx, svc); err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		if !exists("network", netName(f)) {
			t.Fatal("the fault did not model a lost response")
		}
		if m := f.record(netName(f)); m["unanswered"] != true || m["uid"] == "" {
			t.Fatalf("the network create is not tracked: %v", m)
		}
		f.open()
		rm, err := f.res.Remove(ctx, handle(svc))
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if exists("network", netName(f)) {
			t.Fatal("the network survived a confirmed removal")
		}
		if m := f.record(netName(f)); m["unanswered"] == true || m["removed"] != true {
			t.Fatalf("the record: %v", m)
		}
	})

	t.Run("a delayed create is never confirmed removed and cannot recreate the network", func(t *testing.T) {
		f := newKFix(t)
		svc := svcSpec(f, "n2")
		f.flag("swallow-network-create", "1")
		if _, err := f.res.Ensure(ctx, svc); err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		// No replay while the request is unresolved.
		if _, err := f.res.Ensure(ctx, svc); err == nil || !strings.Contains(err.Error(), "unresolved") {
			t.Fatalf("ensure again: %v", err)
		}
		if n := f.calls("network create"); n != 1 {
			t.Fatalf("%d network creates", n)
		}
		for i := 0; i < 2; i++ {
			if rm, err := f.res.Remove(ctx, handle(svc)); err != nil || rm.ConfirmedRemoved() || rm.State != kr.ResourceUnknown {
				t.Fatalf("cleanup was confirmed with a network create unresolved: %+v, %v", rm, err)
			}
		}
		f.plantOwnedNetwork(netName(f)) // the delayed create lands
		rm, err := f.res.Remove(ctx, handle(svc))
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if exists("network", netName(f)) {
			t.Fatal("the landed network survived a confirmed removal")
		}
		// Fenced: nothing of the run creates it again.
		_, err = f.res.Ensure(ctx, svcSpec(f, "n3"))
		is(t, err, kr.ErrStaleFence)
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})

	t.Run("a failed removal is an error until it is retried to confirmation", func(t *testing.T) {
		f := newKFix(t)
		svc := svcSpec(f, "n4")
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		f.flag("fail-network-rm", "1")
		rm, err := f.res.Remove(ctx, handle(svc))
		if err == nil || rm.ConfirmedRemoved() {
			t.Fatalf("a failed network removal was confirmed: %+v, %v", rm, err)
		}
		if !exists("network", netName(f)) {
			t.Fatal("the network vanished")
		}
		f.unflag("fail-network-rm")
		if rm, err = f.res.Remove(ctx, handle(svc)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("retry: %+v, %v", rm, err)
		}
		if exists("network", netName(f)) || len(f.objects()) != 0 {
			t.Fatalf("left %v", f.objects())
		}
	})

	t.Run("a network still in use is finished by the last container off it", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		svc := svcSpec(f, "n5")
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		b := f.binding("client", 1)
		mustStart(t, f, f.start(b, "true", func(r *kr.StartRequest) { r.Workspace = handle(ws); r.Services = []kr.ResourceHandle{handle(svc)} }))
		f.awaitPhase(ctx, b, kr.PhaseExited)

		rm, err := f.res.Remove(ctx, handle(svc))
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("the service itself is removed: %+v, %v", rm, err)
		}
		if !exists("network", netName(f)) {
			t.Fatal("a network in use was removed")
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove the client: %+v, %v", o, err)
		}
		if exists("network", netName(f)) {
			t.Fatal("the last container off the network left it behind")
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})
}

var _ = domain.Scope{}
