package podman_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The Podman test double is the plain binary built from ./fakepodman, once per
// test run: a test binary built with the race detector is too slow to start for
// each of the hundreds of commands the contract issues.
var fakeBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kodama-fakepodman-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fakeBinary = filepath.Join(dir, "fakepodman")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", fakeBinary, "github.com/optimiware/kodama/internal/runtime/podman/fakepodman")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the podman double: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// createdContainer is what the double records of one `podman create`.
type createdContainer struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	ExitCode   int               `json:"exitCode"`
	Image      string            `json:"image"`
	Labels     map[string]string `json:"labels"`
	Volumes    []string          `json:"volumes"`
	Env        []string          `json:"env"`
	Tmpfs      []string          `json:"tmpfs"`
	Entrypoint string            `json:"entrypoint"`
	Args       []string          `json:"args"`
	User       string            `json:"user"`
	Networks   []string          `json:"networks"`
	Aliases    []string          `json:"aliases"`
	CapAdd     []string          `json:"capAdd"`
	// Flags holds every flag that carries no separate value (--read-only,
	// --cap-drop=ALL, ...); Argv is the whole create command after "create".
	Flags []string `json:"flags"`
	Argv  []string `json:"argv"`
	// Foreign marks a container planted by a test, not created through `create`.
	Foreign bool `json:"foreign,omitempty"`
}

type fakeVolume struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

type fakeNetwork struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Labels   map[string]string `json:"labels"`
	Internal bool              `json:"internal"`
}

// fakeEnv is a Podman double and its state directory.
type fakeEnv struct {
	// Binary is the path of the `podman` executable.
	Binary string
	dir    string
}

func newFake(t *testing.T) fakeEnv {
	t.Helper()
	exe := fakeBinary
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	script := fmt.Sprintf("#!/bin/sh\nKODAMA_FAKE_PODMAN=%q exec %q \"$@\"\n", dir, exe)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeEnv{Binary: bin, dir: dir}
}

// ctl runs a test-control command of the double (see fakePodman).
func (f fakeEnv) ctl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(f.Binary, args...).Output()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// set writes a control file of the double (exec-code, probe-code).
func (f fakeEnv) set(t *testing.T, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// outage makes every command of the double fail as an unreachable engine; the
// returned restore ends it.
func (f fakeEnv) outage(t *testing.T) (restore func()) {
	t.Helper()
	f.set(t, "down", "1")
	return func() { _ = os.Remove(filepath.Join(f.dir, "down")) }
}

// argvs returns every invocation, in order.
func (f fakeEnv) argvs(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "argv.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var a []string
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// calls returns the subcommand of every invocation, in order.
func (f fakeEnv) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// created returns the recorded containers.
func (f fakeEnv) created(t *testing.T) []createdContainer {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(f.dir, "containers"))
	var out []createdContainer
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(f.dir, "containers", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var c createdContainer
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// volumes and networks return the recorded objects by name.
func (f fakeEnv) volumes(t *testing.T) map[string]fakeVolume {
	t.Helper()
	out := map[string]fakeVolume{}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "volumes"))
	for _, e := range entries {
		var v fakeVolume
		raw, err := os.ReadFile(filepath.Join(f.dir, "volumes", e.Name()))
		if err != nil || json.Unmarshal(raw, &v) != nil {
			t.Fatal(err)
		}
		out[v.Name] = v
	}
	return out
}

func (f fakeEnv) networks(t *testing.T) map[string]fakeNetwork {
	t.Helper()
	out := map[string]fakeNetwork{}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "networks"))
	for _, e := range entries {
		var n fakeNetwork
		raw, err := os.ReadFile(filepath.Join(f.dir, "networks", e.Name()))
		if err != nil || json.Unmarshal(raw, &n) != nil {
			t.Fatal(err)
		}
		out[n.Name] = n
	}
	return out
}

// testCA returns a self-signed certificate as a PEM bundle.
func testCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "git-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
