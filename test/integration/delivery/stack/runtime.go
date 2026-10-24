package stack

// The Podman runtime of the stack: execution dispatches the admitted step —
// its persisted image, command, arguments and environment — as a Podman
// container whose entrypoint is the real `kodama step`. The entrypoint is
// admitted by runtime admission against the verified container, reads its
// pinned context, resolves the declared credential through the real secrets
// resolver, runs the declared command and submits the result manifest through
// the result route. Nothing about the step is supplied outside the admitted
// definition except the test's checkout of manifests (the Podman runtime issues
// no source checkout) and the host paths the containers need to read.

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	"github.com/optimiware/kodama/test/support/layer"
)

const secretsInstallation = "inst-t067"

// installSecrets replaces the fixture's secrets service by one whose key wrapper
// the stack holds: the runtime route must open the very values the fixtures seal.
// It runs before any secret exists.
func (s *Stack) installSecrets() {
	t := s.T
	dir := t.TempDir()
	key := make([]byte, keyfile.KeySize)
	_, err := rand.Read(key)
	must(t, err)
	path := filepath.Join(dir, "master-key-v1")
	must(t, os.WriteFile(path, key, 0o600))
	provider, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": path}})
	must(t, err)
	s.secretWrapper = provider
	s.SecretSvc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: secretsInstallation, Store: s.X.Secrets, Wrapper: provider, IDs: secrets.RandomIDs{}})
	must(t, err)
	s.X.SecretSvc = s.SecretSvc
}

var (
	stepOnce sync.Once
	stepPath string
	stepErr  error
)

// Close removes what this process built outside the tests' own directories.
func Close() {
	wiringsMu.Lock()
	ws := wirings
	wirings = nil
	wiringsMu.Unlock()
	for _, w := range ws {
		w.removeWorkloads()
	}
	if stepPath != "" {
		_ = os.RemoveAll(filepath.Dir(stepPath))
	}
}

// stepBinary builds the real step entrypoint for the Podman machine's Linux.
func stepBinary(t testing.TB) string {
	t.Helper()
	stepOnce.Do(func() {
		dir, err := os.MkdirTemp("", "t067-step-")
		if err != nil {
			stepErr = err
			return
		}
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			stepErr = err
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(out)))
		arch, err := exec.Command("podman", "info", "--format", "{{.Host.Arch}}").Output()
		if err != nil {
			stepErr = err
			return
		}
		bin := filepath.Join(dir, "kodama")
		cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./test/integration/runtime/stepbin")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)))
		if b, err := cmd.CombinedOutput(); err != nil {
			stepErr = fmt.Errorf("building the entrypoint: %w: %s", err, b)
			return
		}
		stepPath = bin
	})
	if stepErr != nil {
		t.Fatal(stepErr)
	}
	return stepPath
}

type podmanWiring struct {
	dir    string
	srv    *Server
	binary string

	mu      sync.Mutex
	started []string
	repo    *gitRepo

	rt *recordingRuntime
}

var (
	wiringsMu sync.Mutex
	wirings   []*podmanWiring
)

// removeWorkloads force-removes exactly the containers this wiring's runtime was
// asked to start (by their unique workload names, recorded before each start so
// that a failed start is included) and nothing else: an image is not ownership, so
// no container is selected by the image it runs.
func (w *podmanWiring) removeWorkloads() {
	w.mu.Lock()
	names := w.started
	w.started = nil
	w.mu.Unlock()
	for _, n := range names {
		_ = exec.Command("podman", "rm", "--force", "--volumes", n).Run()
	}
}

// RemoveWorkloads removes the containers this stack's runtime started.
func (s *Stack) RemoveWorkloads() {
	if s.pod != nil {
		s.pod.removeWorkloads()
	}
}

// recordingRuntime is the Podman runtime remembering the containers it started,
// so a test removes exactly those.
type recordingRuntime struct {
	*podman.Runtime
	w *podmanWiring
}

func (r *recordingRuntime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	r.w.mu.Lock()
	r.w.started = append(r.w.started, spec.WorkloadName)
	r.w.mu.Unlock()
	return r.Runtime.Start(ctx, spec)
}

// program is the real entrypoint program plus the host-side adaptations a
// rootless Podman with a non-root image needs: the files mounted into the
// container are made readable by the image's user, and the Git CA is named for
// the checkout. The Podman runtime's own program refuses an attempt that consumes
// the workspace because it issues no checkout credential; the repository the
// tests serve is anonymous, so the refusal is lifted here and the real entrypoint
// performs the checkout of the admitted source commit itself.
type program struct {
	w     *podmanWiring
	inner podman.Entrypoint
}

func (p program) Container(ctx context.Context, l podman.Launch) (podman.Container, error) {
	asNone := l
	asNone.Context.Workspace.Access = runtimev1.AccessNone
	c, err := p.inner.Container(ctx, asNone)
	if err != nil {
		return c, err
	}
	for _, m := range c.Mounts {
		if err := readable(m.Source); err != nil {
			return c, err
		}
	}
	c.Env = append(c.Env, "KODAMA_GIT_CA_FILE=/kodama/bootstrap/ca.crt")
	// The Pod recipe gives the checkout directory to the step user through
	// fsGroup; Git refuses a checkout directory another user owns (the inherited
	// M-4 of T-016/T-021). The same ownership is the tmpfs's.
	for i, t := range c.Tmpfs {
		if t == "/workspace" {
			c.Tmpfs[i] = "/workspace:rw,U"
		}
	}
	return c, nil
}

func readable(path string) error {
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if d.IsDir() {
			mode = 0o755
		} else if fi, err := d.Info(); err == nil && fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return os.Chmod(p, mode)
	})
}

func (w *podmanWiring) build(s *Stack) {
	t := s.T
	rt, err := podman.New(podman.Config{
		Program: program{w: w, inner: podman.Entrypoint{
			Binary: w.binary, ServerURL: w.srv.URL, CA: w.srv.CA,
			Issuer: bootstrap.Issuer{Registrar: s.Admission, Actions: deploymentActions},
		}},
		StateDir: filepath.Join(w.dir, "workloads"),
		// The containers share the host's network: the kind API server and the
		// runtime API both listen on loopback.
		Network: "host", StopTimeout: 2 * time.Second,
	})
	must(t, err)
	w.rt = &recordingRuntime{Runtime: rt, w: w}
}

// UsePodman switches the stack's execution to the Podman runtime and starts the
// runtime API server its entrypoints call. dir is a directory for the runtime's
// state and the server's CA; it returns the server for fault injection and
// inspection.
func (s *Stack) UsePodman(dir string) *Server {
	t := s.T
	t.Helper()
	layer.SkipService(t, "Podman")
	w := &podmanWiring{dir: dir, binary: stepBinary(t)}
	s.pod = w
	w.repo = newGitRepo(t, dir)
	w.srv = newServer(t, s, dir)
	wiringsMu.Lock()
	wirings = append(wirings, w)
	wiringsMu.Unlock()
	t.Cleanup(w.removeWorkloads)
	s.build()
	return w.srv
}

// Logs returns the container output of the attempt's workload.
func (s *Stack) Logs(at *execution.Attempt) string {
	var b strings.Builder
	if s.pod != nil {
		_ = s.pod.rt.Logs(context.Background(), at.Workload(), &b)
	}
	return b.String()
}

// awaitStarted drives reconciliation until the control plane has observed the
// attempt's command start (the attempt row's started_at), the launch evidence a
// later crash is judged by.
func (s *Stack) awaitStarted(attempt domain.AttemptID, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.X.Count(`SELECT count(*) FROM step_attempts WHERE id = $1 AND started_at IS NOT NULL`, string(attempt)) == 1 {
			return true
		}
		for _, tenant := range []domain.TenantID{deliverytest.ScopeA.TenantID, deliverytest.ScopeB.TenantID} {
			_, _ = s.Exec.ReconcileTenant(context.Background(), tenant)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
