package natsdriver

// These tests pin explicit trusted TLS: a private CA bundle (and an optional
// client certificate) lets the driver reach a verified TLS broker, while
// hostname verification and certificate validation stay on. There is no
// insecure-verify escape hatch and no implicit system-root fallback: a nil TLS
// configuration keeps the existing plaintext composition (proven in
// nats_test.go).
//
// The peer is the in-process fake NATS protocol server from nats_test.go. It
// runs the real nats.go TLS handshake, so the trust store, client certificate
// and reconnect behavior under test are the production nats.go paths, not a
// driver-local stub.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// tlsTestValidityWindow is one test certificate validity interval.
type tlsTestValidityWindow struct {
	notBefore time.Time
	notAfter  time.Time
}

// testValidityAt anchors the normal test certificate window at anchor: one hour
// earlier to tolerate clock skew and ten years later so the suite never becomes
// a calendar time bomb. The result is at whole-second precision so generated
// certificates stay reproducible for one anchor.
func testValidityAt(anchor time.Time) tlsTestValidityWindow {
	anchor = anchor.UTC().Truncate(time.Second)
	return tlsTestValidityWindow{
		notBefore: anchor.Add(-time.Hour),
		notAfter:  anchor.AddDate(10, 0, 0),
	}
}

// tlsTestValidity is the normal validity window, anchored once at process start
// from the test clock. Tests that need an expired or otherwise unusual leaf
// derive its window explicitly, relative to the same clock.
var tlsTestValidity = testValidityAt(time.Now())

// testCA is one generated private certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	file string
}

// newTestCA generates a private CA and writes its PEM bundle to a temp file.
func newTestCA(t *testing.T, commonName string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             tlsTestValidity.notBefore,
		NotAfter:              tlsTestValidity.notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign |
			x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pemBytes,
		file: writeTestPEM(t, "ca.pem", pemBytes),
	}
}

// TestTLSTestValidityMovesWithClock proves the normal test certificate window
// is derived from the clock instead of a fixed calendar date. A fixed
// 2025-01-01..2030-01-01 window would be a time bomb: every normal certificate
// would be expired after 2030, and the window must also tolerate an anchor
// earlier than a later one.
func TestTLSTestValidityMovesWithClock(t *testing.T) {
	anchors := []time.Time{
		time.Date(2026, time.March, 1, 12, 30, 45, 0, time.UTC),
		time.Date(2040, time.March, 1, 12, 30, 45, 0, time.UTC),
	}
	for _, anchor := range anchors {
		validity := testValidityAt(anchor)
		if want := anchor.Add(-time.Hour); !validity.notBefore.Equal(want) {
			t.Errorf("anchor %s: NotBefore = %s; want %s", anchor.Format(time.RFC3339), validity.notBefore.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if want := anchor.AddDate(10, 0, 0); !validity.notAfter.Equal(want) {
			t.Errorf("anchor %s: NotAfter = %s; want %s", anchor.Format(time.RFC3339), validity.notAfter.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		// The anchor itself must sit inside the window: a NotBefore after the
		// anchor would make normal certificates not yet valid.
		if !validity.notBefore.Before(anchor) || !validity.notAfter.After(anchor) {
			t.Errorf("anchor %s is not covered by its own validity window %s..%s", anchor, validity.notBefore, validity.notAfter)
		}
	}

	// A fixed window would expire on 2030-01-01. A window anchored after that
	// date must move past the bomb rather than keep fixed dates.
	bomb := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	late := testValidityAt(time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC))
	if !late.notBefore.After(bomb) {
		t.Errorf("2040 NotBefore %s did not move past the 2030 expiry", late.notBefore)
	}
	if !late.notAfter.After(bomb) {
		t.Errorf("2040 NotAfter %s did not move past the 2030 expiry", late.notAfter)
	}

	// Second precision keeps a generated certificate reproducible for one
	// anchor regardless of the sub-second clock reading.
	noisy := testValidityAt(time.Date(2026, time.March, 1, 12, 30, 45, 987654321, time.UTC))
	if noisy.notBefore.Nanosecond() != 0 || noisy.notAfter.Nanosecond() != 0 {
		t.Errorf("validity window was not truncated to seconds: %s..%s", noisy.notBefore, noisy.notAfter)
	}

	// The package-level window is anchored once from the real test clock, so
	// the normal identities these tests issue are valid today. Merely checking
	// that "now" is inside the window would also accept a fixed
	// 2025-01-01..2030-01-01 calendar span, so the window is pinned to a
	// clock-derived shape: a one-hour skew allowance before now and a
	// ten-year horizon after it.
	now := time.Now().UTC()
	if now.Before(tlsTestValidity.notBefore) || now.After(tlsTestValidity.notAfter) {
		t.Errorf("current time %s is outside the anchored validity window %s..%s", now, tlsTestValidity.notBefore, tlsTestValidity.notAfter)
	}
	if remaining := tlsTestValidity.notAfter.Sub(now); remaining < 9*365*24*time.Hour {
		t.Errorf("package validity NotAfter %s is only %s after %s; want the clock-derived ten-year window, not a fixed calendar date",
			tlsTestValidity.notAfter.Format(time.RFC3339), remaining, now.Format(time.RFC3339))
	}
	if age := now.Sub(tlsTestValidity.notBefore); age > 48*time.Hour {
		t.Errorf("package validity NotBefore %s is %s before %s; want the clock-derived one-hour skew allowance, not a fixed calendar date",
			tlsTestValidity.notBefore.Format(time.RFC3339), age, now.Format(time.RFC3339))
	}
}

// pool returns the trust pool for this CA.
func (ca *testCA) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.pem) {
		panic("test CA PEM is not a certificate")
	}
	return pool
}

// testIdentity is one issued certificate and key pair.
type testIdentity struct {
	certificate tls.Certificate
	certPEM     []byte
	keyPEM      []byte
	certFile    string
	keyFile     string
}

// issue signs one certificate for the given names and extended key usage.
func (ca *testCA) issue(t *testing.T, serial int64, commonName string, dnsNames []string, ips []net.IP, usage x509.ExtKeyUsage) *testIdentity {
	t.Helper()
	return ca.issueWithValidity(t, serial, commonName, dnsNames, ips, usage,
		tlsTestValidity.notBefore, tlsTestValidity.notAfter)
}

// issueWithValidity signs one certificate with an explicit validity window, so
// a test can present an already expired leaf while the CA itself stays valid.
func (ca *testCA) issueWithValidity(t *testing.T, serial int64, commonName string, dnsNames []string, ips []net.IP, usage x509.ExtKeyUsage, notBefore, notAfter time.Time) *testIdentity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build key pair: %v", err)
	}
	return &testIdentity{
		certificate: certificate,
		certPEM:     certPEM,
		keyPEM:      keyPEM,
		certFile:    writeTestPEM(t, "cert.pem", certPEM),
		keyFile:     writeTestPEM(t, "key.pem", keyPEM),
	}
}

// serverTLS is the server-side configuration for this identity.
func (identity *testIdentity) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{identity.certificate},
		MinVersion:   tls.VersionTLS12,
	}
}

// writeTestPEM writes one PEM file under a fresh temp directory.
func writeTestPEM(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// issueLoopback signs a server certificate that verifies for the fake server's
// 127.0.0.1 address.
func (ca *testCA) issueLoopback(t *testing.T, serial int64) *testIdentity {
	t.Helper()
	return ca.issue(t, serial, "nats.test",
		[]string{"nats.test"}, []net.IP{net.ParseIP("127.0.0.1")},
		x509.ExtKeyUsageServerAuth)
}

// dialConfig is one TLS dial configuration against server.
func dialConfig(url, caFile string) Config {
	return Config{
		URL:            url,
		ConnectTimeout: 2 * time.Second,
		DrainTimeout:   time.Second,
		TLS:            &TLSConfig{CAFile: caFile},
	}
}

// TestNATSDialUsesConfiguredTrust proves the driver verifies the server against
// the configured private CA bundle, that trust is explicit (no implicit
// system-root fallback), and that a TLS configuration without a CA bundle is
// refused instead of silently connecting unverified.
func TestNATSDialUsesConfiguredTrust(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	serverIdentity := ca.issueLoopback(t, 10)
	server := startFakeNATSServer(t, serverIdentity.serverTLS())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := Dial(ctx, dialConfig(server.URL(), ca.file))
	if err != nil {
		t.Fatalf("Dial with configured private CA: %v", err)
	}
	subject := testSubject(t)
	if err := client.PublishCore(ctx, subject, []byte("verified")); err != nil {
		t.Fatalf("PublishCore over verified TLS: %v", err)
	}
	flushCore(t, client)
	waitFakeEvent(t, server.publishCh, "publication over verified TLS")
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close verified TLS client: %v", err)
	}
	if published := server.Published(); len(published) != 1 || published[0] != string(subject) {
		t.Fatalf("TLS publications = %v; want [%s]", published, subject)
	}

	// An empty TLS block is a configuration error, not permission to fall back
	// to the system roots, an unverified handshake or plaintext.
	if _, err := Dial(ctx, Config{
		URL:            server.URL(),
		ConnectTimeout: time.Second,
		TLS:            &TLSConfig{},
	}); err == nil {
		t.Fatal("Dial accepted a TLS configuration without a CA bundle")
	} else {
		requireFaultClass(t, err, fault.InvalidInput)
	}

	// The same server is rejected when no explicit trust is configured. The
	// private CA is absent from the system roots, so this also proves the
	// driver never quietly disables verification.
	if _, err := Dial(ctx, Config{URL: server.URL(), ConnectTimeout: time.Second}); err == nil {
		t.Fatal("Dial trusted the private-CA server without configured trust")
	}
}

// TestNATSClientTLSIsPrivateCAOnly pins the built configuration directly: the
// trust roots are exactly the configured private bundle (never the system
// roots), verification and hostname checks stay on, the TLS 1.2 floor is
// explicit, and the client certificate is only installed when configured.
func TestNATSClientTLSIsPrivateCAOnly(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	config := Config{TLS: &TLSConfig{CAFile: ca.file}}
	built, err := config.clientTLS()
	if err != nil {
		t.Fatalf("clientTLS: %v", err)
	}
	if built == nil {
		t.Fatal("clientTLS returned no configuration for a configured TLS block")
	}
	if built.RootCAs == nil {
		t.Fatal("clientTLS left RootCAs nil; verification would fall back to the system roots")
	}
	if systemRoots, err := x509.SystemCertPool(); err == nil && systemRoots != nil {
		if built.RootCAs.Equal(systemRoots) {
			t.Fatal("clientTLS used the system roots instead of the configured private CA")
		}
	}
	if !built.RootCAs.Equal(ca.pool()) {
		t.Fatal("clientTLS RootCAs is not exactly the configured private CA bundle")
	}
	if built.InsecureSkipVerify {
		t.Fatal("clientTLS enabled InsecureSkipVerify")
	}
	if built.MinVersion < tls.VersionTLS12 {
		t.Fatalf("clientTLS MinVersion = %#x; want at least TLS 1.2", built.MinVersion)
	}
	if built.ServerName != "" {
		t.Fatalf("clientTLS forced ServerName %q; nats.go must derive it from the connected host", built.ServerName)
	}
	if len(built.Certificates) != 0 {
		t.Fatalf("clientTLS installed %d client certificates without one configured", len(built.Certificates))
	}

	clientIdentity := ca.issue(t, 11, "client.test", nil, nil, x509.ExtKeyUsageClientAuth)
	builtWithCert, err := (Config{TLS: &TLSConfig{
		CAFile:   ca.file,
		CertFile: clientIdentity.certFile,
		KeyFile:  clientIdentity.keyFile,
	}}).clientTLS()
	if err != nil {
		t.Fatalf("clientTLS with a client certificate: %v", err)
	}
	if len(builtWithCert.Certificates) != 1 {
		t.Fatalf("clientTLS installed %d client certificates; want 1", len(builtWithCert.Certificates))
	}
	if !builtWithCert.RootCAs.Equal(ca.pool()) {
		t.Fatal("clientTLS with a client certificate changed the trust roots")
	}
}

// TestNATSTLSRefusesPlaintextPeer proves a TLS-configured client never falls
// back to plaintext: against a peer that offers no TLS, and against a peer that
// only advertises TLS as available, the dial is refused before any CONNECT (and
// therefore before any credential) reaches the peer.
func TestNATSTLSRefusesPlaintextPeer(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	refuse := func(t *testing.T, server *fakeNATSServer) {
		t.Helper()
		_, err := Dial(ctx, Config{
			URL:            server.URL(),
			Token:          "plaintext-secret",
			ConnectTimeout: 500 * time.Millisecond,
			DrainTimeout:   time.Second,
			TLS:            &TLSConfig{CAFile: ca.file},
		})
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if connects := server.Connects(); len(connects) != 0 {
			t.Fatalf("plaintext peer recorded %d CONNECT payloads; want none: %v", len(connects), connects)
		}
		if published := server.Published(); len(published) != 0 {
			t.Fatalf("plaintext peer recorded publications %v; want none", published)
		}
	}

	t.Run("no TLS offered", func(t *testing.T) {
		refuse(t, startFakeNATSServer(t, nil))
	})

	t.Run("TLS only available", func(t *testing.T) {
		refuse(t, startFakeNATSServerAdvertisingTLS(t))
	})
}

// TestNATSTLSRefusesOldProtocolVersion proves the explicit TLS 1.2 floor is
// enforced on the wire: a server limited to TLS 1.0-1.1 is refused.
func TestNATSTLSRefusesOldProtocolVersion(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	serverIdentity := ca.issueLoopback(t, 50)
	oldServerTLS := serverIdentity.serverTLS()
	oldServerTLS.MinVersion = tls.VersionTLS10
	oldServerTLS.MaxVersion = tls.VersionTLS11
	server := startFakeNATSServer(t, oldServerTLS)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Dial(ctx, dialConfig(server.URL(), ca.file))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if connects := server.Connects(); len(connects) != 0 {
		t.Fatalf("old-protocol peer recorded CONNECT payloads %v; want none", connects)
	}
}

// TestNATSTLSRefusesExpiredServerCertificate proves certificate validity is
// checked: a server leaf whose validity ended in the past is refused even
// though it chains to the configured private CA. The same leaf is verified by
// the same trust pool and SAN during its valid window first, and the CA itself
// is valid now, so the refusal can only be the leaf's expiry.
func TestNATSTLSRefusesExpiredServerCertificate(t *testing.T) {
	now := time.Now().UTC()
	ca := newTestCA(t, "kodama trusted test CA")
	if now.Before(ca.cert.NotBefore) || now.After(ca.cert.NotAfter) {
		t.Fatalf("test CA is not valid now (%s..%s at %s); a refusal would be vacuous",
			ca.cert.NotBefore.Format(time.RFC3339), ca.cert.NotAfter.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	// The leaf expires a minute ago and was valid half an hour ago, inside the
	// CA's own window.
	expired := ca.issueWithValidity(t, 60, "nats.test",
		[]string{"nats.test"}, []net.IP{net.ParseIP("127.0.0.1")},
		x509.ExtKeyUsageServerAuth,
		now.Add(-30*time.Minute), now.Add(-time.Minute))
	leaf, err := x509.ParseCertificate(expired.certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse expired leaf: %v", err)
	}
	if !leaf.NotAfter.Before(now) {
		t.Fatalf("test leaf NotAfter = %s; want an already expired leaf at %s", leaf.NotAfter.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if !leaf.NotBefore.Before(leaf.NotAfter) {
		t.Fatalf("test leaf window is empty: %s..%s", leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}

	// Positive control: the very same leaf chains to the configured CA and
	// carries the expected SAN while it is valid, so the later refusal proves
	// expiry checking rather than a trust or hostname problem.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       ca.pool(),
		DNSName:     "nats.test",
		CurrentTime: now.Add(-15 * time.Minute),
	}); err != nil {
		t.Fatalf("same leaf is not trusted for nats.test during its valid window: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       ca.pool(),
		DNSName:     "nats.test",
		CurrentTime: now,
	}); err == nil {
		t.Fatal("the test leaf is still valid now; it cannot prove expiry checking")
	}

	server := startFakeNATSServer(t, expired.serverTLS())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = Dial(ctx, dialConfig(server.URL(), ca.file))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if connects := server.Connects(); len(connects) != 0 {
		t.Fatalf("expired-certificate peer recorded CONNECT payloads %v; want none", connects)
	}
}

// TestNATSTLSRejectsUntrustedServer proves certificate validation and hostname
// verification stay on: a wrong CA, a certificate for another name and a
// missing required client certificate all fail, bounded, and no insecure
// escape hatch exists in the configuration.
func TestNATSTLSRejectsUntrustedServer(t *testing.T) {
	trusted := newTestCA(t, "kodama trusted test CA")
	serverIdentity := trusted.issueLoopback(t, 20)
	server := startFakeNATSServer(t, serverIdentity.serverTLS())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("wrong CA", func(t *testing.T) {
		untrusted := newTestCA(t, "kodama untrusted test CA")
		started := time.Now()
		_, err := Dial(ctx, Config{
			URL:            server.URL(),
			ConnectTimeout: 500 * time.Millisecond,
			TLS:            &TLSConfig{CAFile: untrusted.file},
		})
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("untrusted dial took %s; want a bounded failure", elapsed)
		}
	})

	t.Run("wrong SAN", func(t *testing.T) {
		otherName := trusted.issue(t, 21, "nats.other", []string{"nats.other"}, nil, x509.ExtKeyUsageServerAuth)
		otherServer := startFakeNATSServer(t, otherName.serverTLS())
		_, err := Dial(ctx, Config{
			URL:            otherServer.URL(),
			ConnectTimeout: 500 * time.Millisecond,
			TLS:            &TLSConfig{CAFile: trusted.file},
		})
		requireFaultClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("missing required client certificate", func(t *testing.T) {
		clientIdentity := trusted.issue(t, 22, "client.test", nil, nil, x509.ExtKeyUsageClientAuth)
		serverTLS := serverIdentity.serverTLS()
		serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
		serverTLS.ClientCAs = trusted.pool()
		mtlsServer := startFakeNATSServer(t, serverTLS)

		_, err := Dial(ctx, Config{
			URL:            mtlsServer.URL(),
			ConnectTimeout: 500 * time.Millisecond,
			TLS:            &TLSConfig{CAFile: trusted.file},
		})
		if err == nil {
			t.Fatal("a server requiring a client certificate accepted a client without one")
		}

		client, err := Dial(ctx, Config{
			URL:            mtlsServer.URL(),
			ConnectTimeout: 2 * time.Second,
			DrainTimeout:   time.Second,
			TLS: &TLSConfig{
				CAFile:   trusted.file,
				CertFile: clientIdentity.certFile,
				KeyFile:  clientIdentity.keyFile,
			},
		})
		if err != nil {
			t.Fatalf("Dial with a configured client certificate: %v", err)
		}
		if err := client.Close(context.Background()); err != nil {
			t.Fatalf("Close mutual TLS client: %v", err)
		}
	})

	t.Run("half client certificate refused", func(t *testing.T) {
		clientIdentity := trusted.issue(t, 23, "client.test", nil, nil, x509.ExtKeyUsageClientAuth)
		_, err := Dial(ctx, Config{
			URL:            server.URL(),
			ConnectTimeout: 500 * time.Millisecond,
			TLS: &TLSConfig{
				CAFile:   trusted.file,
				CertFile: clientIdentity.certFile,
			},
		})
		requireFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("no insecure escape hatch", func(t *testing.T) {
		for _, typ := range []reflect.Type{reflect.TypeFor[TLSConfig](), reflect.TypeFor[Config]()} {
			for field := range typ.Fields() {
				name := strings.ToLower(field.Name)
				if strings.Contains(name, "insecure") || strings.Contains(name, "skipverify") {
					t.Fatalf("%s exposes an insecure verification escape hatch %q", typ, field.Name)
				}
			}
		}
	})
}

// TestNATSTLSReconnectPreservesTrust drops every live connection (a server
// restart) and proves nats.go reconnects with the same private-CA trust, the
// same client certificate and the same token authentication, then publishes on
// the verified connection.
func TestNATSTLSReconnectPreservesTrust(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	serverIdentity := ca.issueLoopback(t, 30)
	clientIdentity := ca.issue(t, 31, "client.test", nil, nil, x509.ExtKeyUsageClientAuth)

	serverTLSConfig := serverIdentity.serverTLS()
	serverTLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
	serverTLSConfig.ClientCAs = ca.pool()
	server := startFakeNATSServer(t, serverTLSConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := Config{
		URL:            server.URL(),
		Name:           "tls-reconnect",
		Token:          "reconnect-secret",
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  5 * time.Millisecond,
		MaxReconnects:  -1,
		DrainTimeout:   time.Second,
		TLS: &TLSConfig{
			CAFile:   ca.file,
			CertFile: clientIdentity.certFile,
			KeyFile:  clientIdentity.keyFile,
		},
	}
	client, err := Dial(ctx, config)
	if err != nil {
		t.Fatalf("Dial mutual TLS with token: %v", err)
	}
	first := waitFakeEvent(t, server.connectCh, "first verified CONNECT")

	// Simulate a server restart: every connection drops and the client must
	// reconnect from scratch against the same TLS requirements.
	server.DropConnections()
	second := waitFakeEvent(t, server.connectCh, "reconnect CONNECT")

	for index, connect := range []string{first, second} {
		if !strings.Contains(connect, `"auth_token":"reconnect-secret"`) {
			t.Fatalf("CONNECT %d = %s; want the configured token on every verified connection", index, connect)
		}
	}
	if connects := server.Connects(); len(connects) < 2 {
		t.Fatalf("recorded %d CONNECTs; want at least 2 (initial and reconnect)", len(connects))
	}

	// The client certificate and CA must have survived the reconnect, because
	// the server requires and verifies the client certificate on every
	// handshake. The publication proves the reconnected connection is usable.
	subject := testSubject(t)
	if err := client.PublishCore(ctx, subject, []byte("after-restart")); err != nil {
		t.Fatalf("PublishCore after reconnect: %v", err)
	}
	flushCore(t, client)
	waitFakeEvent(t, server.publishCh, "publication after reconnect")
	if published := server.Published(); len(published) != 1 || published[0] != string(subject) {
		t.Fatalf("post-reconnect publications = %v; want [%s]", published, subject)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close reconnected client: %v", err)
	}
}

// TestNATSTLSFailuresAreSecretFree proves TLS configuration and handshake
// failures never render URL credentials, private-key material or CA file
// content.
func TestNATSTLSFailuresAreSecretFree(t *testing.T) {
	ca := newTestCA(t, "kodama trusted test CA")
	serverIdentity := ca.issueLoopback(t, 40)
	server := startFakeNATSServer(t, serverIdentity.serverTLS())
	untrusted := newTestCA(t, "kodama untrusted test CA")
	clientIdentity := ca.issue(t, 41, "client.test", nil, nil, x509.ExtKeyUsageClientAuth)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	withCredentials := strings.Replace(server.URL(), "nats://", "nats://clientuser:supersecret@", 1)
	_, err := Dial(ctx, Config{
		URL:            withCredentials,
		ConnectTimeout: 500 * time.Millisecond,
		DrainTimeout:   time.Second,
		TLS: &TLSConfig{
			CAFile:   untrusted.file,
			CertFile: clientIdentity.certFile,
			KeyFile:  clientIdentity.keyFile,
		},
	})
	requireFaultClass(t, err, fault.DependencyUnavailable)
	message := err.Error()
	for _, secret := range []string{"supersecret", "clientuser"} {
		if strings.Contains(message, secret) {
			t.Fatalf("TLS failure leaked URL credential %q: %v", secret, err)
		}
	}
	for line := range strings.SplitSeq(string(clientIdentity.keyPEM), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 16 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(message, line) {
			t.Fatalf("TLS failure leaked private-key material: %v", err)
		}
	}
	for line := range strings.SplitSeq(string(clientIdentity.certPEM), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 16 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(message, line) {
			t.Fatalf("TLS failure leaked certificate material: %v", err)
		}
	}

	// A CA file with unusable content is refused without echoing the content.
	garbageCA := writeTestPEM(t, "garbage-ca.pem", []byte(
		"-----BEGIN CERTIFICATE-----\nnotacertSUPERSECRETmaterial\n-----END CERTIFICATE-----\n"))
	_, err = Dial(ctx, Config{
		URL:            server.URL(),
		ConnectTimeout: 500 * time.Millisecond,
		TLS:            &TLSConfig{CAFile: garbageCA},
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if strings.Contains(err.Error(), "notacertSUPERSECRETmaterial") {
		t.Fatalf("invalid CA failure echoed the file content: %v", err)
	}

	// A half-configured client certificate is refused without rendering the
	// key file.
	_, err = Dial(ctx, Config{
		URL:            server.URL(),
		ConnectTimeout: 500 * time.Millisecond,
		TLS:            &TLSConfig{CAFile: ca.file, KeyFile: clientIdentity.keyFile},
	})
	requireFaultClass(t, err, fault.InvalidInput)
	for line := range strings.SplitSeq(string(clientIdentity.keyPEM), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 16 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(err.Error(), line) {
			t.Fatalf("configuration failure leaked private-key material: %v", err)
		}
	}
}
