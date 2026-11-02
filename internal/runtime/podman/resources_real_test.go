package podman_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// These tests run the run-resources adapter against the real engine and follow
// the conventions of the other real-Podman tests: they skip when no engine
// answers (make test-podman requires one) and use the pinned image.

// sh runs a command and returns its trimmed combined output.
func sh(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// podmanQuiet runs podman ignoring failure and output (cleanup).
func podmanQuiet(args ...string) { _ = exec.Command("podman", args...).Run() }

// realEnv is a real-engine state directory, a control network, an entrypoint
// stand-in and the dependencies of Resources.
type realEnv struct {
	t       *testing.T
	state   string
	reg     *resRegistrar
	prov    *prepCheckout
	cfg     podman.ResourcesConfig
	control string
	outage  string
}

// newRealEnv prepares a real-engine environment. Everything the test creates is
// labeled with the run IDs it registers and removed at the end, so a failing
// test leaves no container, volume or network behind.
func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	podmantest.Require(t)
	dir := podmantest.SharedDir(t)
	e := &realEnv{t: t, state: filepath.Join(dir, "state"), reg: &resRegistrar{}, prov: &prepCheckout{ca: testCA(t)}, outage: filepath.Join(dir, "outage")}

	// `kodama prepare` stands in as a script: it checks what the real one would
	// be handed and writes into /workspace as the workload user.
	bin := filepath.Join(dir, "kodama")
	script := `#!/bin/sh
set -e
test "$1" = prepare
test -r "$KODAMA_CHECKOUT_CREDENTIAL_FILE"
test -r "$KODAMA_GIT_CA_FILE"
test -r "$KODAMA_ADMISSION_CREDENTIAL_FILE"
test -n "$KODAMA_WORKLOAD_UID"
echo "prepared by $(id -u):$(id -g) as $KODAMA_RESOURCE_KEY" > "$KODAMA_WORKSPACE_DIR/prepared"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// A wrapper that fails as an unreachable engine while the outage file exists.
	wrapper := filepath.Join(dir, "podman")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then echo 'Error: cannot connect to Podman' >&2; exit 125; fi\nexec podman \"$@\"\n", e.outage)), 0o755); err != nil {
		t.Fatal(err)
	}

	e.control = "kt115-" + strings.TrimPrefix(string(runtimetest.NewAttemptID(t)), "att_")
	sh(t, "podman", "network", "create", "--label", podman.LabelManagedBy+"=t115-test", e.control)
	t.Cleanup(func() { podmanQuiet("network", "rm", "--force", e.control) })

	e.cfg = podman.ResourcesConfig{
		StateDir: e.state, Binary: wrapper, EntrypointBinary: bin, ServerURL: "https://runtime.invalid", CA: []byte("runtime-ca"),
		ControlNetwork: e.control, Registrar: e.reg, Checkout: e.prov, ProbeImage: podmantest.Image, StopTimeout: time.Second,
	}
	return e
}

// track removes whatever the run left, by label, at the end of the test.
func (e *realEnv) track(r run) {
	e.t.Helper()
	e.t.Cleanup(func() {
		filter := "label=" + podman.LabelRun + "=" + string(r.id)
		for _, id := range strings.Fields(sh(e.t, "podman", "ps", "-a", "-q", "--filter", filter)) {
			podmanQuiet("rm", "--force", "--time", "0", id)
		}
		for _, v := range strings.Fields(sh(e.t, "podman", "volume", "ls", "-q", "--filter", filter)) {
			podmanQuiet("volume", "rm", "--force", v)
		}
		for _, n := range strings.Fields(sh(e.t, "podman", "network", "ls", "-q", "--filter", filter)) {
			podmanQuiet("network", "rm", "--force", n)
		}
	})
}

func (e *realEnv) open() *podman.Resources {
	e.t.Helper()
	r, err := podman.NewResources(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *realEnv) runtime(prog podman.Program) *podman.Runtime {
	e.t.Helper()
	if prog == nil {
		prog = podman.DeclaredCommand{}
	}
	r, err := podman.New(podman.Config{StateDir: e.state, StopTimeout: time.Second, RunResources: true, Program: prog})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// leftovers lists the engine objects of a run: nothing may remain after cleanup.
func leftovers(t testing.TB, r run) []string {
	t.Helper()
	filter := "label=" + podman.LabelRun + "=" + string(r.id)
	var out []string
	for _, kind := range [][]string{{"ps", "-a"}, {"volume", "ls"}, {"network", "ls"}} {
		args := append(slices.Clone(kind), "-q", "--filter", filter)
		for _, id := range strings.Fields(sh(t, "podman", args...)) {
			out = append(out, kind[0]+":"+id)
		}
	}
	return out
}

// Test_podman_run_resources_contract runs the exported companion-port contract
// over the real engine: a named volume, an internal network, real service and
// preparation containers.
func Test_podman_run_resources_contract(t *testing.T) {
	podmantest.Require(t)
	runtimetest.RunResourcesContract(t, func(t *testing.T) runtimetest.ResourcesFixture {
		e := newRealEnv(t)
		exists := func(h execution.ResourceHandle) bool {
			kind := "container"
			switch h.Kind {
			case execution.KindNetwork:
				kind = "network"
			case execution.KindWorkspace:
				kind = "volume"
			}
			return exec.Command("podman", kind, "exists", h.Name).Run() == nil
		}
		// The contract's run IDs are unknown ahead: sweep through the handles the
		// adapter is asked to ensure.
		var seen []execution.ResourceHandle
		var seenMu sync.Mutex
		t.Cleanup(func() {
			seenMu.Lock()
			defer seenMu.Unlock()
			for _, h := range seen {
				switch h.Kind {
				case execution.KindNetwork:
					podmanQuiet("network", "rm", "--force", h.Name)
				case execution.KindWorkspace:
					podmanQuiet("volume", "rm", "--force", h.Name)
				default:
					podmanQuiet("rm", "--force", "--time", "0", h.Name)
				}
			}
		})
		note := func(h execution.ResourceHandle) {
			seenMu.Lock()
			defer seenMu.Unlock()
			seen = append(seen, h)
		}
		return runtimetest.ResourcesFixture{
			Open: func() execution.RunResources { return &noting{RunResources: e.open(), note: note} },
			Want: execution.ResourceCapabilities{Backend: podman.Backend, Set: podman.ResourceCapabilities},
			Service: func(string) execution.ServiceSpec {
				b := dbBody()
				b.Image, b.Command, b.Args = podmantest.Image, []string{"/bin/sh", "-c"}, []string{"touch /tmp/ready; echo service-up; exec sleep 300"}
				b.Env, b.Storage = map[string]string{}, []execution.StorageSpec{}
				b.Resources = execution.ServiceResourcesSpec{CPUMillis: 500, MemoryMiB: 128, EphemeralMiB: 16}
				b.Readiness = execution.ReadinessSpec{Probe: "exec", Exec: []string{"/bin/sh", "-c", "test -f /tmp/ready"}, TimeoutSecs: 60, ProbeTimeout: 5}
				return b
			},
			Preparation: func() execution.PreparationSpec {
				p := newRun(t).preparation().Preparation
				p.Image = podmantest.Image
				return *p
			},
			Foreign: func(h execution.ResourceHandle) {
				switch h.Kind {
				case execution.KindWorkspace:
					sh(t, "podman", "volume", "create", h.Name)
				case execution.KindNetwork:
					sh(t, "podman", "network", "create", h.Name)
				}
			},
			ForeignIntact: exists,
			Exists:        exists,
			Fault: func() func() {
				if err := os.WriteFile(e.outage, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return func() { _ = os.Remove(e.outage) }
			},
			HoldWorkspace: func(h execution.ResourceHandle) func() {
				rt := e.runtime(nil)
				w, err := rt.Start(context.Background(), runtimetest.SharedSpec(t, h, podmantest.Image, "exec sleep 300"))
				if err != nil {
					t.Fatalf("the holder did not start: %v", err)
				}
				return func() {
					_, _ = rt.Stop(context.Background(), w)
					_ = rt.Remove(context.Background(), w)
				}
			},
			Timeout: 90 * time.Second,
		}
	})
}

// noting records every handle an adapter returns so a failing contract case
// still sweeps its objects.
type noting struct {
	execution.RunResources
	note func(execution.ResourceHandle)
}

func (n *noting) Ensure(ctx context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	n.note(spec.Handle(""))
	return n.RunResources.Ensure(ctx, spec)
}

// secondImage pulls a different image (BusyBox) and returns its digest-pinned
// reference, so the roundtrip crosses two images with different default users.
func secondImage(t *testing.T) string {
	t.Helper()
	const ref = "docker.io/library/busybox:1.37"
	if err := exec.Command("podman", "image", "exists", ref).Run(); err != nil {
		if out, err := exec.Command("podman", "pull", "--quiet", ref).CombinedOutput(); err != nil {
			if os.Getenv("KODAMA_REQUIRE_PODMAN") == "1" {
				t.Fatalf("pulling %s: %v: %s", ref, err, out)
			}
			t.Skipf("the second image is unavailable: %v", err)
		}
	}
	digest := sh(t, "podman", "image", "inspect", "--format", "{{.Digest}}", ref)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("no digest for %s: %q", ref, digest)
	}
	return "docker.io/library/busybox@" + digest
}

// attempt runs a shared attempt to completion and returns its output. The
// attempt's container is removed so the workspace is released for the next one.
func attempt(t *testing.T, rt *podman.Runtime, ws execution.ResourceHandle, image, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := runtimetest.SharedSpec(t, ws, image, script)
	w, err := rt.Start(ctx, s)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), w) })
	var out bytes.Buffer
	for !strings.Contains(out.String(), "attempt-done") {
		out.Reset()
		if err := rt.Logs(ctx, w, &out); err != nil {
			t.Fatalf("logs: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("the attempt never finished: %s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	for {
		ev, err := rt.Stop(ctx, w)
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
		if ev.Stopped {
			break
		}
	}
	if err := rt.Remove(ctx, w); err != nil {
		t.Fatalf("remove: %v", err)
	}
	return out.String()
}

// Test_podman_run_resources_roundtrip is the multi-image volume and service
// roundtrip: preparation fills the run volume, two attempts of different images
// share it (each as the workload user, whatever the image's default user is) and
// reach a run service through its endpoint alias on the run network; the
// service sees neither the workspace nor the host; a restarted adapter adopts
// everything and cleanup leaves no container, volume or network.
func Test_podman_run_resources_roundtrip(t *testing.T) {
	podmantest.Require(t)
	other := secondImage(t)
	e := newRealEnv(t)
	r := newRun(t)
	e.track(r)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res := e.open()

	netSpec, wsSpec := r.network(), r.workspace()
	prepSpec := r.preparation()
	prepSpec.Preparation.Image = podmantest.Image
	body := dbBody()
	body.Image, body.Command, body.Env, body.Storage = podmantest.Image, []string{"/bin/sh", "-c"}, map[string]string{}, []execution.StorageSpec{}
	body.Args = []string{"while true; do echo pong | nc -l -p 5432; done"}
	body.Resources = execution.ServiceResourcesSpec{CPUMillis: 500, MemoryMiB: 128, EphemeralMiB: 16}
	body.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 60, ProbeTimeout: 5}
	dbSpec := r.service("db", body)

	ensure := func(res execution.RunResources, spec execution.ResourceSpec) execution.ResourceHandle {
		obs, err := res.Ensure(ctx, spec)
		if err != nil {
			t.Fatalf("ensure %s: %v", spec.Key, err)
		}
		return obs.Handle
	}
	net := ensure(res, netSpec)
	ws := ensure(res, wsSpec)
	prep := ensure(res, prepSpec)
	db := ensure(res, dbSpec)
	await := func(res execution.RunResources, h execution.ResourceHandle, want execution.ResourcePhase) execution.ResourceObservation {
		t.Helper()
		for {
			obs, err := res.Observe(ctx, h)
			if err != nil {
				t.Fatalf("observe %s: %v", h.Key, err)
			}
			if obs.Phase == want {
				return obs
			}
			if obs.Phase == execution.PhaseFailed || ctx.Err() != nil {
				logs, _ := res.Logs(ctx, h, execution.LogBound{MaxBytes: 4096})
				t.Fatalf("%s is %s, want %s: %+v\n%s", h.Key, obs.Phase, want, obs, logs.Content)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	await(res, net, execution.PhaseReady)
	await(res, ws, execution.PhaseReady)
	await(res, prep, execution.PhaseReady) // the preparation completed
	ready := await(res, db, execution.PhaseReady)
	if ready.ProbeKind != "tcp" {
		t.Fatalf("%+v", ready)
	}

	// The preparation is finished: stop it (execution does so once the receipt is
	// recorded) and its credentials are gone.
	for {
		ev, err := res.Stop(ctx, prep)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Stopped {
			break
		}
	}
	if _, err := os.Stat(filepath.Join(e.state, "resources", prep.Name, "bootstrap")); !os.IsNotExist(err) {
		t.Fatalf("the preparation's credentials survived its stop: %v", err)
	}

	rt := e.runtime(nil)
	host := dbSpec.Service.Host

	// First attempt: alpine. It sees what the preparation wrote as the workload
	// user, writes its own file and reaches the service through the alias.
	out := attempt(t, rt, ws, podmantest.Image, fmt.Sprintf(
		`cat /workspace/prepared; id -u; cat /proc/self/uid_map; echo from-alpine > /workspace/from-alpine; nc -w 5 %s 5432 </dev/null; test -z "$(wget -q -T 3 -O- http://example.com 2>/dev/null)" && echo no-internet; echo attempt-done`, host))
	// The attempt runs in a user namespace of its own (the role's userNamespace),
	// not in the engine's default one.
	defaultMap := sh(t, "podman", "run", "--rm", podmantest.Image, "cat", "/proc/self/uid_map")
	if strings.Contains(out, defaultMap) {
		t.Fatalf("the attempt ran in the engine's default user namespace:\n%s", out)
	}
	for _, want := range []string{"prepared by 65532:65532 as preparation", "\n65532\n", "pong", "no-internet"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the first attempt output lacks %q:\n%s", want, out)
		}
	}

	// Second attempt: a different image (BusyBox), another default user. It reads
	// the first's file through the same volume and writes its own.
	out = attempt(t, rt, ws, other, fmt.Sprintf(
		`cat /workspace/from-alpine; id -u; echo from-busybox > /workspace/from-busybox; nc -w 5 %s 5432 </dev/null; echo attempt-done`, host))
	for _, want := range []string{"from-alpine", "\n65532\n", "pong"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the second attempt output lacks %q:\n%s", want, out)
		}
	}
	// Both attempts' files belong to the workload user: the volume is mapped once.
	// Every file belongs to the workload user of the per-container user namespace
	// the attempts ran in, whatever the image: the same owner for all three, which
	// that user sees as itself.
	owners := sh(t, "podman", "run", "--rm", "--userns=auto", "--user", "65532:65532", "--volume", ws.Name+":/w:ro", podmantest.Image,
		"sh", "-c", "stat -c '%u:%g %n' /w/prepared /w/from-alpine /w/from-busybox")
	if strings.Count(owners, "65532:65532") != 3 {
		t.Fatalf("file ownership in the run volume:\n%s", owners)
	}
	// The service container has no workspace and no engine socket.
	svcID := sh(t, "podman", "ps", "-a", "-q", "--no-trunc", "--filter", "name="+db.Name)
	mounts := sh(t, "podman", "inspect", "--format", "{{range .Mounts}}{{.Destination}} {{end}}", svcID)
	if strings.Contains(mounts, "/workspace") || strings.Contains(mounts, "sock") {
		t.Fatalf("service mounts: %q", mounts)
	}
	if err := exec.Command("podman", "exec", svcID, "ls", "/workspace").Run(); err == nil {
		t.Fatal("the service can see /workspace")
	}
	// Run network isolation by construction: the network is internal.
	if internal := sh(t, "podman", "network", "inspect", "--format", "{{.Internal}}", netSpec.Name); internal != "true" {
		t.Fatalf("the run network is not internal: %q", internal)
	}

	// A restarted adapter (fresh Resources and Runtime over the same state)
	// adopts every resource and cancels them: stop in reverse order, then remove.
	restarted := e.open()
	for _, spec := range []execution.ResourceSpec{netSpec, wsSpec, dbSpec} {
		if again := ensure(restarted, spec); again.UID == "" {
			t.Fatalf("%s: no instance after the restart", spec.Key)
		}
	}
	for _, h := range []execution.ResourceHandle{net, ws} {
		if obs, err := restarted.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseReady {
			t.Fatalf("after the restart %s: %+v %v", h.Key, obs, err)
		}
	}
	await(restarted, db, execution.PhaseReady)
	// The finished preparation is stopped, not running: it stays out of the way.
	if obs, err := restarted.Observe(ctx, prep); err != nil || obs.Phase != execution.PhaseStopped {
		t.Fatalf("the stopped preparation after the restart: %+v %v", obs, err)
	}
	for _, h := range []execution.ResourceHandle{db, prep, ws, net} {
		for {
			ev, err := restarted.Stop(ctx, h)
			if err != nil {
				t.Fatalf("stop %s: %v", h.Key, err)
			}
			if ev.Stopped && (h.Kind != execution.KindWorkspace || ev.Released) {
				break
			}
		}
	}
	for _, h := range []execution.ResourceHandle{db, prep, ws, net} {
		rm, err := restarted.Remove(ctx, h)
		if err != nil || !rm.Removed {
			t.Fatalf("remove %s: %+v %v", h.Key, rm, err)
		}
		if obs, err := restarted.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseAbsent {
			t.Fatalf("after removal %s: %+v %v", h.Key, obs, err)
		}
	}
	if left := leftovers(t, r); len(left) != 0 {
		t.Fatalf("leaked objects: %v", left)
	}
	for _, spec := range []execution.ResourceSpec{netSpec, wsSpec, prepSpec, dbSpec} {
		if _, err := restarted.Ensure(ctx, spec); err == nil || !strings.Contains(err.Error(), "fenced") {
			t.Fatalf("%s: ensure after removal: %v", spec.Key, err)
		}
	}
	if left := leftovers(t, r); len(left) != 0 {
		t.Fatalf("a removed resource was created again: %v", left)
	}
}

// Test_podman_run_resources_secret_service_real: a service with secret bindings
// runs its declared argv through the injected entrypoint binary, which receives
// its identity as the contract names it.
func Test_podman_run_resources_secret_service_real(t *testing.T) {
	podmantest.Require(t)
	e := newRealEnv(t)
	r := newRun(t)
	e.track(r)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res := e.open()
	if _, err := res.Ensure(ctx, r.network()); err != nil {
		t.Fatal(err)
	}
	// The injected binary stands in for `kodama service-run`: it checks its
	// identity, then execs the declared argv after `--`.
	script := `#!/bin/sh
set -e
test "$1" = service-run
test "$2" = --
shift 2
test -r "$KODAMA_ADMISSION_CREDENTIAL_FILE"
test "$KODAMA_RESOURCE_KEY" = service.db
echo "supervised as $KODAMA_WORKLOAD_NAME"
exec "$@"
`
	if err := os.WriteFile(e.cfg.EntrypointBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := dbBody()
	body.Image, body.Command, body.Args = podmantest.Image, []string{"/bin/sh", "-c"}, []string{"echo argv-ok; exec sleep 300"}
	body.Env, body.Storage = map[string]string{}, []execution.StorageSpec{}
	body.Secrets = []execution.SecretPin{{Ref: "secret://project/db", Version: 1, Name: "PW", Delivery: "env"}}
	body.Readiness = execution.ReadinessSpec{Probe: "exec", Exec: []string{"/bin/sh", "-c", "true"}, TimeoutSecs: 60, ProbeTimeout: 5}
	spec := r.service("db", body)
	h, err := res.Ensure(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	for {
		obs, err := res.Observe(ctx, h.Handle)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase == execution.PhaseReady {
			break
		}
		if obs.Phase == execution.PhaseFailed || ctx.Err() != nil {
			logs, _ := res.Logs(ctx, h.Handle, execution.LogBound{MaxBytes: 4096})
			t.Fatalf("%+v\n%s", obs, logs.Content)
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, err := res.Logs(ctx, h.Handle, execution.LogBound{MaxBytes: 4096})
	if err != nil || !strings.Contains(logs.Content, "supervised as "+spec.Name) || !strings.Contains(logs.Content, "argv-ok") {
		t.Fatalf("logs %q: %v", logs.Content, err)
	}
	for {
		ev, err := res.Stop(ctx, h.Handle)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Stopped {
			break
		}
	}
	if rm, err := res.Remove(ctx, h.Handle); err != nil || !rm.Removed {
		t.Fatalf("%+v %v", rm, err)
	}
	netH := execution.ResourceHandle{Scope: r.scope, RunID: r.id, Kind: execution.KindNetwork, Key: execution.KeyNetwork, Name: r.network().Name}
	_, _ = res.Stop(ctx, netH)
	_, _ = res.Remove(ctx, netH)
	if left := leftovers(t, r); len(left) != 0 {
		t.Fatalf("leaked objects: %v", left)
	}
}
