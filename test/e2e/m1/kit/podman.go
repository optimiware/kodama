package kit

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
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

// forwardOriginEnv carries the workload-resolvable object-store origin to the
// child process that runs the tests.
const forwardOriginEnv = "KODAMA_M1_S3_ORIGIN"

// runInChild hosts the object-store forward and re-runs this test binary with
// the proxy in its environment, returning its exit status.
func runInChild() int {
	fwd, err := startS3Forward()
	if err != nil {
		fmt.Fprintln(os.Stderr, "m1:", err)
		return 1
	}
	defer fwd.srv.Close()
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), forwardOriginEnv+"="+fwd.Origin, "HTTP_PROXY="+fwd.proxyURL, "http_proxy="+fwd.proxyURL)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "m1:", err)
		return 1
	}
	return 0
}

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
	if origin := os.Getenv(forwardOriginEnv); origin == "" {
		// The first process only hosts the object-store forward and runs the tests in a
		// child whose environment names it: net/http reads the proxy settings once, at its
		// first use, and anything that touches it earlier in this process (a dependency's
		// initialization, the test binary's own startup) would make a later setting a
		// silent no-op, so the server could never resolve the object store's name.
		return runInChild()
	} else {
		s3forward = &S3Forward{Origin: origin}
	}
	probe, _ := http.NewRequest(http.MethodHead, s3forward.Origin+"/", nil)
	if u, err := http.ProxyFromEnvironment(probe); err != nil || u == nil {
		fmt.Fprintf(os.Stderr, "m1: the object-store forward is not in effect for %s (%v)\n", s3forward.Origin, err)
		return 1
	}
	cleanup := containersConfig()
	code := m.Run()
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
// acceptance observes, is applied only under that pressure (or when
// KODAMA_TEST_PODMAN_NO_KEYRING=1) and is reported on stderr.
func containersConfig() func() {
	dir, err := os.MkdirTemp("", "kodama-m1-containers-")
	if err != nil {
		return func() {}
	}
	conf := "[containers]\nhttp_proxy = false\n"
	if os.Getenv("KODAMA_TEST_PODMAN_NO_KEYRING") == "1" || keyringNearlyFull() {
		conf += "keyring = false\n"
		fmt.Fprintln(os.Stderr, "m1: the kernel key quota is nearly spent; containers are created without a session keyring")
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
}

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
