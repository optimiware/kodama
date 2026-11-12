//go:build qualification

package production

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
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
		Subject:               pkix.Name{CommonName: "Q02 test CA"},
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

// validConfigEnv returns one complete, well-formed qualification environment.
// Each malformed-configuration case mutates exactly one value.
func validConfigEnv(t *testing.T) map[string]string {
	t.Helper()
	caPath, _ := testPKI(t)
	return map[string]string{
		string(InputAPIURL):           "https://127.0.0.1:18443",
		string(InputCAPEM):            caPath,
		string(InputTLSServerName):    "kodama.kodama-system.svc.cluster.local",
		string(InputAdminToken):       "admin-token-value",
		string(InputDeveloperToken):   "developer-token-value",
		string(InputTenantID):         "tenant-a",
		string(InputProjectID):        "tenant-a",
		string(InputTenantNamespace):  "kodama-tenant-tenant-a",
		string(InputSecondTenantID):   "tenant-b",
		string(InputSecondProjectID):  "tenant-b",
		string(InputSecondNamespace):  "kodama-tenant-tenant-b",
		string(InputSecondAdminToken): "second-admin-token-value",
		string(InputGitRepositoryA):   "smoke/tenant-a",
		string(InputGitRepositoryB):   "smoke/tenant-b",
		string(InputGitServiceA):      "kodama-git-tenant-a",
		string(InputGitServiceB):      "kodama-git-tenant-b",
		string(InputKubeContext):      "kodama-stage-k8s",
		string(InputImage):            "rg.example.test/kodama@sha256:" + repeat("a", 64),
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

// installFakeExecutable writes one executable fixture and prepends its private
// directory to PATH for the duration of the test. Fixtures never touch a real
// cluster.
func installFakeExecutable(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("chmod fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}
