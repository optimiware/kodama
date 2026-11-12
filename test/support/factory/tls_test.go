package factory

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
)

// serviceHosts are the loopback and real workload aliases a factory TLS
// service certificate must cover. The composition edge is reached on loopback,
// a rootless Podman workload reaches the host as host.containers.internal and a
// pod in the rootless kind cluster reaches it as 169.254.1.2.
func serviceHosts() []string {
	return []string{
		"127.0.0.1",
		"::1",
		"localhost",
		"host.containers.internal",
		KubernetesHostAlias,
	}
}

// TestTLSFixtureServesServiceSANs proves the opt-in TLS fixture is a private CA
// with one service certificate that covers every real workload alias, that the
// exported CA material really validates the served certificate, and that the
// trusted client never disables verification.
func TestTLSFixtureServesServiceSANs(t *testing.T) {
	fixture := newTLSFixture(t, serviceHosts()...)

	if len(fixture.CAPEM) == 0 {
		t.Fatalf("TLS fixture exported no CA material")
	}
	block, rest := pem.Decode(fixture.CAPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("exported CA material is not a PEM certificate: %q", string(fixture.CAPEM))
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("exported CA material carries trailing bytes outside one certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse exported CA certificate: %v", err)
	}
	if !ca.IsCA {
		t.Fatalf("exported certificate is not a CA")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("exported CA lacks certificate-signing key usage: %v", ca.KeyUsage)
	}

	leaf := fixture.Leaf()
	if leaf == nil {
		t.Fatalf("TLS fixture exposed no service leaf certificate")
	}
	if leaf.IsCA {
		t.Fatalf("service leaf certificate is a CA")
	}
	for _, host := range serviceHosts() {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Fatalf("service certificate does not cover %q: %v", host, err)
		}
	}
	if err := leaf.VerifyHostname("unlisted.invalid"); err == nil {
		t.Fatalf("service certificate covers an unlisted host")
	}

	transport, ok := fixture.Client().Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("trusted client transport = %T; want *http.Transport", fixture.Client().Transport)
	}
	if transport.TLSClientConfig == nil {
		t.Fatalf("trusted client has no TLS configuration")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("trusted client sets InsecureSkipVerify")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatalf("trusted client has no private CA pool")
	}
	if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("trusted client minimum version = %x; want at least TLS 1.2", transport.TLSClientConfig.MinVersion)
	}

	// The exported CA file is the material a workload or Git helper consumes.
	caFile := fixture.CACertFile()
	written, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("read exported CA file: %v", err)
	}
	if !bytes.Equal(written, fixture.CAPEM) {
		t.Fatalf("exported CA file does not contain the exported CA PEM")
	}

	// An ordinary client built from the exported CA validates a real TLS
	// listener served with the fixture's service certificate.
	listener, err := tls.Listen("tcp", "127.0.0.1:0", fixture.ServerConfig())
	if err != nil {
		t.Fatalf("listen with fixture TLS config: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
	})

	response, err := fixture.Client().Get("https://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("trusted client round trip: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("trusted client status = %d; want %d", response.StatusCode, http.StatusNoContent)
	}
}

// TestServiceTLSHostsPinTheWiredNames pins the exact certificate host set the
// harness wires into the service certificate. server.go passes this function's
// result to newTLSFixture, so dropping any real workload alias, a loopback name
// or the run's concrete listen host makes the TLS fixture unusable rather than
// silently narrowing verification.
func TestServiceTLSHostsPinTheWiredNames(t *testing.T) {
	const listenHost = "192.0.2.44"
	want := []string{
		"127.0.0.1",
		"::1",
		"localhost",
		"host.containers.internal",
		KubernetesHostAlias,
		listenHost,
	}
	got := serviceTLSHosts(listenHost)
	if !slices.Equal(got, want) {
		t.Fatalf("service TLS hosts = %v; want exactly %v", got, want)
	}

	seen := make(map[string]struct{}, len(got))
	for _, host := range got {
		if host == "" {
			t.Fatalf("service TLS hosts contain an empty name: %v", got)
		}
		if _, duplicate := seen[host]; duplicate {
			t.Fatalf("service TLS hosts contain duplicate %q: %v", host, got)
		}
		seen[host] = struct{}{}
	}
	if _, ok := seen[listenHost]; !ok {
		t.Fatalf("service TLS hosts omit the run's listen host %q: %v", listenHost, got)
	}
}

// TestTLSFixtureTrustsOnlyItsPrivateCA proves the fixture client's trust is
// exactly the exported CA, not the system store widened by that CA: its pool
// equals a pool built from CAPEM alone, and it refuses a server signed by a
// second, unrelated fixture CA with an unknown-authority error.
func TestTLSFixtureTrustsOnlyItsPrivateCA(t *testing.T) {
	fixture := newTLSFixture(t, serviceHosts()...)

	transport, ok := fixture.Client().Transport.(*http.Transport)
	if !ok || transport == nil || transport.TLSClientConfig == nil {
		t.Fatalf("trusted client has no TLS transport configuration")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("trusted client sets InsecureSkipVerify")
	}
	expected := x509.NewCertPool()
	if !expected.AppendCertsFromPEM(fixture.CAPEM) {
		t.Fatalf("exported CA material is not a usable certificate pool")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatalf("trusted client has no private CA pool")
	}
	if !transport.TLSClientConfig.RootCAs.Equal(expected) {
		t.Fatalf("trusted client pool is not exactly the exported private CA")
	}

	// A second fixture is a genuinely different CA serving a certificate for
	// the same hosts. Only the first fixture's CA may validate it.
	foreign := newTLSFixture(t, serviceHosts()...)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", foreign.ServerConfig())
	if err != nil {
		t.Fatalf("listen with foreign TLS config: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
	})

	response, err := fixture.Client().Get("https://" + listener.Addr().String())
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("trusted client accepted a server signed by an unrelated fixture CA")
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatalf("client error = %v; want a TLS certificate verification error", err)
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); !ok {
		t.Fatalf("client error = %v; want an unknown-authority refusal", err)
	}
}

// TestTLSFixtureRefusesUnknownAuthority proves the fixture is only trusted
// through its exported private CA: an ordinary client using the system trust
// store refuses the same service certificate.
func TestTLSFixtureRefusesUnknownAuthority(t *testing.T) {
	fixture := newTLSFixture(t, serviceHosts()...)

	listener, err := tls.Listen("tcp", "127.0.0.1:0", fixture.ServerConfig())
	if err != nil {
		t.Fatalf("listen with fixture TLS config: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
	})

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		Timeout:   10 * time.Second,
	}
	response, err := client.Get("https://" + listener.Addr().String())
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("system-trust client accepted the private-CA service certificate")
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
		t.Fatalf("client error = %v; want a TLS certificate verification error", err)
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); !ok {
		t.Fatalf("client error = %v; want an unknown-authority refusal", err)
	}
}

// TestHarnessHelperClientRefusesForeignAuthority pins the accessor the REST and
// MCP helpers really use: httpClient must carry the CA-trusting fixture
// transport, reach the fixture's service, and refuse a server signed by an
// unrelated authority.
func TestHarnessHelperClientRefusesForeignAuthority(t *testing.T) {
	fixture := newTLSFixture(t, serviceHosts()...)
	foreign := newTLSFixture(t, serviceHosts()...)

	harness := &Harness{clientTransport: fixture.Client().Transport}
	if harness.httpClient().Transport != harness.roundTripper() {
		t.Fatalf("httpClient does not use the harness round tripper")
	}

	own := serveTLSFixture(t, fixture)
	response, err := harness.httpClient().Get(own)
	if err != nil {
		t.Fatalf("helper client refused its own fixture CA: %v", err)
	}
	_ = response.Body.Close()

	other := serveTLSFixture(t, foreign)
	if response, err := harness.httpClient().Get(other); err == nil {
		_ = response.Body.Close()
		t.Fatalf("helper client accepted a server signed by a foreign CA")
	} else {
		if _, ok := errors.AsType[x509.UnknownAuthorityError](err); !ok {
			t.Fatalf("helper client error = %v, want x509.UnknownAuthorityError", err)
		}
	}
}

// serveTLSFixture serves a 204 over the fixture's service certificate and
// returns its https URL.
func serveTLSFixture(t *testing.T, fixture *TLSFixture) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", fixture.ServerConfig())
	if err != nil {
		t.Fatalf("listen with fixture TLS config: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
	})
	return "https://" + listener.Addr().String()
}
