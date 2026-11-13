package podman

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func generateTestCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-local-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestPodmanWorkloadCAMountsReadOnlyCertAndExposesEnvOptIn(t *testing.T) {
	caPEM := generateTestCAPEM(t)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	fx := newFixture(t)
	adapter, err := New(Config{
		Libpod:         fx.libpod,
		Files:          fx.files,
		Root:           fx.root,
		KodamaURL:      testKodamaURL,
		WorkloadCAFile: caFile,
		Cleanup:        CleanupPolicy{Attempts: 2},
		Clock:          fx.clock,
	})
	if err != nil {
		t.Fatalf("New with WorkloadCAFile: %v", err)
	}
	fx.adapter = adapter
	// Attempt to spoof KODAMA_CA_FILE in literal env; the adapter must override
	// it with the authoritative /kodama/ca.crt path.
	fx.request.Container.Env = append(fx.request.Container.Env,
		definition.EnvVar{Name: EnvWorkloadCAFile, Value: "/tmp/evil.crt"})

	fx.prepare(t)

	mountedHostCA := fx.hostPath(WorkloadCAPath)
	if got := readHostFile(t, mountedHostCA); string(got) != string(caPEM) {
		t.Fatalf("materialized /kodama/ca.crt mismatch")
	}
	if mode := hostFileMode(t, mountedHostCA); mode != 0o444 {
		t.Fatalf("/kodama/ca.crt mode = %o; want 0444", mode)
	}

	if _, err := adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	specs := fx.libpod.createSpecs()
	if len(specs) != 1 {
		t.Fatalf("createSpecs = %d; want 1", len(specs))
	}
	wantCAEnv := EnvWorkloadCAFile + "=" + WorkloadCAPath
	if !slices.Contains(specs[0].Env, wantCAEnv) {
		t.Fatalf("container env %v missing %q", specs[0].Env, wantCAEnv)
	}
	if slices.Contains(specs[0].Env, EnvWorkloadCAFile+"=/tmp/evil.crt") {
		t.Fatalf("container env %v allowed spoofing %s", specs[0].Env, EnvWorkloadCAFile)
	}
}

func TestPodmanWorkloadCARefusesInvalidFiles(t *testing.T) {
	badPEM := filepath.Join(t.TempDir(), "bad.crt")
	if err := os.WriteFile(badPEM, []byte("not-a-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
	}{
		{"relative path", "ca.crt"},
		{"missing file", filepath.Join(t.TempDir(), "absent.crt")},
		{"non-PEM content", badPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Libpod:         newFakeLibpod(),
				Files:          newFakeFiles(),
				Root:           t.TempDir(),
				KodamaURL:      testKodamaURL,
				WorkloadCAFile: tc.path,
				Cleanup:        CleanupPolicy{Attempts: 1},
				Clock:          newFakeClock(),
			})
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}
