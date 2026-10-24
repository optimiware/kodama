package factoryfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.invalid/inventory/test/support/clusterfake"
)

// NewTLS starts the fake behind HTTPS with a throwaway certificate authority and
// returns that authority's PEM. The delivery helper accepts only https origins
// (plain http only on loopback), so a fixture that containers reach over a
// non-loopback address must be an https origin trusted through SSL_CERT_FILE:
// the production validation stays as it is and the fixture adapts to it.
func NewTLS(t testing.TB, log *clusterfake.Timeline, hosts ...string) (*Server, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t043 factory fixture CA"}, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(6 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "t043 factory fixture"}, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(6 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else {
			leaf.DNSNames = append(leaf.DNSNames, h)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Log: log, contexts: map[string]map[string]any{}, revoked: map[string]bool{}, deps: map[string]*record{}, drop: map[string]int{}, down: map[string]int{}, revokeAt: map[string]string{}, Calls: map[string]int{}}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	s.HTTP = srv
	t.Cleanup(srv.Close)
	return s, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}
