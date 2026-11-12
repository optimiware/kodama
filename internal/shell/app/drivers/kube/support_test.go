// Test support for the production Kubernetes Cluster driver.
//
// This external test package drives the driver through net/http/httptest TLS
// servers only. No real API server, process, container, cluster or cloud call is
// involved. The fake API server records every request so a test can assert the
// exact method, path, query, headers and JSON body, and the client certificate,
// bearer token and CA fixtures are generated in-process.
package kube_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
)

const (
	testNamespace = "tenant-acme"
	testToken     = "tok-test-bearer"
	// testTimeout bounds one call in these tests; a caller context may be
	// shorter.
	testTimeout = 5 * time.Second
)

// apiCall is one request the fake API server received.
type apiCall struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// apiServer is a TLS httptest server that records every request.
type apiServer struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []apiCall
}

// newAPI starts a TLS API server with the httptest certificate.
func newAPI(t *testing.T, handler http.HandlerFunc) *apiServer {
	t.Helper()
	return newAPIWithTLS(t, handler, nil)
}

// newAPIWithTLS starts a TLS API server with an explicit TLS configuration.
// A nil configuration keeps the httptest defaults.
func newAPIWithTLS(t *testing.T, handler http.HandlerFunc, tlsConfig *tls.Config) *apiServer {
	t.Helper()
	return newAPIWithListener(t, nil, handler, tlsConfig)
}

// newAPIWithListener is newAPIWithTLS with an explicit listener, so a test can
// bind the API server to a non-loopback address and observe whether the driver
// consults an environment proxy. A nil listener keeps the httptest loopback
// listener.
func newAPIWithListener(
	t *testing.T,
	listener net.Listener,
	handler http.HandlerFunc,
	tlsConfig *tls.Config,
) *apiServer {
	t.Helper()
	api := &apiServer{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		api.mu.Lock()
		api.calls = append(api.calls, apiCall{
			Method:   r.Method,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     append([]byte(nil), body...),
		})
		api.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		handler(w, r)
	}))
	if listener != nil {
		unused := server.Listener
		server.Listener = listener
		t.Cleanup(func() { _ = unused.Close() })
	}
	if tlsConfig != nil {
		server.TLS = tlsConfig
	}
	server.StartTLS()
	api.server = server
	t.Cleanup(server.Close)
	return api
}

// caPEM returns the PEM-encoded certificate that signed the server's leaf.
func (a *apiServer) caPEM(t *testing.T) []byte {
	t.Helper()
	certificate := a.server.Certificate()
	if certificate == nil {
		t.Fatal("fake API server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

// snapshot returns a copy of every recorded call.
func (a *apiServer) snapshot() []apiCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]apiCall(nil), a.calls...)
}

// callCount reports how many requests reached the server.
func (a *apiServer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// lastCall returns the most recent request.
func (a *apiServer) lastCall(t *testing.T) apiCall {
	t.Helper()
	calls := a.snapshot()
	if len(calls) == 0 {
		t.Fatal("no request reached the fake API server")
	}
	return calls[len(calls)-1]
}

// kubeconfig builds a JSON kubeconfig that trusts the fake server and
// authenticates with user. mutateCluster may replace cluster fields (for
// example a certificate-authority file path).
func (a *apiServer) kubeconfig(t *testing.T, user map[string]any, mutateCluster func(map[string]any)) []byte {
	t.Helper()
	cluster := map[string]any{
		"server":                     a.server.URL,
		"certificate-authority-data": base64.StdEncoding.EncodeToString(a.caPEM(t)),
	}
	if mutateCluster != nil {
		mutateCluster(cluster)
	}
	if user == nil {
		user = map[string]any{"token": testToken}
	}
	document := map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters": []any{
			map[string]any{"name": "test", "cluster": cluster},
		},
		"contexts": []any{
			map[string]any{"name": "test", "context": map[string]any{"cluster": "test", "user": "test"}},
		},
		"current-context": "test",
		"users": []any{
			map[string]any{"name": "test", "user": user},
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode kubeconfig fixture: %v", err)
	}
	return encoded
}

// newClusterFor builds a driver bound to the fake server with a default bearer
// token user.
func newClusterFor(t *testing.T, api *apiServer) *driverkube.Cluster {
	t.Helper()
	return newClusterWith(t, api.kubeconfig(t, nil, nil), nil)
}

// newClusterWith builds a driver from one kubeconfig document. mutate may
// replace driver configuration fields.
func newClusterWith(t *testing.T, kubeconfig []byte, mutate func(*driverkube.ClusterConfig)) *driverkube.Cluster {
	t.Helper()
	config := driverkube.ClusterConfig{Kubeconfig: kubeconfig, Timeout: testTimeout}
	if mutate != nil {
		mutate(&config)
	}
	cluster, err := driverkube.NewCluster(config)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	return cluster
}

// writeJSON writes one JSON response.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// decodeJSON decodes one JSON document into a generic value.
func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON %q: %v", data, err)
	}
	return value
}

// assertJSONEqual compares got with the want JSON document semantically. A
// mismatch prints both canonical forms.
func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	gotValue := decodeJSON(t, got)
	wantValue := decodeJSON(t, []byte(want))
	if reflect.DeepEqual(gotValue, wantValue) {
		return
	}
	gotCanonical, _ := json.MarshalIndent(gotValue, "", "  ")
	wantCanonical, _ := json.MarshalIndent(wantValue, "", "  ")
	t.Fatalf("JSON body mismatch\n got: %s\nwant: %s", gotCanonical, wantCanonical)
}

// assertNoCredential proves an error never renders a credential value.
func assertNoCredential(t *testing.T, err error, values ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want a credential-free failure")
	}
	rendered := err.Error()
	for _, value := range values {
		if value == "" {
			continue
		}
		if strings.Contains(rendered, value) {
			t.Fatalf("error %q contains credential value %q", rendered, value)
		}
	}
}

// assertFaultClass asserts the stable public fault class.
func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class; want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s; want %s (error %v)", class, want, err)
	}
}

// gate is an explicit request barrier: the handler signals entered and then
// blocks until the test releases it or the request context ends.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// waitEntered blocks until the handler reached the barrier.
func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the barrier")
	}
}

// clientCertificate builds a CA and a client leaf signed by it, returning PEM
// for the leaf certificate, its key and the CA.
func clientCertificate(t *testing.T) (certPEM, keyPEM, caPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kodama-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCertificate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return certPEM, keyPEM, caPEM
}
