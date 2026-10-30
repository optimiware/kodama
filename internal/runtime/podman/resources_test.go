package podman_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

const (
	svcImage    = "registry.invalid/kodama/db@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	prepImage   = "registry.invalid/kodama/prepare@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	probeImage  = "registry.invalid/kodama/probe@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	repoURL     = "https://git.invalid/org/repo.git"
	controlNet  = "kodama-control"
	entrypoint  = "/host/kodama"
	serverURL   = "https://runtime.invalid"
	runtimeUser = "65532:65532"
)

// resRegistrar is a podman.ResourceRegistrar that records what it was asked to
// register and can be scripted to fail.
type resRegistrar struct {
	mu   sync.Mutex
	cmds []podman.ResourceRegisterCommand
	err  error
}

func (r *resRegistrar) RegisterResource(_ context.Context, cmd podman.ResourceRegisterCommand) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return r.err
}

func (r *resRegistrar) commands() []podman.ResourceRegisterCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cmds)
}

// prepCheckout answers for exactly the preparation's repository.
type prepCheckout struct {
	mu    sync.Mutex
	calls int
	ca    []byte
	deny  error
}

func (p *prepCheckout) Checkout(_ context.Context, spec execution.ResourceSpec) (bootstrap.CheckoutMaterial, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.deny != nil {
		return bootstrap.CheckoutMaterial{}, p.deny
	}
	return bootstrap.CheckoutMaterial{Scope: spec.Scope, RepositoryURL: spec.Preparation.RepositoryURL, Credential: []byte(checkoutUser + ":" + checkoutPass), GitCA: p.ca}, nil
}

func (p *prepCheckout) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// rEnv is a fake engine, a state directory and the dependencies of Resources.
type rEnv struct {
	t     *testing.T
	fake  fakeEnv
	state string
	reg   *resRegistrar
	prov  *prepCheckout
	ca    []byte
	cfg   podman.ResourcesConfig
}

func newREnv(t *testing.T) *rEnv {
	t.Helper()
	fake := newFake(t)
	e := &rEnv{t: t, fake: fake, state: t.TempDir(), reg: &resRegistrar{}, ca: testCA(t)}
	e.prov = &prepCheckout{ca: e.ca}
	e.cfg = podman.ResourcesConfig{
		StateDir: e.state, Binary: fake.Binary, EntrypointBinary: entrypoint, ServerURL: serverURL, CA: []byte("runtime-ca"),
		ControlNetwork: controlNet, Registrar: e.reg, Checkout: e.prov, ProbeImage: probeImage, StopTimeout: time.Second,
	}
	return e
}

func (e *rEnv) open() *podman.Resources {
	e.t.Helper()
	r, err := podman.NewResources(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// runtime is the attempt Runtime over the same state, with run resources on.
func (e *rEnv) runtime(p podman.Program) *podman.Runtime {
	e.t.Helper()
	if p == nil {
		p = podman.DeclaredCommand{}
	}
	r, err := podman.New(podman.Config{Binary: e.fake.Binary, StateDir: e.state, StopTimeout: time.Second, RunResources: true, Program: p})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// run is the resources of one run.
type run struct {
	id       domain.RunID
	scope    domain.Scope
	deadline string
}

func newRun(t *testing.T) run {
	return run{
		id: domain.RunID("run_" + string(runtimetest.NewAttemptID(t))), scope: runtimetest.Scope,
		deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

func (r run) base(kind execution.ResourceKind, key string) execution.ResourceSpec {
	return execution.ResourceSpec{
		Scope: r.scope, RunID: r.id, Backend: podman.Backend, Kind: kind, Key: key, Name: execution.ResourceName(r.scope, r.id, kind, key),
		Policy:     execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape:  "sha256:" + strings.Repeat("b", 64),
		DeadlineAt: r.deadline,
	}
}

func (r run) network() execution.ResourceSpec {
	s := r.base(execution.KindNetwork, execution.KeyNetwork)
	s.Network = &execution.NetworkSpec{
		Services: []string{}, Edges: []execution.EdgeSpec{}, Egress: map[string]string{}, Workloads: map[string][]string{},
		Isolation: execution.IsolationTenant, PublicEgress: true,
	}
	return s
}

func (r run) workspace() execution.ResourceSpec {
	s := r.base(execution.KindWorkspace, execution.KeyWorkspace)
	s.Workspace = &execution.WorkspaceSpec{SizeMiB: 64, Initial: "git"}
	return s
}

func (r run) preparation() execution.ResourceSpec {
	s := r.base(execution.KindPreparation, execution.KeyPreparation)
	s.Preparation = &execution.PreparationSpec{Image: prepImage, RepositoryURL: repoURL, Commit: pinnedCommit, TimeoutSecs: 60, MaxBytes: 1 << 20, MaxAttempts: 1}
	return s
}

func (r run) service(name string, body execution.ServiceSpec) execution.ResourceSpec {
	body.Name, body.Role, body.Host = name, "general", execution.EndpointHost(r.scope, r.id, name)
	s := r.base(execution.KindService, execution.ServiceKey(name))
	s.Service = &body
	return s
}

func dbBody() execution.ServiceSpec {
	return execution.ServiceSpec{
		Image: svcImage, Command: []string{"postgres"}, Args: []string{"-c", "fsync=off"}, Env: map[string]string{"POSTGRES_USER": "kodama"},
		Secrets: []execution.SecretPin{}, Ports: map[string]int64{"sql": 5432}, Egress: []string{},
		Resources: execution.ServiceResourcesSpec{CPUMillis: 500, MemoryMiB: 256, EphemeralMiB: 64, ShmMiB: 16},
		Storage:   []execution.StorageSpec{{Path: "/var/lib/data", SizeMiB: 128}},
		Readiness: execution.ReadinessSpec{Probe: "exec", Exec: []string{"pg_isready", "-U", "kodama"}, TimeoutSecs: 30, PeriodSecs: 1, ProbeTimeout: 2, LossSecs: 5},
	}
}

func (e *rEnv) ensure(res *podman.Resources, spec execution.ResourceSpec) execution.ResourceObservation {
	e.t.Helper()
	obs, err := res.Ensure(context.Background(), spec)
	if err != nil {
		e.t.Fatalf("ensure %s: %v", spec.Key, err)
	}
	return obs
}

// fakeFixture binds Resources over the fake engine to the backend-neutral
// contract. Services are ready the moment they run (the exec probe passes) and a
// preparation completes at once.
func fakeFixture(t *testing.T) runtimetest.ResourcesFixture {
	e := newREnv(t)
	e.fake.set(t, "exec-code", "0")
	exists := func(kind string, name string) bool {
		out, err := exec.Command(e.fake.Binary, kind, "exists", name).CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return false
			}
			t.Fatalf("%s exists: %v %s", kind, err, out)
		}
		return true
	}
	kindOf := func(h execution.ResourceHandle) string {
		switch h.Kind {
		case execution.KindNetwork:
			return "network"
		case execution.KindWorkspace:
			return "volume"
		}
		return "container"
	}
	return runtimetest.ResourcesFixture{
		Open: func() execution.RunResources { return e.open() },
		Want: execution.ResourceCapabilities{Backend: podman.Backend, Set: podman.ResourceCapabilities},
		Service: func(string) execution.ServiceSpec {
			b := dbBody()
			b.Command, b.Args = []string{"sleep"}, []string{"300"}
			return b
		},
		Preparation: func() execution.PreparationSpec { return *newRun(t).preparation().Preparation },
		Foreign: func(h execution.ResourceHandle) {
			switch h.Kind {
			case execution.KindWorkspace:
				e.fake.ctl(t, "volume", "create", h.Name)
			case execution.KindNetwork:
				e.fake.ctl(t, "network", "create", h.Name)
			default:
				e.fake.ctl(t, "__plant", h.Name)
			}
		},
		ForeignIntact: func(h execution.ResourceHandle) bool { return exists(kindOf(h), h.Name) },
		Exists:        func(h execution.ResourceHandle) bool { return exists(kindOf(h), h.Name) },
		Fault:         func() func() { return e.fake.outage(t) },
		HoldWorkspace: func(h execution.ResourceHandle) func() {
			rt := e.runtime(nil)
			w, err := rt.Start(context.Background(), runtimetest.SharedSpec(t, h, runtimetest.HostImage, "exec sleep 300"))
			if err != nil {
				t.Fatalf("the holder did not start: %v", err)
			}
			return func() {
				_, _ = rt.Stop(context.Background(), w)
				_ = rt.Remove(context.Background(), w)
			}
		},
		Timeout: 20 * time.Second,
	}
}

// Test_podman_run_resources_contract_fake runs the exported companion-port
// contract over the Podman resources against the double engine. The real engine
// runs the same suite in Test_podman_run_resources_contract.
func Test_podman_run_resources_contract_fake(t *testing.T) {
	runtimetest.RunResourcesContract(t, fakeFixture)
}

// ---- program assertions ----

func labelArgs(rec map[string]string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(rec)) {
		out = append(out, "--label", k+"="+rec[k])
	}
	return out
}

func resLabels(spec execution.ResourceSpec, uid, fp string) map[string]string {
	return map[string]string{
		podman.LabelManagedBy: podman.ValueManagedByResources, podman.LabelTenant: string(spec.Scope.TenantID), podman.LabelProject: string(spec.Scope.ProjectID),
		podman.LabelRun: string(spec.RunID), podman.LabelKind: string(spec.Kind), podman.LabelKey: spec.Key, podman.LabelUID: uid, podman.LabelFingerprint: fp,
	}
}

func fingerprint(t *testing.T, spec execution.ResourceSpec) string {
	t.Helper()
	fp, err := spec.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

// engineCalls returns the argv of the invocations whose subcommand is one of
// the given verbs (a volume/network call is "volume create", ...).
func engineCalls(argvs [][]string, verbs ...string) [][]string {
	var out [][]string
	for _, a := range argvs {
		v := a[0]
		if (v == "volume" || v == "network" || v == "container") && len(a) > 1 {
			v += " " + a[1]
		}
		if slices.Contains(verbs, v) {
			out = append(out, a)
		}
	}
	return out
}

func envOf(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--env" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// Test_unit_podman_run_resources_program asserts what the adapter asks the
// engine to do, exactly: argv, labels, mounts, networks, users, no engine
// socket or broad access, identity-checked cleanup in reverse order.
func Test_unit_podman_run_resources_program(t *testing.T) {
	ctx := context.Background()
	e := newREnv(t)
	res := e.open()
	r := newRun(t)
	netSpec, wsSpec, prepSpec := r.network(), r.workspace(), r.preparation()
	dbSpec := r.service("db", dbBody())
	runNet := netSpec.Name

	net := e.ensure(res, netSpec)
	ws := e.ensure(res, wsSpec)
	prep := e.ensure(res, prepSpec)
	db := e.ensure(res, dbSpec)
	e.fake.set(t, "exec-code", "0")

	argvs := e.fake.argvs(t)

	t.Run("the run network is internal and labeled", func(t *testing.T) {
		want := append(append([]string{"network", "create", "--internal"}, labelArgs(resLabels(netSpec, net.Handle.UID, fingerprint(t, netSpec)))...), netSpec.Name)
		if got := engineCalls(argvs, "network create"); len(got) != 1 || !slices.Equal(got[0], want) {
			t.Fatalf("network create %v, want %v", got, want)
		}
	})

	t.Run("the run volume is a labeled named volume", func(t *testing.T) {
		want := append(append([]string{"volume", "create"}, labelArgs(resLabels(wsSpec, ws.Handle.UID, fingerprint(t, wsSpec)))...), wsSpec.Name)
		if got := engineCalls(argvs, "volume create"); len(got) != 1 || !slices.Equal(got[0], want) {
			t.Fatalf("volume create %v, want %v", got, want)
		}
		if vol := e.fake.volumes(t)[wsSpec.Name]; vol.Labels[podman.LabelUID] != ws.Handle.UID {
			t.Fatalf("the volume does not carry the bound UID: %+v", vol.Labels)
		}
	})

	creates := engineCalls(argvs, "create")
	if len(creates) != 2 {
		t.Fatalf("%d containers created", len(creates))
	}
	bootstrapDir := func(name string) string { return filepath.Join(e.state, "resources", name, "bootstrap") }
	idEnv := func(spec execution.ResourceSpec, uid string) []string {
		return []string{
			"KODAMA_RUN_ID=" + string(spec.RunID), "KODAMA_RESOURCE_KEY=" + spec.Key, "KODAMA_WORKLOAD_NAME=" + spec.Name, "KODAMA_WORKLOAD_UID=" + uid,
			"KODAMA_SERVER_URL=" + serverURL, "KODAMA_CA_FILE=/kodama/bootstrap/ca.crt", "KODAMA_ADMISSION_CREDENTIAL_FILE=/kodama/bootstrap/admission",
		}
	}

	t.Run("the service container", func(t *testing.T) {
		want := []string{"create", "--name", dbSpec.Name}
		want = append(want, labelArgs(resLabels(dbSpec, db.Handle.UID, fingerprint(t, dbSpec)))...)
		want = append(want,
			"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing", "--userns=auto", "--user", runtimeUser, "--stop-timeout", "1",
			"--network", runNet, "--network-alias", dbSpec.Service.Host, "--pids-limit", "256", "--cpus", "0.500", "--memory", "384m",
			"--tmpfs", "/tmp:rw,size=64m,mode=1777", "--shm-size", "16m", "--tmpfs", "/var/lib/data:rw,size=128m,mode=0700,uid=65532,gid=65532",
			"--volume", bootstrapDir(dbSpec.Name)+":/kodama/bootstrap:ro",
			"--env", "HOME=/tmp", "--env", "TMPDIR=/tmp", "--env", "POSTGRES_USER=kodama")
		for _, v := range idEnv(dbSpec, db.Handle.UID) {
			want = append(want, "--env", v)
		}
		want = append(want, "--entrypoint", `["postgres"]`, svcImage, "-c", "fsync=off")
		var got []string
		for _, c := range creates {
			if slices.Contains(c, dbSpec.Name) {
				got = c
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("service create\n got %v\nwant %v", got, want)
		}
		if slices.Contains(got, controlNet) {
			t.Fatal("an unbound service joined the control network")
		}
	})

	t.Run("the preparation container", func(t *testing.T) {
		want := []string{"create", "--name", prepSpec.Name}
		want = append(want, labelArgs(resLabels(prepSpec, prep.Handle.UID, fingerprint(t, prepSpec)))...)
		want = append(want,
			"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing", "--userns=auto", "--user", runtimeUser, "--stop-timeout", "1",
			"--network", runNet, "--network", controlNet, "--cpus", "1", "--memory", "512m", "--pids-limit", "256", "--tmpfs", "/tmp:rw,size=256m,mode=1777",
			"--volume", wsSpec.Name+":/workspace:U", "--volume", entrypoint+":/kodama/bin/kodama:ro", "--volume", bootstrapDir(prepSpec.Name)+":/kodama/bootstrap:ro",
			"--env", "HOME=/tmp", "--env", "TMPDIR=/tmp")
		for _, v := range append(idEnv(prepSpec, prep.Handle.UID),
			"KODAMA_WORKSPACE_DIR=/workspace", "KODAMA_CHECKOUT_CREDENTIAL_FILE=/kodama/bootstrap/"+bootstrap.CheckoutCredentialFile, "KODAMA_GIT_CA_FILE=/kodama/bootstrap/"+bootstrap.GitCAFile) {
			want = append(want, "--env", v)
		}
		want = append(want, "--entrypoint", `["/kodama/bin/kodama"]`, prepImage, "prepare")
		var got []string
		for _, c := range creates {
			if slices.Contains(c, prepSpec.Name) {
				got = c
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("preparation create\n got %v\nwant %v", got, want)
		}
		// The checkout credential is a read-only file, never an argument or variable.
		raw, err := os.ReadFile(filepath.Join(bootstrapDir(prepSpec.Name), bootstrap.CheckoutCredentialFile))
		if err != nil || string(raw) != checkoutUser+":"+checkoutPass {
			t.Fatalf("checkout credential file: %q %v", raw, err)
		}
		if strings.Contains(strings.Join(got, "\x00"), checkoutPass) {
			t.Fatal("the checkout password is on the command line")
		}
		if e.prov.count() != 1 {
			t.Fatalf("the provider was asked %d times", e.prov.count())
		}
	})

	t.Run("the credential is a private file registered once per workload", func(t *testing.T) {
		cmds := e.reg.commands()
		if len(cmds) != 2 {
			t.Fatalf("%d registrations", len(cmds))
		}
		for _, name := range []string{prepSpec.Name, dbSpec.Name} {
			raw, err := os.ReadFile(filepath.Join(bootstrapDir(name), podman.ResourceCredentialFile))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, c := range cmds {
				found = found || c.Credential.Reveal() == string(raw)
			}
			if !found {
				t.Fatalf("%s: the persisted credential is not the registered one", name)
			}
			// The workload user is not the host user: the mount is readable by it, and
			// the confidentiality rests on the private resource directory above it.
			if fi, _ := os.Stat(filepath.Join(bootstrapDir(name), podman.ResourceCredentialFile)); fi.Mode().Perm() != 0o644 {
				t.Fatalf("credential mode %v", fi.Mode())
			}
			for _, private := range []string{filepath.Join(e.state, "resources"), filepath.Join(e.state, "resources", name)} {
				if fi, err := os.Stat(private); err != nil || fi.Mode().Perm() != 0o700 {
					t.Fatalf("%s is not private: %v %v", private, fi, err)
				}
			}
		}
		for _, a := range argvs {
			if strings.Contains(strings.Join(a, "\x00"), cmds[0].Credential.Reveal()) {
				t.Fatal("an admission credential is on a command line")
			}
		}
	})

	t.Run("nothing reaches the engine socket, the host or a broad network", func(t *testing.T) {
		for _, c := range e.fake.created(t) {
			all := strings.Join(c.Argv, "\x00")
			for _, bad := range []string{"podman.sock", "docker.sock", "/run/", "/var/run", "--privileged", "--pid=host", "--ipc=host", "--userns=host", "--network=host", "--security-opt=seccomp=unconfined", "--cap-add", "--device"} {
				if strings.Contains(all, bad) {
					t.Errorf("%s: %q in %v", c.Name, bad, c.Argv)
				}
			}
			for _, n := range c.Networks {
				if n != runNet && n != controlNet {
					t.Errorf("%s joined %q", c.Name, n)
				}
			}
			if c.Name == dbSpec.Name && (slices.Contains(c.Networks, controlNet) || slices.ContainsFunc(c.Volumes, func(v string) bool { return strings.Contains(v, "workspace") })) {
				t.Errorf("the service reaches the control network or the workspace: %+v", c)
			}
		}
	})

	t.Run("a repeated or restarted ensure creates nothing", func(t *testing.T) {
		before := len(e.fake.argvs(t))
		for _, spec := range []execution.ResourceSpec{netSpec, wsSpec, prepSpec, dbSpec} {
			for _, r := range []*podman.Resources{res, e.open()} {
				if obs, err := r.Ensure(ctx, spec); err != nil || obs.Handle.UID == "" {
					t.Fatalf("%s: %+v %v", spec.Key, obs, err)
				}
			}
		}
		if got := engineCalls(e.fake.argvs(t)[before:], "create", "volume create", "network create", "start"); len(got) != 0 {
			t.Fatalf("an adopting ensure created or started something: %v", got)
		}
		if e.prov.count() != 1 || len(e.reg.commands()) != 2 {
			t.Fatalf("an adopting ensure asked for credentials again: %d %d", e.prov.count(), len(e.reg.commands()))
		}
	})

	t.Run("cleanup acts by verified ID, in reverse order, and leaves nothing", func(t *testing.T) {
		mark := len(e.fake.argvs(t))
		for _, h := range []execution.ResourceHandle{db.Handle, prep.Handle, ws.Handle, net.Handle} {
			var ev execution.ResourceStopEvidence
			var err error
			for range 3 {
				if ev, err = res.Stop(ctx, h); err != nil || ev.Stopped {
					break
				}
			}
			if err != nil || !ev.Stopped {
				t.Fatalf("stop %s: %+v %v", h.Key, ev, err)
			}
			if h.Kind == execution.KindWorkspace && !ev.Released {
				t.Fatalf("the workspace was not released: %+v", ev)
			}
		}
		for _, h := range []execution.ResourceHandle{db.Handle, prep.Handle, ws.Handle, net.Handle} {
			if rm, err := res.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("remove %s: %+v %v", h.Key, rm, err)
			}
		}
		var cleanup [][]string
		for _, a := range e.fake.argvs(t)[mark:] {
			if slices.Contains([]string{"stop", "rm"}, a[0]) || (len(a) > 1 && a[1] == "rm") {
				cleanup = append(cleanup, a)
			}
		}
		for _, a := range cleanup {
			if a[0] == "rm" && !slices.Contains(a, "--force") {
				t.Errorf("unexpected cleanup call %v", a)
			}
			ref := a[len(a)-1]
			switch a[0] {
			case "stop", "rm":
				if ref != db.Handle.UID && len(ref) != 64 {
					t.Errorf("a container was addressed by name: %v", a)
				}
			}
		}
		last := func(i int) string { return cleanup[i][len(cleanup[i])-1] }
		if len(cleanup) < 4 || cleanup[len(cleanup)-2][0] != "volume" || last(len(cleanup)-2) != wsSpec.Name || cleanup[len(cleanup)-1][0] != "network" || last(len(cleanup)-1) != runNet {
			t.Fatalf("the volume then the network are removed last: %v", cleanup)
		}
		if len(e.fake.created(t)) != 0 || len(e.fake.volumes(t)) != 0 || len(e.fake.networks(t)) != 0 {
			t.Fatalf("leaked objects: %d containers, %v, %v", len(e.fake.created(t)), e.fake.volumes(t), e.fake.networks(t))
		}
		for _, name := range []string{prepSpec.Name, dbSpec.Name} {
			if _, err := os.Stat(bootstrapDir(name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: the bootstrap files survived: %v", name, err)
			}
		}
		for _, spec := range []execution.ResourceSpec{netSpec, wsSpec, prepSpec, dbSpec} {
			if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				t.Errorf("%s: ensure after removal: %v", spec.Key, err)
			}
		}
		if len(engineCalls(e.fake.argvs(t)[mark:], "create", "volume create", "network create")) != 0 {
			t.Fatal("a removed resource was created again")
		}
	})
}

// Test_podman_run_resources_secret_service: a service with secret bindings runs
// through the injected entrypoint, joins the control network and keeps the
// declared command and arguments in order after `service-run --`.
func Test_podman_run_resources_secret_service(t *testing.T) {
	e := newREnv(t)
	res := e.open()
	r := newRun(t)
	e.ensure(res, r.network())
	body := dbBody()
	body.Secrets = []execution.SecretPin{{Ref: "secret://project/db", Version: 1, Name: "POSTGRES_PASSWORD", Delivery: "env"}}
	spec := r.service("db", body)
	e.ensure(res, spec)
	var got []string
	for _, c := range e.fake.created(t) {
		if c.Name == spec.Name {
			got = c.Argv
		}
	}
	tail := []string{"--entrypoint", `["/kodama/bin/kodama"]`, svcImage, "service-run", "--", "postgres", "-c", "fsync=off"}
	if !slices.Equal(got[len(got)-len(tail):], tail) {
		t.Fatalf("the supervised service command: %v", got)
	}
	for _, want := range []string{"--network " + controlNet, "--volume " + entrypoint + ":/kodama/bin/kodama:ro"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	// Secret values are never part of the spec the adapter sees; the pins are
	// resolved by the entrypoint through the resource subject.
	if strings.Contains(strings.Join(got, " "), "secret://") {
		t.Errorf("a secret reference is on the command line: %v", got)
	}
}

// Test_podman_run_resources_readiness: exec readiness runs in the container, tcp
// readiness from a probe container on the run network through the endpoint
// alias, and an unanswerable probe is unknown, never ready or failed.
func Test_podman_run_resources_readiness(t *testing.T) {
	ctx := context.Background()
	observe := func(res *podman.Resources, h execution.ResourceHandle) execution.ResourceObservation {
		obs, err := res.Observe(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		return obs
	}

	t.Run("exec", func(t *testing.T) {
		e := newREnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		spec := r.service("db", dbBody())
		h := e.ensure(res, spec).Handle
		if obs := observe(res, h); obs.Phase != execution.PhasePending {
			t.Fatalf("a service whose probe fails is %q", obs.Phase)
		}
		e.fake.set(t, "exec-code", "0")
		obs := observe(res, h)
		if obs.Phase != execution.PhaseReady || obs.ProbeKind != "exec" || obs.ReadyAt == nil {
			t.Fatalf("%+v", obs)
		}
		first := *obs.ReadyAt
		time.Sleep(10 * time.Millisecond)
		if again := observe(res, h); again.ReadyAt == nil || !again.ReadyAt.Equal(first) {
			t.Fatalf("the readiness time moved: %v, %v", first, again.ReadyAt)
		}
		last := engineCalls(e.fake.argvs(t), "exec")
		if len(last) == 0 || !slices.Equal(last[0], []string{"exec", h.UID, "pg_isready", "-U", "kodama"}) && !slices.Equal(last[0][:2], []string{"exec", e.fake.created(t)[0].ID}) {
			t.Fatalf("exec probe argv %v", last)
		}
		// A probe that podman itself cannot run is not an answer.
		e.fake.set(t, "exec-code", "125")
		if obs := observe(res, h); obs.Phase != execution.PhaseUnknown {
			t.Fatalf("an engine failure during the probe is %q", obs.Phase)
		}
		// The probe that fails after readiness is no longer ready, but the service runs.
		e.fake.set(t, "exec-code", "1")
		if obs := observe(res, h); obs.Phase != execution.PhasePending || obs.ReadyAt != nil && !obs.ReadyAt.Equal(first) {
			t.Fatalf("%+v", obs)
		}
	})

	t.Run("tcp through the endpoint alias on the run network", func(t *testing.T) {
		e := newREnv(t)
		res := e.open()
		r := newRun(t)
		netSpec := r.network()
		e.ensure(res, netSpec)
		body := dbBody()
		body.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 30, ProbeTimeout: 3}
		spec := r.service("db", body)
		h := e.ensure(res, spec).Handle
		if obs := observe(res, h); obs.Phase != execution.PhasePending {
			t.Fatalf("%+v", obs)
		}
		e.fake.set(t, "probe-code", "0")
		if obs := observe(res, h); obs.Phase != execution.PhaseReady || obs.ProbeKind != "tcp" {
			t.Fatalf("%+v", obs)
		}
		// The probe is a created container: its intent carries the nonce of the
		// service's probe slot, and it is started attached and deleted by its ID.
		raw, err := os.ReadFile(filepath.Join(e.state, "resources", spec.Name, "record.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			ProbeSlot struct {
				Nonce string `json:"nonce"`
			} `json:"probeSlot"`
		}
		if err := json.Unmarshal(raw, &rec); err != nil || rec.ProbeSlot.Nonce == "" {
			t.Fatalf("no probe nonce: %s %v", raw, err)
		}
		want := append([]string{"create", "--name", fakeProbeName(spec)}, labelArgs(map[string]string{
			podman.LabelManagedBy: podman.ValueManagedByResources, podman.LabelKind: "probe", podman.LabelRun: string(r.id),
			podman.LabelUID: h.UID, podman.LabelKey: spec.Key, podman.LabelProbeNonce: rec.ProbeSlot.Nonce,
		})...)
		want = append(want, "--network", netSpec.Name, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing", "--userns=auto",
			"--user", runtimeUser, "--pids-limit", "32", "--memory", "32m", probeImage, "nc", "-z", "-w", "3", spec.Service.Host, "5432")
		var creates [][]string
		for _, a := range e.fake.argvs(t) {
			if a[0] == "create" && slices.Contains(a, fakeProbeName(spec)) {
				creates = append(creates, a)
			}
		}
		if len(creates) == 0 || !slices.Equal(creates[len(creates)-1], want) {
			t.Fatalf("tcp probe argv %v, want %v", creates, want)
		}
		if len(engineCalls(e.fake.argvs(t), "run")) != 0 {
			t.Fatal("the probe was run without an intent")
		}
		e.fake.set(t, "probe-code", "125")
		if obs := observe(res, h); obs.Phase != execution.PhaseUnknown {
			t.Fatalf("an engine failure during the probe is %q", obs.Phase)
		}
	})
}

// Test_podman_run_resources_lifecycle: failure, loss, replacement and outage
// are reported honestly and a lost resource is never recreated.
func Test_podman_run_resources_lifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("a preparation exit decides completed or failed", func(t *testing.T) {
		for code, want := range map[string]execution.ResourcePhase{"0": execution.PhaseReady, "3": execution.PhaseFailed} {
			e := newREnv(t)
			e.fake.set(t, "prepare-code", code)
			res := e.open()
			r := newRun(t)
			e.ensure(res, r.network())
			e.ensure(res, r.workspace())
			h := e.ensure(res, r.preparation()).Handle
			obs, err := res.Observe(ctx, h)
			if err != nil || obs.Phase != want {
				t.Fatalf("exit %s: %+v %v", code, obs, err)
			}
			if want == execution.PhaseFailed && (obs.ExitCode == nil || *obs.ExitCode != 3 || obs.Reason != execution.ReasonPreparationFailed) {
				t.Fatalf("%+v", obs)
			}
			// A finished preparation is never run again, and a repeat adopts it.
			if again, err := res.Ensure(ctx, r.preparation()); err != nil || again.Handle != h {
				t.Fatalf("%+v %v", again, err)
			}
			if n := len(engineCalls(e.fake.argvs(t), "create")); n != 1 {
				t.Fatalf("%d preparation containers created", n)
			}
		}
	})

	t.Run("a refused checkout creates nothing and registers nothing", func(t *testing.T) {
		for name, deny := range map[string]error{"denied": bootstrap.ErrCheckoutDenied, "outage": errors.New("provider down")} {
			e := newREnv(t)
			e.prov.deny = deny
			res := e.open()
			r := newRun(t)
			e.ensure(res, r.network())
			e.ensure(res, r.workspace())
			_, err := res.Ensure(ctx, r.preparation())
			if name == "denied" && !errors.Is(err, bootstrap.ErrCheckoutDenied) || name == "outage" && !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("%s: %v", name, err)
			}
			if len(engineCalls(e.fake.argvs(t), "create")) != 0 || len(e.reg.commands()) != 0 {
				t.Fatalf("%s: a refused checkout had effects", name)
			}
			// The credential never appears in a refusal.
			if err != nil && strings.Contains(err.Error(), checkoutPass) {
				t.Fatal("the error carries the credential")
			}
		}
	})

	t.Run("a service that exits is failed and a vanished one is absent, never recreated", func(t *testing.T) {
		e := newREnv(t)
		e.fake.set(t, "exec-code", "0")
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		spec := r.service("db", dbBody())
		obs := e.ensure(res, spec)
		id := e.fake.created(t)[0].ID
		e.fake.ctl(t, "__exit", id, "7")
		got, err := res.Observe(ctx, obs.Handle)
		if err != nil || got.Phase != execution.PhaseFailed || got.ExitCode == nil || *got.ExitCode != 7 || got.Reason != execution.ReasonServiceLost {
			t.Fatalf("%+v %v", got, err)
		}
		e.fake.ctl(t, "rm", id)
		if got, err := res.Observe(ctx, obs.Handle); err != nil || got.Phase != execution.PhaseAbsent {
			t.Fatalf("a vanished container: %+v %v", got, err)
		}
		again, err := e.open().Ensure(ctx, spec)
		if err != nil || again.Handle != obs.Handle || again.Phase != execution.PhaseAbsent {
			t.Fatalf("a repeated ensure after the loss: %+v %v", again, err)
		}
		if n := len(engineCalls(e.fake.argvs(t), "create")); n != 1 {
			t.Fatalf("the lost service was created %d times", n)
		}
	})

	t.Run("a replacement under the same name is not ours and is never touched", func(t *testing.T) {
		e := newREnv(t)
		e.fake.set(t, "exec-code", "0")
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		spec := r.service("db", dbBody())
		obs := e.ensure(res, spec)
		e.fake.ctl(t, "rm", e.fake.created(t)[0].ID)
		replacement := e.fake.ctl(t, "__plant", spec.Name, podman.LabelUID+"="+obs.Handle.UID)
		// Containers are acted on by the recorded ID only: the replacement is a
		// container that is not ours, so ours is gone.
		if got, err := res.Observe(ctx, obs.Handle); err != nil || got.Phase != execution.PhaseAbsent {
			t.Fatalf("observe: %+v %v", got, err)
		}
		if again, err := e.open().Ensure(ctx, spec); err != nil || again.Phase != execution.PhaseAbsent || again.Handle != obs.Handle {
			t.Fatalf("a repeated ensure adopted or replaced: %+v %v", again, err)
		}
		if ev, err := res.Stop(ctx, obs.Handle); err != nil || !ev.Stopped || ev.Launch != execution.LaunchStarted {
			t.Fatalf("stop: %+v %v", ev, err)
		}
		if rm, err := res.Remove(ctx, obs.Handle); err != nil || !rm.Removed {
			t.Fatalf("remove: %+v %v", rm, err)
		}
		for _, a := range e.fake.argvs(t) {
			if (a[0] == "stop" || a[0] == "rm") && a[len(a)-1] == replacement {
				t.Fatalf("the replacement was addressed: %v", a)
			}
		}
		if len(e.fake.created(t)) != 1 {
			t.Fatalf("the replacement was removed or another container created: %d", len(e.fake.created(t)))
		}
	})

	t.Run("an outage is unknown, never absent and never a confirmed stop", func(t *testing.T) {
		e := newREnv(t)
		res := e.open()
		r := newRun(t)
		h := e.ensure(res, r.network()).Handle
		ws := e.ensure(res, r.workspace()).Handle
		restore := e.fake.outage(t)
		defer restore()
		for _, h := range []execution.ResourceHandle{h, ws} {
			obs, err := res.Observe(ctx, h)
			if err != nil || obs.Phase != execution.PhaseUnknown || obs.Reason == "" {
				t.Fatalf("%s: %+v %v", h.Key, obs, err)
			}
		}
		if ev, err := res.Stop(ctx, ws); err == nil && ev.Stopped {
			t.Fatalf("a workspace stop confirmed during an outage: %+v", ev)
		}
		if _, err := res.Ensure(ctx, r.preparation()); err == nil {
			t.Fatal("a create succeeded during an outage")
		}
		restore()
		if obs, err := res.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseReady {
			t.Fatalf("%+v %v", obs, err)
		}
		// The interrupted stop left its fence: the workspace is never reused.
		if _, err := res.Ensure(ctx, r.workspace()); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("ensure of a workspace whose stop was interrupted: %v", err)
		}
	})

	t.Run("a registration that conflicts launches nothing and the credential is stable across retries", func(t *testing.T) {
		e := newREnv(t)
		res := e.open()
		r := newRun(t)
		e.ensure(res, r.network())
		spec := r.service("db", dbBody())
		e.reg.err = runtimeauth.ErrConflict
		if _, err := res.Ensure(ctx, spec); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("%v", err)
		}
		e.reg.err = errors.New("admission down")
		if _, err := res.Ensure(ctx, spec); err == nil {
			t.Fatal("an unavailable registrar was accepted")
		}
		if len(engineCalls(e.fake.argvs(t), "create")) != 0 {
			t.Fatal("a container was created without a registered credential")
		}
		e.reg.err = nil
		e.ensure(res, spec)
		cmds := e.reg.commands()
		if len(cmds) != 3 || cmds[0].Credential.Digest() != cmds[2].Credential.Digest() {
			t.Fatalf("the credential changed between attempts: %d", len(cmds))
		}
	})
}

// Test_podman_run_resources_collision: an object of ours that is not the
// record's (another UID or fingerprint label) is never adopted.
func Test_podman_run_resources_collision(t *testing.T) {
	ctx := context.Background()
	e := newREnv(t)
	res := e.open()
	r := newRun(t)
	netSpec := r.network()
	// A foreign network at the name, with our labels but another instance UID.
	e.fake.ctl(t, "network", "create", "--label", podman.LabelManagedBy+"="+podman.ValueManagedByResources, "--label", podman.LabelUID+"=someone-else", netSpec.Name)
	if _, err := res.Ensure(ctx, netSpec); !errors.Is(err, execution.ErrResourceCollision) {
		t.Fatalf("ensure over a lookalike network: %v", err)
	}
	if len(engineCalls(e.fake.argvs(t), "network rm", "network create")) != 1 {
		t.Fatal("the lookalike network was touched")
	}
}

// ---- the attempt runtime: run_shared launches ----

// Test_podman_run_resources_shared_attempt: a run_shared attempt mounts the run
// volume at /workspace, runs as the workload user on the run network, takes
// only the pinned role, selects the v2 wire and gets no checkout credential.
func Test_podman_run_resources_shared_attempt(t *testing.T) {
	ctx := context.Background()
	e := newREnv(t)
	res := e.open()
	r := newRun(t)
	net := e.ensure(res, r.network())
	ws := e.ensure(res, r.workspace())
	bin := filepath.Join(t.TempDir(), "kodama")
	prog := podman.Entrypoint{Binary: bin, ServerURL: serverURL, Issuer: bootstrap.Issuer{Registrar: &registrar{}}, Checkout: issuing(testCA(t))}
	rt := e.runtime(prog)

	if caps := rt.Capabilities(); caps.Guarantees&execution.RunResourcesMarker == 0 || caps.Guarantees&execution.NetworkPolicy != 0 {
		t.Fatalf("capabilities %+v", caps)
	}

	s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
	// The attempt consumes the workspace: a v1 attempt would need a checkout.
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	_ = c
	w, err := rt.Start(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	cs := e.fake.created(t)
	var got createdContainer
	for _, c := range cs {
		if c.Name == w.Name {
			got = c
		}
	}
	if got.Name == "" {
		t.Fatal("no attempt container")
	}
	for _, want := range []string{ws.Handle.Name + ":/workspace:U"} {
		if !slices.Contains(got.Volumes, want) {
			t.Errorf("missing mount %q in %v", want, got.Volumes)
		}
	}
	if slices.Contains(got.Tmpfs, "/workspace") {
		t.Errorf("/workspace is in-memory: %v", got.Tmpfs)
	}
	if got.User != runtimeUser || !slices.Equal(got.Networks, []string{r.network().Name}) {
		t.Errorf("user %q networks %v", got.User, got.Networks)
	}
	if !slices.Contains(got.Flags, "--cap-drop=ALL") || !slices.Contains(got.Flags, "--security-opt=no-new-privileges") || len(got.CapAdd) != 0 {
		t.Errorf("role shape: flags %v caps %v", got.Flags, got.CapAdd)
	}
	if !slices.Contains(got.Env, "KODAMA_RUNTIME_API=v2") || !slices.Contains(got.Env, "KODAMA_WORKSPACE_DIR=/workspace") {
		t.Errorf("env %v", got.Env)
	}
	if hasFlag(got.Args, "--checkout-credential-file") || hasFlag(got.Args, "--git-ca-file") || strings.Contains(strings.Join(got.Volumes, " "), bootstrap.CheckoutCredentialFile) {
		t.Errorf("a v2 attempt has checkout material: %v", got.Args)
	}
	if got.Labels[podman.LabelRun] != string(r.id) {
		t.Errorf("labels %v", got.Labels)
	}
	if _, err := os.Stat(filepath.Join(e.state, s.WorkloadName, "bootstrap", bootstrap.CheckoutCredentialFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a checkout credential was written: %v", err)
	}

	// The workspace is held until the attempt's container exits.
	if obs, err := res.Observe(ctx, ws.Handle); err != nil || obs.Released {
		t.Fatalf("the workspace is not held: %+v %v", obs, err)
	}
	if ev, err := res.Stop(ctx, ws.Handle); err != nil || !ev.Stopped || ev.Released {
		t.Fatalf("stop of a held workspace: %+v %v", ev, err)
	}
	// A later attempt on the stopped workspace is fenced even though the volume is held.
	if _, err := rt.Start(ctx, runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("an attempt on a stopped workspace: %v", err)
	}
	if rm, err := res.Remove(ctx, ws.Handle); err != nil || rm.Removed {
		t.Fatalf("a held volume was removed: %+v %v", rm, err)
	}
	if _, err := rt.Stop(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, w); err != nil {
		t.Fatal(err)
	}
	if ev, err := res.Stop(ctx, ws.Handle); err != nil || !ev.Released {
		t.Fatalf("not released after the holder was removed: %+v %v", ev, err)
	}
	_ = net
}

// Test_podman_run_resources_shared_refusals: a launch that names another
// instance, a missing or fenced workspace or network, an unenforceable role or a
// runtime without run resources creates no container.
func Test_podman_run_resources_shared_refusals(t *testing.T) {
	ctx := context.Background()
	e := newREnv(t)
	res := e.open()
	r := newRun(t)
	e.ensure(res, r.network())
	ws := e.ensure(res, r.workspace())
	rt := e.runtime(nil)

	creates := func() int { return len(engineCalls(e.fake.argvs(t), "create")) }
	refused := func(name string, s execution.AttemptSpec, rt *podman.Runtime, want error) {
		t.Helper()
		before := creates()
		if _, err := rt.Start(ctx, s); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", name, err, want)
		}
		if creates() != before {
			t.Fatalf("%s: a container was created", name)
		}
	}

	wrong := ws.Handle
	wrong.UID = "99999999-9999-4999-8999-999999999999"
	refused("another instance's workspace", runtimetest.SharedSpec(t, wrong, runtimetest.HostImage, "true"), rt, execution.ErrResourceCollision)

	nowhere := newRun(t)
	other := execution.ResourceHandle{Scope: nowhere.scope, RunID: nowhere.id, Kind: execution.KindWorkspace, Key: execution.KeyWorkspace, Name: nowhere.workspace().Name, UID: ws.Handle.UID}
	refused("a run without a workspace", runtimetest.SharedSpec(t, other, runtimetest.HostImage, "true"), rt, execution.ErrConflict)

	bare, err := podman.New(podman.Config{Binary: e.fake.Binary, StateDir: e.state, Program: podman.DeclaredCommand{}})
	if err != nil {
		t.Fatal(err)
	}
	refused("a runtime without run resources", runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true"), bare, execution.ErrNotConfigured)
	if bare.Capabilities().Guarantees&execution.RunResourcesMarker != 0 {
		t.Fatal("a runtime without run resources declares the marker")
	}

	for name, mutate := range map[string]func(*execution.RoleShape){
		"a root user":              func(s *execution.RoleShape) { s.User = "root" },
		"a host user namespace":    func(s *execution.RoleShape) { s.UserNamespace = "host" },
		"a localhost seccomp":      func(s *execution.RoleShape) { s.Seccomp = "Localhost:kodama/x.json" },
		"an LSM profile":           func(s *execution.RoleShape) { s.LSM = "kodama-rootless-build-v1" },
		"a device":                 func(s *execution.RoleShape) { s.Devices = []string{"/dev/fuse"} },
		"an unmasked proc":         func(s *execution.RoleShape) { s.ProcMount = "unmasked" },
		"a capability beyond ID":   func(s *execution.RoleShape) { s.CapabilitiesAdd = []string{"NET_ADMIN"} },
		"SYS_ADMIN in a long list": func(s *execution.RoleShape) { s.CapabilitiesAdd = []string{"SETUID", "SYS_ADMIN"} },
	} {
		s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
		role := s.Shared.Role
		mutate(&role)
		s.Shared.Role = role
		d, err := role.Digest()
		if err != nil {
			t.Fatal(err)
		}
		s.Shared.RoleDigest = d
		refused(name, s, rt, execution.ErrNotConfigured)
	}

	// SETUID and SETGID are the one thing a role may add, and add escalation.
	s := runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true")
	role := s.Shared.Role
	role.CapabilitiesAdd = []string{"SETUID", "SETGID"}
	s.Shared.Role = role
	s.Shared.RoleDigest, _ = role.Digest()
	if _, err := rt.Start(ctx, s); err != nil {
		t.Fatal(err)
	}
	var got createdContainer
	for _, c := range e.fake.created(t) {
		if c.Name == s.WorkloadName {
			got = c
		}
	}
	if !slices.Equal(got.CapAdd, []string{"SETUID", "SETGID"}) || !slices.Contains(got.Flags, "--cap-drop=ALL") || slices.Contains(got.Flags, "--security-opt=no-new-privileges") {
		t.Fatalf("the step role was not applied: caps %v flags %v", got.CapAdd, got.Flags)
	}

	// A program that supplies /workspace itself is refused.
	prog := programFunc(func(ctx context.Context, l podman.Launch) (podman.Container, error) {
		c, err := podman.DeclaredCommand{}.Container(ctx, l)
		c.Tmpfs = append(c.Tmpfs, "/workspace")
		return c, err
	})
	refused("a program that provides /workspace", runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true"), e.runtime(prog), execution.ErrInvalid)

	// A stopped run network fences the launch.
	netHandle := execution.ResourceHandle{Scope: r.scope, RunID: r.id, Kind: execution.KindNetwork, Key: execution.KeyNetwork, Name: r.network().Name}
	if ev, err := res.Stop(ctx, netHandle); err != nil || !ev.Stopped {
		t.Fatalf("%+v %v", ev, err)
	}
	refused("a stopped run network", runtimetest.SharedSpec(t, ws.Handle, runtimetest.HostImage, "true"), rt, execution.ErrResourceFenced)
}

type programFunc func(ctx context.Context, l podman.Launch) (podman.Container, error)

func (f programFunc) Container(ctx context.Context, l podman.Launch) (podman.Container, error) {
	return f(ctx, l)
}

// ---- capabilities ----

// Test_unit_local_capabilities_refuse_unenforced: the Podman backend declares
// only what its tests enforce, so every derivation of what a policy or document
// needs beyond that finds a member missing, and the adapter refuses what it
// cannot enforce before the first effect.
func Test_unit_local_capabilities_refuse_unenforced(t *testing.T) {
	ctx := context.Background()
	e := newREnv(t)
	res := e.open()
	rt := e.runtime(nil)
	set := res.Capabilities(ctx).Set
	guarantees := rt.Capabilities().Guarantees

	if res.Capabilities(ctx).Backend != podman.Backend || set != podman.ResourceCapabilities {
		t.Fatalf("the declaration moved: %s", set)
	}
	if never := execution.Set(
		execution.CapRunNetworkIsolation, execution.CapDeclaredEdges, execution.CapNamedEgress, execution.CapEgressDefaultDeny,
		execution.CapBuilderRootless, execution.CapBuilderService, execution.CapBuilderIsolatedWorker,
	); set&never != 0 {
		t.Fatalf("the podman backend advertises members it does not enforce: %s", execution.ResourceCapabilitySet(set&never))
	}
	if guarantees&(execution.NetworkPolicy|execution.ResourceQuotas) != 0 {
		t.Fatalf("podman declares a network or quota guarantee it does not enforce: %s", guarantees)
	}

	services := map[string]pipeline.RunService{"db": {}}
	tenant := execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true}
	for name, c := range map[string]struct {
		doc     pipeline.Document
		policy  execution.ExecutionPolicy
		missing execution.ResourceCapabilitySet
	}{
		"tenant model with a service":    {pipeline.Document{Spec: pipeline.Spec{Services: services}}, execution.ExecutionPolicy{Network: tenant}, 0},
		"run isolation and default deny": {pipeline.Document{}, execution.ExecutionPolicy{Network: execution.PolicyNetwork{Isolation: execution.IsolationRun}}, execution.Set(execution.CapRunNetworkIsolation, execution.CapEgressDefaultDeny)},
		"a rootless builder required":    {pipeline.Document{}, execution.ExecutionPolicy{Requires: execution.Set(execution.CapBuilderRootless), Network: tenant}, execution.Set(execution.CapBuilderRootless)},
	} {
		t.Run(name, func(t *testing.T) {
			required := execution.RequiredResourceCapabilities(c.doc, true, c.policy)
			if got := set.Missing(required); got != c.missing {
				t.Fatalf("missing %s, want %s", got, c.missing)
			}
			// Whatever the document, the tenant boundary is missing: no run_shared
			// admission accepts a run on this backend.
			if got := guarantees.Missing(execution.RunGuarantees(c.doc, execution.DefaultRequirements)); got&execution.NetworkPolicy == 0 {
				t.Fatalf("network_policy is not missing: %s", got)
			}
		})
	}

	// The adapter refuses, before any effect, what it cannot enforce.
	r := newRun(t)
	noEffect := func(name string, e *rEnv, spec execution.ResourceSpec, want error) {
		t.Helper()
		before := len(e.fake.argvs(t))
		if _, err := e.open().Ensure(ctx, spec); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", name, err, want)
		}
		for _, a := range e.fake.argvs(t)[before:] {
			if slices.Contains([]string{"create", "start", "run"}, a[0]) || (len(a) > 1 && a[1] == "create") {
				t.Fatalf("%s: effect %v", name, a)
			}
		}
		if _, err := os.Stat(filepath.Join(e.state, "resources", spec.Name, "record.json")); err == nil {
			t.Fatalf("%s: a refused spec left a record", name)
		}
	}
	httpProbe := dbBody()
	httpProbe.Readiness = execution.ReadinessSpec{Probe: "http", Port: "sql", Path: "/", TimeoutSecs: 5}
	noEffect("an http probe", e, r.service("db", httpProbe), execution.ErrInvalid)

	tcp := dbBody()
	tcp.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 5}
	noProbe := newREnv(t)
	noProbe.cfg.ProbeImage = ""
	noEffect("tcp readiness without a probe image", noProbe, r.service("db", tcp), execution.ErrNotConfigured)

	bound := dbBody()
	bound.Command = nil
	bound.Secrets = []execution.SecretPin{{Ref: "secret://project/db", Version: 1, Name: "PW", Delivery: "env"}}
	noEffect("a secret-bound service without a command", e, r.service("db", bound), execution.ErrInvalid)

	noBin := newREnv(t)
	noBin.cfg.EntrypointBinary = ""
	noEffect("a preparation without an entrypoint", noBin, r.preparation(), execution.ErrNotConfigured)
	noCtl := newREnv(t)
	noCtl.cfg.ControlNetwork = ""
	noEffect("a preparation without a control network", noCtl, r.preparation(), execution.ErrNotConfigured)

	other := dbBody()
	spec := r.service("db", other)
	spec.Backend = "process"
	noEffect("another backend's spec", e, spec, execution.ErrInvalid)
}

// fakeProbeName is the probe container name of a service spec.
func fakeProbeName(spec execution.ResourceSpec) string { return spec.Name + "-probe" }
