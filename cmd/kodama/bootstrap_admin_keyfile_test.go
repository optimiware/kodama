package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// sealCapture wraps the real keyfile cipher and records what it sealed.
type sealCapture struct {
	inner      secret.EnvelopeCipher
	provider   keys.Provider
	associated []secret.AssociatedData
	envelopes  []secret.Envelope
}

func (capture *sealCapture) Seal(ctx context.Context, associated secret.AssociatedData, plaintext []byte) (secret.Envelope, error) {
	envelope, err := capture.inner.Seal(ctx, associated, plaintext)
	if err == nil {
		capture.associated = append(capture.associated, associated)
		capture.envelopes = append(capture.envelopes, envelope)
	}
	return envelope, err
}

func (capture *sealCapture) Open(ctx context.Context, associated secret.AssociatedData, envelope secret.Envelope) ([]byte, error) {
	return capture.inner.Open(ctx, associated, envelope)
}

// testProfileBootstrap turns the production-shaped OIDC fixture into the test
// profile: keyfile settings instead of the cloud KMS, no cloud credential.
func testProfileBootstrap(t *testing.T) (*oidcBootstrapFixture, string) {
	t.Helper()
	fixture := newOIDCBootstrapFixture(t)
	keyfile := filepath.Join(t.TempDir(), "keys", "kek-v1.key")
	fixture.values["KODAMA_PROFILE"] = "test"
	fixture.values["KODAMA_KEY_ADAPTER"] = "keyfile"
	fixture.values["KODAMA_KEY_REF"] = "keyfile:kek-v1"
	fixture.values["KODAMA_KEYFILE"] = keyfile
	fixture.values["KODAMA_INSTALLATION_ID"] = "installation-test-1"
	delete(fixture.values, "KODAMA_KEY_BLOBS_DIR")
	delete(fixture.values, "SCW_SECRET_KEY")
	return fixture, keyfile
}

func keyfileProbeDeps(probe *bootstrapAdminProbe, capture *sealCapture) bootstrapAdminDeps {
	deps := probe.deps()
	deps.newCipher = func(provider keys.Provider) (secret.EnvelopeCipher, error) {
		inner, err := keys.NewCipher(provider, rand.Reader)
		if err != nil {
			return nil, err
		}
		capture.inner = inner
		capture.provider = provider
		return capture, nil
	}
	return deps
}

func TestBootstrapAdminTestProfileUsesKeyfile(t *testing.T) {
	fixture, keyfile := testProfileBootstrap(t)
	fixture.setToken(t, fixture.keys.signToken(t, "RS256", fixture.claims()))
	tenant := id.TenantID(oidcTestTenant)

	// Without the explicit initialization flag a missing key is a refusal and
	// no key is created.
	probe := newBootstrapAdminProbe()
	capture := &sealCapture{}
	if _, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, tenant, "Tenant A", fixture.tokenFile,
		bootstrapKeyOptions{profile: "test"}, keyfileProbeDeps(probe, capture)); err == nil {
		t.Fatal("bootstrap accepted a missing keyfile without --init-keyfile")
	}
	if _, err := os.Lstat(keyfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bootstrap created a keyfile without --init-keyfile")
	}
	if probe.executor.calls != 0 {
		t.Fatal("a missing keyfile still reached the commit unit")
	}

	// Explicit initialization creates the key, seals the tenant derivation key
	// with it and commits through the runtime database only.
	_, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, tenant, "Tenant A", fixture.tokenFile,
		bootstrapKeyOptions{profile: "test", initKeyfile: true}, keyfileProbeDeps(probe, capture))
	if !errors.Is(err, errRecordingExecutorReached) {
		t.Fatalf("err = %v; want the verified token to reach the commit unit", err)
	}
	if probe.connString != fixture.values["KODAMA_DATABASE_URL"] {
		t.Fatal("bootstrap did not commit through the runtime database URL")
	}
	if probe.config.KeyVersion != secret.MinKeyVersion || probe.config.InstallationID != "installation-test-1" {
		t.Fatalf("bootstrap config = %+v", probe.config)
	}
	original, err := os.ReadFile(keyfile)
	if err != nil || len(original) != secret.DataKeyBytes {
		t.Fatalf("initialized keyfile = %d bytes, %v", len(original), err)
	}
	if len(capture.envelopes) != 1 {
		t.Fatalf("sealed envelopes = %d; want the one tenant derivation key", len(capture.envelopes))
	}
	associated, envelope := capture.associated[0], capture.envelopes[0]
	if associated.InstallationID != "installation-test-1" || associated.SecretID != "tenant-derivation-key" ||
		associated.Name != "TENANT_DERIVATION_KEY" || associated.KeyVersion != secret.MinKeyVersion {
		t.Fatalf("derivation key identity = %+v; want the server's fixed derivation identity", associated)
	}
	if envelope.KeyRef != "keyfile:kek-v1" {
		t.Fatalf("envelope key reference = %q; want the keyfile reference", envelope.KeyRef)
	}
	// A separately reconstructed provider over the same file (the server's
	// view after a restart) opens the committed envelope.
	reconstructed, err := keys.NewKeyfile("keyfile:kek-v1", keyfile, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _ := keys.NewCipher(reconstructed, rand.Reader)
	if plaintext, err := reopened.Open(context.Background(), associated, envelope); err != nil || len(plaintext) == 0 {
		t.Fatalf("the reconstructed keyfile provider could not open the derivation key: %v", err)
	}

	// A retry without the flag reuses the published key; a retry with the
	// flag refuses before touching the database and never replaces it.
	calls := probe.executor.calls
	if _, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, tenant, "Tenant A", fixture.tokenFile,
		bootstrapKeyOptions{profile: "test"}, keyfileProbeDeps(probe, &sealCapture{})); !errors.Is(err, errRecordingExecutorReached) {
		t.Fatalf("retry without initialization = %v; want the commit unit", err)
	}
	calls++
	if _, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, tenant, "Tenant A", fixture.tokenFile,
		bootstrapKeyOptions{profile: "test", initKeyfile: true}, keyfileProbeDeps(probe, &sealCapture{})); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second initialization = %v; want an already-exists refusal", err)
	}
	if probe.executor.calls != calls {
		t.Fatal("a refused initialization reached the commit unit")
	}
	if after, _ := os.ReadFile(keyfile); !bytes.Equal(original, after) {
		t.Fatal("bootstrap replaced an existing key")
	}

	// Every refused token commits nothing under the test profile too.
	for _, refused := range []struct {
		name   string
		claims func(map[string]any)
	}{
		{"wrong issuer", func(claims map[string]any) { claims["iss"] = "https://attacker.example" }},
		{"wrong audience", func(claims map[string]any) { claims["aud"] = "https://other.example" }},
		{"wrong tenant", func(claims map[string]any) { claims["org"] = "tenant-b" }},
		{"project scoped", func(claims map[string]any) { claims["project"] = "project-1" }},
		{"expired", func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Hour).Unix() }},
	} {
		t.Run(refused.name, func(t *testing.T) {
			claims := fixture.claims()
			refused.claims(claims)
			signed := fixture.keys.signToken(t, "RS256", claims)
			fixture.setToken(t, signed)
			before := probe.executor.calls
			_, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, tenant, "Tenant A", fixture.tokenFile,
				bootstrapKeyOptions{profile: "test"}, keyfileProbeDeps(probe, &sealCapture{}))
			if err == nil || errors.Is(err, errRecordingExecutorReached) || probe.executor.calls != before {
				t.Fatalf("%s reached the commit unit: %v", refused.name, err)
			}
			if strings.Contains(err.Error(), signed) || bytes.Contains([]byte(err.Error()), original) {
				t.Fatalf("refusal rendered secret material: %v", err)
			}
		})
	}
}

// TestBootstrapAdminTestProfileRequiresItsSettings is the test-profile
// counterpart of TestBootstrapAdminRequiresEveryProductionSetting: the keyfile
// path needs no cloud credential but every keyfile and identity setting.
func TestBootstrapAdminTestProfileRequiresItsSettings(t *testing.T) {
	for _, key := range []string{
		"KODAMA_DATABASE_URL", "KODAMA_INSTALLATION_ID", "KODAMA_KEY_ADAPTER", "KODAMA_KEY_REF", "KODAMA_KEYFILE",
		"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
	} {
		t.Run(key, func(t *testing.T) {
			fixture, keyfile := testProfileBootstrap(t)
			fixture.setToken(t, fixture.keys.signToken(t, "RS256", fixture.claims()))
			delete(fixture.values, key)
			probe := newBootstrapAdminProbe()
			_, err := bootstrapAdminKeyed(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant), "Tenant A",
				fixture.tokenFile, bootstrapKeyOptions{profile: "test", initKeyfile: true}, keyfileProbeDeps(probe, &sealCapture{}))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing %s = %v; want a refusal naming it", key, err)
			}
			if probe.executor.calls != 0 {
				t.Fatal("an incomplete configuration reached the commit unit")
			}
			if key != "KODAMA_KEYFILE" {
				if _, statErr := os.Lstat(keyfile); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("an incomplete configuration (%s) initialized a keyfile", key)
				}
			}
		})
	}
}
