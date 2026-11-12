// Behavioural specification of the production Kubernetes Cluster driver.
//
// The driver under test lives in this directory and implements the
// internal/shell/runtime/kube.Cluster seam over the Kubernetes REST API using
// only the standard library. These tests pin its public surface.
//
// Driver API
//
//	package kube // import path kodama/internal/shell/app/drivers/kube
//
//	type ClusterConfig struct {
//	    // Kubeconfig is the raw JSON document produced by
//	    //   kubectl config view --raw --minify -o json
//	    Kubeconfig []byte
//	    // Context selects one kubeconfig context. Empty uses current-context.
//	    Context string
//	    // MaxResponseBytes bounds one non-log API response.
//	    // Zero uses DefaultMaxResponseBytes.
//	    MaxResponseBytes int64
//	    // Timeout bounds one API call. Zero uses DefaultRequestTimeout.
//	    // A negative value disables the internal timeout.
//	    Timeout time.Duration
//	}
//
//	func NewCluster(ClusterConfig) (*Cluster, error)
//	type Cluster struct{ ... }
//	var _ runtimekube.Cluster = (*Cluster)(nil)
//
//	// ErrAmbiguous reports an external effect whose outcome cannot be
//	// classified: transport failure, timeout, cancellation, a refused
//	// redirect, an untrusted certificate or an oversized response.
//	var ErrAmbiguous error
//
//	const DefaultMaxResponseBytes int64 = 8 << 20
//	const DefaultRequestTimeout = 30 * time.Second
//
// Error mapping (the adapter above interprets only these sentinels):
//
//	HTTP 404                              -> runtimekube.ErrNotFound
//	HTTP 409 on create                    -> runtimekube.ErrAlreadyExists
//	HTTP 409 on update/patch/delete       -> runtimekube.ErrConflict
//	HTTP 4xx (other than 404/409)         -> fault.DependencyUnavailable
//	HTTP 5xx, 3xx, transport, timeout,
//	  cancellation, TLS failure, oversized -> ErrAmbiguous
//
// No error renders a bearer token, client key, request body or response body.
//
// REST mapping (pinned by rest_test.go):
//
//	namespace                     GET  /api/v1/namespaces/{name}
//	jobs                          /apis/batch/v1/namespaces/{ns}/jobs
//	pods                          /api/v1/namespaces/{ns}/pods?labelSelector=...
//	pod logs                      /api/v1/namespaces/{ns}/pods/{pod}/log
//	                              ?container=workload&follow=false&stream=...
//	configmaps                    /api/v1/namespaces/{ns}/configmaps
//	secrets                       /api/v1/namespaces/{ns}/secrets
//	persistent volume claims      /api/v1/namespaces/{ns}/persistentvolumeclaims
//
// The activation seam carries absolute file paths as ConfigMap keys and as
// volume item paths. Kubernetes requires ConfigMap keys to match
// [-._a-zA-Z0-9]+ and volume item paths to be relative to the mount, so the
// driver encodes every ConfigMap key as base64url(path) and makes every item
// path relative to its volume mount path. The Job container is named
// "workload". Secret data keys are already valid names and are not rewritten.
// ConfigMap bytes travel as binaryData (base64) and Secret bytes as data
// (base64) so arbitrary activation bytes round-trip exactly.
//
// Operation semantics:
//
//	CreateJob             one POST; a 409 is ErrAlreadyExists and is never
//	                      retried or rewritten by the driver.
//	PutConfigMap          create-once: POST; on 409 it GETs the existing object
//	                      and returns it when the binary data matches, else
//	                      ErrConflict. A non-empty ResourceVersion is refused
//	                      (the activation snapshot is immutable).
//	PutSecret             upsert: empty ResourceVersion POSTs, falling back on
//	                      409 to GET + PUT with the observed version; a set
//	                      ResourceVersion is a single conditional PUT and a 409
//	                      is ErrConflict. Bodies carry type Opaque.
//	UpdateClaim           GET then metadata-only merge PATCH guarded by the
//	                      caller's ResourceVersion. Keys present on the server
//	                      but absent from the caller are removed with an
//	                      explicit null; spec, finalizers and the deletion
//	                      timestamp are never sent.
//	DeleteClaimVersioned  DELETE with DeleteOptions.preconditions
//	                      .resourceVersion; a 409 is ErrConflict.
//	ReadPodLogs           ?container=workload&follow=false&stream=...
//	                      then discard Offset bytes and read at most Limit.
//	                      NextOffset = Offset + len(Data) and Truncated reports
//	                      that more bytes remained. A limit outside
//	                      (0, runtimekube.MaxLogBytes] or a negative offset is
//	                      refused before any request.
//	volumeMounts          readOnly is emitted only when true; the writable
//	                      workspace mount omits it. A volume item path outside
//	                      its mount path is refused before any request.
package kube_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestDriverDefaultsArePinned pins the bounded-call and bounded-response
// defaults so a caller can rely on them without inspecting internals.
func TestDriverDefaultsArePinned(t *testing.T) {
	if driverkube.DefaultMaxResponseBytes != 8<<20 {
		t.Fatalf("DefaultMaxResponseBytes = %d; want %d", driverkube.DefaultMaxResponseBytes, int64(8<<20))
	}
	if driverkube.DefaultRequestTimeout != 30*time.Second {
		t.Fatalf("DefaultRequestTimeout = %s; want %s", driverkube.DefaultRequestTimeout, 30*time.Second)
	}
	if driverkube.ErrAmbiguous == nil {
		t.Fatal("ErrAmbiguous is nil")
	}
}

// TestNewClusterRefusesInvalidConfiguration proves an unusable kubeconfig or
// credential set is refused before any network call.
func TestNewClusterRefusesInvalidConfiguration(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {})
	certPEM, _, _ := clientCertificate(t)

	valid := func(mutate func(map[string]any)) []byte {
		return mutateKubeconfig(t, api.kubeconfig(t, nil, nil), mutate)
	}

	cases := []struct {
		name       string
		kubeconfig []byte
		configure  func(*driverkube.ClusterConfig)
	}{
		{
			name:       "empty document",
			kubeconfig: nil,
		},
		{
			name:       "malformed JSON",
			kubeconfig: []byte("{"),
		},
		{
			name:       "missing current context",
			kubeconfig: valid(func(document map[string]any) { delete(document, "current-context") }),
		},
		{
			name:       "unknown selected context",
			kubeconfig: api.kubeconfig(t, nil, nil),
			configure:  func(config *driverkube.ClusterConfig) { config.Context = "missing" },
		},
		{
			name: "non-http server",
			kubeconfig: valid(func(document map[string]any) {
				cluster := firstCluster(t, document)
				cluster["server"] = "unix:///var/run/kubernetes.sock"
			}),
		},
		{
			name: "unparsable CA data",
			kubeconfig: valid(func(document map[string]any) {
				cluster := firstCluster(t, document)
				cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString([]byte("not a certificate"))
			}),
		},
		{
			name: "invalid base64 CA data",
			kubeconfig: valid(func(document map[string]any) {
				cluster := firstCluster(t, document)
				cluster["certificate-authority-data"] = "%%%"
			}),
		},
		{
			name: "client certificate without key",
			kubeconfig: api.kubeconfig(t, map[string]any{
				"client-certificate-data": base64.StdEncoding.EncodeToString(certPEM),
			}, nil),
		},
		{
			name:       "no credential material",
			kubeconfig: api.kubeconfig(t, map[string]any{}, nil),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := driverkube.ClusterConfig{Kubeconfig: testCase.kubeconfig, Timeout: testTimeout}
			if testCase.configure != nil {
				testCase.configure(&config)
			}
			cluster, err := driverkube.NewCluster(config)
			if err == nil {
				t.Fatalf("NewCluster = %v; want an invalid-input refusal", cluster)
			}
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
	if calls := api.callCount(); calls != 0 {
		t.Fatalf("invalid configurations made %d API calls; want 0", calls)
	}
}

// TestClusterAuthenticatesWithInlineCAAndBearerToken proves the inline
// certificate-authority-data path and the bearer token header.
func TestClusterAuthenticatesWithInlineCAAndBearerToken(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/"+testNamespace {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q; want bearer token", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name":   testNamespace,
				"labels": map[string]string{runtimekube.LabelTenant: testNamespace},
			},
		})
	})
	cluster := newClusterFor(t, api)

	namespace, err := cluster.GetNamespace(context.Background(), testNamespace)
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if namespace.Name != testNamespace {
		t.Fatalf("namespace name = %q; want %q", namespace.Name, testNamespace)
	}
	if namespace.Labels[runtimekube.LabelTenant] != testNamespace {
		t.Fatalf("namespace labels = %v; want the tenant label", namespace.Labels)
	}
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls = %d; want 1", calls)
	}
}

// TestClusterAuthenticatesWithCAFile proves the certificate-authority file path
// is read and trusted.
func TestClusterAuthenticatesWithCAFile(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, api.caPEM(t), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	kubeconfig := api.kubeconfig(t, nil, func(cluster map[string]any) {
		delete(cluster, "certificate-authority-data")
		cluster["certificate-authority"] = caPath
	})
	cluster := newClusterWith(t, kubeconfig, nil)

	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("GetNamespace with a CA file: %v", err)
	}
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls = %d; want 1", calls)
	}
}

// TestClusterAuthenticatesWithClientCertificate proves mutual TLS from the
// inline client-certificate-data/client-key-data pair.
func TestClusterAuthenticatesWithClientCertificate(t *testing.T) {
	certPEM, keyPEM, caPEM := clientCertificate(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("client CA PEM is not usable")
	}
	api := newAPIWithTLS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if got := r.TLS.PeerCertificates[0].Subject.CommonName; got != "kodama-test-client" {
			http.Error(w, "unexpected client "+got, http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	}, &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool})

	kubeconfig := api.kubeconfig(t, map[string]any{
		"client-certificate-data": base64.StdEncoding.EncodeToString(certPEM),
		"client-key-data":         base64.StdEncoding.EncodeToString(keyPEM),
	}, nil)
	cluster := newClusterWith(t, kubeconfig, nil)

	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("GetNamespace with a client certificate: %v", err)
	}
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls = %d; want 1", calls)
	}
}

// TestClusterReadsRotatingTokenFilePerRequest proves the token file is read for
// every request, so a rotated execution credential takes effect without
// rebuilding the client, and a trailing newline is trimmed.
func TestClusterReadsRotatingTokenFilePerRequest(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("tok-one\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	kubeconfig := api.kubeconfig(t, map[string]any{"tokenFile": tokenPath}, nil)
	cluster := newClusterWith(t, kubeconfig, nil)

	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("first GetNamespace: %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("tok-two\n"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("second GetNamespace: %v", err)
	}

	calls := api.snapshot()
	if len(calls) != 2 {
		t.Fatalf("API calls = %d; want 2", len(calls))
	}
	if got := calls[0].Header.Get("Authorization"); got != "Bearer tok-one" {
		t.Fatalf("first Authorization = %q; want the original token", got)
	}
	if got := calls[1].Header.Get("Authorization"); got != "Bearer tok-two" {
		t.Fatalf("second Authorization = %q; want the rotated token", got)
	}
}

// TestClusterRefusesMissingTokenFileWithoutSending proves a rotated-away token
// file is a dependency failure before any request, never a stale credential
// reuse and never a leaked token value.
func TestClusterRefusesMissingTokenFileWithoutSending(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": testNamespace}})
	})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("tok-original\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	kubeconfig := api.kubeconfig(t, map[string]any{"tokenFile": tokenPath}, nil)
	cluster := newClusterWith(t, kubeconfig, nil)
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); err != nil {
		t.Fatalf("first GetNamespace: %v", err)
	}
	if err := os.Remove(tokenPath); err != nil {
		t.Fatalf("remove token file: %v", err)
	}

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if err == nil {
		t.Fatal("GetNamespace = nil; want a missing-token failure")
	}
	assertNoCredential(t, err, "tok-original")
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("API calls = %d; want only the first request", calls)
	}
}

// TestClusterRefusesUntrustedServerCertificate proves a TLS trust failure is
// ambiguous and sends no request.
func TestClusterRefusesUntrustedServerCertificate(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("request reached an untrusted server")
	})
	_, _, otherCA := clientCertificate(t)
	kubeconfig := api.kubeconfig(t, nil, func(cluster map[string]any) {
		cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString(otherCA)
	})
	cluster := newClusterWith(t, kubeconfig, nil)

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error = %v; want ErrAmbiguous", err)
	}
	assertNoCredential(t, err, testToken)
	if calls := api.callCount(); calls != 0 {
		t.Fatalf("API calls = %d; want 0", calls)
	}
}

// TestClusterClassifiesTransportFailureAsAmbiguous proves a lost connection is
// never reported as a missing object or a conflict.
func TestClusterClassifiesTransportFailureAsAmbiguous(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {})
	cluster := newClusterFor(t, api)
	api.server.Close()

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error = %v; want ErrAmbiguous", err)
	}
	for _, sentinel := range []error{runtimekube.ErrNotFound, runtimekube.ErrAlreadyExists, runtimekube.ErrConflict} {
		if errors.Is(err, sentinel) {
			t.Fatalf("transport failure matched sentinel %v; it must stay ambiguous", sentinel)
		}
	}
	assertNoCredential(t, err, testToken)
}

// TestClusterBoundsCallWithTimeout proves one call cannot hang past the
// configured timeout.
func TestClusterBoundsCallWithTimeout(t *testing.T) {
	api := newAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	cluster := newClusterWith(t, api.kubeconfig(t, nil, nil), func(config *driverkube.ClusterConfig) {
		config.Timeout = 100 * time.Millisecond
	})

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error = %v; want ErrAmbiguous", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v; want a deadline cause", err)
	}
}

// TestClusterCancellationIsAmbiguous proves a cancelled call is ambiguous (the
// effect may have happened) and that the server observes cancellation.
func TestClusterCancellationIsAmbiguous(t *testing.T) {
	gate := newGate()
	observed := make(chan struct{})
	api := newAPI(t, func(_ http.ResponseWriter, r *http.Request) {
		gate.entered <- struct{}{}
		<-r.Context().Done()
		close(observed)
	})
	cluster := newClusterFor(t, api)

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
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed the cancelled request")
	}
}

// TestClusterRefusesRedirects proves a redirect is never followed, so a token
// cannot be forwarded to another endpoint.
func TestClusterRefusesRedirects(t *testing.T) {
	target := newAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target received a request")
	})
	redirector := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.server.URL+"/api/v1/namespaces/"+testNamespace, http.StatusFound)
	})
	cluster := newClusterFor(t, redirector)

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error = %v; want ErrAmbiguous", err)
	}
	if calls := target.callCount(); calls != 0 {
		t.Fatalf("redirect target calls = %d; want 0", calls)
	}
	if calls := redirector.callCount(); calls != 1 {
		t.Fatalf("redirector calls = %d; want 1", calls)
	}
}

// TestClusterBoundsResponseBody proves an oversized response is refused instead
// of being buffered without bound.
func TestClusterBoundsResponseBody(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"metadata":{"name":"` + strings.Repeat("x", 256) + `"}}`))
	})
	cluster := newClusterWith(t, api.kubeconfig(t, nil, nil), func(config *driverkube.ClusterConfig) {
		config.MaxResponseBytes = 64
	})

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("error = %v; want ErrAmbiguous", err)
	}
}

// TestClusterErrorsNeverContainCredentialValues proves an error never renders a
// bearer token, a Secret value or the response body.
func TestClusterErrorsNeverContainCredentialValues(t *testing.T) {
	const secretValue = "s3cr3t-value"
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("failure for " + testToken + " and " + secretValue))
	})
	cluster := newClusterFor(t, api)

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	assertNoCredential(t, err, testToken, secretValue)

	_, err = cluster.PutSecret(context.Background(), runtimekube.Secret{
		Name:      "kodama-credential-test",
		Namespace: testNamespace,
		Data:      map[string][]byte{"KODAMA_TOKEN": []byte(secretValue)},
	})
	assertNoCredential(t, err, testToken, secretValue)
}

// mutateKubeconfig decodes a kubeconfig document, applies mutate and encodes it
// again.
func mutateKubeconfig(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode kubeconfig fixture: %v", err)
	}
	mutate(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode kubeconfig fixture: %v", err)
	}
	return encoded
}

// firstCluster returns the first cluster entry of a kubeconfig document.
func firstCluster(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	clusters, ok := document["clusters"].([]any)
	if !ok || len(clusters) == 0 {
		t.Fatal("kubeconfig has no clusters")
	}
	entry, ok := clusters[0].(map[string]any)
	if !ok {
		t.Fatal("kubeconfig cluster entry is not an object")
	}
	cluster, ok := entry["cluster"].(map[string]any)
	if !ok {
		t.Fatal("kubeconfig cluster is not an object")
	}
	return cluster
}
