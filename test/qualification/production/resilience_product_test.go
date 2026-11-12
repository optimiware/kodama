//go:build qualification

package production

// Q02 product-failure probes that need no live stage: the production
// entrypoint's own refusals, the cloud KMS negative matrix and the identity
// credential matrix. They run the real command/package code, so a regression in
// the product fails the qualification suite even when no stage is configured.
// Unlike the stage probes they need no stage inputs and therefore never skip.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// resilienceProductionEnv is the complete environment the production entrypoint
// reads before it can build a composition. Every value is a non-secret dummy;
// the tests change exactly one setting and require the entrypoint to refuse
// before it contacts a cluster, a database or a KMS.
func resilienceProductionEnv(dir string) map[string]string {
	return map[string]string{
		"KODAMA_DATABASE_URL":                "postgres://kodama_runtime:pw@database:5432/kodama?sslmode=verify-full",
		"KODAMA_CONTROLLER_DATABASE_URL":     "postgres://kodama_controller:pw@database:5432/kodama?sslmode=verify-full",
		"KODAMA_PUBLISHER_DATABASE_URL":      "postgres://kodama_publisher:pw@database:5432/kodama?sslmode=verify-full",
		"KODAMA_AUTH_DATABASE_URL":           "postgres://kodama_auth:pw@database:5432/kodama?sslmode=verify-full",
		"KODAMA_NATS_URL":                    "tls://nats:4222",
		"KODAMA_NATS_CA_FILE":                filepath.Join(dir, "ca.crt"),
		"KODAMA_NATS_CERT_FILE":              filepath.Join(dir, "tls.crt"),
		"KODAMA_NATS_KEY_FILE":               filepath.Join(dir, "tls.key"),
		"KODAMA_KEY_ADAPTER":                 "scw-kms",
		"KODAMA_KEY_REF":                     "scw-kms:key-1",
		"KODAMA_KEY_BLOBS_DIR":               dir,
		"KODAMA_KEY_CURRENT_VERSION":         "1",
		"SCW_SECRET_KEY":                     "dummy-scaleway-secret",
		"KODAMA_TLS_DIR":                     dir,
		"KODAMA_OIDC_ISSUER":                 "https://issuer.example",
		"KODAMA_OIDC_AUDIENCE":               "kodama",
		"KODAMA_OIDC_JWKS_URL":               "https://issuer.example/jwks",
		"KODAMA_OIDC_TENANT_CLAIM":           "org_id",
		"KODAMA_WORKSPACE_PVC_STORAGE_CLASS": "standard",
		"KODAMA_WORKSPACE_PVC_SIZE":          "1Gi",
		"KODAMA_WORKSPACE_PVC_ACCESS_MODE":   "ReadWriteOnce",
		"KODAMA_TENANT":                      "tenant-a",
		"KODAMA_INSTALLATION_ID":             "installation-qualification",
		"KODAMA_DATA_DIR":                    dir,
		"KODAMA_PUBLIC_URL":                  "https://kodama.example",
	}
}

// writeEntrypointTLSDir writes one self-signed installation CA and server
// certificate. The entrypoint only needs the files to parse; a refusal after
// this point proves the adapter/identity rule, not a missing fixture.
func writeEntrypointTLSDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama qualification CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kodama.kodama-system.svc"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	files := map[string][]byte{
		"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// resilienceBuildEntrypoint builds the real `kodama` command once. A missing go
// toolchain is a named skip; it is never a pass.
func resilienceBuildEntrypoint(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "kodama-qualification")
	command := exec.Command("go", "build", "-o", binary, "./cmd/kodama")
	command.Dir = root
	command.Env = os.Environ()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			if os.Getenv("Q02_REQUIRE_STAGE") == "1" || os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
				t.Fatalf("unconfigured Q02 tool: go toolchain is not on PATH: %v", err)
			}
			t.Skip("unconfigured Q02 tool: go toolchain is not on PATH")
		}
		t.Fatalf("build the production entrypoint: %v (stderr %q)", err, stderr.String())
	}
	return binary
}

// resilienceRunEntrypoint runs `kodama server --profile=production` with the
// exact environment and returns its exit code and stderr. A process that does
// not refuse within the bound fails the probe.
func resilienceRunEntrypoint(t *testing.T, binary string, env map[string]string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "server", "--profile=production")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	for name, value := range env {
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("the production entrypoint neither served nor refused within 20s; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run the production entrypoint: %v", err)
		}
		code = exitError.ExitCode()
	}
	return code, stderr.String()
}

// TestHarnessProductionEntrypointRefusals is Q02 failure probe (g5). It runs
// the real production entrypoint and requires it to refuse: weaker test/dev
// key adapters, plaintext broker transport, empty required OIDC settings and a
// missing key reference. Each refusal must be non-zero, name the setting and
// never render a configured secret.
func TestHarnessProductionEntrypointRefusals(t *testing.T) {
	binary := resilienceBuildEntrypoint(t)
	tlsDir := writeEntrypointTLSDir(t)

	// An "empty" case sets the setting explicitly empty instead of unsetting
	// it: production must refuse a present-but-empty required value, not only
	// an absent one, and an absent value would not distinguish the two.
	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{
			name:   "test keyfile adapter",
			mutate: func(env map[string]string) { env["KODAMA_KEY_ADAPTER"] = "keyfile" },
			want:   "KODAMA_KEY_ADAPTER",
		},
		{
			name:   "development ephemeral adapter",
			mutate: func(env map[string]string) { env["KODAMA_KEY_ADAPTER"] = "ephemeral" },
			want:   "KODAMA_KEY_ADAPTER",
		},
		{
			name:   "plaintext broker",
			mutate: func(env map[string]string) { env["KODAMA_NATS_URL"] = "nats://nats:4222" },
			want:   "NATS URL",
		},
		{
			name:   "empty OIDC issuer",
			mutate: func(env map[string]string) { env["KODAMA_OIDC_ISSUER"] = "" },
			want:   "KODAMA_OIDC_ISSUER",
		},
		{
			name:   "empty OIDC audience",
			mutate: func(env map[string]string) { env["KODAMA_OIDC_AUDIENCE"] = "" },
			want:   "KODAMA_OIDC_AUDIENCE",
		},
		{
			name:   "empty OIDC JWKS",
			mutate: func(env map[string]string) { env["KODAMA_OIDC_JWKS_URL"] = "" },
			want:   "KODAMA_OIDC_JWKS_URL",
		},
		{
			name:   "empty OIDC tenant claim",
			mutate: func(env map[string]string) { env["KODAMA_OIDC_TENANT_CLAIM"] = "" },
			want:   "KODAMA_OIDC_TENANT_CLAIM",
		},
		{
			name:   "empty key reference",
			mutate: func(env map[string]string) { env["KODAMA_KEY_REF"] = "" },
			want:   "KODAMA_KEY_REF",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := resilienceProductionEnv(tlsDir)
			testCase.mutate(env)
			code, stderr := resilienceRunEntrypoint(t, binary, env)
			if code == 0 {
				t.Fatalf("the production entrypoint accepted %s", testCase.name)
			}
			if !strings.Contains(stderr, testCase.want) {
				t.Fatalf("refusal for %s = %q; want it to name %q", testCase.name, stderr, testCase.want)
			}
			for _, secret := range []string{"dummy-scaleway-secret", "kodama_runtime:pw", "kodama_controller:pw"} {
				if strings.Contains(stderr, secret) {
					t.Fatalf("refusal for %s rendered a configured secret", testCase.name)
				}
			}
		})
	}
}

// resilienceFakeKMS is a bounded KMS port: it records calls and returns one
// configured result. It never logs key material.
type resilienceFakeKMS struct {
	material []byte
	err      error
	calls    int
	lastRef  secret.KeyRef
}

func (client *resilienceFakeKMS) Decrypt(_ context.Context, keyRef secret.KeyRef, _ []byte) ([]byte, error) {
	client.calls++
	client.lastRef = keyRef
	if client.err != nil {
		return nil, client.err
	}
	return append([]byte(nil), client.material...), nil
}

// TestHarnessKMSNegativeMatrix is Q02 failure probe (g6). It exercises the
// production KMS-backed key provider through the real package: a foreign key
// family, unknown version, provider failure, wrong key length and a tampered
// envelope are refused without returning plaintext, and the KMS is not called
// for a reference/version that was never configured.
func TestHarnessKMSNegativeMatrix(t *testing.T) {
	ctx := context.Background()
	kek := bytes.Repeat([]byte{0x42}, secret.DataKeyBytes)
	cloud := keys.CloudConfig{
		KeyRef:   secret.KeyRef("scw-kms:key-1"),
		Current:  1,
		Versions: map[secret.KeyVersion][]byte{1: []byte("opaque-blob")},
	}
	associated := []byte("installation/tenant/project/secret/v1")
	request := secret.WrapRequest{
		KeyRef:         cloud.KeyRef,
		KeyVersion:     1,
		Plaintext:      bytes.Repeat([]byte{0x11}, secret.DataKeyBytes),
		AssociatedData: associated,
	}

	t.Run("foreign family is refused before any KMS call", func(t *testing.T) {
		client := &resilienceFakeKMS{material: kek}
		if _, err := keys.NewScaleway(client, keys.CloudConfig{
			KeyRef:   secret.KeyRef("aws-kms:key-1"),
			Current:  1,
			Versions: map[secret.KeyVersion][]byte{1: []byte("opaque-blob")},
		}, rand.Reader); err == nil {
			t.Fatal("NewScaleway accepted an AWS key reference")
		}
		if client.calls != 0 {
			t.Fatalf("foreign family construction reached the KMS %d time(s)", client.calls)
		}

		client = &resilienceFakeKMS{material: kek}
		provider, err := keys.NewScaleway(client, cloud, rand.Reader)
		if err != nil {
			t.Fatalf("NewScaleway: %v", err)
		}
		foreign := request
		foreign.KeyRef = secret.KeyRef("gcp-kms:key-1")
		if _, err := provider.Wrap(ctx, foreign); err == nil {
			t.Fatal("Wrap accepted a foreign key reference")
		}
		if client.calls != 0 {
			t.Fatalf("foreign reference reached the KMS %d time(s)", client.calls)
		}
	})

	t.Run("unknown version is refused before any KMS call", func(t *testing.T) {
		client := &resilienceFakeKMS{material: kek}
		provider, err := keys.NewScaleway(client, cloud, rand.Reader)
		if err != nil {
			t.Fatalf("NewScaleway: %v", err)
		}
		unknown := request
		unknown.KeyVersion = 9
		if _, err := provider.Wrap(ctx, unknown); err == nil {
			t.Fatal("Wrap accepted an unconfigured key version")
		}
		if client.calls != 0 {
			t.Fatalf("unknown version reached the KMS %d time(s)", client.calls)
		}
	})

	t.Run("provider failure is secret-free and returns no plaintext", func(t *testing.T) {
		client := &resilienceFakeKMS{err: errors.New("SENTINEL-KMS-RESPONSE")}
		provider, err := keys.NewScaleway(client, cloud, rand.Reader)
		if err != nil {
			t.Fatalf("NewScaleway: %v", err)
		}
		wrapped, err := provider.Wrap(ctx, request)
		if err == nil {
			t.Fatal("Wrap succeeded while the KMS failed")
		}
		if strings.Contains(err.Error(), "SENTINEL-KMS-RESPONSE") {
			t.Fatalf("KMS failure rendered the provider response: %v", err)
		}
		if len(wrapped.Wrapped) != 0 || len(wrapped.Nonce) != 0 {
			t.Fatal("a failed wrap returned key material")
		}
	})

	t.Run("wrong key length is refused", func(t *testing.T) {
		client := &resilienceFakeKMS{material: bytes.Repeat([]byte{0x01}, secret.DataKeyBytes-1)}
		provider, err := keys.NewScaleway(client, cloud, rand.Reader)
		if err != nil {
			t.Fatalf("NewScaleway: %v", err)
		}
		if _, err := provider.Wrap(ctx, request); err == nil {
			t.Fatal("Wrap accepted a KMS key of the wrong length")
		}
	})

	t.Run("tampered envelope is refused", func(t *testing.T) {
		client := &resilienceFakeKMS{material: kek}
		provider, err := keys.NewScaleway(client, cloud, rand.Reader)
		if err != nil {
			t.Fatalf("NewScaleway: %v", err)
		}
		wrapped, err := provider.Wrap(ctx, request)
		if err != nil {
			t.Fatalf("Wrap: %v", err)
		}
		if _, err := provider.Unwrap(ctx, secret.UnwrapRequest{
			KeyRef:         wrapped.KeyRef,
			KeyVersion:     wrapped.KeyVersion,
			Wrapped:        wrapped.Wrapped,
			Nonce:          wrapped.Nonce,
			AssociatedData: associated,
		}); err != nil {
			t.Fatalf("a genuine envelope did not unwrap: %v", err)
		}
		tampered := wrapped
		tampered.Wrapped = append([]byte(nil), wrapped.Wrapped...)
		tampered.Wrapped[len(tampered.Wrapped)-1] ^= 0xff
		plaintext, err := provider.Unwrap(ctx, secret.UnwrapRequest{
			KeyRef:         tampered.KeyRef,
			KeyVersion:     tampered.KeyVersion,
			Wrapped:        tampered.Wrapped,
			Nonce:          tampered.Nonce,
			AssociatedData: associated,
		})
		if err == nil {
			t.Fatal("Unwrap accepted a tampered envelope")
		}
		if len(plaintext) != 0 {
			t.Fatal("a failed unwrap returned plaintext")
		}
	})
}

// TestHarnessIdentityCredentialMatrix is Q02 failure probe (g7). It exercises
// the real authorization matrix for every credential kind and pins the
// no-fallback invariants: Admin is not Reviewer, a service never becomes
// Admin, an execution cannot resolve requests or administer, a foreign tenant
// scope never authorizes by role alone, a disabled principal is refused and an
// unknown operation is an error.
func TestHarnessIdentityCredentialMatrix(t *testing.T) {
	matrix := authorize.NewMatrix()
	ctx := context.Background()
	tenant := tenancy.Scope{TenantID: "tenant-a"}
	project := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	foreign := tenancy.Scope{TenantID: "tenant-b", ProjectID: "project-b"}

	human := func(label string, role tenancy.Role, scope tenancy.Scope, enabled bool) identity.Principal {
		return identity.Principal{
			ID:      id.PrincipalID("principal-" + label),
			Kind:    identity.KindHuman,
			Scope:   scope,
			Roles:   []tenancy.Role{role},
			Enabled: enabled,
			Version: 1,
		}
	}
	decision := func(t *testing.T, principal identity.Principal, operation authorize.OperationName, scope tenancy.Scope) authorize.Decision {
		t.Helper()
		got, err := matrix.Authorize(ctx, principal, operation, scope)
		if err != nil {
			t.Fatalf("%s for %s/%s: unexpected error: %v", operation, principal.Kind, principal.ID, err)
		}
		if !got.Valid() {
			t.Fatalf("%s for %s/%s: decision %q is not frozen", operation, principal.Kind, principal.ID, got)
		}
		return got
	}

	if got := decision(t, human("admin", tenancy.RoleAdmin, project, true), "task.inbox", project); got != authorize.DecisionDeny {
		t.Fatalf("Admin task.inbox = %s; Admin must not imply Reviewer", got)
	}
	if got := decision(t, human("reviewer", tenancy.RoleReviewer, project, true), "task.inbox", project); got != authorize.DecisionAllow {
		t.Fatalf("Reviewer task.inbox = %s; want allow", got)
	}
	if got := decision(t, human("reviewer", tenancy.RoleReviewer, project, true), "pipeline.apply", project); got != authorize.DecisionDeny {
		t.Fatalf("Reviewer pipeline.apply = %s; want deny", got)
	}
	if got := decision(t, human("engineer", tenancy.RoleEngineer, project, true), "pipeline.apply", project); got != authorize.DecisionAllow {
		t.Fatalf("Engineer pipeline.apply = %s; want allow", got)
	}
	if got := decision(t, human("viewer", tenancy.RoleViewer, project, true), "task.read", project); got != authorize.DecisionAllow {
		t.Fatalf("Viewer task.read = %s; want allow", got)
	}
	if got := decision(t, human("viewer", tenancy.RoleViewer, project, true), "secret.write", project); got != authorize.DecisionDeny {
		t.Fatalf("Viewer secret.write = %s; want deny", got)
	}
	if got := decision(t, human("engineer", tenancy.RoleEngineer, project, false), "pipeline.apply", project); got != authorize.DecisionDeny {
		t.Fatalf("disabled Engineer pipeline.apply = %s; want deny", got)
	}
	if got := decision(t, human("engineer", tenancy.RoleEngineer, project, true), "pipeline.apply", foreign); got != authorize.DecisionDeny {
		t.Fatalf("foreign-tenant Engineer pipeline.apply = %s; a role in one scope must not authorize another", got)
	}

	service := identity.Principal{
		ID:      id.PrincipalID("principal-service-a"),
		Kind:    identity.KindService,
		Scope:   project,
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
	if got, err := matrix.Authorize(ctx, service, "service.create", project); got == authorize.DecisionAllow {
		t.Fatalf("service holding Admin service.create = %s; a service must never become Admin", got)
	} else if err != nil {
		if class, ok := fault.ClassOf(err); !ok || (class != fault.InvalidInput && class != fault.Forbidden) {
			t.Fatalf("service holding Admin refusal class = %v (ok=%v); want invalid_input or forbidden", class, ok)
		}
	}

	execution := identity.Principal{
		ID:          "principal-execution-a",
		Kind:        identity.KindExecution,
		Scope:       project,
		Enabled:     true,
		Version:     1,
		ExecutionID: "execution-a",
	}
	admitted := authorize.AdmittedExecution{
		Principal:   execution,
		ExecutionID: "execution-a",
		ActionID:    "action-a",
		Permission:  authorize.ExecOwnOutputs,
		Deadline:    time.Now().Add(time.Minute),
	}
	if got, err := authorize.AuthorizeExecution(admitted, "execution.action.outputs", project); err != nil || got != authorize.DecisionAllow {
		t.Fatalf("execution.action.outputs for own outputs = %s, err = %v; want allow", got, err)
	}
	if got, err := authorize.AuthorizeExecution(admitted, "execution.request.resolve", project); err != nil || got != authorize.DecisionDeny {
		t.Fatalf("execution.request.resolve for an execution = %s, err = %v; want deny", got, err)
	}
	if got, err := authorize.AuthorizeExecution(admitted, "service.create", project); err != nil || got != authorize.DecisionDeny {
		t.Fatalf("service.create for an execution = %s, err = %v; want deny", got, err)
	}

	if _, err := matrix.Authorize(ctx, human("admin", tenancy.RoleAdmin, tenant, true), "unknown.operation", tenant); err == nil {
		t.Fatal("an unknown operation was authorized instead of refused")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("unknown operation class = %v (ok=%v); want invalid_input", class, ok)
	}
}
