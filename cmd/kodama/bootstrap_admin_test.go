package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
)

// bootstrapEnv is one complete `kodama bootstrap-admin` environment. The blob
// content and the token are sentinels so a refusal can prove neither is
// rendered.
func bootstrapEnv(t *testing.T) (productionEnv, string) {
	t.Helper()
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(keys, "kek-v1.blob")
	if err := os.WriteFile(blob, []byte("SENTINEL-WRAPPED-KEY-BLOB"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("SENTINEL-OIDC-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KODAMA_DATABASE_URL":        "postgres://kodama_runtime:SENTINEL-DB-PW@db:5432/kodama",
		"KODAMA_INSTALLATION_ID":     "installation-1",
		"KODAMA_KEY_ADAPTER":         "scw-kms",
		"KODAMA_KEY_REF":             "scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
		"KODAMA_KEY_CURRENT_VERSION": "1",
		"KODAMA_KEY_BLOBS_DIR":       keys,
		"SCW_SECRET_KEY":             "SENTINEL-SCW-SECRET-KEY",
		"KODAMA_OIDC_ISSUER":         "https://idp.example.com",
		"KODAMA_OIDC_AUDIENCE":       "https://api.example.com",
		"KODAMA_OIDC_JWKS_URL":       "https://idp.example.com/jwks",
		"KODAMA_OIDC_TENANT_CLAIM":   "org_id",
		"KODAMA_TENANT":              "tenant-a",
		"KODAMA_PROFILE":             "production",
	}
	return func(name string) string { return env[name] }, token
}

func TestBootstrapAdminEntryRequiresItsFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags", nil, "bootstrap-admin requires --tenant-name and --token-file"},
		{"no tenant name", []string{"--token-file", "/tmp/token"}, "bootstrap-admin requires --tenant-name and --token-file"},
		{"no token", []string{"--tenant-name", "Tenant A", "--tenant", "tenant-a"}, "bootstrap-admin requires --tenant-name and --token-file"},
		{"bad tenant", []string{"--tenant-name", "Tenant A", "--token-file", "/tmp/token", "--tenant", "Not A Tenant"}, "tenant:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr lockedBuffer
			code := runBootstrapAdminEntry(context.Background(), tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q; want it to mention %q", stderr.String(), tc.want)
			}
			if stdout.String() != "" {
				t.Fatalf("a refused bootstrap wrote to stdout: %q", stdout.String())
			}
		})
	}
}

func TestBootstrapAdminRequiresEveryProductionSetting(t *testing.T) {
	for _, key := range []string{
		"KODAMA_DATABASE_URL", "KODAMA_INSTALLATION_ID", "KODAMA_KEY_REF", "KODAMA_KEY_BLOBS_DIR",
		"SCW_SECRET_KEY", "KODAMA_KEY_CURRENT_VERSION", "KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE",
		"KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
	} {
		t.Run(key, func(t *testing.T) {
			base, token := bootstrapEnv(t)
			getenv := func(name string) string {
				if name == key {
					return ""
				}
				return base(name)
			}
			_, err := bootstrapAdmin(context.Background(), getenv, id.TenantID("tenant-a"), "Tenant A", token)
			if err == nil {
				t.Fatalf("bootstrap accepted a missing %s", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v; want it to name %s", err, key)
			}
			assertBootstrapErrorSecretFree(t, err)
		})
	}
}

func TestBootstrapAdminRefusesAWeakKeyAdapter(t *testing.T) {
	for _, adapter := range []string{"", "keyfile", "ephemeral"} {
		t.Run(adapter, func(t *testing.T) {
			base, token := bootstrapEnv(t)
			getenv := func(name string) string {
				if name == "KODAMA_KEY_ADAPTER" {
					return adapter
				}
				return base(name)
			}
			_, err := bootstrapAdmin(context.Background(), getenv, id.TenantID("tenant-a"), "Tenant A", token)
			if err == nil || !strings.Contains(err.Error(), "scw-kms") {
				t.Fatalf("error = %v; want a scw-kms refusal", err)
			}
			assertBootstrapErrorSecretFree(t, err)
		})
	}
}

// TestBootstrapAdminRefusesAForeignKeyReference pins that selecting another
// cloud adapter does not reuse the configured reference: the adapter and the
// reference family must agree before any key is loaded.
func TestBootstrapAdminRefusesAForeignKeyReference(t *testing.T) {
	for _, adapter := range []string{"aws-kms", "gcp-kms"} {
		t.Run(adapter, func(t *testing.T) {
			base, token := bootstrapEnv(t)
			getenv := func(name string) string {
				if name == "KODAMA_KEY_ADAPTER" {
					return adapter
				}
				return base(name)
			}
			_, err := bootstrapAdmin(context.Background(), getenv, id.TenantID("tenant-a"), "Tenant A", token)
			if err == nil || !strings.Contains(err.Error(), "key reference") {
				t.Fatalf("error = %v; want a key reference refusal", err)
			}
			assertBootstrapErrorSecretFree(t, err)
		})
	}
}

func TestBootstrapAdminNeverPrintsKeyMaterial(t *testing.T) {
	base, token := bootstrapEnv(t)
	getenv := func(name string) string {
		// Force the run to stop after the key material is loaded.
		if name == "KODAMA_OIDC_ISSUER" {
			return ""
		}
		return base(name)
	}
	_, err := bootstrapAdmin(context.Background(), getenv, id.TenantID("tenant-a"), "Tenant A", token)
	if err == nil {
		t.Fatal("bootstrap succeeded with an incomplete environment")
	}
	assertBootstrapErrorSecretFree(t, err)
}

// bootstrapEntryEnvironment sets one complete os.Getenv environment for the
// real `kodama bootstrap-admin` entry and returns the token file. Every secret
// is a distinct sentinel so a failure's two streams can be scanned.
func bootstrapEntryEnvironment(t *testing.T) (tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "kek-v1.blob"), []byte("SENTINEL-WRAPPED-KEY-BLOB"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile = filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("SENTINEL-OIDC-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"KODAMA_DATABASE_URL":        "postgres://kodama_runtime:SENTINEL-DB-PW@db:5432/kodama",
		"KODAMA_INSTALLATION_ID":     "installation-1",
		"KODAMA_KEY_ADAPTER":         "scw-kms",
		"KODAMA_KEY_REF":             "scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
		"KODAMA_KEY_CURRENT_VERSION": "1",
		"KODAMA_KEY_BLOBS_DIR":       keys,
		"SCW_SECRET_KEY":             "SENTINEL-SCW-SECRET-KEY",
		"KODAMA_OIDC_ISSUER":         "https://idp.example.com",
		"KODAMA_OIDC_AUDIENCE":       "https://api.example.com",
		"KODAMA_OIDC_JWKS_URL":       "https://idp.example.com/jwks",
		"KODAMA_OIDC_TENANT_CLAIM":   "org_id",
		"KODAMA_TENANT":              "tenant-a",
		"KODAMA_PROFILE":             "production",
	} {
		t.Setenv(name, value)
	}
	return tokenFile
}

// TestBootstrapAdminEntryExitCodesAndSecretFreeStreams drives the real entry
// function: a configuration refusal exits 2, a runtime/configuration failure
// inside the bootstrap exits 1 (so a regression to a success or a different
// code is caught), and neither stream carries key material or a password.
func TestBootstrapAdminEntryExitCodesAndSecretFreeStreams(t *testing.T) {
	t.Run("configuration refusal exits 2", func(t *testing.T) {
		bootstrapEntryEnvironment(t)
		var stdout, stderr lockedBuffer
		code := runBootstrapAdminEntry(context.Background(), []string{"--tenant-name", "Tenant A"}, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit code = %d; want 2 for a missing token file (stderr=%q)", code, stderr.String())
		}
		if stdout.String() != "" {
			t.Fatalf("a configuration refusal wrote stdout: %q", stdout.String())
		}
		assertBootstrapStreamsSecretFree(t, stdout.String(), stderr.String())
	})

	t.Run("bootstrap failure exits 1", func(t *testing.T) {
		tokenFile := bootstrapEntryEnvironment(t)
		// The key material is complete; the identity provider is not
		// configured, so the run fails after parsing flags.
		t.Setenv("KODAMA_OIDC_ISSUER", "")
		var stdout, stderr lockedBuffer
		code := runBootstrapAdminEntry(context.Background(), []string{"--tenant-name", "Tenant A", "--token-file", tokenFile}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 for a bootstrap failure (stderr=%q)", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "KODAMA_OIDC_ISSUER") {
			t.Fatalf("stderr = %q; want it to name the missing setting", stderr.String())
		}
		if stdout.String() != "" {
			t.Fatalf("a failed bootstrap wrote stdout: %q", stdout.String())
		}
		assertBootstrapStreamsSecretFree(t, stdout.String(), stderr.String())
	})

	t.Run("KODAMA_TENANT is the --tenant default", func(t *testing.T) {
		tokenFile := bootstrapEntryEnvironment(t)
		t.Setenv("KODAMA_TENANT", "tenant-env")
		// No --tenant flag: the environment's tenant must be parsed and
		// accepted, so the run proceeds past tenant validation to the missing
		// issuer (exit 1). A lost default would fail tenant parsing (exit 2).
		t.Setenv("KODAMA_OIDC_ISSUER", "")
		var stdout, stderr lockedBuffer
		code := runBootstrapAdminEntry(context.Background(), []string{"--tenant-name", "Tenant A", "--token-file", tokenFile}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d; want 1 (the environment tenant was accepted); stderr=%q", code, stderr.String())
		}
		if strings.Contains(stderr.String(), "tenant:") {
			t.Fatalf("stderr = %q; KODAMA_TENANT was not the --tenant default", stderr.String())
		}
		assertBootstrapStreamsSecretFree(t, stdout.String(), stderr.String())
	})
}

// TestBootstrapAdminRefusalNeverRendersConfiguredSecrets proves a refusal
// whose own configured value carries a credential never renders that value or
// the credential, whichever layer produced the error.
func TestBootstrapAdminRefusalNeverRendersConfiguredSecrets(t *testing.T) {
	const sentinel = "SENTINEL-CONFIGURED-PW"
	for _, tc := range []struct{ name, key, value, want string }{
		{"invalid database URL", "KODAMA_DATABASE_URL", "postgres://kodama_runtime:" + sentinel + "@database:notaport/kodama", "database connection string is invalid"},
		{"invalid OIDC issuer scheme", "KODAMA_OIDC_ISSUER", "ftp://idp:" + sentinel + "@example.com", "OIDC issuer must be an explicit https URL"},
		{"invalid JWKS URL", "KODAMA_OIDC_JWKS_URL", "http://idp.example.com/jwks?token=" + sentinel, "OIDC JWKS URL must be an explicit https URL"},
		{"invalid tenant claim", "KODAMA_OIDC_TENANT_CLAIM", "org id " + sentinel, "OIDC tenant claim name is empty or contains whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, token := bootstrapEnv(t)
			getenv := func(name string) string {
				if name == "SCW_SECRET_KEY" {
					return "SENTINEL-SCW-SECRET-KEY"
				}
				if name == "KODAMA_TENANT" {
					return "tenant-a"
				}
				if name == tc.key {
					return tc.value
				}
				return base(name)
			}
			_, err := bootstrapAdmin(context.Background(), getenv, id.TenantID("tenant-a"), "Tenant A", token)
			if err == nil {
				t.Fatalf("bootstrap accepted %s = %q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal = %v; want the named reason %q", err, tc.want)
			}
			if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), tc.value) {
				t.Fatalf("refusal rendered the configured value: %v", err)
			}
			assertBootstrapErrorSecretFree(t, err)
		})
	}
}

func assertBootstrapStreamsSecretFree(t *testing.T, stdout, stderr string) {
	t.Helper()
	combined := stdout + stderr
	for _, secret := range []string{
		"SENTINEL-WRAPPED-KEY-BLOB", "SENTINEL-OIDC-TOKEN",
		"SENTINEL-SCW-SECRET-KEY", "SENTINEL-DB-PW",
	} {
		if strings.Contains(combined, secret) {
			t.Fatalf("bootstrap output rendered key or credential material: %q", combined)
		}
	}
}

func assertBootstrapErrorSecretFree(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{
		"SENTINEL-WRAPPED-KEY-BLOB", "SENTINEL-OIDC-TOKEN",
		"SENTINEL-SCW-SECRET-KEY", "SENTINEL-DB-PW",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error rendered key or credential material: %v", err)
		}
	}
}
