// Behavioural specification of the explicitly selected in-cluster
// service-account configuration of the production Kubernetes Cluster driver.
//
// This file belongs to the same external test package as cluster_test.go and,
// like it, pins the driver API.
//
// In-cluster API
//
//	type InClusterConfig struct {
//	    // Server is the HTTPS Kubernetes API endpoint. Required.
//	    Server string
//	    // TokenFile is the projected service-account token path. Empty uses
//	    // DefaultServiceAccountTokenFile.
//	    TokenFile string
//	    // CAFile is the service-account CA bundle path. Empty uses
//	    // DefaultServiceAccountCAFile.
//	    CAFile string
//	    // InsecureSkipTLSVerify is accepted only as false; an in-cluster
//	    // connection is never unverified.
//	    InsecureSkipTLSVerify bool
//	}
//
//	type ClusterConfig struct {
//	    Kubeconfig []byte
//	    Context string
//	    // InCluster explicitly selects in-cluster service-account
//	    // authentication. It is mutually exclusive with Kubeconfig.
//	    InCluster *InClusterConfig
//	    MaxResponseBytes int64
//	    Timeout time.Duration
//	}
//
//	const DefaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
//	const DefaultServiceAccountCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
//
//	// Close releases the transport the driver owns. It is idempotent and does
//	// not abandon a connection in active use.
//	func (c *Cluster) Close() error
//
// Invariants pinned here:
//
//   - in-cluster is selected explicitly: a kubeconfig alone never falls back to
//     the conventional service-account paths, and mixing the two is refused;
//   - only https is accepted, the CA bundle is mandatory and verification is
//     never skipped; the token and CA must be present and usable before the
//     client is built;
//   - the token file is re-read on every request, including a projected-volume
//     symlink rotation; an empty or missing rotated token sends no request;
//   - trust and credential confinement: a wrong CA, a wrong SAN, a redirect and
//     an environment proxy never receive the bearer token;
//   - cancellation and transport loss stay ambiguous, never ErrNotFound and
//     never a stop proof;
//   - Close releases idle connections and is idempotent.
package kube_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestInClusterConfigurationRequiresHTTPSCAAndToken proves an in-cluster
// selection is explicit, https-only, CA-verified and credential-bearing, and
// that an unusable selection is refused before any network call.
func TestInClusterConfigurationRequiresHTTPSCAAndToken(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {})
	caPath := writeCAFile(t, api.caPEM(t))
	tokenPath := writeTokenFile(t, "tok-incluster\n")
	kubeconfig := api.kubeconfig(t, nil, nil)

	valid := func() *driverkube.InClusterConfig {
		return &driverkube.InClusterConfig{
			Server:    api.server.URL,
			TokenFile: tokenPath,
			CAFile:    caPath,
		}
	}

	missingCA := filepath.Join(t.TempDir(), "absent-ca.crt")
	emptyCA := filepath.Join(t.TempDir(), "empty-ca.crt")
	if err := os.WriteFile(emptyCA, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatalf("write empty CA fixture: %v", err)
	}
	invalidCA := filepath.Join(t.TempDir(), "invalid-ca.crt")
	if err := os.WriteFile(invalidCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid CA fixture: %v", err)
	}
	missingToken := filepath.Join(t.TempDir(), "absent-token")
	emptyToken := filepath.Join(t.TempDir(), "empty-token")
	if err := os.WriteFile(emptyToken, []byte(" \n\t\n"), 0o600); err != nil {
		t.Fatalf("write empty token fixture: %v", err)
	}

	httpEndpoint := strings.Replace(api.server.URL, "https://", "http://", 1)

	cases := []struct {
		name   string
		config driverkube.ClusterConfig
	}{
		{
			name:   "no explicit selection",
			config: driverkube.ClusterConfig{},
		},
		{
			name: "mixed kubeconfig and in-cluster",
			config: driverkube.ClusterConfig{
				Kubeconfig: kubeconfig,
				InCluster:  valid(),
			},
		},
		{
			name: "http endpoint",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: httpEndpoint, TokenFile: tokenPath, CAFile: caPath,
			}},
		},
		{
			name: "empty endpoint",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				TokenFile: tokenPath, CAFile: caPath,
			}},
		},
		{
			name: "unparsable endpoint",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: "://not-a-url", TokenFile: tokenPath, CAFile: caPath,
			}},
		},
		{
			name: "insecure TLS",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: tokenPath, CAFile: caPath,
				InsecureSkipTLSVerify: true,
			}},
		},
		{
			name: "missing CA file",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: tokenPath, CAFile: missingCA,
			}},
		},
		{
			name: "empty CA file",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: tokenPath, CAFile: emptyCA,
			}},
		},
		{
			name: "invalid CA file",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: tokenPath, CAFile: invalidCA,
			}},
		},
		{
			name: "CA path is a directory",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: tokenPath, CAFile: t.TempDir(),
			}},
		},
		{
			name: "missing token file",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: missingToken, CAFile: caPath,
			}},
		},
		{
			name: "empty token file",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: emptyToken, CAFile: caPath,
			}},
		},
		{
			name: "token path is a directory",
			config: driverkube.ClusterConfig{InCluster: &driverkube.InClusterConfig{
				Server: api.server.URL, TokenFile: t.TempDir(), CAFile: caPath,
			}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := testCase.config
			config.Timeout = testTimeout
			cluster, err := driverkube.NewCluster(config)
			if err == nil {
				_ = cluster.Close()
				t.Fatalf("NewCluster = %v; want an invalid-input refusal", cluster)
			}
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}

	if calls := api.callCount(); calls != 0 {
		t.Fatalf("invalid in-cluster configurations made %d API calls; want 0", calls)
	}
}

// TestInClusterLimitsAreEnforced proves an explicitly selected in-cluster
// configuration is bounded exactly like the kubeconfig path: a negative
// response bound is refused before any request, an oversized response is
// ambiguous, and the configured request timeout actually bounds a call.
func TestInClusterLimitsAreEnforced(t *testing.T) {
	t.Run("negative response bound", func(t *testing.T) {
		api := newAPI(t, func(http.ResponseWriter, *http.Request) {})
		cluster, err := driverkube.NewCluster(driverkube.ClusterConfig{
			InCluster: &driverkube.InClusterConfig{
				Server:    api.server.URL,
				TokenFile: writeTokenFile(t, "tok-negative-bound"),
				CAFile:    writeCAFile(t, api.caPEM(t)),
			},
			MaxResponseBytes: -1,
			Timeout:          testTimeout,
		})
		if err == nil {
			_ = cluster.Close()
			t.Fatal("NewCluster with a negative response bound = nil; want an invalid-input refusal")
		}
		assertFaultClass(t, err, fault.InvalidInput)
		if calls := api.callCount(); calls != 0 {
			t.Fatalf("API calls = %d; want 0", calls)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		body := `{"metadata":{"name":"` + strings.Repeat("a", 128) + `"}}`
		api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		})
		cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), writeTokenFile(t, "tok-oversized"), func(config *driverkube.ClusterConfig) {
			config.MaxResponseBytes = 64
		})

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous for a response over the configured bound", err)
		}
		if calls := api.callCount(); calls != 1 {
			t.Fatalf("API calls = %d; want 1", calls)
		}
	})

	t.Run("configured timeout", func(t *testing.T) {
		gate := newGate()
		release := make(chan struct{})
		var once sync.Once
		api := newAPI(t, func(_ http.ResponseWriter, r *http.Request) {
			gate.entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), writeTokenFile(t, "tok-timeout"), func(config *driverkube.ClusterConfig) {
			config.Timeout = 250 * time.Millisecond
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := cluster.GetNamespace(ctx, testNamespace)
			result <- err
		}()
		gate.waitEntered(t)

		select {
		case err := <-result:
			if !errors.Is(err, driverkube.ErrAmbiguous) {
				t.Fatalf("error = %v; want ErrAmbiguous for a timeout", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v; want a context.DeadlineExceeded cause", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("the configured request timeout did not bound the hanging call")
		}
	})
}

// TestInClusterDefaultsOnlyAfterExplicitSelection proves the conventional
// service-account paths are consulted only for an explicitly selected
// in-cluster configuration: with valid default files present, an empty
// InCluster file set builds and authenticates, while nothing selected and a
// credential-less kubeconfig still refuse without any request. The seam
// replaces the host-mounted paths with isolated temporary files, so the result
// does not depend on what the host mounts.
func TestInClusterDefaultsOnlyAfterExplicitSelection(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	driverkube.SetServiceAccountDefaultsForTest(t,
		writeTokenFile(t, "tok-seamed-default\n"),
		writeCAFile(t, api.caPEM(t)),
	)

	if driverkube.DefaultServiceAccountTokenFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatalf("DefaultServiceAccountTokenFile = %q; want the conventional projected token path",
			driverkube.DefaultServiceAccountTokenFile)
	}
	if driverkube.DefaultServiceAccountCAFile != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" {
		t.Fatalf("DefaultServiceAccountCAFile = %q; want the conventional service-account CA path",
			driverkube.DefaultServiceAccountCAFile)
	}

	t.Run("explicit selection uses the conventional defaults", func(t *testing.T) {
		cluster, err := driverkube.NewCluster(driverkube.ClusterConfig{
			InCluster: &driverkube.InClusterConfig{Server: api.server.URL},
			Timeout:   testTimeout,
		})
		if err != nil {
			t.Fatalf("NewCluster with empty in-cluster file fields: %v", err)
		}
		t.Cleanup(func() { _ = cluster.Close() })

		if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
			t.Fatalf("GetNamespace with the conventional token and CA: %v", err)
		}
		call := api.lastCall(t)
		if got := call.Header.Get("Authorization"); got != "Bearer tok-seamed-default" {
			t.Fatalf("Authorization = %q; want the conventional token", got)
		}
	})

	t.Run("no selection never falls back to the defaults", func(t *testing.T) {
		before := api.callCount()
		cases := []struct {
			name   string
			config driverkube.ClusterConfig
		}{
			{
				name:   "nothing selected",
				config: driverkube.ClusterConfig{Timeout: testTimeout},
			},
			{
				name: "kubeconfig without a credential",
				config: driverkube.ClusterConfig{
					Kubeconfig: api.kubeconfig(t, map[string]any{}, nil),
					Timeout:    testTimeout,
				},
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				cluster, err := driverkube.NewCluster(testCase.config)
				if err == nil {
					_ = cluster.Close()
					t.Fatal("NewCluster = nil; want an invalid-input refusal instead of an implicit service-account fallback")
				}
				assertFaultClass(t, err, fault.InvalidInput)
			})
		}
		if calls := api.callCount(); calls != before {
			t.Fatalf("API calls = %d; want %d (no request for an unselected configuration)", calls, before)
		}
	})
}

// TestInClusterProjectedTokenRotation proves the token file is re-read for
// every request through the projected-volume symlink layout, so a rotated
// service-account token is used on the next request. It also pins that the
// driver does not apply the keyfile provider's symlink prohibition: the token
// path itself is a symlink.
func TestInClusterProjectedTokenRotation(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	tokens := newProjectedToken(t, "tok-projected-one")
	info, err := os.Lstat(tokens.path)
	if err != nil {
		t.Fatalf("Lstat projected token: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture token %s is not a symlink; the projected-volume rotation would not be exercised", tokens.path)
	}
	cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), tokens.path, nil)

	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("first GetNamespace: %v", err)
	}
	tokens.rotate(t, "tok-projected-two")
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("second GetNamespace after projected rotation: %v", err)
	}

	calls := api.snapshot()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d; want 2", len(calls))
	}
	if got := calls[0].Header.Get("Authorization"); got != "Bearer tok-projected-one" {
		t.Fatalf("first Authorization = %q; want the original projected token", got)
	}
	if got := calls[1].Header.Get("Authorization"); got != "Bearer tok-projected-two" {
		t.Fatalf("second Authorization = %q; want the rotated projected token", got)
	}
}

// TestInClusterMissingTokenFailsClosed proves an empty or missing rotated token
// fails closed before any request: no request is sent, no stale credential is
// reused, no credential value is rendered, and a later valid rotation recovers.
func TestInClusterMissingTokenFailsClosed(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	tokens := newProjectedToken(t, "tok-fail-closed")
	cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), tokens.path, nil)

	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("first GetNamespace: %v", err)
	}

	tokens.rotate(t, " \n\t\n")
	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if err == nil {
		t.Fatal("GetNamespace with an empty rotated token = nil; want a fail-closed refusal")
	}
	assertFaultClass(t, err, fault.DependencyUnavailable)
	assertNoCredential(t, err, "tok-fail-closed")
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls after an empty token = %d; want only the first request", calls)
	}

	tokens.rotateWithoutToken(t)
	_, err = cluster.GetNamespace(context.Background(), testNamespace)
	if err == nil {
		t.Fatal("GetNamespace with a missing rotated token = nil; want a fail-closed refusal")
	}
	assertFaultClass(t, err, fault.DependencyUnavailable)
	assertNoCredential(t, err, "tok-fail-closed")
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls after a missing token = %d; want only the first request", calls)
	}

	tokens.rotate(t, "tok-recovered\n")
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("GetNamespace after a valid rotation: %v", err)
	}
	calls := api.snapshot()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d; want 2", len(calls))
	}
	if got := calls[1].Header.Get("Authorization"); got != "Bearer tok-recovered" {
		t.Fatalf("recovered Authorization = %q; want the rotated token", got)
	}
}

// TestInClusterTrustAndCredentialConfinement proves an in-cluster client never
// discloses the service-account token to a party the API server trust boundary
// does not cover: a wrong CA, a wrong SAN, a redirect target or an environment
// proxy.
func TestInClusterTrustAndCredentialConfinement(t *testing.T) {
	t.Run("untrusted certificate authority", func(t *testing.T) {
		const token = "tok-untrusted-ca"
		api := newAPI(t, func(http.ResponseWriter, *http.Request) {
			t.Error("request reached a server the configured CA does not sign")
		})
		_, _, unrelatedCA := clientCertificate(t)
		cluster := newInClusterClient(t, api.server.URL, unrelatedCA, writeTokenFile(t, token), nil)

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		assertNoCredential(t, err, token)
		if calls := api.callCount(); calls != 0 {
			t.Fatalf("API calls = %d; want 0", calls)
		}
	})

	t.Run("wrong server name", func(t *testing.T) {
		const token = "tok-wrong-san"
		certificate, caPEM := serverCertificate(t, []string{"api.kodama.test"}, nil)
		api := newAPIWithTLS(t, func(http.ResponseWriter, *http.Request) {
			t.Error("request reached a server whose certificate does not cover the endpoint")
		}, &tls.Config{Certificates: []tls.Certificate{certificate}})
		cluster := newInClusterClient(t, api.server.URL, caPEM, writeTokenFile(t, token), nil)

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		assertNoCredential(t, err, token)
		if calls := api.callCount(); calls != 0 {
			t.Fatalf("API calls = %d; want 0", calls)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		const token = "tok-redirect"
		target := newAPI(t, func(http.ResponseWriter, *http.Request) {
			t.Error("redirect target received a request")
		})
		redirector := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.server.URL+"/api/v1/namespaces/"+testNamespace, http.StatusFound)
		})
		cluster := newInClusterClient(t, redirector.server.URL, redirector.caPEM(t), writeTokenFile(t, token), nil)

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		assertNoCredential(t, err, token)
		if calls := target.callCount(); calls != 0 {
			t.Fatalf("redirect target calls = %d; want 0", calls)
		}
		if calls := redirector.callCount(); calls != 1 {
			t.Fatalf("redirector calls = %d; want 1", calls)
		}
	})

	t.Run("environment proxy", func(t *testing.T) {
		// http.ProxyFromEnvironment caches the environment on its first call
		// for the life of the process, so a repeated -count run in one process
		// would keep dialing the first run's (now closed) proxy. Run exactly
		// this subtest in a fresh process; the child creates its proxy, sets
		// the environment and warms the cache before the first lookup.
		if os.Getenv(kubeProxyChildEnv) != "1" {
			runEnvironmentProxyChild(t)
			return
		}

		const token = "tok-proxy-confinement"
		proxy := newProxyRecorder(t)
		t.Setenv("HTTPS_PROXY", proxy.server.URL)
		t.Setenv("HTTP_PROXY", proxy.server.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")

		// Warm the process-wide proxy environment cache with the proxy address,
		// so the in-cluster request below is a real test of the driver's
		// Proxy=nil transport rather than a cache miss.
		control := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
		response, err := control.Get("http://proxy-probe.kodama.invalid/")
		if err != nil {
			t.Fatalf("control request through the proxy: %v", err)
		}
		_ = response.Body.Close()
		if proxy.count() == 0 {
			t.Fatal("control request did not reach the proxy; the environment is not being honored")
		}
		before := proxy.count()

		ip := nonLoopbackIPv4(t)
		certificate, caPEM := serverCertificate(t, nil, []net.IP{ip})
		listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
		if err != nil {
			t.Skipf("cannot bind a non-loopback API listener on %s: %v", ip, err)
		}
		api := newAPIWithListener(t, listener, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
		}, &tls.Config{Certificates: []tls.Certificate{certificate}})
		cluster := newInClusterClient(t, api.server.URL, caPEM, writeTokenFile(t, token), nil)

		if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
			t.Fatalf("direct in-cluster request: %v", err)
		}
		if calls := api.callCount(); calls != 1 {
			t.Fatalf("direct API calls = %d; want 1", calls)
		}
		if got := proxy.count(); got != before {
			t.Fatalf("the driver used the environment proxy: proxy calls %d -> %d", before, got)
		}
		for _, call := range proxy.snapshot() {
			if strings.Contains(call, token) {
				t.Fatalf("the proxy observed the bearer token in %q", call)
			}
		}
	})
}

// kubeProxyChildEnv marks the isolated child process that runs the environment
// proxy subtest. It also stops that child from spawning another child.
const kubeProxyChildEnv = "KODAMA_KUBE_PROXY_TEST_CHILD"

// environmentProxySubtest is exactly the subtest the child executes.
const environmentProxySubtest = "^TestInClusterTrustAndCredentialConfinement$/^environment_proxy$"

// runEnvironmentProxyChild runs the environment-proxy confinement subtest in a
// fresh test process and propagates its verdict with the child's diagnostics.
// The child is bounded and joined before this process continues, so every
// parent repetition gets a cache warmed by its own proxy address.
func runEnvironmentProxyChild(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable,
		"-test.run", environmentProxySubtest,
		"-test.count=1",
		"-test.v",
	)
	child.Env = append(os.Environ(), kubeProxyChildEnv+"=1")
	output, waitErr := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("environment proxy child exceeded its bound: %v\n%s", ctx.Err(), output)
	}
	if waitErr != nil {
		t.Fatalf("environment proxy child failed: %v\n%s", waitErr, output)
	}
	const passLine = "--- PASS: TestInClusterTrustAndCredentialConfinement/environment_proxy"
	if !strings.Contains(string(output), passLine) {
		const skipLine = "--- SKIP: TestInClusterTrustAndCredentialConfinement/environment_proxy"
		if strings.Contains(string(output), skipLine) {
			t.Skipf("environment proxy child skipped:\n%s", output)
		}
		t.Fatalf("environment proxy child produced no pass line:\n%s", output)
	}
}

// TestInClusterFailuresRemainAmbiguous proves cancellation, transport loss and
// a server-side failure are never reported as an absent object or a conflict:
// the outcome stays ambiguous and the credential is never rendered.
func TestInClusterFailuresRemainAmbiguous(t *testing.T) {
	assertNoStopProof := func(t *testing.T, err error) {
		t.Helper()
		for _, sentinel := range []error{runtimekube.ErrNotFound, runtimekube.ErrAlreadyExists, runtimekube.ErrConflict} {
			if errors.Is(err, sentinel) {
				t.Fatalf("error %v matched sentinel %v; it must stay ambiguous", err, sentinel)
			}
		}
	}

	t.Run("cancellation", func(t *testing.T) {
		const token = "tok-cancelled"
		gate := newGate()
		observed := make(chan struct{})
		api := newAPI(t, func(_ http.ResponseWriter, r *http.Request) {
			gate.entered <- struct{}{}
			<-r.Context().Done()
			close(observed)
		})
		cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), writeTokenFile(t, token), nil)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := cluster.GetNamespace(ctx, testNamespace)
			result <- err
		}()
		gate.waitEntered(t)
		cancel()

		err := <-result
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v; want a cancellation cause", err)
		}
		assertNoStopProof(t, err)
		assertNoCredential(t, err, token)
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			t.Fatal("server never observed the cancelled request")
		}
	})

	t.Run("transport loss", func(t *testing.T) {
		const token = "tok-transport-loss"
		api := newAPI(t, func(http.ResponseWriter, *http.Request) {})
		cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), writeTokenFile(t, token), nil)
		api.server.Close()

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		assertNoStopProof(t, err)
		assertNoCredential(t, err, token)
	})

	t.Run("server failure", func(t *testing.T) {
		const token = "tok-server-failure"
		api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		cluster := newInClusterClient(t, api.server.URL, api.caPEM(t), writeTokenFile(t, token), nil)

		_, err := cluster.GetNamespace(context.Background(), testNamespace)
		if !errors.Is(err, driverkube.ErrAmbiguous) {
			t.Fatalf("error = %v; want ErrAmbiguous", err)
		}
		assertNoStopProof(t, err)
		assertNoCredential(t, err, token)
	})
}

// TestInClusterOwnsTransportCleanup proves the driver owns its transport: Close
// releases the idle API connection a completed request left behind, and a
// second Close is harmless. A composition can therefore stop the driver without
// leaking a keep-alive connection.
func TestInClusterOwnsTransportCleanup(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	cluster, err := driverkube.NewCluster(driverkube.ClusterConfig{
		InCluster: &driverkube.InClusterConfig{
			Server:    server.URL,
			TokenFile: writeTokenFile(t, "tok-cleanup"),
			CAFile:    writeCAFile(t, caPEM),
		},
		Timeout: testTimeout,
	})
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}

	if err := cluster.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the driver's idle API connection")
	}
	if err := cluster.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// writeCAFile writes one CA bundle to an isolated temporary file.
func writeCAFile(t *testing.T, pemData []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return path
}

// writeTokenFile writes one bearer token to an isolated temporary file.
func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

// newInClusterClient builds an explicitly selected in-cluster driver. mutate may
// replace driver configuration fields.
func newInClusterClient(
	t *testing.T,
	serverURL string,
	caPEM []byte,
	tokenPath string,
	mutate func(*driverkube.ClusterConfig),
) *driverkube.Cluster {
	t.Helper()
	config := driverkube.ClusterConfig{
		InCluster: &driverkube.InClusterConfig{
			Server:    serverURL,
			TokenFile: tokenPath,
			CAFile:    writeCAFile(t, caPEM),
		},
		Timeout: testTimeout,
	}
	if mutate != nil {
		mutate(&config)
	}
	cluster, err := driverkube.NewCluster(config)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Close() })
	return cluster
}

// projectedToken is a deterministic stand-in for a Kubernetes projected
// service-account volume: token is a symlink to ..data/token and ..data is a
// symlink to one timestamped directory. A rotation atomically replaces the
// ..data symlink, exactly as the kubelet does.
type projectedToken struct {
	root     string
	path     string
	sequence int
}

// newProjectedToken creates the initial projected layout with value.
func newProjectedToken(t *testing.T, value string) *projectedToken {
	t.Helper()
	root := t.TempDir()
	fixture := &projectedToken{root: root, path: filepath.Join(root, "token")}
	fixture.rotate(t, value)
	if err := os.Symlink("..data/token", fixture.path); err != nil {
		t.Fatalf("symlink projected token: %v", err)
	}
	return fixture
}

// rotate publishes value in a fresh timestamped directory and swaps ..data.
func (p *projectedToken) rotate(t *testing.T, value string) {
	t.Helper()
	p.sequence++
	directory := filepath.Join(p.root, fmt.Sprintf("..%04d", p.sequence))
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create projected token directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "token"), []byte(value), 0o600); err != nil {
		t.Fatalf("write projected token: %v", err)
	}
	p.publish(t, directory)
}

// rotateWithoutToken publishes a fresh directory that has no token file, which
// is how a projected volume appears while the credential is being replaced.
func (p *projectedToken) rotateWithoutToken(t *testing.T) {
	t.Helper()
	p.sequence++
	directory := filepath.Join(p.root, fmt.Sprintf("..%04d", p.sequence))
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create empty projected token directory: %v", err)
	}
	p.publish(t, directory)
}

// publish atomically replaces the ..data symlink with one pointing at directory.
func (p *projectedToken) publish(t *testing.T, directory string) {
	t.Helper()
	staged := filepath.Join(p.root, fmt.Sprintf("..data.tmp-%d", p.sequence))
	if err := os.Symlink(filepath.Base(directory), staged); err != nil {
		t.Fatalf("stage ..data symlink: %v", err)
	}
	if err := os.Rename(staged, filepath.Join(p.root, "..data")); err != nil {
		t.Fatalf("publish ..data symlink: %v", err)
	}
}

// serverCertificate builds a self-signed server certificate for the supplied
// names and returns it with the PEM the client can trust as its CA.
func serverCertificate(t *testing.T, dnsNames []string, ips []net.IP) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-test-api"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certificate, caPEM
}

// proxyRecorder is an HTTP proxy that records every request it receives. It
// never forwards anything: a request through it is a test failure, not a
// successful call.
type proxyRecorder struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
}

// newProxyRecorder starts one recording proxy.
func newProxyRecorder(t *testing.T) *proxyRecorder {
	t.Helper()
	recorder := &proxyRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.calls = append(recorder.calls, r.Method+" "+r.URL.String())
		recorder.mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

// count reports how many requests reached the proxy.
func (p *proxyRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// snapshot returns a copy of every recorded proxied request.
func (p *proxyRecorder) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// nonLoopbackIPv4 returns one assigned non-loopback IPv4 address. Go's
// ProxyFromEnvironment always bypasses a loopback destination, so a meaningful
// proxy-confinement test needs a non-loopback API endpoint.
func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addresses: %v", err)
	}
	for _, addr := range addrs {
		network, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := network.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		return ip
	}
	t.Skip("no non-loopback IPv4 address; the environment proxy cannot be distinguished from the loopback bypass")
	return nil
}
