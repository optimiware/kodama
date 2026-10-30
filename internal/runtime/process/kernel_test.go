package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

var kScope = domain.Scope{TenantID: "tnt_kernel", ProjectID: "prj_kernel"}

// kenv is a Process kernel adapter over a temporary state directory and one run.
type kenv struct {
	t   *testing.T
	k   *Kernel
	dir string
	run domain.RunID
}

func newKenv(t *testing.T) *kenv {
	t.Helper()
	dir := t.TempDir()
	e := &kenv{t: t, dir: dir, run: "run_kernel"}
	e.k = e.open()
	t.Cleanup(func() {
		// Nothing the test started may outlive it: reconcile owns no run.
		k := e.k
		if _, err := k.Reconcile(context.Background(), func(domain.Scope, domain.RunID) bool { return false }); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		k.Idle(10 * time.Second)
	})
	return e
}

func (e *kenv) open() *Kernel {
	e.t.Helper()
	k, err := NewKernel(KernelConfig{Dir: e.dir, KillGrace: time.Second})
	if err != nil {
		e.t.Fatal(err)
	}
	return k
}

// restart opens a second adapter over the same directory, as a restarted
// controller would, after the first one lost track of its processes.
func (e *kenv) restart() *Kernel {
	e.t.Helper()
	old := e.k
	old.h.mu.Lock()
	old.h.live = map[string]*kproc{} // the old controller is gone: it knows nothing
	old.h.mu.Unlock()
	e.k = e.open()
	return e.k
}

func (e *kenv) workspaceSpec(run domain.RunID, fence kr.Fence) kr.ResourceSpec {
	return kr.ResourceSpec{
		Scope: kScope, RunID: run, Kind: kr.ResourceWorkspace, Name: "workspace-" + string(run), Lifetime: kr.LifetimePipeline,
		Identity: "ws-" + string(run), SpecDigest: "sha256:ws", Fence: fence,
		Preparation: &kr.PreparationInputs{Scope: kScope, RunID: run, Digest: "sha256:in"},
	}
}

func (e *kenv) workspace(run domain.RunID) kr.ResourceHandle {
	e.t.Helper()
	obs, err := e.k.Resources.Ensure(context.Background(), e.workspaceSpec(run, 1))
	if err != nil {
		e.t.Fatal(err)
	}
	if obs.State != kr.ResourceReady {
		e.t.Fatalf("workspace is %s", obs.State)
	}
	return obs.Handle
}

func (e *kenv) binding(id string, fence kr.Fence) kr.RuntimeBinding {
	return kr.RuntimeBinding{
		Scope: kScope, RunID: e.run, StepKey: domain.StepID("step_" + id), Attempt: 1, WorkloadID: id,
		SpecDigest: "sha256:" + id, Backend: Backend, Fence: fence,
	}
}

func (e *kenv) request(b kr.RuntimeBinding, ws kr.ResourceHandle, script string) kr.StartRequest {
	return kr.StartRequest{Binding: b, Spec: kr.WorkloadSpec{Image: "registry.invalid/ignored@sha256:0", Commands: []string{script}}, Workspace: ws}
}

func (e *kenv) service(name string, fence kr.Fence, script string, ports ...int) kr.ResourceSpec {
	return kr.ResourceSpec{
		Scope: kScope, RunID: e.run, Kind: kr.ResourceService, Name: "svc-" + name, Lifetime: kr.LifetimePipeline,
		Identity: "svc-" + name, SpecDigest: "sha256:svc-" + name, Fence: fence,
		Service: &kr.ServiceSpec{Name: name, Lifetime: kr.LifetimePipeline, Image: "registry.invalid/ignored", Commands: []string{script}, Ports: ports},
	}
}

func kAwait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *kenv) observe(b kr.RuntimeBinding) kr.WorkloadObservation {
	e.t.Helper()
	obs, err := e.k.Runtime.Observe(context.Background(), b)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := obs.Validate(); err != nil {
		e.t.Fatalf("invalid observation: %v", err)
	}
	return obs
}

func (e *kenv) settled(b kr.RuntimeBinding) kr.WorkloadObservation {
	e.t.Helper()
	var obs kr.WorkloadObservation
	kAwait(e.t, "the workload to exit", func() bool { obs = e.observe(b); return obs.Phase == kr.PhaseExited })
	return obs
}

func (e *kenv) pgid(typ, id string) int {
	e.t.Helper()
	rec, found, err := e.k.h.st.load(typ, objectKey(kScope, typ, id))
	if err != nil || !found {
		e.t.Fatalf("no record of %s %s: %v", typ, id, err)
	}
	return rec.Pgid
}

func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

func readAll(t *testing.T, rc io.ReadCloser, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Test_unit_process_kernel_runtime_contract exercises the kernel Runtime and
// ResourceManager over the real host backend, without a container.
func Test_unit_process_kernel_runtime_contract(t *testing.T) {
	ctx := context.Background()

	t.Run("capabilities are honest and admission refuses the rest", func(t *testing.T) {
		e := newKenv(t)
		rc, err := e.k.Runtime.Capabilities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := e.k.Resources.Capabilities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, set := range []kr.CapabilitySet{rc, sc} {
			for _, never := range []kr.Capability{kr.CapNonRoot, kr.CapNetworkIsolation, kr.CapResourceLimits, kr.CapImagePinning, kr.CapPersistentService} {
				if set.Has(never) {
					t.Errorf("process advertises %s, which it cannot enforce", never)
				}
				if err := set.Satisfies(kr.CapabilitySet{never}); !errors.Is(err, kr.ErrCapability) {
					t.Errorf("a requirement of %s is satisfied: %v", never, err)
				}
			}
			for _, want := range []kr.Capability{kr.CapAdoptWorkload, kr.CapFencedStop} {
				if !set.Has(want) {
					t.Errorf("process lost %s", want)
				}
			}
		}
		if !sc.Has(kr.CapRunServices) {
			t.Error("the resource manager runs run services")
		}

		ws := e.workspace(e.run)
		for _, c := range []kr.Capability{kr.CapImagePinning, kr.CapNetworkIsolation, kr.CapNonRoot, kr.CapResourceLimits} {
			req := e.request(e.binding("refused-"+string(c), 1), ws, "echo never")
			req.Spec.Requires = kr.CapabilitySet{c}
			_, err := e.k.Runtime.Start(ctx, req)
			if !errors.Is(err, kr.ErrCapability) || !errors.Is(err, kr.ErrNotReached) || kr.ClassifyEffect(err) != kr.CommandRejected {
				t.Fatalf("start requiring %s: %v", c, err)
			}
			if obs := e.observe(req.Binding); obs.Phase != kr.PhaseAbsent {
				t.Fatalf("a refused start left %s behind", obs.Phase)
			}
		}

		persistent := e.service("db", 1, "exec sleep 30")
		persistent.Lifetime, persistent.Service.Lifetime = kr.LifetimeProject, kr.LifetimeProject
		secret := e.service("secret", 1, "exec sleep 30")
		secret.Service.Secrets = []kr.SecretRef{{Owner: domain.Owner{TenantID: "tnt_kernel"}, Name: "TOKEN"}}
		imageOnly := e.service("image", 1, "exec sleep 30")
		imageOnly.Service.Commands = nil
		required := e.service("req", 1, "exec sleep 30")
		required.Requires = kr.CapabilitySet{kr.CapNetworkIsolation}
		withSources := e.workspaceSpec("run_sources", 1)
		withSources.Preparation.Sources = []kr.SourceInput{{Name: "app", Locator: "opaque", Revision: "rev1", Path: "app"}}
		for name, spec := range map[string]kr.ResourceSpec{
			"persistent lifetime": persistent, "service secrets": secret, "image without commands": imageOnly,
			"required isolation": required, "workspace sources": withSources,
		} {
			_, err := e.k.Resources.Ensure(ctx, spec)
			if !errors.Is(err, kr.ErrNotReached) || kr.ClassifyEffect(err) != kr.CommandRejected {
				t.Fatalf("%s: %v", name, err)
			}
			if !errors.Is(err, kr.ErrCapability) && name != "image without commands" {
				t.Fatalf("%s is not a capability refusal: %v", name, err)
			}
			h := kr.ResourceHandle{Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Name: spec.Name, Identity: spec.Identity, Fence: 1}
			if obs, err := e.k.Resources.Observe(ctx, h); err != nil || obs.State != kr.ResourceRemoved {
				t.Fatalf("%s left something behind: %v %v", name, obs.State, err)
			}
		}

		b := e.binding("other-backend", 1)
		b.Backend = "podman"
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "true")); !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("another backend's binding: %v", err)
		}
	})

	t.Run("start observe settle stop remove", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)

		ok := e.binding("ok", 1)
		obs, err := e.k.Runtime.Start(ctx, e.request(ok, ws, "echo out; echo err >&2; echo data > made.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase != kr.PhaseRunning && obs.Phase != kr.PhaseExited {
			t.Fatalf("a started workload is %s", obs.Phase)
		}
		done := e.settled(ok)
		if done.Result != (kr.Result{Code: kr.ResultSucceeded}) {
			t.Fatalf("result %+v", done.Result)
		}
		rc, err := e.k.Runtime.Logs(ctx, ok, kr.LogOptions{})
		if out := readAll(t, rc, err); !strings.Contains(out, "out") || !strings.Contains(out, "err") {
			t.Fatalf("logs %q", out)
		}
		if _, err := os.Stat(filepath.Join(e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity)), "made.txt")); err != nil {
			t.Fatalf("the step did not run in the workspace: %v", err)
		}

		// A stop after the workload settled changes nothing, and a duplicate returns the same.
		first, err := e.k.Runtime.Stop(ctx, ok)
		if err != nil {
			t.Fatal(err)
		}
		second, err := e.k.Runtime.Stop(ctx, ok)
		if err != nil {
			t.Fatal(err)
		}
		if first.Phase != kr.PhaseExited || first.Result != done.Result || second.Phase != first.Phase || second.Result != first.Result {
			t.Fatalf("stop after settle: %+v then %+v, want %+v", first, second, done)
		}

		// Removal is confirmed only by PhaseAbsent; the identity is never started again.
		removed, err := e.k.Runtime.Remove(ctx, ok)
		if err != nil || removed.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v %v", removed, err)
		}
		if again, err := e.k.Runtime.Remove(ctx, ok); err != nil || again.Phase != kr.PhaseAbsent {
			t.Fatalf("a second remove: %+v %v", again, err)
		}
		if exists, _ := e.k.h.st.dirExists(typeWorkload, objectKey(kScope, typeWorkload, ok.WorkloadID)); exists {
			t.Fatal("removal left the workload directory")
		}
		for _, f := range []kr.Fence{1, 2} {
			b := ok
			b.Fence = f
			_, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo again"))
			if !errors.Is(err, ErrFenced) || !errors.Is(err, kr.ErrNotReached) {
				t.Fatalf("restart after remove at fence %d: %v", f, err)
			}
		}
		if _, err := e.k.Runtime.Logs(ctx, ok, kr.LogOptions{}); err == nil {
			t.Fatal("logs of a removed workload")
		}

		failing := e.binding("failing", 1)
		if _, err := e.k.Runtime.Start(ctx, e.request(failing, ws, "echo before; exit 3")); err != nil {
			t.Fatal(err)
		}
		if r := e.settled(failing).Result; r != (kr.Result{Code: kr.ResultFailed, ExitCode: 3, Reason: kr.ReasonCommandFailed}) {
			t.Fatalf("a failing step: %+v", r)
		}
	})

	t.Run("stop terminates a running workload and is idempotent", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("long", 1)
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo up; exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		if obs := e.observe(b); obs.Phase != kr.PhaseRunning {
			t.Fatalf("a long step is %s", obs.Phase)
		}
		pgid := e.pgid(typeWorkload, b.WorkloadID)
		stopped, err := e.k.Runtime.Stop(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if want := (kr.Result{Code: kr.ResultCancelled, Reason: kr.ReasonCancelled}); stopped.Phase != kr.PhaseExited || stopped.Result != want {
			t.Fatalf("stop: %+v", stopped)
		}
		if !groupGone(pgid) {
			t.Fatal("a confirmed stop left the process group running")
		}
		dup, err := e.k.Runtime.Stop(ctx, b)
		if err != nil || dup.Result != stopped.Result || dup.Phase != stopped.Phase {
			t.Fatalf("duplicate stop: %+v %v", dup, err)
		}
		if obs := e.observe(b); obs.Result != stopped.Result {
			t.Fatalf("observe after stop: %+v", obs)
		}
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "exec sleep 300")); !errors.Is(err, ErrFenced) {
			t.Fatalf("a stopped workload was relaunched: %v", err)
		}
		if rm, err := e.k.Runtime.Remove(ctx, b); err != nil || rm.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v %v", rm, err)
		}
	})

	t.Run("stop racing with settle decides once", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		for i := range 20 {
			b := e.binding(fmt.Sprintf("race-%d", i), 1)
			if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "sleep 0.02")); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			results := make([]kr.WorkloadObservation, 3)
			errs := make([]error, 3)
			for j := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if j == 2 {
						results[j], errs[j] = e.k.Runtime.Observe(ctx, b)
						return
					}
					time.Sleep(time.Duration(i) * time.Millisecond)
					results[j], errs[j] = e.k.Runtime.Stop(ctx, b)
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			final := e.observe(b)
			if final.Phase != kr.PhaseExited || (final.Result.Code != kr.ResultSucceeded && final.Result.Code != kr.ResultCancelled) {
				t.Fatalf("race %d settled as %+v", i, final)
			}
			for _, r := range results[:2] {
				if r.Phase != kr.PhaseExited || r.Result != final.Result {
					t.Fatalf("race %d: a stop reported %+v, the record says %+v", i, r, final)
				}
			}
			if again, err := e.k.Runtime.Stop(ctx, b); err != nil || again.Result != final.Result {
				t.Fatalf("race %d: a later stop changed the outcome: %+v %v", i, again, err)
			}
			if !groupGone(e.pgid(typeWorkload, b.WorkloadID)) {
				t.Fatalf("race %d left a process", i)
			}
		}
	})

	t.Run("a stop before the start fences it", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("early", 2)
		obs, err := e.k.Runtime.Stop(ctx, b)
		if err != nil || obs.Phase != kr.PhaseAbsent {
			t.Fatalf("stop of an unknown workload: %+v %v", obs, err)
		}
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo late > late.txt")); !errors.Is(err, ErrFenced) || !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("a start after its stop: %v", err)
		}
		old := b
		old.Fence = 1
		if _, err := e.k.Runtime.Start(ctx, e.request(old, ws, "true")); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a delayed create from an older fence: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity)), "late.txt")); err == nil {
			t.Fatal("a fenced workload ran")
		}
	})

	t.Run("start adopts by identity and fences stale writers", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("adopt", 2)
		req := e.request(b, ws, "exec sleep 300")
		if _, err := e.k.Runtime.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		pgid := e.pgid(typeWorkload, b.WorkloadID)
		for range 3 {
			obs, err := e.k.Runtime.Start(ctx, req)
			if err != nil || obs.Phase != kr.PhaseRunning || e.pgid(typeWorkload, b.WorkloadID) != pgid {
				t.Fatalf("a repeated start did not adopt: %+v %v", obs, err)
			}
		}
		other := req
		other.Binding.SpecDigest = "sha256:other"
		if _, err := e.k.Runtime.Start(ctx, other); !errors.Is(err, kr.ErrSpecMismatch) {
			t.Fatalf("another digest: %v", err)
		}
		stale := req
		stale.Binding.Fence = 1
		if _, err := e.k.Runtime.Start(ctx, stale); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stale start: %v", err)
		}
		if _, err := e.k.Runtime.Stop(ctx, stale.Binding); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stale stop: %v", err)
		}
		if _, err := e.k.Runtime.Remove(ctx, stale.Binding); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stale remove: %v", err)
		}
		if obs := e.observe(b); obs.Phase != kr.PhaseRunning {
			t.Fatalf("a refused stale writer stopped the workload: %s", obs.Phase)
		}
		takeover := req
		takeover.Binding.Fence = 3
		if obs, err := e.k.Runtime.Start(ctx, takeover); err != nil || obs.Phase != kr.PhaseRunning {
			t.Fatalf("a takeover adopts: %+v %v", obs, err)
		}
		if _, err := e.k.Runtime.Stop(ctx, req.Binding); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("the superseded writer may still stop: %v", err)
		}
		if obs, err := e.k.Runtime.Stop(ctx, takeover.Binding); err != nil || obs.Result.Code != kr.ResultCancelled {
			t.Fatalf("the new writer stops: %+v %v", obs, err)
		}
		conflicting := req
		conflicting.Binding.StepKey = "step_other"
		if _, err := e.k.Runtime.Start(ctx, conflicting); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("another step on the identity: %v", err)
		}
	})

	t.Run("a timeout is enforced on the host", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("slow", 1)
		req := e.request(b, ws, "exec sleep 300")
		req.Spec.Timeout = 200 * time.Millisecond
		if _, err := e.k.Runtime.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		if r := e.settled(b).Result; r.Code != kr.ResultTimedOut || r.Reason != kr.ReasonStepTimeout {
			t.Fatalf("result %+v", r)
		}
	})

	t.Run("tenant and project are authority", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("mine", 1)
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		foreign := b
		foreign.Scope = domain.Scope{TenantID: "tnt_other", ProjectID: b.Scope.ProjectID}
		if obs := e.observe(foreign); obs.Phase != kr.PhaseAbsent {
			t.Fatalf("another tenant observes %s", obs.Phase)
		}
		if _, err := e.k.Runtime.Logs(ctx, foreign, kr.LogOptions{}); err == nil {
			t.Fatal("another tenant reads the logs")
		}
		if obs, err := e.k.Runtime.Stop(ctx, foreign); err != nil || obs.Phase != kr.PhaseAbsent {
			t.Fatalf("another tenant's stop: %+v %v", obs, err)
		}
		if obs := e.observe(b); obs.Phase != kr.PhaseRunning {
			t.Fatalf("a foreign stop reached the workload: %s", obs.Phase)
		}
		wrongRun := b
		wrongRun.RunID = "run_other"
		if _, err := e.k.Runtime.Observe(ctx, wrongRun); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("another run on the identity: %v", err)
		}
		if _, err := e.k.Runtime.Stop(ctx, b); err != nil {
			t.Fatal(err)
		}
		// The workspace and the secrets of another run are not usable.
		req := e.request(e.binding("cross", 1), kr.ResourceHandle{Scope: kScope, RunID: "run_other", Kind: kr.ResourceWorkspace, Name: "w", Identity: "ws-run_other", Fence: 1}, "true")
		if _, err := e.k.Runtime.Start(ctx, req); !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("another run's workspace: %v", err)
		}
		req = e.request(e.binding("secrets", 1), ws, "true")
		req.Secrets = kr.Materialization{Scope: kScope, RunID: e.run, StepKey: "step_other", Directory: t.TempDir()}
		if _, err := e.k.Runtime.Start(ctx, req); !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("another step's secrets: %v", err)
		}
		req.Secrets.StepKey = "step_secrets"
		req.Secrets.Directory = filepath.Join(e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity)), "secrets")
		if _, err := e.k.Runtime.Start(ctx, req); !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("secrets inside the workspace: %v", err)
		}
		req.Secrets.Directory = t.TempDir()
		req.Spec.Commands = []string{`test "$KODAMA_SECRETS_DIR" = ` + req.Secrets.Directory}
		if _, err := e.k.Runtime.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		if r := e.settled(req.Binding).Result; r.Code != kr.ResultSucceeded {
			t.Fatalf("a step sees its private secrets directory: %+v", r)
		}
	})

	t.Run("a restarted adapter adopts, and reports what it cannot know", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		keep, drop := e.binding("keep", 1), e.binding("drop", 1)
		other := domain.RunID("run_second")
		ws2 := e.workspace(other)
		dropB := drop
		dropB.RunID = other
		if _, err := e.k.Runtime.Start(ctx, e.request(keep, ws, "exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.k.Runtime.Start(ctx, e.request(dropB, ws2, "exec sleep 300")); err != nil {
			t.Fatal(err)
		}
		keepPgid, dropPgid := e.pgid(typeWorkload, keep.WorkloadID), e.pgid(typeWorkload, dropB.WorkloadID)

		k2 := e.restart()
		if obs, err := k2.Runtime.Observe(ctx, keep); err != nil || obs.Phase != kr.PhaseRunning {
			t.Fatalf("the restarted adapter does not see the running workload: %+v %v", obs, err)
		}
		if obs, err := k2.Runtime.Start(ctx, e.request(keep, ws, "exec sleep 300")); err != nil || obs.Phase != kr.PhaseRunning || e.pgid(typeWorkload, keep.WorkloadID) != keepPgid {
			t.Fatalf("the restarted adapter does not adopt: %+v %v", obs, err)
		}

		// Orphan reconciliation: only what the caller does not own is touched.
		rep, err := k2.Reconcile(ctx, func(_ domain.Scope, run domain.RunID) bool { return run == e.run })
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Retained) != 0 || !containsAll(rep.Removed, dropB.WorkloadID, ws2.Identity) {
			t.Fatalf("report %+v", rep)
		}
		if !groupGone(dropPgid) || groupGone(keepPgid) {
			t.Fatalf("reconcile stopped the wrong process: orphan gone=%v owned gone=%v", groupGone(dropPgid), groupGone(keepPgid))
		}
		if obs, err := k2.Runtime.Observe(ctx, dropB); err != nil || obs.Phase != kr.PhaseAbsent {
			t.Fatalf("an orphan is not removed: %+v %v", obs, err)
		}
		if obs, err := k2.Resources.Observe(ctx, ws2); err != nil || !obs.ConfirmedRemoved() {
			t.Fatalf("an orphan workspace is not removed: %+v %v", obs, err)
		}

		// A process that ended while no adapter watched has no known exit status.
		if err := syscall.Kill(-keepPgid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		kAwait(t, "the group to end", func() bool { return groupGone(keepPgid) })
		obs, err := k2.Runtime.Observe(ctx, keep)
		if err != nil || obs.Phase != kr.PhaseExited || obs.Result != (kr.Result{Code: kr.ResultLost, Reason: kr.ReasonWorkloadLost}) {
			t.Fatalf("a lost workload: %+v %v", obs, err)
		}
	})

	t.Run("a launch whose group was never recorded cannot have run", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("announced", 1)
		now := time.Now().UTC()
		// The adapter died after announcing the launch and before recording the group.
		rec := kRecord{Type: typeWorkload, Key: objectKey(kScope, typeWorkload, b.WorkloadID), Scope: kScope, RunID: e.run, StepKey: b.StepKey,
			Attempt: 1, ID: b.WorkloadID, Digest: b.SpecDigest, Fence: 1, UID: "u", IssuedAt: &now}
		if err := e.k.h.st.create(rec); err != nil {
			t.Fatal(err)
		}
		obs, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo should-not-run > ghost.txt"))
		if err != nil || obs.Phase != kr.PhaseExited || obs.Result.Reason != kr.ReasonLaunchNotReached {
			t.Fatalf("adopting an announced launch: %+v %v", obs, err)
		}
		if _, err := os.Stat(filepath.Join(e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity)), "ghost.txt")); err == nil {
			t.Fatal("an announced launch was repeated")
		}
		if rm, err := e.k.Runtime.Remove(ctx, b); err != nil || rm.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v %v", rm, err)
		}
	})

	t.Run("followed logs end with the workload", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		b := e.binding("follow", 1)
		if _, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo one; sleep 0.2; echo two")); err != nil {
			t.Fatal(err)
		}
		rc, err := e.k.Runtime.Logs(ctx, b, kr.LogOptions{Follow: true})
		if out := readAll(t, rc, err); out != "one\ntwo\n" {
			t.Fatalf("followed logs %q", out)
		}
	})

	t.Run("services and workspaces have a confirmed lifecycle", func(t *testing.T) {
		e := newKenv(t)
		ws := e.workspace(e.run)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		port := ln.Addr().(*net.TCPAddr).Port

		spec := e.service("db", 1, "echo service-up; exec sleep 300", port)
		obs, err := e.k.Resources.Ensure(ctx, spec)
		if err != nil || obs.State != kr.ResourceReady {
			t.Fatalf("ensure: %+v %v", obs, err)
		}
		if err := obs.Validate(); err != nil {
			t.Fatal(err)
		}
		h := obs.Handle
		pgid := e.pgid(typeService, spec.Identity)
		kAwait(t, "the service log", func() bool {
			rc, err := e.k.Resources.Logs(ctx, h, kr.LogOptions{})
			return err == nil && strings.Contains(readAll(t, rc, nil), "service-up")
		})
		if again, err := e.k.Resources.Ensure(ctx, spec); err != nil || again.State != kr.ResourceReady || e.pgid(typeService, spec.Identity) != pgid {
			t.Fatalf("ensure is not idempotent: %+v %v", again, err)
		}
		changed := spec
		changed.SpecDigest = "sha256:changed"
		if _, err := e.k.Resources.Ensure(ctx, changed); !errors.Is(err, kr.ErrSpecMismatch) {
			t.Fatalf("another digest: %v", err)
		}
		// A workload sees only ready services.
		req := e.request(e.binding("with-service", 1), ws, `test "$KODAMA_SERVICES" = db`)
		req.Services = []kr.ResourceHandle{h}
		if _, err := e.k.Runtime.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		if r := e.settled(req.Binding).Result; r.Code != kr.ResultSucceeded {
			t.Fatalf("a step names its services: %+v", r)
		}
		down := e.service("down", 1, "exec sleep 300", 1) // nothing listens on port 1
		dobs, err := e.k.Resources.Ensure(ctx, down)
		if err != nil || dobs.State != kr.ResourceStarting {
			t.Fatalf("a service that does not answer: %+v %v", dobs, err)
		}
		req = e.request(e.binding("with-down", 1), ws, "true")
		req.Services = []kr.ResourceHandle{dobs.Handle}
		if _, err := e.k.Runtime.Start(ctx, req); !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("a step started against a service that is not ready: %v", err)
		}

		// Loss is reported, never hidden.
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		kAwait(t, "the service to be lost", func() bool {
			o, err := e.k.Resources.Observe(ctx, h)
			return err == nil && o.State == kr.ResourceFailed && o.Reason == kr.ReasonServiceLost
		})

		// Stop is fenced and verified; remove is confirmed and fences a late create.
		newer := h
		newer.Fence = 3
		if _, err := e.k.Resources.Ensure(ctx, func() kr.ResourceSpec { s := spec; s.Fence = 3; return s }()); err != nil {
			t.Fatal(err)
		}
		if _, err := e.k.Resources.Stop(ctx, h); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stale stop: %v", err)
		}
		if _, err := e.k.Resources.Remove(ctx, h); !errors.Is(err, kr.ErrStaleFence) {
			t.Fatalf("a stale remove: %v", err)
		}
		stopped, err := e.k.Resources.Stop(ctx, newer)
		if err != nil || stopped.State != kr.ResourceStopped {
			t.Fatalf("stop: %+v %v", stopped, err)
		}
		if dup, err := e.k.Resources.Stop(ctx, newer); err != nil || dup.State != kr.ResourceStopped {
			t.Fatalf("duplicate stop: %+v %v", dup, err)
		}
		if !groupGone(e.pgid(typeService, spec.Identity)) {
			t.Fatal("a stopped service still runs")
		}
		removed, err := e.k.Resources.Remove(ctx, newer)
		if err != nil || !removed.ConfirmedRemoved() {
			t.Fatalf("remove: %+v %v", removed, err)
		}
		late := spec
		late.Fence = 3
		if _, err := e.k.Resources.Ensure(ctx, late); !errors.Is(err, ErrFenced) || !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("a create after removal: %v", err)
		}
		if _, err := e.k.Resources.Logs(ctx, newer, kr.LogOptions{}); err == nil {
			t.Fatal("logs of a removed service")
		}
		if _, err := e.k.Resources.Logs(ctx, ws, kr.LogOptions{}); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("a workspace has no logs: %v", err)
		}

		// A stop before a create leaves a tombstone that refuses it.
		early := e.service("early", 5, "echo x")
		eh := kr.ResourceHandle{Scope: kScope, RunID: e.run, Kind: kr.ResourceService, Name: early.Name, Identity: early.Identity, Fence: 5}
		if o, err := e.k.Resources.Stop(ctx, eh); err != nil || o.State != kr.ResourceStopped {
			t.Fatalf("stop of an unknown service: %+v %v", o, err)
		}
		if _, err := e.k.Resources.Ensure(ctx, early); !errors.Is(err, ErrFenced) {
			t.Fatalf("a create after its stop: %v", err)
		}
		if o, err := e.k.Resources.Observe(ctx, eh); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("a stopped tombstone is not a removal: %+v %v", o, err)
		}
	})

	t.Run("a workspace is created once, verified and never recreated", func(t *testing.T) {
		e := newKenv(t)
		spec := e.workspaceSpec(e.run, 1)
		obs, err := e.k.Resources.Ensure(ctx, spec)
		if err != nil || obs.State != kr.ResourceReady {
			t.Fatalf("ensure: %+v %v", obs, err)
		}
		wsDir := e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, spec.Identity))
		if err := os.WriteFile(filepath.Join(wsDir, "keep"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if again, err := e.k.Resources.Ensure(ctx, spec); err != nil || again.State != kr.ResourceReady {
			t.Fatalf("adoption: %+v %v", again, err)
		}
		if _, err := os.Stat(filepath.Join(wsDir, "keep")); err != nil {
			t.Fatal("adopting a workspace recreated it")
		}
		changed := spec
		changed.SpecDigest = "sha256:changed"
		if _, err := e.k.Resources.Ensure(ctx, changed); !errors.Is(err, kr.ErrSpecMismatch) {
			t.Fatalf("another digest: %v", err)
		}

		// A replacement at the path is a collision and is never deleted.
		moved := wsDir + ".moved"
		if err := os.Rename(wsDir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(wsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wsDir, "foreign"), []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.k.Resources.Observe(ctx, obs.Handle); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a replaced workspace: %v", err)
		}
		if _, err := e.k.Resources.Remove(ctx, obs.Handle); err == nil {
			t.Fatal("removed a directory that is not the recorded instance")
		}
		if _, err := os.Stat(filepath.Join(wsDir, "foreign")); err != nil {
			t.Fatal("a foreign directory was deleted")
		}
		if err := os.RemoveAll(wsDir); err != nil {
			t.Fatal(err)
		}

		// Lost scratch is reported and never recreated.
		if err := os.RemoveAll(moved); err != nil {
			t.Fatal(err)
		}
		lost, err := e.k.Resources.Observe(ctx, obs.Handle)
		if err != nil || lost.State != kr.ResourceFailed || lost.Reason != kr.ReasonWorkspaceLost {
			t.Fatalf("lost scratch: %+v %v", lost, err)
		}
		if _, err := os.Stat(wsDir); err == nil {
			t.Fatal("lost scratch was recreated")
		}
		rm, err := e.k.Resources.Remove(ctx, obs.Handle)
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v %v", rm, err)
		}
		if _, err := e.k.Resources.Ensure(ctx, spec); !errors.Is(err, ErrFenced) {
			t.Fatalf("a workspace is created once: %v", err)
		}

		// A directory nobody recorded is foreign.
		foreign := e.workspaceSpec("run_foreign", 1)
		fdir := e.k.h.st.dir(typeWorkspace, objectKey(kScope, typeWorkspace, foreign.Identity))
		if err := os.MkdirAll(fdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := e.k.Resources.Ensure(ctx, foreign); !errors.Is(err, domain.ErrConflict) || !errors.Is(err, kr.ErrNotReached) {
			t.Fatalf("a foreign directory: %v", err)
		}
		if _, err := os.Stat(fdir); err != nil {
			t.Fatal("a foreign directory was touched")
		}
	})

	t.Run("a preparation hook fills the workspace and is retried when interrupted", func(t *testing.T) {
		dir := t.TempDir()
		calls := 0
		k, err := NewKernel(KernelConfig{Dir: dir, Prepare: func(_ context.Context, ws string, in kr.PreparationInputs) error {
			calls++
			if calls == 1 {
				return errors.New("interrupted")
			}
			return os.WriteFile(filepath.Join(ws, in.Sources[0].Path), []byte(in.Sources[0].Revision), 0o600)
		}})
		if err != nil {
			t.Fatal(err)
		}
		e := &kenv{t: t, dir: dir, run: "run_prep", k: k}
		spec := e.workspaceSpec(e.run, 1)
		spec.Preparation.Sources = []kr.SourceInput{{Name: "app", Locator: "opaque", Revision: "rev1", Path: "app.txt"}}
		_, err = k.Resources.Ensure(ctx, spec)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("an interrupted preparation is uncertain: %v", err)
		}
		obs, err := k.Resources.Ensure(ctx, spec)
		if err != nil || obs.State != kr.ResourceReady || calls != 2 {
			t.Fatalf("the retry: %+v %v (calls %d)", obs, err, calls)
		}
		if b, _ := os.ReadFile(filepath.Join(k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, spec.Identity)), "app.txt")); string(b) != "rev1" {
			t.Fatalf("workspace content %q", b)
		}
		if _, err := k.Resources.Ensure(ctx, spec); err != nil || calls != 2 {
			t.Fatalf("a prepared workspace is prepared once: %v (calls %d)", err, calls)
		}
		_, _ = k.Resources.Remove(ctx, obs.Handle)
	})
}

func containsAll(have []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			found = found || h == w
		}
		if !found {
			return false
		}
	}
	return true
}

// Test_unit_process_workspace_exclusion: one executable step holds a run's
// workspace at a time. A second start is refused deterministically (and proven
// not to have reached the backend, so the engine may retry it), a stop or
// removal of a held workspace is not confirmed, and the release is observable.
func Test_unit_process_workspace_exclusion(t *testing.T) {
	ctx := context.Background()
	e := newKenv(t)
	ws := e.workspace(e.run)
	wsDir := e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity))
	holderOf := func() (string, bool) {
		t.Helper()
		id, held, err := e.k.Resources.WorkspaceHolder(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return id, held
	}
	if _, held := holderOf(); held {
		t.Fatal("a new workspace is held")
	}

	a, b := e.binding("a", 1), e.binding("b", 1)
	if _, err := e.k.Runtime.Start(ctx, e.request(a, ws, "echo a > a.txt; exec sleep 300")); err != nil {
		t.Fatal(err)
	}
	if id, held := holderOf(); !held || id != "a" {
		t.Fatalf("holder %q held=%v", id, held)
	}
	for range 3 {
		_, err := e.k.Runtime.Start(ctx, e.request(b, ws, "echo b > b.txt"))
		if !errors.Is(err, ErrWorkspaceBusy) || !errors.Is(err, kr.ErrNotReached) || kr.ClassifyEffect(err) != kr.CommandRejected {
			t.Fatalf("a second step on a held workspace: %v", err)
		}
	}
	if obs := e.observe(b); obs.Phase != kr.PhaseAbsent {
		t.Fatalf("the refused step left %s", obs.Phase)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "b.txt")); err == nil {
		t.Fatal("the refused step ran")
	}
	// The holder itself is adopted, not refused.
	if obs, err := e.k.Runtime.Start(ctx, e.request(a, ws, "echo a > a.txt; exec sleep 300")); err != nil || obs.Phase != kr.PhaseRunning {
		t.Fatalf("the holder's own start: %+v %v", obs, err)
	}

	// The exclusion survives a restart: the holder is recorded on disk.
	k2 := e.restart()
	if _, held, err := k2.Resources.WorkspaceHolder(ctx, ws); err != nil || !held {
		t.Fatalf("the restarted adapter lost the holder: %v %v", held, err)
	}
	if _, err := k2.Runtime.Start(ctx, e.request(b, ws, "echo b > b.txt")); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("after a restart: %v", err)
	}

	// A held workspace is neither stopped nor removed.
	stopped, err := e.k.Resources.Stop(ctx, ws)
	if err != nil || stopped.State != kr.ResourceUnknown || stopped.Reason != kr.ReasonStopUncertain || stopped.ConfirmedRemoved() {
		t.Fatalf("stop of a held workspace: %+v %v", stopped, err)
	}
	removed, err := e.k.Resources.Remove(ctx, ws)
	if err != nil || removed.ConfirmedRemoved() {
		t.Fatalf("remove of a held workspace: %+v %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "a.txt")); err != nil {
		t.Fatal("a held workspace was deleted")
	}

	// Stopping the holder releases the workspace, observably; the workspace's own
	// stop intent still fences new steps until it is stopped and removed.
	if obs, err := k2.Runtime.Stop(ctx, a); err != nil || obs.Result.Code != kr.ResultCancelled {
		t.Fatalf("stop the holder: %+v %v", obs, err)
	}
	if _, held := holderOf(); held {
		t.Fatal("the stopped holder still holds the workspace")
	}
	if obs, err := k2.Resources.Observe(ctx, ws); err != nil || obs.State != kr.ResourceUnknown {
		t.Fatalf("a workspace stopping but not yet verified: %+v %v", obs, err)
	}
	if _, err := k2.Runtime.Start(ctx, e.request(b, ws, "true")); !errors.Is(err, ErrFenced) {
		t.Fatalf("a step on a workspace being stopped: %v", err)
	}
	if stopped, err := k2.Resources.Stop(ctx, ws); err != nil || stopped.State != kr.ResourceStopped {
		t.Fatalf("the released workspace stops: %+v %v", stopped, err)
	}
	if rm, err := k2.Resources.Remove(ctx, ws); err != nil || !rm.ConfirmedRemoved() {
		t.Fatalf("the released workspace is removed: %+v %v", rm, err)
	}
	if _, err := os.Stat(wsDir); err == nil {
		t.Fatal("removal left the workspace")
	}
}

// Test_process_workspace_turns: steps take the workspace in
// turn, a step that ends on its own releases it, and exactly one of many
// concurrent starts wins.
func Test_process_workspace_turns(t *testing.T) {
	ctx := context.Background()
	e := newKenv(t)
	ws := e.workspace(e.run)

	// Steps in sequence: each ends on its own, which releases the workspace.
	for i := range 3 {
		b := e.binding(fmt.Sprintf("seq-%d", i), 1)
		kAwait(t, "the workspace to be released", func() bool {
			_, err := e.k.Runtime.Start(ctx, e.request(b, ws, "sleep 0.05; echo step >> order.txt"))
			if err == nil {
				return true
			}
			if !errors.Is(err, ErrWorkspaceBusy) {
				t.Fatal(err)
			}
			return false
		})
		if e.settled(b).Result.Code != kr.ResultSucceeded {
			t.Fatal("a step failed")
		}
		if _, held, _ := e.k.Resources.WorkspaceHolder(ctx, ws); held {
			t.Fatalf("step %d ended and the workspace is still held", i)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.k.h.st.workspaceDir(objectKey(kScope, typeWorkspace, ws.Identity)), "order.txt")); string(b) != "step\nstep\nstep\n" {
		t.Fatalf("order %q", b)
	}

	// Concurrent starts: exactly one holds the workspace.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.k.Runtime.Start(ctx, e.request(e.binding(fmt.Sprintf("par-%d", i), 1), ws, "exec sleep 300"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrWorkspaceBusy):
			t.Fatalf("a losing start: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d steps hold one workspace", won)
	}

	// Another run's workspace is independent.
	other := domain.RunID("run_independent")
	ws2 := e.workspace(other)
	b := e.binding("independent", 1)
	b.RunID = other
	if _, err := e.k.Runtime.Start(ctx, e.request(b, ws2, "exec sleep 300")); err != nil {
		t.Fatalf("a held workspace blocks another run: %v", err)
	}
}
