// Package runtimeacceptance proves, against a live "services" scope, that the
// endpoint files configure the real Podman runtime adapter
// (internal/runtime/podman through execution.Runtime): a workload it starts
// reaches PostgreSQL by its workload URL and the host callback by the
// advertised host name, and Remove leaves nothing behind.
//
// It needs a scope built by `make dev-up DEV_STACK=services` and skips
// without one. Run it through `scripts/dev/environment.py runtime`, or:
//
//	KODAMA_DEV_SCOPE_DIR=dev/.state/environments/<scope> go test ./dev/runtimeacceptance -count=1 -v
package runtimeacceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// ScopeDirEnv names the scope directory (dev/.state/environments/<scope>).
const ScopeDirEnv = "KODAMA_DEV_SCOPE_DIR"

type scope struct {
	dir      string
	host     map[string]string
	workload map[string]string
	image    string
	secrets  []string
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func load(t *testing.T) scope {
	t.Helper()
	dir := os.Getenv(ScopeDirEnv)
	if dir == "" {
		t.Skipf("%s is unset: no services scope to accept against", ScopeDirEnv)
	}
	s := scope{dir: dir}
	readJSON(t, filepath.Join(dir, "host-env.json"), &s.host)
	readJSON(t, filepath.Join(dir, "endpoints.json"), &s.workload)
	var state struct{ Image string }
	readJSON(t, filepath.Join(dir, "state.json"), &state)
	s.image = state.Image
	if s.host["DEV_STACK"] != "services" {
		t.Fatalf("%s is a %q scope; the runtime acceptance needs DEV_STACK=services", dir, s.host["DEV_STACK"])
	}
	if s.host["DEV_PROVIDER"] != "podman" {
		t.Skip("the Podman runtime needs DEV_PROVIDER=podman")
	}
	if s.host["KUBECTL"] != "" || s.host["KODAMA_TEST_KUBECONFIG"] != "" {
		t.Fatal("a services scope must not name a kubeconfig")
	}
	if s.image == "" || s.workload["KODAMA_PODMAN_NETWORK"] == "" || s.workload["KODAMA_SERVER_CALLBACK_HOST"] == "" {
		t.Fatalf("incomplete endpoint files in %s", dir)
	}
	if s.host["KODAMA_TEST_PODMAN_NETWORK"] != s.workload["KODAMA_PODMAN_NETWORK"] ||
		s.host["KODAMA_TEST_SERVER_CALLBACK_HOST"] != s.workload["KODAMA_SERVER_CALLBACK_HOST"] {
		t.Fatal("host and workload endpoint files disagree about the network or callback host")
	}
	// The scope registry is plain HTTP, so the runtime's own --pull=missing
	// cannot fetch from it; the harness pulls the pinned image from it once.
	if out, err := exec.Command("podman", "pull", "--quiet", "--tls-verify=false", s.image).CombinedOutput(); err != nil {
		t.Fatalf("pull %s from the scope registry: %v: %s", s.image, err, out)
	}
	for _, name := range []string{"database-runtime-url", "database-migration-url"} {
		s.secrets = append(s.secrets, filepath.Join(s.host["KODAMA_SECRETS_DIR"], name))
	}
	return s
}

// program starts the dependency probe image's own binaries, configured only
// from the endpoint files: the Podman runtime supplies network, limits,
// read-only root and labels, exactly as for a real attempt.
type program struct {
	entrypoint []string
	args       []string
	env        map[string]string
	secrets    []string
}

func (p program) Container(_ context.Context, _ podman.Launch) (podman.Container, error) {
	c := podman.Container{Entrypoint: p.entrypoint, Args: p.args, Tmpfs: []string{"/tmp"}, Env: []string{"HOME=/tmp"}}
	for k, v := range p.env {
		c.Env = append(c.Env, k+"="+v)
	}
	for _, s := range p.secrets {
		c.Mounts = append(c.Mounts, podman.Mount{Source: s, Target: "/secrets/" + filepath.Base(s), ReadOnly: true})
	}
	return c, nil
}

func (s scope) runtime(t *testing.T, p program) *podman.Runtime {
	t.Helper()
	r, err := podman.New(podman.Config{
		Program:     p,
		StateDir:    podmantest.SharedDir(t),
		Network:     s.workload["KODAMA_PODMAN_NETWORK"], // from the endpoint file
		StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// run starts one workload, waits until its logs contain want (failing as soon
// as the runtime observes it failed), checks it joined the scoped network,
// and removes it.
func (s scope) run(t *testing.T, p program, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt := s.runtime(t, p)
	spec := runtimetest.Spec(t, runtimetest.NewAttemptID(t), s.image, "unused: the program decides what runs")
	w, err := rt.Start(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = rt.Remove(context.Background(), w)
		}
	})
	var logs strings.Builder
	deadline := time.Now().Add(2 * time.Minute)
	for {
		logs.Reset()
		if err := rt.Logs(ctx, w, &logs); err != nil {
			t.Fatalf("logs: %v", err)
		}
		if strings.Contains(logs.String(), want) {
			break
		}
		obs, err := rt.Observe(ctx, w)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if obs.Kind == execution.ObservedFailed {
			t.Fatalf("workload failed before reporting %q:\n%s", want, logs.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q:\n%s", want, logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if out, err := exec.Command("podman", "container", "inspect", "--format", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", w.Name).Output(); err != nil ||
		strings.TrimSpace(string(out)) != s.workload["KODAMA_PODMAN_NETWORK"] {
		t.Fatalf("workload networks = %q (%v), want only %q", out, err, s.workload["KODAMA_PODMAN_NETWORK"])
	}
	if err := rt.Remove(ctx, w); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removed = true
	if out, err := exec.Command("podman", "ps", "-aq", "--filter", "name="+w.Name).Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("workload container left behind after Remove: %q (%v)", out, err)
	}
	return logs.String()
}

// TestWorkloadReachesPostgreSQLByWorkloadURL starts the dependency probe's
// PostgreSQL check as a Podman-runtime workload. It connects with the
// database URLs in the scope's fixtures, whose host is the container name the
// endpoint files advertise.
func TestWorkloadReachesPostgreSQLByWorkloadURL(t *testing.T) {
	podmantest.Require(t)
	s := load(t)
	postgresURL, err := url.Parse(s.workload["KODAMA_POSTGRES_URL"])
	if err != nil || postgresURL.Hostname() == "" || postgresURL.Hostname() == "127.0.0.1" || postgresURL.Hostname() == "localhost" {
		t.Fatalf("KODAMA_POSTGRES_URL %q is not a workload URL (%v)", s.workload["KODAMA_POSTGRES_URL"], err)
	}
	for _, path := range s.secrets {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "@"+postgresURL.Host+"/") {
			t.Fatalf("%s does not address %s (%v)", filepath.Base(path), postgresURL.Host, err)
		}
	}
	env := map[string]string{}
	for k, v := range s.workload {
		env[k] = v
	}
	env["KODAMA_PROBE_CHECKS"] = "postgres"
	env["KODAMA_SERVER_URL"] = "unused"
	logs := s.run(t, program{entrypoint: []string{"/usr/local/bin/probe"}, env: env, secrets: s.secrets}, "PASS postgres")
	t.Logf("workload reached %s: %s", postgresURL.Host, strings.TrimSpace(logs))
}

// TestWorkloadReachesTheHostCallback listens on the host the way the server
// under test does and has a Podman-runtime workload call it through the
// advertised callback host name.
func TestWorkloadReachesTheHostCallback(t *testing.T) {
	podmantest.Require(t)
	s := load(t)
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	nonce := fmt.Sprintf("callback-%d", time.Now().UnixNano())
	var hits atomic.Int32
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprint(w, nonce)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	address := "http://" + net.JoinHostPort(s.workload["KODAMA_SERVER_CALLBACK_HOST"], port) + "/callback"
	p := program{entrypoint: []string{"/bin/sh", "-c"}, args: []string{`wget -q -T 10 -O - "$CALLBACK_URL"`}, env: map[string]string{"CALLBACK_URL": address}}
	logs := s.run(t, p, nonce)
	if hits.Load() == 0 {
		t.Fatalf("the host server never saw the callback although the workload printed %q", logs)
	}
	t.Logf("workload called %s", address)
}
