package kit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	podmantest "github.com/optimiware/kodama/test/support/podman"
)

// PodmanImage is the pinned image the steps run in.
const PodmanImage = podmantest.Image

// Main runs a package's tests with the services the acceptance names required
// (a missing service fails instead of skipping) and removes what the process
// built. Call it from TestMain.
func Main(m *testing.M) int {
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", Prefix)
	}
	for _, k := range []string{"KODAMA_REQUIRE_POSTGRES", "KODAMA_REQUIRE_S3", "KODAMA_REQUIRE_PODMAN"} {
		os.Setenv(k, "1")
	}
	cleanup := containersConfig()
	flag.Parse()
	var warm sync.WaitGroup
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() == "" {
		// The whole suite runs: build the tools image and create the kind cluster
		// while the tests that need neither run. A filtered run builds on demand.
		warm.Add(2)
		go func() { defer warm.Done(); prepareTools() }()
		go func() { defer warm.Done(); prepareCluster() }()
	}
	code := m.Run()
	warm.Wait()
	// The cleanups run Podman and kind, which need this process's containers
	// configuration: remove it last.
	runCleanups()
	cleanup()
	if entryDir != "" {
		os.RemoveAll(entryDir)
	}
	return code
}

// containersConfig writes the Podman configuration override of this process:
// the object-store forward is set as HTTP_PROXY for the server, and Podman must
// not copy it into the workloads (their object store is reached directly on the
// workload network). When the user's kernel key quota is nearly spent it also
// creates containers without a session keyring: every running container holds
// one key of a per-user quota (kernel.keys.maxkeys, 200 by default), so a host
// that already runs many scopes refuses the next container ("create keyring:
// Disk quota exceeded") whatever the test does. That setting changes nothing the
// acceptance observes; this kit applies it always (KODAMA_TEST_PODMAN_KEYRING=1
// keeps the default).
func containersConfig() func() {
	dir, err := os.MkdirTemp("", "kodama-m1-containers-")
	if err != nil {
		return func() {}
	}
	conf := "[containers]\nhttp_proxy = false\n"
	// The M2 suite always creates its containers (workloads, dependencies it
	// starts, the kind node) without a session keyring: it runs dozens beside other
	// scopes and tasks on a shared host, and the quota can fill between this check
	// and the next container, so a start-time threshold is not enough.
	if os.Getenv("KODAMA_TEST_PODMAN_KEYRING") != "1" {
		conf += "keyring = false\n"
		if keyringNearlyFull() {
			fmt.Fprintln(os.Stderr, "m2: the kernel key quota is nearly spent; containers are created without a session keyring")
		}
	}
	path := filepath.Join(dir, "containers.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		os.RemoveAll(dir)
		return func() {}
	}
	os.Setenv("CONTAINERS_CONF_OVERRIDE", path)
	return func() { os.Unsetenv("CONTAINERS_CONF_OVERRIDE"); os.RemoveAll(dir) }
}

// keyringNearlyFull reports whether the current user holds almost every key of
// its quota, from /proc/key-users.
func keyringNearlyFull() bool {
	data, err := os.ReadFile("/proc/key-users")
	if err != nil {
		return false
	}
	prefix := fmt.Sprintf("%d:", os.Getuid())
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != prefix {
			continue
		}
		var used, max int
		if _, err := fmt.Sscanf(f[3], "%d/%d", &used, &max); err != nil || max == 0 {
			return false
		}
		return max-used < 40
	}
	return false
}

func requirePodman(t testing.TB) { podmantest.Require(t) }

func podmanSharedDir(t testing.TB) string { return podmantest.SharedDir(t) }

var (
	entryOnce sync.Once
	entryDir  string
	entryBin  string
	entryErr  error
)

// EntrypointBinary builds the real `kodama` (cmd/kodama, which serves
// `kodama step`) as a static Linux binary for the Podman machine's
// architecture, once per test process.
func EntrypointBinary(t testing.TB) string {
	t.Helper()
	entryOnce.Do(func() {
		root := podmantestRoot()
		if err := os.MkdirAll(root, 0o700); err != nil {
			entryErr = err
			return
		}
		entryDir, entryErr = os.MkdirTemp(root, "kodama-m1-bin-")
		if entryErr != nil {
			return
		}
		entryBin = filepath.Join(entryDir, "kodama")
		cmd := exec.Command("go", "build", "-trimpath", "-o", entryBin, "./cmd/kodama")
		cmd.Dir = repoRoot()
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+podmantest.Arch(t))
		if out, err := cmd.CombinedOutput(); err != nil {
			entryErr = fmt.Errorf("building the entrypoint: %v: %s", err, out)
		}
	})
	if entryErr != nil {
		t.Fatal(entryErr)
	}
	return entryBin
}

// podmantestRoot is where the binary is placed: a path the Podman engine can
// bind-mount.
func podmantestRoot() string {
	if root := os.Getenv("KODAMA_TEST_PODMAN_DIR"); root != "" {
		return root
	}
	return os.TempDir()
}

func repoRoot() string {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

// Front is the TLS origin a container reaches the runtime API through. It
// forwards only /runtime/v1/ to the running process: a workload never reaches
// the public API through it. It outlives server restarts, so the origin (the
// runtime tokens' issuer) is stable; while no process is serving it answers 502
// and the entrypoint retries.
type Front struct {
	// URL is the origin as the workload reaches it; CAFile trusts it.
	URL    string
	CAFile string

	srv      *http.Server
	requests atomic.Int64

	faultMu sync.Mutex
	fault   func(method, path string) Fault
}

// Fault is what the front does to one runtime request.
type Fault int

const (
	// FaultNone forwards the request and its answer.
	FaultNone Fault = iota
	// FaultDropRequest loses the request before the server sees it.
	FaultDropRequest
	// FaultDropResponse delivers the request, lets the server commit and answer,
	// and loses the answer: an acknowledgment loss the caller cannot tell from a
	// lost request.
	FaultDropResponse
)

// Faults installs the fault injector (nil removes it). It is consulted for every
// runtime request, with its method and path.
func (f *Front) Faults(fn func(method, path string) Fault) {
	f.faultMu.Lock()
	f.fault = fn
	f.faultMu.Unlock()
}

func (f *Front) faultFor(r *http.Request) Fault {
	f.faultMu.Lock()
	fn := f.fault
	f.faultMu.Unlock()
	if fn == nil {
		return FaultNone
	}
	return fn(r.Method, r.URL.Path)
}

// discard receives and drops an upstream answer.
type discard struct{ h http.Header }

func (d discard) Header() http.Header         { return d.h }
func (d discard) WriteHeader(int)             {}
func (d discard) Write(b []byte) (int, error) { return len(b), nil }
func (d discard) Flush()                      {}

// Requests counts requests the front forwarded.
func (f *Front) Requests() int64 { return f.requests.Load() }

func newFront(t testing.TB, host string, dial func() string) *Front {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey, leafKey := key(), key()
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: Prefix + " runtime front ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: Prefix + " runtime front"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost", host}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	caFile := filepath.Join(t.TempDir(), "front-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &Front{URL: "https://" + net.JoinHostPort(host, port), CAFile: caFile}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: dial()})
			r.Out.Host = r.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the server is not serving", http.StatusBadGateway)
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/runtime/v1/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		switch f.faultFor(r) {
		case FaultDropRequest:
			panic(http.ErrAbortHandler)
		case FaultDropResponse:
			proxy.ServeHTTP(discard{h: http.Header{}}, r) // the server processed and committed it
			panic(http.ErrAbortHandler)
		}
		proxy.ServeHTTP(w, r)
	})
	f.srv = &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = f.srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}
