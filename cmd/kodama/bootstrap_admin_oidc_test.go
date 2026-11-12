package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	oidcTestIssuer   = "https://issuer.production.example"
	oidcTestAudience = "kodama-api"
	oidcTestSubject  = "admin@example.com"
	oidcTestTenant   = "tenant-a"
	oidcTestProject  = "project-1"
)

// oidcTestKeys is one RSA/EC key pair set served as a local JWKS over TLS.
type oidcTestKeys struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

const (
	oidcRSAKeyID = "rsa-test-key"
	oidcECKeyID  = "ec-test-key"
)

func newOIDCTestKeys(t *testing.T) *oidcTestKeys {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the RSA key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the EC key: %v", err)
	}
	return &oidcTestKeys{rsa: rsaKey, ec: ecKey}
}

func base64URL(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func base64URLInt(value *big.Int) string { return base64URL(value.Bytes()) }

func (keys *oidcTestKeys) jwks() []byte {
	document := map[string]any{"keys": []map[string]any{
		{
			"kty": "RSA", "kid": oidcRSAKeyID, "use": "sig", "alg": "RS256",
			"n": base64URLInt(keys.rsa.N), "e": base64URLInt(big.NewInt(int64(keys.rsa.E))),
		},
		{
			"kty": "EC", "kid": oidcECKeyID, "use": "sig", "alg": "ES256", "crv": "P-256",
			"x": base64URLInt(keys.ec.X), "y": base64URLInt(keys.ec.Y),
		},
	}}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return encoded
}

// signToken builds one compact JWS over the exact header/claims with a real
// signature for the configured algorithm.
func (keys *oidcTestKeys) signToken(t *testing.T, algorithm string, claims map[string]any) string {
	t.Helper()
	kid := oidcRSAKeyID
	if algorithm == "ES256" {
		kid = oidcECKeyID
	}
	return keys.encodeToken(t, map[string]any{"alg": algorithm, "kid": kid, "typ": "JWT"}, claims, algorithm)
}

func (keys *oidcTestKeys) encodeToken(t *testing.T, header, claims map[string]any, algorithm string) string {
	t.Helper()
	encodedHeader, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("encode token header: %v", err)
	}
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encode token claims: %v", err)
	}
	signingInput := base64URL(encodedHeader) + "." + base64URL(encodedClaims)
	digest := sha256.Sum256([]byte(signingInput))
	var signature []byte
	switch algorithm {
	case "RS256":
		signature, err = rsa.SignPKCS1v15(rand.Reader, keys.rsa, crypto.SHA256, digest[:])
	case "ES256":
		signature, err = ecdsa.SignASN1(rand.Reader, keys.ec, digest[:])
	case "HS256":
		// An attacker keyed the HMAC with the public key material. The verifier
		// must refuse HS* before it ever looks at a key.
		public, marshalErr := x509.MarshalPKIXPublicKey(&keys.rsa.PublicKey)
		if marshalErr != nil {
			t.Fatalf("marshal the public key: %v", marshalErr)
		}
		mac := hmac.New(sha256.New, public)
		_, _ = mac.Write([]byte(signingInput))
		signature = mac.Sum(nil)
	case "none", "":
		signature = nil
	default:
		t.Fatalf("unsupported test algorithm %q", algorithm)
	}
	if err != nil {
		t.Fatalf("sign the token: %v", err)
	}
	return signingInput + "." + base64URL(signature)
}

// oidcBootstrapFixture is one production-shaped bootstrap-admin environment
// whose identity provider is a local TLS server signing real tokens.
type oidcBootstrapFixture struct {
	values    map[string]string
	keys      *oidcTestKeys
	server    *httptest.Server
	tokenFile string
}

func newOIDCBootstrapFixture(t *testing.T) *oidcBootstrapFixture {
	t.Helper()
	keys := newOIDCTestKeys(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(keys.jwks())
	}))
	t.Cleanup(server.Close)

	certificatePath := filepath.Join(t.TempDir(), "oidc-ca.crt")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificatePath, certificatePEM, 0o600); err != nil {
		t.Fatalf("write the identity provider CA: %v", err)
	}

	dir := t.TempDir()
	blobs := filepath.Join(dir, "keys")
	if err := os.Mkdir(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "kek-v1.blob"), []byte("SENTINEL-WRAPPED-KEY-BLOB"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &oidcBootstrapFixture{
		keys:      keys,
		server:    server,
		tokenFile: filepath.Join(dir, "token"),
		values: map[string]string{
			"KODAMA_DATABASE_URL":        "postgres://kodama_runtime:SENTINEL-DB-PW@127.0.0.1:1/kodama?sslmode=disable&connect_timeout=1",
			"KODAMA_INSTALLATION_ID":     "installation-production-7",
			"KODAMA_KEY_ADAPTER":         "scw-kms",
			"KODAMA_KEY_REF":             "scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
			"KODAMA_KEY_CURRENT_VERSION": "1",
			"KODAMA_KEY_BLOBS_DIR":       blobs,
			"SCW_SECRET_KEY":             "SENTINEL-SCW-SECRET-KEY",
			"KODAMA_OIDC_ISSUER":         oidcTestIssuer,
			"KODAMA_OIDC_AUDIENCE":       oidcTestAudience,
			"KODAMA_OIDC_JWKS_URL":       server.URL + "/jwks",
			"KODAMA_OIDC_CA_FILE":        certificatePath,
			"KODAMA_OIDC_TENANT_CLAIM":   "org",
			"KODAMA_OIDC_SUBJECT_CLAIM":  "email",
			"KODAMA_OIDC_PROJECT_CLAIM":  "project",
			"KODAMA_TENANT":              oidcTestTenant,
		},
	}
}

func (fixture *oidcBootstrapFixture) getenv(name string) string { return fixture.values[name] }

func (fixture *oidcBootstrapFixture) setToken(t *testing.T, token string) {
	t.Helper()
	if err := os.WriteFile(fixture.tokenFile, []byte(token), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
}

// claims is a valid tenant-wide admin token body with the configured claim
// names. The project claim is absent so each refusal case reaches its own rule.
func (fixture *oidcBootstrapFixture) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   oidcTestIssuer,
		"aud":   oidcTestAudience,
		"email": oidcTestSubject,
		"org":   oidcTestTenant,
		"exp":   now.Add(5 * time.Minute).Unix(),
		"iat":   now.Add(-time.Minute).Unix(),
	}
}

// acceptingCipher returns a structurally valid envelope without calling a KMS,
// so the test can reach the commit unit boundary; production uses the KMS.
type acceptingCipher struct{ sealed int }

func (cipher *acceptingCipher) Seal(_ context.Context, _ secret.AssociatedData, plaintext []byte) (secret.Envelope, error) {
	cipher.sealed++
	return secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     secret.KeyRef("scw-kms:fr-par/test"),
		KeyVersion: 1,
		Nonce:      make([]byte, secret.ContentNonceBytes),
		Ciphertext: append([]byte(nil), plaintext...),
		WrappedKey: make([]byte, secret.WrappedKeyBytes),
		WrapNonce:  make([]byte, secret.WrapNonceBytes),
	}, nil
}

func (cipher *acceptingCipher) Open(context.Context, secret.AssociatedData, secret.Envelope) ([]byte, error) {
	return nil, errors.New("not used")
}

var errRecordingExecutorReached = errors.New("recording executor reached")

// recordingExecutor records whether the bootstrap reached its commit unit.
type recordingExecutor struct {
	calls int
}

func (executor *recordingExecutor) Execute(context.Context, tenancy.Scope, store.Unit) (store.UnitResult, error) {
	executor.calls++
	return store.UnitResult{}, errRecordingExecutorReached
}

func (executor *recordingExecutor) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, errRecordingExecutorReached
}

type bootstrapAdminProbe struct {
	executor   *recordingExecutor
	connString string
	config     shellapp.BootstrapAdminConfig
	closed     int
}

func (probe *bootstrapAdminProbe) deps() bootstrapAdminDeps {
	return bootstrapAdminDeps{
		openExecutor: func(_ context.Context, connString string) (store.Executor, func(), error) {
			probe.connString = connString
			return probe.executor, func() { probe.closed++ }, nil
		},
		newBootstrap: func(config shellapp.BootstrapAdminConfig, dependencies shellapp.BootstrapAdminDependencies) (bootstrapAdminRunner, error) {
			probe.config = config
			return shellapp.NewBootstrapAdmin(config, dependencies)
		},
		newCipher: func(keys.Provider) (secret.EnvelopeCipher, error) {
			return &acceptingCipher{}, nil
		},
	}
}

func newBootstrapAdminProbe() *bootstrapAdminProbe {
	return &bootstrapAdminProbe{executor: &recordingExecutor{}}
}

// TestBootstrapAdminVerifiesTheTokenBeforeDatabaseAccess proves every refused
// token shape stops before the commit unit: no executor call and no secret in
// the error. The valid RS256 and ES256 tokens reach it, which also pins that
// both supported algorithms stay allowed and that the configured claim names
// (email/org/project) are honoured rather than hardcoded defaults.
func TestBootstrapAdminVerifiesTheTokenBeforeDatabaseAccess(t *testing.T) {
	fixture := newOIDCBootstrapFixture(t)
	refused := []struct {
		name  string
		token func(*oidcBootstrapFixture) string
	}{
		{"wrong issuer", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			claims["iss"] = "https://attacker.example"
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"wrong audience", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			claims["aud"] = "another-api"
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"wrong tenant claim value", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			claims["org"] = "tenant-b"
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"missing tenant claim", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			delete(claims, "org")
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"missing custom subject claim", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			delete(claims, "email")
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"project-scoped principal", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			claims["project"] = oidcTestProject
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"expired", func(f *oidcBootstrapFixture) string {
			claims := f.claims()
			claims["exp"] = time.Now().Add(-time.Hour).Unix()
			return f.keys.signToken(t, "RS256", claims)
		}},
		{"unsigned RS256 token", func(f *oidcBootstrapFixture) string {
			return f.keys.encodeToken(t, map[string]any{"alg": "RS256", "kid": oidcRSAKeyID}, f.claims(), "")
		}},
		{"alg none token", func(f *oidcBootstrapFixture) string {
			return f.keys.encodeToken(t, map[string]any{"alg": "none", "kid": oidcRSAKeyID}, f.claims(), "none")
		}},
		{"HS256 signed with the public key", func(f *oidcBootstrapFixture) string {
			return f.keys.encodeToken(t, map[string]any{"alg": "HS256", "kid": oidcRSAKeyID}, f.claims(), "HS256")
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			token := tc.token(fixture)
			fixture.setToken(t, token)
			probe := newBootstrapAdminProbe()
			_, err := bootstrapAdminWith(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant),
				"Tenant A", fixture.tokenFile, probe.deps())
			if err == nil {
				t.Fatal("a refused token completed the bootstrap")
			}
			if probe.executor.calls != 0 {
				t.Fatalf("executor calls = %d; the token refusal reached the commit unit", probe.executor.calls)
			}
			if probe.closed != 1 {
				t.Fatalf("executor closes = %d; want exactly one", probe.closed)
			}
			for _, secretValue := range []string{token, "SENTINEL-SCW-SECRET-KEY", "SENTINEL-DB-PW", "SENTINEL-WRAPPED-KEY-BLOB"} {
				if strings.Contains(err.Error(), secretValue) {
					t.Fatalf("refusal rendered credential material: %v", err)
				}
			}
		})
	}
}

// TestBootstrapAdminAcceptsBothConfiguredAlgorithms proves a verified admin
// token reaches the commit unit for each allowed algorithm, and that the
// runtime database URL and installation identity are the configured ones.
func TestBootstrapAdminAcceptsBothConfiguredAlgorithms(t *testing.T) {
	for _, algorithm := range []string{"RS256", "ES256"} {
		t.Run(algorithm, func(t *testing.T) {
			fixture := newOIDCBootstrapFixture(t)
			fixture.setToken(t, fixture.keys.signToken(t, algorithm, fixture.claims()))
			probe := newBootstrapAdminProbe()
			_, err := bootstrapAdminWith(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant),
				"Tenant A", fixture.tokenFile, probe.deps())
			if !errors.Is(err, errRecordingExecutorReached) {
				t.Fatalf("err = %v; want the recording executor to be reached", err)
			}
			if probe.executor.calls != 1 {
				t.Fatalf("executor calls = %d; want 1", probe.executor.calls)
			}
			if probe.connString != fixture.values["KODAMA_DATABASE_URL"] {
				t.Fatalf("bootstrap opened %q; want the runtime KODAMA_DATABASE_URL", probe.connString)
			}
			if probe.config.InstallationID != fixture.values["KODAMA_INSTALLATION_ID"] {
				t.Fatalf("installation id = %q; want KODAMA_INSTALLATION_ID", probe.config.InstallationID)
			}
			if probe.config.TenantID != id.TenantID(oidcTestTenant) || probe.config.KeyVersion != 1 {
				t.Fatalf("bootstrap config = %+v; want the configured tenant and key version", probe.config)
			}
		})
	}
}

// recordingListener counts accepted TCP connections and closes each one.
func recordingListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var connections atomic.Int64
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String(), &connections
}

// TestBootstrapAdminRefusalPrecedesAnyDial proves directly that a refused token
// opens no database connection: the runtime URL points at a listener that
// records connections, and the refused token never reaches it. A valid token
// does reach the runtime database (and not a controller listener), so the test
// is not vacuous.
func TestBootstrapAdminRefusalPrecedesAnyDial(t *testing.T) {
	fixture := newOIDCBootstrapFixture(t)
	runtimeAddress, runtimeConnections := recordingListener(t)
	controllerAddress, controllerConnections := recordingListener(t)
	dsn := func(address string) string {
		return "postgres://kodama_runtime:SENTINEL-DB-PW@" + address + "/kodama?sslmode=disable&connect_timeout=1"
	}
	fixture.values["KODAMA_DATABASE_URL"] = dsn(runtimeAddress)
	fixture.values["KODAMA_CONTROLLER_DATABASE_URL"] = dsn(controllerAddress)

	// The only overridden dependency is the envelope cipher: the real pgdriver
	// opens (and dials) the connection when the commit unit executes.
	withCipherOnly := func() bootstrapAdminDeps {
		deps := defaultBootstrapAdminDeps()
		deps.newCipher = func(keys.Provider) (secret.EnvelopeCipher, error) { return &acceptingCipher{}, nil }
		return deps
	}

	t.Run("refused token dials nothing", func(t *testing.T) {
		claims := fixture.claims()
		claims["iss"] = "https://attacker.example"
		fixture.setToken(t, fixture.keys.signToken(t, "RS256", claims))
		if _, err := bootstrapAdminWith(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant),
			"Tenant A", fixture.tokenFile, withCipherOnly()); err == nil {
			t.Fatal("the wrong-issuer token completed the bootstrap")
		}
		time.Sleep(50 * time.Millisecond)
		if runtimeConnections.Load() != 0 || controllerConnections.Load() != 0 {
			t.Fatalf("a refused token dialed the database: runtime=%d controller=%d",
				runtimeConnections.Load(), controllerConnections.Load())
		}
	})

	t.Run("verified token dials only the runtime database", func(t *testing.T) {
		fixture.setToken(t, fixture.keys.signToken(t, "RS256", fixture.claims()))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := bootstrapAdminWith(ctx, fixture.getenv, id.TenantID(oidcTestTenant),
			"Tenant A", fixture.tokenFile, withCipherOnly()); err == nil {
			t.Fatal("the bootstrap unexpectedly committed against a closed listener")
		}
		deadline := time.Now().Add(5 * time.Second)
		for runtimeConnections.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if runtimeConnections.Load() == 0 {
			t.Fatal("a verified token never dialed the runtime database")
		}
		if controllerConnections.Load() != 0 {
			t.Fatalf("bootstrap dialed the controller database %d time(s); it must use the runtime URL", controllerConnections.Load())
		}
	})
}

// TestBootstrapAdminRefusalNeverRendersKeyMaterial proves a missing or
// unreadable blob directory is refused with a fixed diagnostic that never
// renders the KMS secret key or a database password.
func TestBootstrapAdminRefusalNeverRendersKeyMaterial(t *testing.T) {
	const kmsSecret = "SENTINEL-SCW-SECRET-KEY-IO"
	const dsnSecret = "SENTINEL-DB-PASSWORD-IO"
	for _, tc := range []struct{ name, key, value string }{
		{"missing blob directory", "KODAMA_KEY_BLOBS_DIR", "/nonexistent/key-blobs"},
		{"missing blob file", "KODAMA_KEY_CURRENT_VERSION", "9"},
		{"missing identity provider CA", "KODAMA_OIDC_CA_FILE", "/nonexistent/oidc-ca.crt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newOIDCBootstrapFixture(t)
			fixture.values["SCW_SECRET_KEY"] = kmsSecret
			fixture.values["KODAMA_DATABASE_URL"] = "postgres://kodama_runtime:" + dsnSecret + "@127.0.0.1:1/kodama?sslmode=disable"
			fixture.values[tc.key] = tc.value
			probe := newBootstrapAdminProbe()
			_, err := bootstrapAdminWith(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant),
				"Tenant A", fixture.tokenFile, probe.deps())
			if err == nil {
				t.Fatalf("bootstrap accepted %s", tc.key)
			}
			if strings.Contains(err.Error(), kmsSecret) || strings.Contains(err.Error(), dsnSecret) {
				t.Fatalf("refusal rendered credential material: %v", err)
			}
		})
	}
}

// TestBootstrapAdminRejectsTheDevelopmentInstallationDefault proves the
// installation identity comes from the environment and the development
// default is never substituted.
func TestBootstrapAdminRejectsTheDevelopmentInstallationDefault(t *testing.T) {
	fixture := newOIDCBootstrapFixture(t)
	fixture.values["KODAMA_INSTALLATION_ID"] = "installation-dev"
	fixture.setToken(t, fixture.keys.signToken(t, "RS256", fixture.claims()))
	probe := newBootstrapAdminProbe()
	_, err := bootstrapAdminWith(context.Background(), fixture.getenv, id.TenantID(oidcTestTenant),
		"Tenant A", fixture.tokenFile, probe.deps())
	if !errors.Is(err, errRecordingExecutorReached) {
		t.Fatalf("err = %v; want the verified token to reach the commit unit", err)
	}
	if probe.config.InstallationID != "installation-dev" {
		t.Fatalf("installation id = %q; want the environment's explicit value", probe.config.InstallationID)
	}
}
