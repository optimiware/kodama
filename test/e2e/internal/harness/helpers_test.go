//go:build e2e

package harness

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
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI issues a self-signed CA and one server leaf for the test API. The
// leaf carries the controlled DNS SAN the API helper verifies against.
func testPKI(t *testing.T) (string, tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test CA key: %v", err)
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Q01 test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create test CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse test CA: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kodama.test"},
		DNSNames:     []string{"kodama.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create test leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parse test leaf: %v", err)
	}

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if caPEM == nil {
		t.Fatal("encode test CA")
	}
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write test CA: %v", err)
	}
	return caPath, tls.Certificate{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
}

// validConfigEnv returns one complete, well-formed e2e environment. Each
// malformed-configuration case mutates exactly one value.
func validConfigEnv(t *testing.T) map[string]string {
	t.Helper()
	caPath, _ := testPKI(t)
	return map[string]string{
		string(InputAPIURL):           "https://127.0.0.1:18443",
		string(InputCAPEM):            caPath,
		string(InputTLSServerName):    "kodama.kodama-system.svc.cluster.local",
		string(InputToken):            "project-token-value",
		string(InputProjectID):        "tenant-a",
		string(InputKubeContext):      "kodama-stage-k8s",
		string(InputImage):            "docker.io/library/node:26.13.0-alpine",
		string(InputEvidenceDir):      t.TempDir(),
		string(InputHarnessRevision):  "be43abd",
		string(InputDeployedRevision): "be43abd",
	}
}

// envGetter adapts a map to the Config loader's read function.
func envGetter(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func repeat(value string, count int) string {
	out := make([]byte, 0, len(value)*count)
	for range count {
		out = append(out, value...)
	}
	return string(out)
}

// writeTempFile writes one bounded test file.
func writeTempFile(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/contents"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
