package podman_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/runtime/podman"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// These tests run the kernel Runtime and ResourceManager adapters against the
// real Podman engine. They do not use the unit-layer service skip: the engine is
// the thing under test, so they run in every layer when an engine answers, skip
// cleanly when none does, and fail instead of skipping with
// KODAMA_REQUIRE_PODMAN=1. A skipped run is not evidence.

var kScope = domain.Scope{TenantID: "t177_tenant", ProjectID: "p177_project"}

// kEngine returns once the engine answers and the pinned image is present.
func kEngine(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "podman", "info", "--format", "{{.Host.Arch}}").CombinedOutput(); err != nil {
		if os.Getenv("KODAMA_REQUIRE_PODMAN") == "1" {
			t.Fatalf("podman is required: %v: %s", err, out)
		}
		t.Skipf("podman is unavailable: %v", err)
	}
	if err := exec.CommandContext(ctx, "podman", "image", "exists", podmantest.Image).Run(); err != nil {
		if out, err := exec.CommandContext(ctx, "podman", "pull", "--quiet", podmantest.Image).CombinedOutput(); err != nil {
			t.Fatalf("pulling %s: %v: %s", podmantest.Image, err, out)
		}
	}
}

// kFix is one run's adapters over a state directory, with a podman wrapper that
// records every call and injects the faults of a lost response or an outage.
type kFix struct {
	t     *testing.T
	dir   string
	state string
	run   domain.RunID
	cfg   podman.KernelConfig
	rt    *podman.KernelRuntime
	res   *podman.KernelResources
	prep  *kPreparer
}

type kPreparer struct {
	command []string
}

func (p *kPreparer) Preparation(_ context.Context, in kr.PreparationInputs) (podman.PreparationJob, error) {
	return podman.PreparationJob{Image: podmantest.Image, Command: p.command}, nil
}

func newKFix(t *testing.T) *kFix {
	t.Helper()
	kEngine(t)
	dir := podmantest.SharedDir(t)
	real, err := exec.LookPath("podman")
	if err != nil {
		t.Fatal(err)
	}
	f := &kFix{t: t, dir: dir, state: filepath.Join(dir, "state"), run: domain.RunID(fmt.Sprintf("run_k%d", time.Now().UnixNano())), prep: &kPreparer{}}
	for name, content := range map[string]string{"run": string(f.run), "image": podmantest.Image} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := filepath.Join(dir, "podman")
	script := fmt.Sprintf(`#!/bin/sh
D=%q
REAL=%q
echo "$1 $2" >> "$D/calls.log"
if [ -e "$D/down" ]; then echo "Error: cannot connect to Podman" >&2; exit 125; fi
for k in "delay-$1" "delay-$1-$2"; do
  if [ -e "$D/$k" ]; then s=$(cat "$D/$k"); rm -f "$D/$k"; sleep "$s"; fi
done
if [ -e "$D/chmod-after-rm" ] && [ "$1" = rm ]; then
  p=$(cat "$D/chmod-after-rm"); rm -f "$D/chmod-after-rm"
  "$REAL" "$@"; rc=$?
  chmod a-w "$p"
  exit $rc
fi
if [ -e "$D/fail-network-rm" ] && [ "$1" = network ] && [ "$2" = rm ]; then echo "Error: cannot remove network" >&2; exit 125; fi
if [ -e "$D/fail-next" ] && [ "$1" != "create" ] && [ "$2" != "create" ]; then
  n=$(cat "$D/fail-next")
  if [ "$n" -gt 0 ]; then echo $((n-1)) > "$D/fail-next"; echo "Error: cannot connect to Podman" >&2; exit 125; fi
fi
if [ -e "$D/lose-create" ] && [ "$1" = create ]; then
  rm -f "$D/lose-create"
  "$REAL" "$@" > /dev/null
  echo 1 > "$D/fail-next"
  echo "Error: EOF" >&2
  exit 125
fi
if [ -e "$D/lose-volume-create" ] && [ "$1" = volume ] && [ "$2" = create ]; then
  rm -f "$D/lose-volume-create"
  "$REAL" "$@" > /dev/null
  echo 1 > "$D/fail-next"
  echo "Error: EOF" >&2
  exit 125
fi
if [ -e "$D/swallow-volume-create" ] && [ "$1" = volume ] && [ "$2" = create ]; then
  rm -f "$D/swallow-volume-create"
  echo "Error: EOF" >&2
  exit 125
fi
if [ -e "$D/lose-network-create" ] && [ "$1" = network ] && [ "$2" = create ]; then
  rm -f "$D/lose-network-create"
  "$REAL" "$@" > /dev/null
  echo 1 > "$D/fail-next"
  echo "Error: EOF" >&2
  exit 125
fi
if [ -e "$D/swallow-network-create" ] && [ "$1" = network ] && [ "$2" = create ]; then
  rm -f "$D/swallow-network-create"
  echo "Error: EOF" >&2
  exit 125
fi
# kill-*: a foreign object takes the name, then the CLI is killed with no answer.
# collide-*: a foreign object takes the name, then the real create is refused.
plant() {
  case "$1" in
    container) "$REAL" run -d --name "$2" --label "kodama.optimi.com/run=$(cat "$D/run")" "$(cat "$D/image")" sleep 60 > /dev/null ;;
    volume) "$REAL" volume create --label "kodama.optimi.com/run=$(cat "$D/run")" "$2" > /dev/null ;;
    network) "$REAL" network create --label "kodama.optimi.com/run=$(cat "$D/run")" "$2" > /dev/null ;;
  esac
}
lastarg() { for a in "$@"; do n=$a; done; echo "$n"; }
kind=""
if [ "$1" = create ]; then kind=container; name="$3"; fi
if [ "$1" = volume ] && [ "$2" = create ]; then kind=volume; name=$(lastarg "$@"); fi
if [ "$1" = network ] && [ "$2" = create ]; then kind=network; name=$(lastarg "$@"); fi
if [ -n "$kind" ]; then
  for mode in kill collide; do
    if [ -e "$D/$mode-$kind-create" ]; then
      rm -f "$D/$mode-$kind-create"
      plant "$kind" "$name"
      if [ "$mode" = kill ]; then kill -9 $$; fi
    fi
  done
fi
if [ -e "$D/swallow-create" ] && [ "$1" = create ]; then
  rm -f "$D/swallow-create"
  echo "Error: EOF" >&2
  exit 125
fi
exec "$REAL" "$@"
`, dir, real)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.cfg = podman.KernelConfig{StateDir: f.state, Binary: wrapper, StopTimeout: time.Second, Preparer: f.prep}
	f.open()
	t.Cleanup(func() {
		filter := "label=" + podman.LabelRun + "=" + string(f.run)
		for _, id := range strings.Fields(sh(t, "podman", "ps", "-a", "-q", "--filter", filter)) {
			podmanQuiet("rm", "--force", "--time", "0", id)
		}
		for _, v := range strings.Fields(sh(t, "podman", "volume", "ls", "-q", "--filter", filter)) {
			podmanQuiet("volume", "rm", "--force", v)
		}
		for _, n := range strings.Fields(sh(t, "podman", "network", "ls", "-q", "--filter", filter)) {
			podmanQuiet("network", "rm", "--force", n)
		}
	})
	return f
}

// open (re)creates the adapters over the same state: a restarted process.
func (f *kFix) open() {
	f.t.Helper()
	var err error
	if f.rt, err = podman.NewKernelRuntime(f.cfg); err != nil {
		f.t.Fatal(err)
	}
	if f.res, err = podman.NewKernelResources(f.cfg); err != nil {
		f.t.Fatal(err)
	}
}

func (f *kFix) flag(name, value string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(value), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *kFix) unflag(name string) { _ = os.Remove(filepath.Join(f.dir, name)) }

// calls counts the recorded podman invocations that begin with prefix.
func (f *kFix) calls(prefix string) int {
	raw, _ := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// objects lists the engine objects labelled with the run.
func (f *kFix) objects() []string {
	f.t.Helper()
	filter := "label=" + podman.LabelRun + "=" + string(f.run)
	var out []string
	for _, kind := range [][]string{{"ps", "-a"}, {"volume", "ls"}, {"network", "ls"}} {
		for _, id := range strings.Fields(sh(f.t, "podman", append(append([]string{}, kind...), "-q", "--filter", filter)...)) {
			out = append(out, kind[0]+":"+id)
		}
	}
	return out
}

func (f *kFix) binding(id string, fence kr.Fence) kr.RuntimeBinding {
	return kr.RuntimeBinding{
		Scope: kScope, RunID: f.run, StepKey: "build", Attempt: 1, WorkloadID: id,
		SpecDigest: "sha256:" + strings.Repeat("a", 64), Backend: podman.Backend, Fence: fence,
	}
}

func (f *kFix) start(b kr.RuntimeBinding, script string, mut ...func(*kr.StartRequest)) kr.StartRequest {
	req := kr.StartRequest{
		Binding: b,
		Spec:    kr.WorkloadSpec{Image: podmantest.Image, Commands: []string{script}, Env: map[string]string{"KODAMA_T177": "1"}, Requires: kr.CapabilitySet{kr.CapNonRoot, kr.CapImagePinning}},
	}
	for _, m := range mut {
		m(&req)
	}
	return req
}

func (f *kFix) workspaceSpec(fence kr.Fence, sources ...kr.SourceInput) kr.ResourceSpec {
	return kr.ResourceSpec{
		Scope: kScope, Owner: kr.OwnerFor(kr.LifetimePipeline, kScope), RunID: f.run, Kind: kr.ResourceWorkspace, Name: "workspace", Lifetime: kr.LifetimePipeline,
		Identity: "ws-" + string(f.run), SpecDigest: "sha256:" + strings.Repeat("b", 64), Fence: fence,
		Preparation: &kr.PreparationInputs{Scope: kScope, RunID: f.run, Sources: sources, Digest: "inputs-1"},
	}
}

func (f *kFix) serviceSpec(name string, fence kr.Fence, commands ...string) kr.ResourceSpec {
	return kr.ResourceSpec{
		Scope: kScope, Owner: kr.OwnerFor(kr.LifetimePipeline, kScope), RunID: f.run, Kind: kr.ResourceService, Name: name, Lifetime: kr.LifetimePipeline,
		Identity: "svc-" + name + "-" + string(f.run), SpecDigest: "sha256:" + strings.Repeat("c", 64), Fence: fence,
		Service: &kr.ServiceSpec{Name: name, Lifetime: kr.LifetimePipeline, Image: podmantest.Image, Commands: commands, Ports: []int{8080}},
	}
}

func handle(s kr.ResourceSpec) kr.ResourceHandle {
	return kr.ResourceHandle{Scope: s.Scope, Owner: s.Owner, Lifetime: s.Lifetime, RunID: s.RunID, Kind: s.Kind, Name: s.Name, Identity: s.Identity, Fence: s.Fence}
}

// kWait polls until done reports true (the adapters' own calls are the only
// coordination, as the engine itself would learn).
func kWait[T any](t *testing.T, what string, poll func() (T, bool)) T {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		v, ok := poll()
		if ok {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (last: %+v)", what, v)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (f *kFix) awaitPhase(ctx context.Context, b kr.RuntimeBinding, want kr.WorkloadPhase) kr.WorkloadObservation {
	f.t.Helper()
	return kWait(f.t, "phase "+string(want), func() (kr.WorkloadObservation, bool) {
		o, err := f.rt.Observe(ctx, b)
		if err != nil {
			f.t.Fatalf("observe: %v", err)
		}
		if err := o.Validate(); err != nil {
			f.t.Fatalf("invalid observation: %v", err)
		}
		return o, o.Phase == want
	})
}

func (f *kFix) awaitState(ctx context.Context, h kr.ResourceHandle, want kr.ResourceState) kr.ResourceObservation {
	f.t.Helper()
	return kWait(f.t, "resource state "+string(want), func() (kr.ResourceObservation, bool) {
		o, err := f.res.Observe(ctx, h)
		if err != nil {
			f.t.Fatalf("observe: %v", err)
		}
		if err := o.Validate(); err != nil {
			f.t.Fatalf("invalid observation: %v", err)
		}
		return o, o.State == want
	})
}

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustStart(t *testing.T, f *kFix, req kr.StartRequest) kr.WorkloadObservation {
	t.Helper()
	o, err := f.rt.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("invalid observation: %v", err)
	}
	return o
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want an error wrapping %v", err, want)
	}
}

// Test_unit_podman_kernel_runtime_contract runs the lifecycle contract of the
// kernel Runtime and ResourceManager ports against the real engine.
func Test_unit_podman_kernel_runtime_contract(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Run("capabilities are declared honestly", func(t *testing.T) {
		f := newKFix(t)
		rc, _ := f.rt.Capabilities(ctx)
		sc, _ := f.res.Capabilities(ctx)
		for _, set := range []kr.CapabilitySet{rc, sc} {
			if err := set.Satisfies(kr.CapabilitySet{kr.CapNonRoot, kr.CapResourceLimits, kr.CapImagePinning, kr.CapAdoptWorkload, kr.CapFencedStop, kr.CapRunServices}); err != nil {
				t.Fatalf("a held guarantee is missing: %v", err)
			}
			for _, over := range []kr.Capability{kr.CapNetworkIsolation, kr.CapPersistentService} {
				if set.Has(over) {
					t.Fatalf("the podman backend over-claims %s", over)
				}
			}
		}
	})

	t.Run("capability refusal happens before any effect", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("refused", 1)
		for name, req := range map[string]kr.StartRequest{
			"unmet capability": f.start(b, "true", func(r *kr.StartRequest) { r.Spec.Requires = kr.CapabilitySet{kr.CapNetworkIsolation} }),
			"unpinned image":   f.start(b, "true", func(r *kr.StartRequest) { r.Spec.Image = "docker.io/library/alpine:3.22" }),
			"foreign backend":  f.start(b, "true", func(r *kr.StartRequest) { r.Binding.Backend = "kubernetes" }),
			"secrets declared but not materialized": f.start(b, "true", func(r *kr.StartRequest) {
				r.Spec.Secrets = []kr.SecretRef{{Owner: domain.Owner{TenantID: kScope.TenantID, ProjectID: kScope.ProjectID}, Name: "TOKEN"}}
			}),
			"workspace of another run": f.start(b, "true", func(r *kr.StartRequest) {
				r.Workspace = kr.ResourceHandle{Scope: kScope, Owner: kr.OwnerFor(kr.LifetimePipeline, kScope), Lifetime: kr.LifetimePipeline, RunID: "other_run", Kind: kr.ResourceWorkspace, Name: "w", Identity: "w", Fence: 1}
			}),
		} {
			_, err := f.rt.Start(ctx, req)
			is(t, err, kr.ErrNotReached)
			if kr.ClassifyEffect(err) != kr.CommandRejected {
				t.Fatalf("%s: %v is not classified rejected", name, err)
			}
		}
		_, err := f.rt.Start(ctx, f.start(b, "true", func(r *kr.StartRequest) { r.Spec.Requires = kr.CapabilitySet{kr.CapNetworkIsolation} }))
		is(t, err, kr.ErrCapability)

		persistent := f.serviceSpec("db", 1)
		persistent.Lifetime, persistent.Service.Lifetime, persistent.RunID = kr.LifetimeProject, kr.LifetimeProject, ""
		_, err = f.res.Ensure(ctx, persistent)
		is(t, err, kr.ErrNotReached)
		is(t, err, kr.ErrCapability)

		if objs := f.objects(); len(objs) != 0 || f.calls("create") != 0 {
			t.Fatalf("a refused request left %v (%d creates)", objs, f.calls("create"))
		}
	})

	t.Run("start observe logs stop remove", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if o, err := f.res.Ensure(ctx, ws); err != nil || o.State != kr.ResourceReady {
			t.Fatalf("workspace: %+v, %v", o, err)
		}
		b := f.binding("life", 1)
		req := f.start(b, `id -u; touch /rootfs 2>/dev/null && echo ROOTFS-WRITABLE || echo ROOTFS-READONLY; echo hello > /workspace/out; cat /workspace/out; sleep 1; echo kodama-done`,
			func(r *kr.StartRequest) { r.Workspace = handle(ws) })
		first := mustStart(t, f, req)
		if first.Binding != b {
			t.Fatalf("the observation names another binding: %+v", first.Binding)
		}
		again := mustStart(t, f, req) // adopted, never created twice
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates for one identity", n)
		}
		_ = again
		o := f.awaitPhase(ctx, b, kr.PhaseExited)
		if o.Result.Code != kr.ResultSucceeded {
			t.Fatalf("result: %+v", o.Result)
		}
		rc, err := f.rt.Logs(ctx, b, kr.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out := readAll(t, rc)
		for _, want := range []string{"65532", "ROOTFS-READONLY", "hello", "kodama-done"} {
			if !strings.Contains(out, want) {
				t.Fatalf("logs %q lack %q", out, want)
			}
		}
		st, err := f.rt.Stop(ctx, b)
		if err != nil || st.Phase != kr.PhaseExited {
			t.Fatalf("stop of an exited workload: %+v, %v", st, err)
		}
		rm, err := f.rt.Remove(ctx, b)
		if err != nil || rm.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if got := sh(t, "podman", "ps", "-a", "-q", "--filter", "label="+podman.LabelRun+"="+string(f.run)); got != "" {
			t.Fatalf("the container survived a confirmed removal: %s", got)
		}
		if rm, err = f.rt.Remove(ctx, b); err != nil || rm.Phase != kr.PhaseAbsent {
			t.Fatalf("a repeated remove: %+v, %v", rm, err)
		}
		_, err = f.rt.Start(ctx, req)
		is(t, err, kr.ErrStaleFence) // a removed identity is never started again
		if n := f.calls("create"); n != 1 {
			t.Fatalf("a removed identity was created again (%d creates)", n)
		}
	})

	t.Run("failing and timed out workloads", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("fails", 1)
		mustStart(t, f, f.start(b, "echo boom; exit 7"))
		o := f.awaitPhase(ctx, b, kr.PhaseExited)
		if o.Result.Code != kr.ResultFailed || o.Result.ExitCode != 7 || o.Result.Reason != kr.ReasonCommandFailed {
			t.Fatalf("result: %+v", o.Result)
		}
		tb := f.binding("slow", 1)
		mustStart(t, f, f.start(tb, "sleep 60", func(r *kr.StartRequest) { r.Spec.Timeout = 2 * time.Second }))
		o = f.awaitPhase(ctx, tb, kr.PhaseExited)
		if o.Result.Code != kr.ResultTimedOut || o.Result.Reason != kr.ReasonStepTimeout {
			t.Fatalf("result: %+v", o.Result)
		}
	})

	t.Run("fenced stop and stale writers", func(t *testing.T) {
		f := newKFix(t)
		b2 := f.binding("fenced", 2)
		mustStart(t, f, f.start(b2, "sleep 60"))
		f.awaitPhase(ctx, b2, kr.PhaseRunning)

		// A writer superseded by the takeover can neither start, stop nor remove.
		b1 := b2
		b1.Fence = 1
		_, err := f.rt.Start(ctx, f.start(b1, "sleep 60"))
		is(t, err, kr.ErrStaleFence)
		_, err = f.rt.Stop(ctx, b1)
		is(t, err, kr.ErrStaleFence)
		_, err = f.rt.Remove(ctx, b1)
		is(t, err, kr.ErrStaleFence)
		if o, _ := f.rt.Observe(ctx, b2); o.Phase != kr.PhaseRunning {
			t.Fatalf("a stale writer disturbed the workload: %+v", o)
		}

		// A different specification under the same identity is refused.
		other := b2
		other.SpecDigest = "sha256:" + strings.Repeat("d", 64)
		_, err = f.rt.Start(ctx, f.start(other, "sleep 60"))
		is(t, err, kr.ErrSpecMismatch)

		// The current fence stops it; the stop fences every later create.
		st, err := f.rt.Stop(ctx, b2)
		if err != nil || st.Phase != kr.PhaseExited || st.Result.Code != kr.ResultCancelled {
			t.Fatalf("stop: %+v, %v", st, err)
		}
		_, err = f.rt.Start(ctx, f.start(b2, "sleep 60"))
		is(t, err, kr.ErrStaleFence)
		higher := b2
		higher.Fence = 3
		_, err = f.rt.Start(ctx, f.start(higher, "sleep 60"))
		is(t, err, kr.ErrStaleFence)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates", n)
		}
	})

	t.Run("scope and backend authority", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("scoped", 1)
		mustStart(t, f, f.start(b, "sleep 60"))
		f.awaitPhase(ctx, b, kr.PhaseRunning)

		foreign := b
		foreign.Scope = domain.Scope{TenantID: "other_tenant", ProjectID: kScope.ProjectID}
		for name, bb := range map[string]kr.RuntimeBinding{"foreign tenant": foreign, "foreign backend": func() kr.RuntimeBinding { x := b; x.Backend = "kubernetes"; return x }()} {
			o, err := f.rt.Observe(ctx, bb)
			if err != nil || o.Phase != kr.PhaseAbsent {
				t.Fatalf("%s observe: %+v, %v", name, o, err)
			}
			if o, err = f.rt.Stop(ctx, bb); err != nil || o.Phase != kr.PhaseAbsent {
				t.Fatalf("%s stop: %+v, %v", name, o, err)
			}
			if o, err = f.rt.Remove(ctx, bb); err != nil || o.Phase != kr.PhaseAbsent {
				t.Fatalf("%s remove: %+v, %v", name, o, err)
			}
		}
		if o, _ := f.rt.Observe(ctx, b); o.Phase != kr.PhaseRunning {
			t.Fatalf("another scope's stop reached the workload: %+v", o)
		}
		if _, err := f.rt.Logs(ctx, foreign, kr.LogOptions{}); err == nil {
			t.Fatal("another scope read the logs")
		}
		_, _ = f.rt.Remove(ctx, b)
	})

	t.Run("a foreign container holding the name is never adopted or touched", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("collision", 1)
		// Create the workload, remove its container behind the adapter's back and
		// plant a lookalike at the same name.
		mustStart(t, f, f.start(b, "sleep 60"))
		f.awaitPhase(ctx, b, kr.PhaseRunning)
		name := strings.TrimSpace(sh(t, "podman", "ps", "-a", "--filter", "label="+podman.LabelRun+"="+string(f.run), "--format", "{{.Names}}"))
		id := sh(t, "podman", "ps", "-a", "-q", "--no-trunc", "--filter", "name="+name)
		sh(t, "podman", "rm", "--force", "--time", "0", id)
		sh(t, "podman", "run", "-d", "--name", name, "--label", podman.LabelRun+"="+string(f.run), podmantest.Image, "sleep", "60")
		o, err := f.rt.Observe(ctx, b)
		if err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("the replaced container was reported as the workload: %+v, %v", o, err)
		}
		if o, err = f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", o, err)
		}
		if got := sh(t, "podman", "ps", "-q", "--filter", "name="+name); got == "" {
			t.Fatal("the foreign container was removed")
		}
	})

	t.Run("service readiness and reachability by name", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		svc := f.serviceSpec("web", 1, "sleep 4; exec nc -lk -p 8080 -e /bin/hostname")
		o, err := f.res.Ensure(ctx, svc)
		if err != nil {
			t.Fatal(err)
		}
		if o.State == kr.ResourceReady {
			t.Fatalf("a service that is not listening is ready: %+v", o)
		}
		if again, err := f.res.Ensure(ctx, svc); err != nil || again.Handle != o.Handle {
			t.Fatalf("a repeated ensure: %+v, %v", again, err)
		}
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates for one service", n)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)

		b := f.binding("client", 1)
		mustStart(t, f, f.start(b, "echo | nc -w 5 web 8080",
			func(r *kr.StartRequest) { r.Workspace = handle(ws); r.Services = []kr.ResourceHandle{handle(svc)} }))
		if o := f.awaitPhase(ctx, b, kr.PhaseExited); o.Result.Code != kr.ResultSucceeded {
			t.Fatalf("the workload could not reach the service: %+v", o.Result)
		}
		rc, err := f.rt.Logs(ctx, b, kr.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if out := readAll(t, rc); strings.TrimSpace(out) == "" {
			t.Fatalf("the service did not answer: %q", out) // it replies with its own hostname
		}
		if rc, err := f.res.Logs(ctx, handle(svc), kr.LogOptions{}); err != nil {
			t.Fatal(err)
		} else {
			rc.Close()
		}
		if _, err := f.res.Logs(ctx, handle(ws), kr.LogOptions{}); err == nil {
			t.Fatal("a workspace has logs")
		}

		// Cleanup: a workspace in use is neither released nor removed, then everything is confirmed.
		if _, err := f.res.Remove(ctx, handle(ws)); err == nil {
			t.Fatal("a workspace referenced by a container was removed")
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove workload: %+v, %v", o, err)
		}
		st, err := f.res.Stop(ctx, handle(svc))
		if err != nil || st.State != kr.ResourceStopped {
			t.Fatalf("stop service: %+v, %v", st, err)
		}
		for _, h := range []kr.ResourceHandle{handle(svc), handle(ws)} {
			rm, err := f.res.Remove(ctx, h)
			if err != nil || !rm.ConfirmedRemoved() {
				t.Fatalf("remove %s: %+v, %v", h.Name, rm, err)
			}
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("a confirmed cleanup left %v", objs)
		}
		_, err = f.res.Ensure(ctx, svc)
		is(t, err, kr.ErrStaleFence)
	})

	t.Run("workspace preparation", func(t *testing.T) {
		f := newKFix(t)
		f.prep.command = []string{"/bin/sh", "-c", "sleep 2; echo prepared > /workspace/prepared"}
		ws := f.workspaceSpec(1, kr.SourceInput{Name: "repo", Locator: "x", Revision: strings.Repeat("1", 40), Path: "repo"})
		o, err := f.res.Ensure(ctx, ws)
		if err != nil || o.State != kr.ResourceStarting {
			t.Fatalf("ensure: %+v, %v", o, err)
		}
		// A workload cannot use a workspace that is still being prepared.
		b := f.binding("early", 1)
		_, err = f.rt.Start(ctx, f.start(b, "true", func(r *kr.StartRequest) { r.Workspace = handle(ws) }))
		is(t, err, kr.ErrNotReached)
		f.awaitState(ctx, handle(ws), kr.ResourceReady)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d preparation creates", n)
		}
		if _, err := f.res.Ensure(ctx, ws); err != nil || f.calls("create") != 1 {
			t.Fatalf("a repeated ensure prepared again: %v", err)
		}
		c := f.binding("reader", 1)
		mustStart(t, f, f.start(c, "cat /workspace/prepared", func(r *kr.StartRequest) { r.Workspace = handle(ws) }))
		f.awaitPhase(ctx, c, kr.PhaseExited)
		rc, _ := f.rt.Logs(ctx, c, kr.LogOptions{})
		if out := readAll(t, rc); !strings.Contains(out, "prepared") {
			t.Fatalf("the workspace was not prepared: %q", out)
		}
		_, _ = f.rt.Remove(ctx, c)
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}

		// A failing preparation fails the workspace and nothing mounts it.
		g := newKFix(t)
		g.prep.command = []string{"/bin/sh", "-c", "exit 3"}
		bad := g.workspaceSpec(1, kr.SourceInput{Name: "repo", Locator: "x", Revision: strings.Repeat("1", 40), Path: "repo"})
		if _, err := g.res.Ensure(ctx, bad); err != nil {
			t.Fatal(err)
		}
		fo := g.awaitState(ctx, handle(bad), kr.ResourceFailed)
		if fo.Reason != kr.ReasonPreparationFailed {
			t.Fatalf("reason: %q", fo.Reason)
		}
		if rm, err := g.res.Remove(ctx, handle(bad)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove a failed workspace: %+v, %v", rm, err)
		}
	})

	t.Run("sources without a preparer are refused", func(t *testing.T) {
		f := newKFix(t)
		cfg := f.cfg
		cfg.Preparer = nil
		res, err := podman.NewKernelResources(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = res.Ensure(ctx, f.workspaceSpec(1, kr.SourceInput{Name: "repo", Locator: "x", Revision: strings.Repeat("1", 40), Path: "repo"}))
		is(t, err, kr.ErrNotReached)
		is(t, err, kr.ErrCapability)
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("a refused workspace left %v", objs)
		}
	})
}

// Test_unit_podman_adoption_and_cleanup proves adoption after a lost response
// and cleanup that confirms removal, including a delayed create that must be
// rejected, against the real engine.
func Test_unit_podman_adoption_and_cleanup(t *testing.T) {
	kEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	t.Run("a lost create response is adopted, never replayed", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("lost", 1)
		req := f.start(b, "echo adopted; sleep 30")
		f.flag("lose-create", "1") // the engine creates; the response is lost, and so is the first look at the engine
		_, err := f.rt.Start(ctx, req)
		if err == nil {
			t.Fatal("a lost response was reported as success")
		}
		if kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("the failure is classified %s", kr.ClassifyEffect(err))
		}
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates", n)
		}
		if got := sh(t, "podman", "ps", "-a", "-q", "--filter", "label="+podman.LabelRun+"="+string(f.run)); got == "" {
			t.Fatal("the engine holds no container: the fault did not model a lost response")
		}

		f.open() // a restarted process finds the container by identity, not by replaying
		o, err := f.rt.Observe(ctx, b)
		if err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("a created, unstarted container is %+v, %v", o, err) // adoptable, not yet running
		}
		o = mustStart(t, f, req)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("the adopted workload was created again (%d creates)", n)
		}
		f.awaitPhase(ctx, b, kr.PhaseRunning)
		if ids := strings.Fields(sh(t, "podman", "ps", "-a", "-q", "--filter", "label="+podman.LabelRun+"="+string(f.run))); len(ids) != 1 {
			t.Fatalf("%d containers for one identity", len(ids))
		}
		rc, _ := f.rt.Logs(ctx, b, kr.LogOptions{})
		_ = rc.Close()
		if rm, err := f.rt.Remove(ctx, b); err != nil || rm.Phase != kr.PhaseAbsent {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
	})

	t.Run("an unresolved create is never replayed, never confirmed removed", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("unresolved", 1)
		req := f.start(b, "sleep 30")
		f.flag("swallow-create", "1") // the answer is lost and nothing was created
		_, err := f.rt.Start(ctx, req)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("start: %v", err)
		}
		_, err = f.rt.Start(ctx, req)
		is(t, err, kr.ErrUncertain)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("an unresolved create was replayed (%d creates)", n)
		}
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		// Stop fences it; the removal cannot be confirmed while the create is unresolved.
		if o, err := f.rt.Stop(ctx, b); err != nil || o.Phase != kr.PhaseUnknown {
			t.Fatalf("stop: %+v, %v", o, err)
		}
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase == kr.PhaseAbsent {
			t.Fatalf("an unresolved create was confirmed removed: %+v, %v", o, err)
		}
		// A late create is rejected: nothing is created from the fenced identity.
		_, err = f.rt.Start(ctx, req)
		is(t, err, kr.ErrStaleFence)
		if n := f.calls("create"); n != 1 {
			t.Fatalf("a delayed create was issued (%d creates)", n)
		}
	})

	t.Run("a delayed create after stop is rejected", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("delayed", 5)
		// The stop arrives first, for an identity nobody recorded.
		o, err := f.rt.Stop(ctx, b)
		if err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("stop of an unrecorded workload: %+v, %v", o, err)
		}
		for _, fence := range []kr.Fence{3, 5, 6} {
			late := b
			late.Fence = fence
			_, err = f.rt.Start(ctx, f.start(late, "sleep 30"))
			is(t, err, kr.ErrStaleFence)
		}
		if f.calls("create") != 0 || len(f.objects()) != 0 {
			t.Fatalf("a delayed create reached the engine: %v", f.objects())
		}
		// The same for a resource.
		ws := f.workspaceSpec(4)
		if o, err := f.res.Stop(ctx, handle(ws)); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("stop of an unrecorded workspace: %+v, %v", o, err)
		}
		_, err = f.res.Ensure(ctx, ws)
		is(t, err, kr.ErrStaleFence)
		if len(f.objects()) != 0 {
			t.Fatalf("a delayed ensure created %v", f.objects())
		}
	})

	t.Run("an uncertain stop is reconciled, not replayed", func(t *testing.T) {
		f := newKFix(t)
		b := f.binding("stopping", 1)
		mustStart(t, f, f.start(b, "sleep 60"))
		f.awaitPhase(ctx, b, kr.PhaseRunning)

		f.flag("down", "1") // the engine cannot be reached during the stop
		if _, err := f.rt.Stop(ctx, b); err == nil {
			t.Fatal("a stop that could not reach the engine succeeded")
		}
		f.unflag("down")
		// The intent was recorded: the workload cannot be launched again, and the
		// observable state decides what is left to do.
		_, err := f.rt.Start(ctx, f.start(b, "sleep 60"))
		is(t, err, kr.ErrStaleFence)
		if o, err := f.rt.Observe(ctx, b); err != nil || o.Phase != kr.PhaseRunning {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		st, err := f.rt.Stop(ctx, b)
		if err != nil || st.Phase != kr.PhaseExited || st.Result.Code != kr.ResultCancelled {
			t.Fatalf("stop: %+v, %v", st, err)
		}
	})

	t.Run("cleanup confirms removal and retries after an outage", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		if _, err := f.res.Ensure(ctx, ws); err != nil {
			t.Fatal(err)
		}
		svc := f.serviceSpec("db", 1, "exec sleep 120")
		svc.Service.Ports = nil
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		b := f.binding("job", 1)
		mustStart(t, f, f.start(b, "sleep 120", func(r *kr.StartRequest) { r.Workspace = handle(ws); r.Services = []kr.ResourceHandle{handle(svc)} }))
		f.awaitPhase(ctx, b, kr.PhaseRunning)
		f.awaitState(ctx, handle(svc), kr.ResourceReady)

		f.flag("down", "1")
		if o, err := f.rt.Remove(ctx, b); err == nil && o.Phase == kr.PhaseAbsent {
			t.Fatalf("removal confirmed with the engine down: %+v", o)
		}
		f.unflag("down")
		if o, err := f.rt.Remove(ctx, b); err != nil || o.Phase != kr.PhaseAbsent {
			t.Fatalf("remove after the outage: %+v, %v", o, err)
		}
		// The workspace is released only after every holder is gone, and removal is
		// confirmed only when the engine no longer has the object.
		if o, err := f.res.Stop(ctx, handle(ws)); err != nil || o.State != kr.ResourceStopped {
			t.Fatalf("stop workspace: %+v, %v", o, err)
		}
		for _, h := range []kr.ResourceHandle{handle(svc), handle(ws)} {
			o, err := f.res.Remove(ctx, h)
			if err != nil || !o.ConfirmedRemoved() {
				t.Fatalf("remove %s: %+v, %v", h.Name, o, err)
			}
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("cleanup left %v", objs)
		}
		// Observation after the cleanup is positive absence, repeatable.
		for _, h := range []kr.ResourceHandle{handle(svc), handle(ws)} {
			if o, err := f.res.Observe(ctx, h); err != nil || !o.ConfirmedRemoved() {
				t.Fatalf("observe %s after removal: %+v, %v", h.Name, o, err)
			}
		}
	})

	t.Run("a lost volume create response is adopted", func(t *testing.T) {
		f := newKFix(t)
		ws := f.workspaceSpec(1)
		f.flag("lose-volume-create", "1")
		_, err := f.res.Ensure(ctx, ws)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		f.open()
		o, err := f.res.Ensure(ctx, ws)
		if err != nil || o.State != kr.ResourceReady {
			t.Fatalf("the volume was not adopted: %+v, %v", o, err)
		}
		if n := f.calls("volume create"); n != 1 {
			t.Fatalf("%d volume creates for one identity", n)
		}
		if rm, err := f.res.Remove(ctx, handle(ws)); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove: %+v, %v", rm, err)
		}
	})

	t.Run("a lost service create response is adopted", func(t *testing.T) {
		f := newKFix(t)
		svc := f.serviceSpec("cache", 1, "exec sleep 120")
		svc.Service.Ports = nil
		f.flag("lose-create", "1")
		_, err := f.res.Ensure(ctx, svc)
		if err == nil || kr.ClassifyEffect(err) != kr.CommandUncertain {
			t.Fatalf("ensure: %v", err)
		}
		f.open()
		if o, err := f.res.Observe(ctx, handle(svc)); err != nil || o.State == kr.ResourceRemoved || o.State == kr.ResourceFailed {
			t.Fatalf("observe: %+v, %v", o, err)
		}
		if _, err := f.res.Ensure(ctx, svc); err != nil {
			t.Fatal(err)
		}
		if n := f.calls("create"); n != 1 {
			t.Fatalf("%d creates", n)
		}
		f.awaitState(ctx, handle(svc), kr.ResourceReady)
		for _, step := range []func(context.Context, kr.ResourceHandle) (kr.ResourceObservation, error){f.res.Stop, f.res.Remove} {
			if _, err := step(ctx, handle(svc)); err != nil {
				t.Fatal(err)
			}
		}
		if objs := f.objects(); len(objs) != 0 {
			t.Fatalf("left %v", objs)
		}
	})
}

// Test_unit_podman_kernel_adapter_boundary keeps the kernel adapters free of
// shell, SQL, broker, Git, task, identity and agent dependencies: they import
// the standard library, internal/domain and the kernel runtime contract only.
func Test_unit_podman_kernel_adapter_boundary(t *testing.T) {
	files, err := filepath.Glob("kernel_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no kernel adapter sources: %v", err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			switch {
			case path == "github.com/optimiware/kodama/internal/domain", path == "github.com/optimiware/kodama/internal/kernel/runtime":
			case strings.Contains(first, "."):
				t.Errorf("%s imports %s: the kernel adapters use the standard library and the kernel contract only", f, path)
			case path == "net/http" || path == "database/sql":
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
	if checked < 3 {
		t.Fatalf("expected the core, runtime and resources sources; checked %d", checked)
	}
}
