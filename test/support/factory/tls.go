package factory

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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TLSFixture is the opt-in harness TLS material: one private per-run CA, one
// service certificate covering every host a real workload alias dials, the
// exported CA PEM and file, and a client transport that trusts only that CA.
//
// It is deliberately self-contained. Nothing here reads or writes
// http.DefaultTransport, http.DefaultClient or the process trust store, so a
// harness running beside other lanes cannot change how any other test verifies
// TLS. Verification is never disabled: the client validates the served
// certificate against the fixture CA with a TLS 1.2 floor.
type TLSFixture struct {
	// CAPEM is the PEM encoding of the private CA certificate. It is the only
	// trust material an ordinary client needs and the exact bytes CACertFile
	// contains.
	CAPEM []byte

	caCert     *x509.Certificate
	caKey      *ecdsa.PrivateKey
	leaf       *x509.Certificate
	server     *tls.Config
	client     *http.Transport
	caCertFile string
}

// tlsValidity bounds the fixture certificate lifetime. A factory harness is
// short lived, so one day is generous while keeping the certificate from
// looking like a long-lived reusable credential.
const tlsValidity = 24 * time.Hour

// serviceTLSHosts returns the exact service certificate names the opt-in TLS
// edge is wired to cover: the loopback names host-local clients and the
// composition dial, the two real workload aliases, and the concrete host
// address this run's workload edge binds. It is the single list server.go
// passes to newTLSFixture, so a name can only be dropped here and the unit
// test pins that exact set. The listen host is the route-derived interface
// address, never the unspecified wildcard.
func serviceTLSHosts(listenHost string) []string {
	return []string{
		"127.0.0.1",
		"::1",
		"localhost",
		"host.containers.internal",
		KubernetesHostAlias,
		listenHost,
	}
}

// newTLSFixture builds one private CA and one service certificate covering the
// supplied host names. A host that parses as an IP address becomes an
// IPAddresses SAN; every other host becomes a DNSNames SAN. The CA PEM is
// written to the test's temporary directory so a workload or Git helper can
// consume it through a path.
func newTLSFixture(t testing.TB, hosts ...string) *TLSFixture {
	t.Helper()
	if len(hosts) == 0 {
		t.Fatalf("factory: TLS fixture needs at least one service host")
	}
	now := time.Now().UTC()

	caKey := newFixtureKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          newCertificateSerial(t),
		Subject:               pkix.Name{CommonName: "Kodama Factory Test CA", Organization: []string{"Kodama Test"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(tlsValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("factory: create TLS fixture CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("factory: parse TLS fixture CA certificate: %v", err)
	}

	leafKey := newFixtureKey(t)
	leafTemplate := &x509.Certificate{
		SerialNumber: newCertificateSerial(t),
		Subject:      pkix.Name{CommonName: "kodama-factory-service", Organization: []string{"Kodama Test"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(tlsValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if host == "" {
			t.Fatalf("factory: TLS fixture host is empty")
		}
		if address := net.ParseIP(host); address != nil {
			leafTemplate.IPAddresses = append(leafTemplate.IPAddresses, address)
			continue
		}
		leafTemplate.DNSNames = append(leafTemplate.DNSNames, host)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("factory: create TLS fixture service certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("factory: parse TLS fixture service certificate: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("factory: TLS fixture CA PEM is not a usable certificate pool")
	}

	caFile := filepath.Join(t.TempDir(), "kodama-factory-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatalf("factory: write TLS fixture CA file: %v", err)
	}

	return &TLSFixture{
		CAPEM:  caPEM,
		caCert: caCert,
		caKey:  caKey,
		leaf:   leaf,
		server: &tls.Config{
			Certificates: []tls.Certificate{{
				Certificate: [][]byte{leafDER},
				PrivateKey:  leafKey,
				Leaf:        leaf,
			}},
			MinVersion: tls.VersionTLS12,
		},
		client: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}},
		caCertFile: caFile,
	}
}

// newFixtureKey returns one fresh P-256 private key for the fixture.
func newFixtureKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("factory: generate TLS fixture key: %v", err)
	}
	return key
}

// newCertificateSerial returns one unpredictable positive certificate serial.
func newCertificateSerial(t testing.TB) *big.Int {
	t.Helper()
	material := make([]byte, 16)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("factory: generate TLS fixture serial: %v", err)
	}
	serial := new(big.Int).SetBytes(material)
	serial.Add(serial, big.NewInt(1))
	return serial
}

// Leaf returns the parsed service certificate the fixture serves.
func (fixture *TLSFixture) Leaf() *x509.Certificate { return fixture.leaf }

// ServerConfig returns the server TLS configuration serving the fixture leaf.
// The returned value is the fixture's own; callers must not modify it.
func (fixture *TLSFixture) ServerConfig() *tls.Config { return fixture.server }

// Client returns an HTTP client that trusts only the fixture CA and never
// disables verification.
func (fixture *TLSFixture) Client() *http.Client {
	return &http.Client{Transport: fixture.client}
}

// CACertFile returns the path of the exported CA PEM.
func (fixture *TLSFixture) CACertFile() string { return fixture.caCertFile }

// String renders the fixture CA subject for diagnostics without exposing key
// material.
func (fixture *TLSFixture) String() string {
	return fmt.Sprintf("TLSFixture(CA=%s serial=%s)", fixture.caCert.Subject.CommonName, fixture.caCert.SerialNumber)
}
