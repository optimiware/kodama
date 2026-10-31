package process

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

const (
	testPrepImage = "registry.invalid/kodama/prepare@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testCommit    = "0123456789abcdef0123456789abcdef01234567"
)

// resRegistrar is a ResourceRegistrar that records what it was asked to register
// and can be scripted to fail.
type resRegistrar struct {
	mu   sync.Mutex
	cmds []ResourceRegisterCommand
	err  error
}

func (r *resRegistrar) RegisterResource(_ context.Context, cmd ResourceRegisterCommand) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return r.err
}

func (r *resRegistrar) commands() []ResourceRegisterCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cmds)
}

// testProgram runs services as declared and the preparation as a host script
// that writes a file into the run workspace and exits.
func testProgram(prep string) ResourceProgram {
	return ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
		if l.Spec.Kind == execution.KindPreparation {
			return Command{Path: "/bin/sh", Args: []string{"-c", prep}, Env: slices.Concat([]string{"PATH=" + os.Getenv("PATH")}, l.Env), Dir: l.WorkspaceDir}, nil
		}
		return DeclaredService{}.Command(ctx, l)
	})
}

const prepScript = `echo prepared > "$KODAMA_WORKSPACE_DIR/prepared"; echo prep-done`

func newResources(t *testing.T, dir string, prog ResourceProgram, reg ResourceRegistrar) *Resources {
	t.Helper()
	r, err := NewResources(ResourcesConfig{
		Dir: dir, Program: prog, Registrar: reg, ServerURL: "https://runtime.invalid", CAFile: "/etc/ca.pem",
		KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.idle(10 * time.Second) })
	return r
}

func serviceBody(script string, port int64) execution.ServiceSpec {
	return execution.ServiceSpec{
		Image: runtimetest.HostImage, Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{},
		Secrets: []execution.SecretPin{}, Ports: map[string]int64{"sql": port}, Storage: []execution.StorageSpec{}, Egress: []string{},
		Resources: execution.ServiceResourcesSpec{CPUMillis: 100, MemoryMiB: 64, EphemeralMiB: 64},
		Readiness: execution.ReadinessSpec{
			Probe: "exec", Exec: []string{"/bin/sh", "-c", "test -f ready"}, TimeoutSecs: 30, PeriodSecs: 1, ProbeTimeout: 2, LossSecs: 5,
		},
	}
}

// processFixture binds the Process resources, over one directory, to the
// backend-neutral contract.
func processFixture(t *testing.T) runtimetest.ResourcesFixture {
	dir := t.TempDir()
	reg := &resRegistrar{}
	return runtimetest.ResourcesFixture{
		Open: func() execution.RunResources { return newResources(t, dir, testProgram(prepScript), reg) },
		Want: execution.ResourceCapabilities{Backend: Backend, Set: ResourceCapabilities},
		Service: func(string) execution.ServiceSpec {
			return serviceBody("touch ready; echo service-up; exec sleep 300", 5432)
		},
		Preparation: func() execution.PreparationSpec {
			return execution.PreparationSpec{Image: testPrepImage, RepositoryURL: "https://git.invalid/org/repo.git", Commit: testCommit, TimeoutSecs: 60, MaxBytes: 1 << 20, MaxAttempts: 1}
		},
		Foreign: func(h execution.ResourceHandle) {
			if err := os.MkdirAll(filepath.Join(dir, h.Name), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, h.Name, "foreign"), []byte("not ours"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		ForeignIntact: func(h execution.ResourceHandle) bool {
			b, err := os.ReadFile(filepath.Join(dir, h.Name, "foreign"))
			return err == nil && string(b) == "not ours"
		},
		Exists: func(h execution.ResourceHandle) bool {
			for _, p := range []string{filepath.Join(dir, h.Name), filepath.Join(dir, recordsDir, h.Name+".json")} {
				if _, err := os.Lstat(p); err == nil {
					return true
				}
			}
			return false
		},
		Fault: func() func() {
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			restore := func() { _ = os.Chmod(dir, 0o700) }
			t.Cleanup(restore)
			return restore
		},
		HoldWorkspace: func(h execution.ResourceHandle) func() {
			rt, err := New(Config{Program: DeclaredCommand{}, Dir: t.TempDir(), ResourcesDir: dir, KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			s := runtimetest.SharedSpec(t, h, "", "exec sleep 300")
			w, err := rt.Start(context.Background(), s)
			if err != nil {
				t.Fatalf("the holder did not start: %v", err)
			}
			t.Cleanup(func() { _ = rt.Remove(context.Background(), w) })
			return func() { _, _ = rt.Stop(context.Background(), w) }
		},
	}
}

// Test_unit_run_resources_contract_process runs the exported companion-port
// contract over the Process resources.
func Test_unit_run_resources_contract_process(t *testing.T) {
	runtimetest.RunResourcesContract(t, processFixture)
}

// sharedEnv is a Process runtime and resources over one directory.
type sharedEnv struct {
	res *Resources
	rt  *Runtime
	reg *resRegistrar
	dir string
}

func newSharedEnv(t *testing.T) sharedEnv {
	t.Helper()
	dir := t.TempDir()
	reg := &resRegistrar{}
	rt, err := New(Config{Program: DeclaredCommand{}, Dir: t.TempDir(), ResourcesDir: dir, KillGrace: time.Second, DrainTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return sharedEnv{res: newResources(t, dir, testProgram(prepScript), reg), rt: rt, reg: reg, dir: dir}
}

// workspace ensures the run's workspace and returns its handle and the run's
// attempt ID.
func (e sharedEnv) workspace(t *testing.T) execution.ResourceHandle {
	t.Helper()
	h, _ := e.workspaceSpec(t)
	return h
}

// workspaceSpec ensures the run's workspace and returns its handle and spec.
func (e sharedEnv) workspaceSpec(t *testing.T) (execution.ResourceHandle, execution.ResourceSpec) {
	t.Helper()
	spec := execution.ResourceSpec{
		Scope: runtimetest.Scope, RunID: domain.RunID("run_" + string(runtimetest.NewAttemptID(t))), Backend: Backend, Kind: execution.KindWorkspace, Key: execution.KeyWorkspace,
		Policy:    execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape: "sha256:" + strings.Repeat("b", 64), DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Workspace: &execution.WorkspaceSpec{SizeMiB: 64, Initial: "empty"},
	}
	spec.Name = execution.ResourceName(spec.Scope, spec.RunID, spec.Kind, spec.Key)
	obs, err := e.res.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return obs.Handle, spec
}

// Test_unit_process_workspace_shared_across_attempts: successive attempts of a
// run see one workspace directory (S2), the entrypoint attempt selects the v2
// wire and receives no checkout credential, and the workspace fence holds.
func Test_unit_process_workspace_shared_across_attempts(t *testing.T) {
	ctx := context.Background()

	t.Run("two attempts share the run workspace", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		dir := e.res.st.workspace(ws.Name)

		a := runtimetest.SharedSpec(t, ws, "", `echo from-a > "$KODAMA_WORKSPACE_DIR/a.txt"; test "$(pwd -P)" = "$(cd "$KODAMA_WORKSPACE_DIR" && pwd -P)"`)
		wa, err := e.rt.Start(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.rt.Wait(ctx, wa); err != nil {
			t.Fatal(err)
		}
		if obs, err := e.rt.Observe(ctx, wa); err != nil || obs.Kind == execution.ObservedFailed {
			t.Fatalf("the first attempt failed: %+v %v", obs, err)
		}
		// The next attempt of the run (another image in a container world) reads
		// what the first wrote, from the same directory, and adds its own file.
		b := runtimetest.SharedSpec(t, ws, "", `grep -x from-a "$KODAMA_WORKSPACE_DIR/a.txt" && echo from-b > "$KODAMA_WORKSPACE_DIR/b.txt"`)
		wb, err := e.rt.Start(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.rt.Wait(ctx, wb); err != nil {
			t.Fatal(err)
		}
		if obs, err := e.rt.Observe(ctx, wb); err != nil || obs.Kind == execution.ObservedFailed {
			var logs bytes.Buffer
			_ = e.rt.Logs(ctx, wb, &logs)
			t.Fatalf("the second attempt did not see the first's file: %+v %v\n%s", obs, err, logs.String())
		}
		for name, want := range map[string]string{"a.txt": "from-a\n", "b.txt": "from-b\n"} {
			if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
				t.Fatalf("%s: %q %v", name, got, err)
			}
		}
		// Once both ended nothing holds the workspace.
		if obs, err := e.res.Observe(ctx, ws); err != nil || !obs.Released {
			t.Fatalf("the workspace is still held: %+v %v", obs, err)
		}
	})

	t.Run("an attempt names the bound instance, a live workspace and a configured backend", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)

		wrong := ws
		wrong.UID = "99999999-9999-4999-8999-999999999999"
		if _, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, wrong, "", "true")); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("another instance's workspace: %v", err)
		}
		nowhere := ws
		nowhere.RunID = domain.RunID("run_" + string(runtimetest.NewAttemptID(t)))
		nowhere.Name = execution.ResourceName(nowhere.Scope, nowhere.RunID, nowhere.Kind, nowhere.Key)
		if _, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, nowhere, "", "true")); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a run without a workspace: %v", err)
		}
		bare, err := New(Config{Program: DeclaredCommand{}, Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bare.Start(ctx, runtimetest.SharedSpec(t, ws, "", "true")); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a runtime without run resources: %v", err)
		}
		if bare.Capabilities().Guarantees&execution.RunResourcesMarker != 0 || e.rt.Capabilities().Guarantees&execution.RunResourcesMarker == 0 {
			t.Fatal("the run-resources marker follows the configuration")
		}
		// An attempt-private spec still starts without any workspace.
		if w, err := e.rt.Start(ctx, spec(t, "true")); err != nil {
			t.Fatal(err)
		} else {
			_ = e.rt.Remove(ctx, w)
		}
	})

	t.Run("the workspace is fenced against stop while an attempt holds it and after", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		w, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, ws, "", "exec sleep 300"))
		if err != nil {
			t.Fatal(err)
		}
		if obs, err := e.res.Observe(ctx, ws); err != nil || obs.Released {
			t.Fatalf("a running attempt does not hold the workspace: %+v %v", obs, err)
		}
		if ev, err := e.res.Stop(ctx, ws); err != nil || !ev.Stopped || ev.Released {
			t.Fatalf("stop of a held workspace: %+v %v", ev, err)
		}
		// A later attempt of the stopped workspace is fenced even though the volume
		// is still held by the first.
		if _, err := e.rt.Start(ctx, runtimetest.SharedSpec(t, ws, "", "true")); !errors.Is(err, execution.ErrResourceFenced) {
			t.Fatalf("an attempt started on a stopped workspace: %v", err)
		}
		if rm, err := e.res.Remove(ctx, ws); err != nil || rm.Removed {
			t.Fatalf("a held workspace was removed: %+v %v", rm, err)
		}
		if _, err := e.rt.Stop(ctx, w); err != nil {
			t.Fatal(err)
		}
		if ev, err := e.res.Stop(ctx, ws); err != nil || !ev.Released {
			t.Fatalf("not released after the holder stopped: %+v %v", ev, err)
		}
		if rm, err := e.res.Remove(ctx, ws); err != nil || !rm.Removed {
			t.Fatalf("remove: %+v %v", rm, err)
		}
	})

	t.Run("the entrypoint attempt selects the v2 wire in the shared workspace without a checkout credential", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		wsDir := e.res.st.workspace(ws.Name)
		reg := &registrarV2{}
		p := Entrypoint{Binary: "/opt/kodama", ServerURL: "https://runtime.invalid", Issuer: bootstrap.Issuer{Registrar: reg}}

		// The issuer registers a run_shared attempt with its ContextV2 (the v2
		// spec); the attempt-private checks below use the v1 spec of the same run.
		private := runtimetest.SharedSpec(t, ws, "", "true")
		s := runtimetest.SharedSpecV2(t, ws, "", "true")
		c := sourceContext(t, private)
		dir := t.TempDir()
		l := Launch{Spec: s, Context: c, Workload: s.Workload("11111111-2222-4333-8444-555555555555"), Dir: dir, Workspace: wsDir}
		cmd, err := p.Command(ctx, l)
		if err != nil {
			t.Fatalf("a source-consuming run_shared attempt: %v", err)
		}
		args := strings.Join(cmd.Args, " ")
		for _, want := range []string{"--workspace-dir " + wsDir, "--attempt-id " + string(s.AttemptID)} {
			if !strings.Contains(args, want) {
				t.Errorf("missing %q in %v", want, cmd.Args)
			}
		}
		if strings.Contains(args, "checkout") || strings.Contains(args, "git-ca") {
			t.Errorf("a v2 attempt received checkout arguments: %v", cmd.Args)
		}
		for _, want := range []string{"KODAMA_RUNTIME_API=v2", "KODAMA_WORKSPACE_DIR=" + wsDir} {
			if !slices.Contains(cmd.Env, want) {
				t.Errorf("missing %q in %v", want, cmd.Env)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "workspace")); err == nil {
			t.Error("a private workspace directory was created next to the shared one")
		}
		for _, e := range cmd.Env {
			if strings.Contains(e, "CHECKOUT") {
				t.Errorf("checkout variable %q", e)
			}
		}
		if len(reg.cmds2) != 1 || reg.cmds2[0].AttemptID != s.AttemptID || reg.cmds2[0].Context.ContextVersion != 2 || len(reg.cmds) != 0 {
			t.Errorf("the attempt was not registered once on the v2 wire: v2 %d v1 %d", len(reg.cmds2), len(reg.cmds))
		}
		// The same context without the shared launch still takes the v1 path and
		// still needs checkout material the Process runtime cannot give.
		l.Spec, l.Workspace = private, ""
		l.Spec.Shared = nil
		if _, err := p.Command(ctx, l); !errors.Is(err, bootstrap.ErrNoCheckout) {
			t.Fatalf("an attempt-private source-consuming attempt: %v", err)
		}
		// v1 workloads never get the wire selector.
		l.Context.Workspace.Access = runtimev1.AccessNone
		v1, err := p.Command(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(v1.Env, "KODAMA_RUNTIME_API=v2") {
			t.Error("an attempt-private workload selected the v2 wire")
		}
		// A shared attempt without a run workspace is refused, never given a private one.
		l.Spec.Shared = private.Shared
		if _, err := p.Command(ctx, l); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a shared attempt without a workspace: %v", err)
		}
	})
}

// registrarV2 is a registrar that also serves the run_shared wire.
type registrarV2 struct {
	registrar
	cmds2 []runtimeauth.RegisterCommandV2
}

func (r *registrarV2) RegisterV2(_ context.Context, cmd runtimeauth.RegisterCommandV2) (runtimeauth.Registration, error) {
	r.cmds2 = append(r.cmds2, cmd)
	return runtimeauth.Registration{}, nil
}

// sourceContext is the pinned context of s changed to consume the workspace.
func sourceContext(t *testing.T, s execution.AttemptSpec) runtimev1.Context {
	t.Helper()
	c, err := s.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{RepositoryURL: "https://git.invalid/org/repo.git", Commit: testCommit}}
	return c
}

func serviceSpec(t *testing.T, e sharedEnv, name string, body execution.ServiceSpec) execution.ResourceSpec {
	t.Helper()
	run := domain.RunID("run_" + string(runtimetest.NewAttemptID(t)))
	body.Name, body.Role, body.Host = name, "general", execution.EndpointHost(runtimetest.Scope, run, name)
	spec := execution.ResourceSpec{
		Scope: runtimetest.Scope, RunID: run, Backend: Backend, Kind: execution.KindService, Key: execution.ServiceKey(name),
		Policy:    execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape: "sha256:" + strings.Repeat("b", 64), DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Service: &body,
	}
	spec.Name = execution.ResourceName(spec.Scope, spec.RunID, spec.Kind, spec.Key)
	return spec
}

func awaitPhase(t *testing.T, res execution.RunResources, h execution.ResourceHandle, want execution.ResourcePhase) execution.ResourceObservation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		obs, err := res.Observe(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Phase == want {
			return obs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became %s", h.Key, want)
	return execution.ResourceObservation{}
}

// Test_process_resources_workload: a service is launched exactly once whatever
// happens to it, is probed by tcp as well as exec, its admission credential is
// persisted before it is registered and removed at stop, and its identity
// environment is the S4 one.
func Test_process_resources_workload(t *testing.T) {
	ctx := context.Background()

	t.Run("a service is launched once and a failure is never replaced", func(t *testing.T) {
		e := newSharedEnv(t)
		launches := 0
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			launches++
			return DeclaredService{}.Command(ctx, l)
		})
		res := newResources(t, e.dir, prog, nil)
		spec := serviceSpec(t, e, "db", serviceBody("exit 3", 5432))
		first, err := res.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		failed := awaitPhase(t, res, first.Handle, execution.PhaseFailed)
		if failed.ExitCode == nil || *failed.ExitCode != 3 || failed.Reason != execution.ReasonServiceLost {
			t.Fatalf("a failed service: %+v", failed)
		}
		for _, r := range []*Resources{res, newResources(t, e.dir, prog, nil)} {
			if again, err := r.Ensure(ctx, spec); err != nil || again.Handle != first.Handle || again.Phase != execution.PhaseFailed {
				t.Fatalf("a repeated ensure of a failed service: %+v %v", again, err)
			}
		}
		if launches != 1 {
			t.Fatalf("the service was launched %d times", launches)
		}
	})

	t.Run("a failed launch is retried and creates no second instance", func(t *testing.T) {
		e := newSharedEnv(t)
		fail := true
		var uids []string
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			uids = append(uids, l.Handle.UID)
			if fail {
				return Command{}, errors.New("program unavailable")
			}
			return DeclaredService{}.Command(ctx, l)
		})
		res := newResources(t, e.dir, prog, nil)
		spec := serviceSpec(t, e, "db", serviceBody("touch ready; exec sleep 300", 5432))
		if _, err := res.Ensure(ctx, spec); err == nil {
			t.Fatal("a failing program was accepted")
		}
		fail = false
		obs, err := res.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if len(uids) != 2 || uids[0] != uids[1] || uids[0] != obs.Handle.UID {
			t.Fatalf("the retried launch changed the instance: %v, %q", uids, obs.Handle.UID)
		}
		awaitPhase(t, res, obs.Handle, execution.PhaseReady)
		_, _ = res.Stop(ctx, obs.Handle)
		_, _ = res.Remove(ctx, obs.Handle)
	})

	t.Run("readiness by tcp", func(t *testing.T) {
		e := newSharedEnv(t)
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("no loopback listener")
		}
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		port := int64(l.Addr().(*net.TCPAddr).Port)
		body := serviceBody("exec sleep 300", port)
		body.Readiness = execution.ReadinessSpec{Probe: "tcp", Port: "sql", TimeoutSecs: 30, ProbeTimeout: 1}
		spec := serviceSpec(t, e, "db", body)
		obs, err := e.res.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		ready := awaitPhase(t, e.res, obs.Handle, execution.PhaseReady)
		if ready.ProbeKind != "tcp" || ready.ReadyAt == nil {
			t.Fatalf("%+v", ready)
		}
		l.Close() // the endpoint disappears: the service is no longer ready, but it still runs
		awaitPhase(t, e.res, obs.Handle, execution.PhasePending)
		_, _ = e.res.Stop(ctx, obs.Handle)
		_, _ = e.res.Remove(ctx, obs.Handle)
	})

	t.Run("a readiness contract the backend cannot probe is refused", func(t *testing.T) {
		e := newSharedEnv(t)
		for name, rd := range map[string]execution.ReadinessSpec{
			"http":              {Probe: "http", Port: "sql", Path: "/", TimeoutSecs: 5},
			"tcp on an unnamed": {Probe: "tcp", Port: "nope", TimeoutSecs: 5},
			"exec without argv": {Probe: "exec", TimeoutSecs: 5},
		} {
			body := serviceBody("exec sleep 300", 5432)
			body.Readiness = rd
			if _, err := e.res.Ensure(ctx, serviceSpec(t, e, "db", body)); !errors.Is(err, execution.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the admission credential is persisted before it is registered and destroyed at stop", func(t *testing.T) {
		e := newSharedEnv(t)
		var envs [][]string
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			envs = append(envs, l.Env)
			// The credential file exists, holds the very value that was registered and is private.
			cmds := e.reg.commands()
			raw, err := os.ReadFile(l.CredentialFile)
			if err != nil || len(cmds) != 1 || string(raw) != cmds[0].Credential.Reveal() {
				return Command{}, errors.New("the credential was not persisted before registration")
			}
			return DeclaredService{}.Command(ctx, l)
		})
		res := newResources(t, e.dir, prog, e.reg)
		spec := serviceSpec(t, e, "db", serviceBody("touch ready; exec sleep 300", 5432))
		obs, err := res.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		cmds := e.reg.commands()
		fp, _ := spec.Fingerprint()
		if len(cmds) != 1 || cmds[0].Scope != spec.Scope || cmds[0].RunID != spec.RunID || cmds[0].Key != spec.Key || cmds[0].SpecFingerprint != fp ||
			!cmds[0].ExpiresAt.Equal(mustTime(t, spec.DeadlineAt)) {
			t.Fatalf("registration %+v", cmds)
		}
		cred := filepath.Join(res.st.resDir(spec.Name), "identity", CredentialFile)
		if fi, err := os.Stat(cred); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("credential file: %v %v", fi, err)
		}
		for _, want := range []string{
			"KODAMA_SERVER_URL=https://runtime.invalid", "KODAMA_CA_FILE=/etc/ca.pem", "KODAMA_RUN_ID=" + string(spec.RunID),
			"KODAMA_RESOURCE_KEY=service.db", "KODAMA_ADMISSION_CREDENTIAL_FILE=" + cred, "KODAMA_WORKLOAD_NAME=" + spec.Name, "KODAMA_WORKLOAD_UID=" + obs.Handle.UID,
		} {
			if !slices.Contains(envs[0], want) {
				t.Errorf("missing %q in %v", want, envs[0])
			}
		}
		for _, e := range envs[0] {
			if strings.HasPrefix(e, "KODAMA_POD_") || strings.HasPrefix(e, "KODAMA_WORKSPACE_DIR=") {
				t.Errorf("a service received %q", e)
			}
			if strings.Contains(e, cmds[0].Credential.Reveal()) {
				t.Errorf("the credential value is in the environment")
			}
		}
		// A restart adopts: no second registration, no second launch.
		if _, err := newResources(t, e.dir, prog, e.reg).Ensure(ctx, spec); err != nil || len(e.reg.commands()) != 1 || len(envs) != 1 {
			t.Fatalf("adoption registered or launched again: %v %d %d", err, len(e.reg.commands()), len(envs))
		}
		if _, err := res.Stop(ctx, obs.Handle); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(cred); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the credential survived the stop: %v", err)
		}
		_, _ = res.Remove(ctx, obs.Handle)
	})

	t.Run("a registration that conflicts or fails launches nothing", func(t *testing.T) {
		e := newSharedEnv(t)
		launched := false
		prog := ResourceProgramFunc(func(ctx context.Context, l ResourceLaunch) (Command, error) {
			launched = true
			return DeclaredService{}.Command(ctx, l)
		})
		spec := serviceSpec(t, e, "db", serviceBody("touch ready; exec sleep 300", 5432))
		reg := &resRegistrar{err: runtimeauth.ErrConflict}
		if _, err := newResources(t, e.dir, prog, reg).Ensure(ctx, spec); !errors.Is(err, execution.ErrConflict) || launched {
			t.Fatalf("a conflicting credential: %v launched=%v", err, launched)
		}
		reg.err = errors.New("admission unavailable")
		if _, err := newResources(t, e.dir, prog, reg).Ensure(ctx, spec); err == nil || launched {
			t.Fatalf("an unavailable registrar: %v launched=%v", err, launched)
		}
		// The same persisted credential is registered again on the retry.
		reg.err = nil
		obs, err := newResources(t, e.dir, prog, reg).Ensure(ctx, spec)
		if err != nil || !launched {
			t.Fatalf("retry: %v", err)
		}
		cmds := reg.commands()
		if len(cmds) != 3 || cmds[0].Credential.Digest() != cmds[2].Credential.Digest() {
			t.Fatalf("the credential changed between attempts: %d", len(cmds))
		}
		_, _ = e.res.Stop(ctx, obs.Handle)
		_, _ = e.res.Remove(ctx, obs.Handle)
	})

	t.Run("a preparation runs against the run workspace only and refuses another run's", func(t *testing.T) {
		e := newSharedEnv(t)
		ws := e.workspace(t)
		spec := execution.ResourceSpec{
			Scope: ws.Scope, RunID: ws.RunID, Backend: Backend, Kind: execution.KindPreparation, Key: execution.KeyPreparation,
			Policy:    execution.PolicyPin{ID: "run-contract", Digest: "sha256:" + strings.Repeat("a", 64)},
			RoleShape: "sha256:" + strings.Repeat("b", 64), DeadlineAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			Preparation: &execution.PreparationSpec{Image: testPrepImage, RepositoryURL: "https://git.invalid/org/repo.git", Commit: testCommit, TimeoutSecs: 60, MaxBytes: 1 << 20, MaxAttempts: 1},
		}
		spec.Name = execution.ResourceName(spec.Scope, spec.RunID, spec.Kind, spec.Key)
		obs, err := e.res.Ensure(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		ready := awaitPhase(t, e.res, obs.Handle, execution.PhaseReady) // exit 0 is "completed"
		if ready.ReadyAt == nil {
			t.Fatalf("%+v", ready)
		}
		if got, err := os.ReadFile(filepath.Join(e.res.st.workspace(ws.Name), "prepared")); err != nil || string(got) != "prepared\n" {
			t.Fatalf("the preparation did not write the run workspace: %q %v", got, err)
		}
		other := spec
		other.RunID = domain.RunID("run_" + string(runtimetest.NewAttemptID(t)))
		other.Name = execution.ResourceName(other.Scope, other.RunID, other.Kind, other.Key)
		if _, err := e.res.Ensure(ctx, other); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a preparation of a run without a workspace: %v", err)
		}
	})
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UTC()
}

// Test_unit_local_capabilities_refuse_unenforced: the Process backend declares
// lifecycle members only, so every derivation of what a run_shared run needs
// finds something missing (always the network_policy guarantee, and the
// isolation and network members its policy or documents add), and the adapter
// itself refuses a spec it cannot enforce.
func Test_unit_local_capabilities_refuse_unenforced(t *testing.T) {
	ctx := context.Background()
	e := newSharedEnv(t)
	set := e.res.Capabilities(ctx).Set
	guarantees := e.rt.Capabilities().Guarantees

	if set != ResourceCapabilities || e.res.Capabilities(ctx).Backend != Backend {
		t.Fatalf("the declaration moved: %s", set)
	}
	if never := execution.Set(
		execution.CapServiceIsolation, execution.CapRunNetworkIsolation, execution.CapDeclaredEdges, execution.CapNamedEgress,
		execution.CapEgressDefaultDeny, execution.CapBuilderRootless, execution.CapBuilderService, execution.CapBuilderIsolatedWorker,
	); set&never != 0 {
		t.Fatalf("the process backend advertises members it cannot enforce: %s", execution.ResourceCapabilitySet(set&never))
	}
	if guarantees&execution.AllGuarantees != 0 {
		t.Fatalf("host processes give no attempt guarantee: %s", guarantees)
	}

	services := map[string]pipeline.RunService{"db": {}}
	for name, c := range map[string]struct {
		doc     pipeline.Document
		policy  execution.ExecutionPolicy
		missing execution.ResourceCapabilitySet
	}{
		"tenant model, no service": {pipeline.Document{}, execution.ExecutionPolicy{Network: execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true}}, 0},
		"tenant model, a service": {
			pipeline.Document{Spec: pipeline.Spec{Services: services}}, execution.ExecutionPolicy{Network: execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true}},
			execution.Set(execution.CapServiceIsolation),
		},
		"run isolation and default deny": {
			pipeline.Document{}, execution.ExecutionPolicy{Network: execution.PolicyNetwork{Isolation: execution.IsolationRun}},
			execution.Set(execution.CapRunNetworkIsolation, execution.CapEgressDefaultDeny),
		},
		"a rootless builder required": {
			pipeline.Document{}, execution.ExecutionPolicy{Requires: execution.Set(execution.CapBuilderRootless), Network: execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true}},
			execution.Set(execution.CapBuilderRootless),
		},
	} {
		t.Run(name, func(t *testing.T) {
			required := execution.RequiredResourceCapabilities(c.doc, true, c.policy)
			if got := set.Missing(required); got != c.missing {
				t.Fatalf("missing %s, want %s", got, c.missing)
			}
			// Whatever the document, the tenant boundary is missing: no run_shared
			// admission accepts a run on this backend, even a service-free one.
			if got := guarantees.Missing(execution.RunGuarantees(c.doc, execution.DefaultRequirements)); got&execution.NetworkPolicy == 0 {
				t.Fatalf("network_policy is not missing: %s", got)
			}
		})
	}

	t.Run("a spec of another backend is refused and leaves nothing behind", func(t *testing.T) {
		spec := serviceSpec(t, e, "db", serviceBody("exec sleep 300", 5432))
		spec.Backend = "podman"
		if _, err := e.res.Ensure(ctx, spec); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("another backend's spec: %v", err)
		}
		if _, err := os.Stat(filepath.Join(e.dir, spec.Name)); err == nil {
			t.Fatal("a refused spec left an object behind")
		}
	})
}

// execCommand runs a Command to completion and returns its output.
func execCommand(c Command) (string, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env, cmd.Dir = c.Env, c.Dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
