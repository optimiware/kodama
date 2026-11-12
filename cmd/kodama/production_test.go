package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
)

// selfSigned writes a certificate and key pair (also usable as a CA bundle).
func selfSigned(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kodama.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		DNSNames: []string{"kodama.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for name, data := range map[string][]byte{
		"tls.crt": certPEM, "ca.crt": certPEM,
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func productionFixture(t *testing.T) (productionEnv, driverkube.InClusterConfig, serverOptions) {
	t.Helper()
	dir := t.TempDir()
	selfSigned(t, dir)
	keys := filepath.Join(dir, "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "kek-v1.blob"), []byte("wrapped"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		// The chart's real shapes: one URL-form DSN per role, an openssl-style
		// hex password and the pinned PostgreSQL CA in the installation.
		"KODAMA_DATABASE_URL":                "postgres://kodama_runtime:0d1f5a3c9b7e2468d0f1a2b3c4d5e6f7a8b9c0d1@database:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt",
		"KODAMA_CONTROLLER_DATABASE_URL":     "postgres://kodama_controller:1e2f6b4d0c8f3579e1f2b3c4d5e6f7a8b9c0d1e2@database:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt",
		"KODAMA_PUBLISHER_DATABASE_URL":      "postgres://kodama_publisher:2f307c5e1d9a4680f2a3b4c5d6e7f8a9b0c1d2e3@database:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt",
		"KODAMA_AUTH_DATABASE_URL":           "postgres://kodama_auth:30418d6f2eab5791a3b4c5d6e7f8a9b0c1d2e3f4@database:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt",
		"KODAMA_NATS_URL":                    "tls://nats:4222",
		"KODAMA_NATS_CA_FILE":                filepath.Join(dir, "ca.crt"),
		"KODAMA_NATS_CERT_FILE":              filepath.Join(dir, "tls.crt"),
		"KODAMA_NATS_KEY_FILE":               filepath.Join(dir, "tls.key"),
		"KODAMA_KEY_ADAPTER":                 "scw-kms",
		"KODAMA_KEY_REF":                     "scw-kms:fr-par/3b4a80d8-53a7-4a88-9b39-f5eb1ad20ee9",
		"KODAMA_KEY_CURRENT_VERSION":         "1",
		"KODAMA_KEY_BLOBS_DIR":               keys,
		"KODAMA_TLS_DIR":                     dir,
		"KODAMA_TENANT":                      "tenant-a",
		"SCW_SECRET_KEY":                     "k",
		"KODAMA_OIDC_ISSUER":                 "https://idp.example.com",
		"KODAMA_OIDC_AUDIENCE":               "https://api.example.com",
		"KODAMA_OIDC_JWKS_URL":               "https://idp.example.com/jwks",
		"KODAMA_OIDC_TENANT_CLAIM":           "org_id",
		"KODAMA_WORKSPACE_PVC_STORAGE_CLASS": "sbs-default",
		"KODAMA_WORKSPACE_PVC_SIZE":          "10Gi",
		"KODAMA_WORKSPACE_PVC_ACCESS_MODE":   "ReadWriteOnce",
		"KUBERNETES_SERVICE_HOST":            "10.0.0.1",
		"KUBERNETES_SERVICE_PORT":            "443",
	}
	options := serverOptions{
		InstallationID: "kodama-test", PublicURL: "https://kodama.kodama-system.svc.cluster.local:8443",
		DataDir: t.TempDir(), MCPPath: "/mcp",
	}
	return func(name string) string { return env[name] },
		driverkube.InClusterConfig{TokenFile: token, CAFile: filepath.Join(dir, "ca.crt")}, options
}

// TestBuildProductionPinsEachRoleConnectionURL proves each of the four pools
// is built from its own environment setting. The fixture gives every role a
// distinct host and password, so swapping which variable feeds which Config
// field cannot pass: a controller pool built from the runtime URL (or the
// reverse) would connect with another role's least-privilege credentials.
func TestBuildProductionPinsEachRoleConnectionURL(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_DATABASE_URL":
			return "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@runtime-host:5432/kodama?sslmode=verify-full"
		case "KODAMA_CONTROLLER_DATABASE_URL":
			return "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@controller-host:5432/kodama?sslmode=verify-full"
		case "KODAMA_PUBLISHER_DATABASE_URL":
			return "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@publisher-host:5432/kodama?sslmode=verify-full"
		case "KODAMA_AUTH_DATABASE_URL":
			return "postgres://kodama_auth:SENTINEL-AUTH-PW@auth-host:5432/kodama?sslmode=verify-full"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	config := build.Config
	for _, tc := range []struct {
		name string
		got  string
		key  string
	}{
		{"runtime", config.Installation.DatabaseURL, "KODAMA_DATABASE_URL"},
		{"controller", config.Production.ControllerURL, "KODAMA_CONTROLLER_DATABASE_URL"},
		{"publisher", config.Production.PublisherURL, "KODAMA_PUBLISHER_DATABASE_URL"},
		{"auth", config.Production.AuthURL, "KODAMA_AUTH_DATABASE_URL"},
	} {
		if tc.got != override(tc.key) {
			t.Fatalf("%s pool DSN = %q; want its own %s value", tc.name, tc.got, tc.key)
		}
	}
	// The four values stay distinct: a test that passed with one shared value
	// would not prove the mapping.
	seen := map[string]bool{}
	for _, dsn := range []string{config.Installation.DatabaseURL, config.Production.ControllerURL, config.Production.PublisherURL, config.Production.AuthURL} {
		if seen[dsn] {
			t.Fatalf("two roles share the DSN %q", dsn)
		}
		seen[dsn] = true
	}
}

// TestBuildProductionWiresGitStorageRoot proves the production entry passes the
// mounted root into the composition and refuses an unusable value instead of
// letting the keeper create an empty directory.
func TestBuildProductionWiresGitStorageRoot(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	root := t.TempDir()
	override := func(name string) string {
		if name == "KODAMA_GIT_STORAGE_ROOT" {
			return root
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if build.Config.GitStorageRoot != root {
		t.Fatalf("GitStorageRoot = %q; want %q", build.Config.GitStorageRoot, root)
	}

	// Empty keeps the sweep disabled without an error.
	build, err = buildProduction(options, func(name string) string {
		if name == "KODAMA_GIT_STORAGE_ROOT" {
			return ""
		}
		return getenv(name)
	}, kube)
	if err != nil {
		t.Fatalf("buildProduction without a root: %v", err)
	}
	if build.Config.GitStorageRoot != "" {
		t.Fatalf("GitStorageRoot = %q; want disabled", build.Config.GitStorageRoot)
	}

	for _, testCase := range []struct {
		name string
		root string
	}{
		{name: "missing directory", root: filepath.Join(t.TempDir(), "unmounted")},
		{name: "relative path", root: "relative/git-repos"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := buildProduction(options, func(name string) string {
				if name == "KODAMA_GIT_STORAGE_ROOT" {
					return testCase.root
				}
				return getenv(name)
			}, kube)
			if !errors.Is(err, shellapp.ErrKeepRefsConfiguration) {
				t.Fatalf("buildProduction error = %v; want ErrKeepRefsConfiguration", err)
			}
		})
	}
}

func TestBuildProductionAssemblesAValidComposition(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	build, err := buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	config := build.Config
	if config.Profile != shellapp.ProfileProduction || config.Production == nil || config.Dev != nil {
		t.Fatalf("composition is not the production one: %+v", config.Profile)
	}
	if config.Production.ClaimProvisioner == nil || config.WorkspacePVC == nil || config.Production.NATSTLS == nil {
		t.Fatal("claim provisioning and broker TLS must be wired")
	}
	if config.WorkspacePVC.Namespace != "kodama-tenant-tenant-a" {
		t.Fatalf("namespace = %q", config.WorkspacePVC.Namespace)
	}
	if config.WorkspacePVC.TenantID != "tenant-a" {
		t.Fatalf("workspace tenant = %q; production serves only KODAMA_TENANT", config.WorkspacePVC.TenantID)
	}
	if len(build.TLS.Certificates) != 1 || build.TLS.MinVersion < 0x0303 {
		t.Fatal("server TLS is not configured")
	}
	// The registry resolver refuses a nil transport, but only after every
	// client has been built; this pins that the composed registry config
	// carries one at all instead of relying on that later refusal.
	if config.Production.Registry.Transport == nil {
		t.Fatal("the composed registry configuration has no transport")
	}
	// The Kubernetes client and adapter carry fixed operational bounds. A
	// zero timeout would silently select the driver's shorter default, and a
	// zero cleanup attempt count would hide a cleanup bound regression.
	adapter := inspectKubeAdapter(t, config.Production.Runtime)
	if adapter.requestTimeout != 2*time.Minute {
		t.Fatalf("kubernetes client request timeout = %s; want 2m", adapter.requestTimeout)
	}
	if adapter.cleanupAttempts != 3 || adapter.cleanupBackoff != time.Second {
		t.Fatalf("kubernetes cleanup policy = %d attempts / %s; want 3 attempts / 1s", adapter.cleanupAttempts, adapter.cleanupBackoff)
	}
}

// kubeAdapterView is the otherwise-unexported workload policy the production
// Kubernetes adapter received from the environment.
type kubeAdapterView struct {
	cpuRequest      string
	cpuLimit        string
	memoryRequest   string
	memoryLimit     string
	jobTTLSeconds   int
	workloadCA      string
	namespace       string
	tenant          string
	kodamaURL       string
	cleanupAttempts int
	cleanupBackoff  time.Duration
	requestTimeout  time.Duration
}

// inspectKubeAdapter reads the adapter's own fields. The Kubernetes adapter
// exposes no accessor for the admitted policy or the CA Secret name, and this
// test must prove the environment values reached the object the composition
// actually runs, not merely a copied config field.
func inspectKubeAdapter(t *testing.T, adapter any) kubeAdapterView {
	t.Helper()
	value := reflect.ValueOf(adapter)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		t.Fatalf("runtime adapter = %T; want a non-nil pointer adapter", adapter)
	}
	element := value.Elem()
	for _, name := range []string{"policy", "cleanup", "cluster"} {
		if !element.FieldByName(name).IsValid() {
			t.Fatalf("runtime adapter %T has no %s field", adapter, name)
		}
	}
	policy := element.FieldByName("policy")
	cleanup := element.FieldByName("cleanup")
	// The cluster is held behind the runtime's narrow port interface, so the
	// concrete driver is reached through the interface value. A nil or renamed
	// value fails loudly instead of returning zero timings that would make the
	// timeout assertion vacuous.
	cluster := element.FieldByName("cluster")
	if cluster.Kind() != reflect.Interface || cluster.IsNil() {
		t.Fatalf("runtime adapter cluster field is %s (nil=%t); want the injected client", cluster.Kind(), cluster.IsNil())
	}
	concrete := cluster.Elem()
	if concrete.Kind() != reflect.Pointer || concrete.IsNil() {
		t.Fatalf("runtime cluster = %s; want a non-nil pointer client", concrete.Kind())
	}
	timeout := concrete.Elem().FieldByName("requestTimeout")
	if !timeout.IsValid() {
		t.Fatalf("kubernetes client %s has no requestTimeout field", concrete.Type())
	}
	return kubeAdapterView{
		cpuRequest:      policy.FieldByName("CPURequest").String(),
		cpuLimit:        policy.FieldByName("CPULimit").String(),
		memoryRequest:   policy.FieldByName("MemoryRequest").String(),
		memoryLimit:     policy.FieldByName("MemoryLimit").String(),
		jobTTLSeconds:   int(policy.FieldByName("JobTTLSeconds").Int()),
		workloadCA:      element.FieldByName("workloadCA").FieldByName("SecretName").String(),
		namespace:       element.FieldByName("namespace").String(),
		tenant:          element.FieldByName("tenant").String(),
		kodamaURL:       element.FieldByName("kodamaURL").String(),
		cleanupAttempts: int(cleanup.FieldByName("Attempts").Int()),
		cleanupBackoff:  time.Duration(cleanup.FieldByName("Backoff").Int()),
		requestTimeout:  time.Duration(timeout.Int()),
	}
}

// TestBuildProductionMapsEveryEnvironmentSettingToItsOwnField gives every
// setting the command reads a distinct sentinel and asserts each built field
// (and each adapter field the composition runs) carries its own setting. A
// swapped source variable, a hard-coded data directory, MCP path or access mode,
// a blanked workload CA Secret and a changed resource policy all fail here.
func TestBuildProductionMapsEveryEnvironmentSettingToItsOwnField(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	const (
		runtimeDSN    = "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@runtime-sentinel:5432/kodama?sslmode=verify-full"
		controllerDSN = "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@controller-sentinel:5432/kodama?sslmode=verify-full"
		publisherDSN  = "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@publisher-sentinel:5432/kodama?sslmode=verify-full"
		authDSN       = "postgres://kodama_auth:SENTINEL-AUTH-PW@auth-sentinel:5432/kodama?sslmode=verify-full"
	)
	keysDir := getenv("KODAMA_KEY_BLOBS_DIR")
	if err := os.WriteFile(filepath.Join(keysDir, "kek-v2.blob"), []byte("wrapped-v2-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"KODAMA_DATABASE_URL":                runtimeDSN,
		"KODAMA_CONTROLLER_DATABASE_URL":     controllerDSN,
		"KODAMA_PUBLISHER_DATABASE_URL":      publisherDSN,
		"KODAMA_AUTH_DATABASE_URL":           authDSN,
		"KODAMA_NATS_URL":                    "tls://nats-sentinel:4222",
		"KODAMA_KEY_ADAPTER":                 "scw-kms",
		"KODAMA_KEY_REF":                     "scw-kms:fr-par/key-ref-sentinel",
		"KODAMA_KEY_CURRENT_VERSION":         "2",
		"KODAMA_KEY_BLOBS_DIR":               keysDir,
		"SCW_SECRET_KEY":                     "scw-secret-sentinel",
		"KODAMA_TENANT":                      "tenant-sentinel",
		"KODAMA_TENANT_NAMESPACE":            "namespace-sentinel",
		"KODAMA_OIDC_ISSUER":                 "https://issuer-sentinel.example",
		"KODAMA_OIDC_AUDIENCE":               "audience-sentinel",
		"KODAMA_OIDC_JWKS_URL":               "https://jwks-sentinel.example/jwks",
		"KODAMA_OIDC_TENANT_CLAIM":           "tenant-claim-sentinel",
		"KODAMA_OIDC_SUBJECT_CLAIM":          "subject-claim-sentinel",
		"KODAMA_OIDC_PROJECT_CLAIM":          "project-claim-sentinel",
		"KODAMA_WORKSPACE_PVC_STORAGE_CLASS": "storage-class-sentinel",
		"KODAMA_WORKSPACE_PVC_SIZE":          "42Gi",
		"KODAMA_WORKSPACE_PVC_ACCESS_MODE":   "ReadWriteMany",
		"KODAMA_WORKLOAD_CPU_REQUEST":        "cpu-request-sentinel",
		"KODAMA_WORKLOAD_CPU_LIMIT":          "cpu-limit-sentinel",
		"KODAMA_WORKLOAD_MEMORY_REQUEST":     "memory-request-sentinel",
		"KODAMA_WORKLOAD_MEMORY_LIMIT":       "memory-limit-sentinel",
		"KODAMA_WORKLOAD_CA_SECRET":          "workload-ca-sentinel",
		"KUBERNETES_SERVICE_HOST":            "kubernetes-service-host-sentinel",
		"KUBERNETES_SERVICE_PORT":            "6443",
		"KODAMA_TLS_DIR":                     getenv("KODAMA_TLS_DIR"),
	}
	override := func(name string) string {
		if value, ok := values[name]; ok {
			return value
		}
		return getenv(name)
	}
	options.InstallationID = "installation-sentinel"
	options.DataDir = "/data-dir-sentinel"
	options.MCPPath = "/mcp-sentinel"
	options.PublicURL = "https://public-url-sentinel.example"
	options.Discovery = discoveryOptions{
		Issuer:                      "https://discovery-issuer-sentinel.example",
		ClientID:                    "client-id-sentinel",
		Audience:                    "discovery-audience-sentinel",
		AuthorizationEndpoint:       "https://discovery-issuer-sentinel.example/authorize-sentinel",
		TokenEndpoint:               "https://discovery-issuer-sentinel.example/token-sentinel",
		DeviceAuthorizationEndpoint: "https://discovery-issuer-sentinel.example/device-sentinel",
		RevocationEndpoint:          "https://discovery-issuer-sentinel.example/revoke-sentinel",
		APIURL:                      "https://discovery-api-sentinel.example",
		Scopes:                      []string{"scope-sentinel"},
	}

	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	config := build.Config
	if string(config.Installation.Environment) != "production" || string(config.Installation.State) != "postgres" ||
		string(config.Installation.Messaging) != "nats" || string(config.Installation.Runtime) != "kubernetes" ||
		string(config.Installation.Keys) != "scw-kms" {
		t.Fatalf("installation adapters = %+v; want the production set", config.Installation)
	}
	if config.Installation.DatabaseURL != runtimeDSN {
		t.Fatalf("runtime DSN = %q; want KODAMA_DATABASE_URL", config.Installation.DatabaseURL)
	}
	if config.Installation.NATSURL != "tls://nats-sentinel:4222" {
		t.Fatalf("NATS URL = %q", config.Installation.NATSURL)
	}
	if config.Installation.KeyRef != "scw-kms:fr-par/key-ref-sentinel" {
		t.Fatalf("key reference = %q", config.Installation.KeyRef)
	}
	if config.Installation.PublicURL != options.PublicURL {
		t.Fatalf("public URL = %q; want the option's own value", config.Installation.PublicURL)
	}
	if config.InstallationID != options.InstallationID {
		t.Fatalf("installation id = %q; want %q", config.InstallationID, options.InstallationID)
	}
	if config.DataDir != options.DataDir {
		t.Fatalf("data directory = %q; want %q", config.DataDir, options.DataDir)
	}
	if config.MCPPath != options.MCPPath {
		t.Fatalf("MCP path = %q; want %q", config.MCPPath, options.MCPPath)
	}
	if config.WorkspacePVC == nil {
		t.Fatal("workspace claim template is missing")
	}
	if config.WorkspacePVC.Namespace != "namespace-sentinel" {
		t.Fatalf("workspace namespace = %q; want KODAMA_TENANT_NAMESPACE", config.WorkspacePVC.Namespace)
	}
	if string(config.WorkspacePVC.TenantID) != "tenant-sentinel" {
		t.Fatalf("workspace tenant = %q; want KODAMA_TENANT", config.WorkspacePVC.TenantID)
	}
	if config.WorkspacePVC.StorageClass != "storage-class-sentinel" {
		t.Fatalf("storage class = %q; want KODAMA_WORKSPACE_PVC_STORAGE_CLASS", config.WorkspacePVC.StorageClass)
	}
	if config.WorkspacePVC.Size != "42Gi" {
		t.Fatalf("storage size = %q; want KODAMA_WORKSPACE_PVC_SIZE", config.WorkspacePVC.Size)
	}
	if config.WorkspacePVC.AccessMode != "ReadWriteMany" {
		t.Fatalf("access mode = %q; want KODAMA_WORKSPACE_PVC_ACCESS_MODE", config.WorkspacePVC.AccessMode)
	}
	if config.Production.ControllerURL != controllerDSN || config.Production.PublisherURL != publisherDSN || config.Production.AuthURL != authDSN {
		t.Fatalf("role DSNs = %q/%q/%q; want their own settings", config.Production.ControllerURL, config.Production.PublisherURL, config.Production.AuthURL)
	}
	if config.Production.NATSTLS.CAFile != getenv("KODAMA_NATS_CA_FILE") ||
		config.Production.NATSTLS.CertFile != getenv("KODAMA_NATS_CERT_FILE") ||
		config.Production.NATSTLS.KeyFile != getenv("KODAMA_NATS_KEY_FILE") {
		t.Fatalf("broker TLS files = %+v; want the environment's distinct paths", config.Production.NATSTLS)
	}
	verifier := config.Production.OIDC
	if verifier.Issuer != "https://issuer-sentinel.example" || verifier.Audience != "audience-sentinel" ||
		verifier.JWKSURL != "https://jwks-sentinel.example/jwks" {
		t.Fatalf("OIDC identity wiring = %+v", verifier)
	}
	if verifier.TenantClaim != "tenant-claim-sentinel" || verifier.SubjectClaim != "subject-claim-sentinel" || verifier.ProjectClaim != "project-claim-sentinel" {
		t.Fatalf("OIDC claims = %q/%q/%q", verifier.TenantClaim, verifier.SubjectClaim, verifier.ProjectClaim)
	}
	if verifier.ClockSkew != time.Minute || verifier.CacheTTL != 10*time.Minute ||
		verifier.MaxJWKSBytes != 1<<20 || verifier.MaxTokenBytes != 1<<20 {
		t.Fatalf("OIDC bounds = %+v; want the documented minute/10m/1MiB values", verifier)
	}
	if len(verifier.Algorithms) != 2 || verifier.Algorithms[0] != oidc.AlgorithmRS256 || verifier.Algorithms[1] != oidc.AlgorithmES256 {
		t.Fatalf("OIDC algorithms = %v", verifier.Algorithms)
	}
	if config.Production.Cloud.KeyRef != "scw-kms:fr-par/key-ref-sentinel" || config.Production.Cloud.Current != 2 {
		t.Fatalf("cloud key configuration = %+v", config.Production.Cloud)
	}
	if blob := config.Production.Cloud.Versions[2]; string(blob) != "wrapped-v2-sentinel" {
		t.Fatalf("current key blob = %q; want the current version's file", blob)
	}
	if config.Production.Discovery == nil {
		t.Fatal("the configured discovery document was dropped")
	}
	if config.Production.Discovery.Issuer != "https://discovery-issuer-sentinel.example" ||
		config.Production.Discovery.ClientID != "client-id-sentinel" ||
		config.Production.Discovery.Audience != "discovery-audience-sentinel" ||
		config.Production.Discovery.AuthorizationEndpoint != "https://discovery-issuer-sentinel.example/authorize-sentinel" ||
		config.Production.Discovery.TokenEndpoint != "https://discovery-issuer-sentinel.example/token-sentinel" ||
		config.Production.Discovery.DeviceAuthorizationEndpoint != "https://discovery-issuer-sentinel.example/device-sentinel" ||
		config.Production.Discovery.RevocationEndpoint != "https://discovery-issuer-sentinel.example/revoke-sentinel" ||
		config.Production.Discovery.APIURL != "https://discovery-api-sentinel.example" ||
		len(config.Production.Discovery.Scopes) != 1 || config.Production.Discovery.Scopes[0] != "scope-sentinel" {
		t.Fatalf("discovery document = %+v; want every configured field", config.Production.Discovery)
	}

	adapter := inspectKubeAdapter(t, config.Production.Runtime)
	wantAdapter := kubeAdapterView{
		cpuRequest:      "cpu-request-sentinel",
		cpuLimit:        "cpu-limit-sentinel",
		memoryRequest:   "memory-request-sentinel",
		memoryLimit:     "memory-limit-sentinel",
		jobTTLSeconds:   600,
		workloadCA:      "workload-ca-sentinel",
		namespace:       "namespace-sentinel",
		tenant:          "tenant-sentinel",
		kodamaURL:       options.PublicURL,
		cleanupAttempts: 3,
		cleanupBackoff:  time.Second,
		requestTimeout:  2 * time.Minute,
	}
	if adapter != wantAdapter {
		t.Fatalf("workload adapter = %+v; want %+v", adapter, wantAdapter)
	}
}

// TestBuildProductionWorkloadPolicyDefaults pins the documented defaults when
// the workload settings are unset: the resource requests/limits, the finished
// Job TTL and the derived tenant namespace. A changed default or a blanked
// optional CA reference is caught here.
func TestBuildProductionWorkloadPolicyDefaults(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_WORKLOAD_CPU_REQUEST", "KODAMA_WORKLOAD_CPU_LIMIT",
			"KODAMA_WORKLOAD_MEMORY_REQUEST", "KODAMA_WORKLOAD_MEMORY_LIMIT",
			"KODAMA_WORKLOAD_CA_SECRET", "KODAMA_TENANT_NAMESPACE":
			return ""
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	adapter := inspectKubeAdapter(t, build.Config.Production.Runtime)
	want := kubeAdapterView{
		cpuRequest:      "250m",
		cpuLimit:        "1",
		memoryRequest:   "256Mi",
		memoryLimit:     "1Gi",
		jobTTLSeconds:   600,
		workloadCA:      "",
		namespace:       "kodama-tenant-tenant-a",
		tenant:          "tenant-a",
		kodamaURL:       options.PublicURL,
		cleanupAttempts: 3,
		cleanupBackoff:  time.Second,
		requestTimeout:  2 * time.Minute,
	}
	if adapter != want {
		t.Fatalf("default workload adapter = %+v; want the documented defaults %+v", adapter, want)
	}
}

// TestBuildProductionServesEveryConfiguredTenant proves the production entry
// serves the whole explicit tenant list: KODAMA_TENANTS yields one runtime
// adapter and one workspace claim template per tenant, each bound to its own
// tenant id and derived namespace, with no installation-wide singleton
// template left behind.
func TestBuildProductionServesEveryConfiguredTenant(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_TENANT":
			return ""
		case "KODAMA_TENANTS":
			return `["tenant-a","tenant-b"]`
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	config := build.Config
	if config.WorkspacePVC != nil {
		t.Fatalf("single-tenant workspace template = %+v; want one template per tenant", config.WorkspacePVC)
	}
	tenants := config.Production.Tenants
	if len(tenants) != 2 {
		t.Fatalf("served tenants = %d; want both configured tenants", len(tenants))
	}
	for index, want := range []struct {
		tenant    string
		namespace string
	}{{"tenant-a", "kodama-tenant-tenant-a"}, {"tenant-b", "kodama-tenant-tenant-b"}} {
		tenant := tenants[index]
		if string(tenant.TenantID) != want.tenant {
			t.Fatalf("tenant %d = %q; want %q in configuration order", index, tenant.TenantID, want.tenant)
		}
		if tenant.WorkspacePVC == nil {
			t.Fatalf("tenant %s has no workspace claim template", want.tenant)
		}
		if tenant.WorkspacePVC.TenantID != tenant.TenantID {
			t.Fatalf("tenant %s claim template tenant = %s; want its own", want.tenant, tenant.WorkspacePVC.TenantID)
		}
		if tenant.WorkspacePVC.Namespace != want.namespace {
			t.Fatalf("tenant %s claim namespace = %q; want %q", want.tenant, tenant.WorkspacePVC.Namespace, want.namespace)
		}
		if tenant.WorkspacePVC.StorageClass != "sbs-default" || tenant.WorkspacePVC.Size != "10Gi" || tenant.WorkspacePVC.AccessMode != "ReadWriteOnce" {
			t.Fatalf("tenant %s claim template = %+v; want the configured storage settings", want.tenant, tenant.WorkspacePVC)
		}
		adapter := inspectKubeAdapter(t, tenant.Runtime)
		if adapter.tenant != want.tenant || adapter.namespace != want.namespace {
			t.Fatalf("tenant %s adapter = tenant %q namespace %q; want its own binding", want.tenant, adapter.tenant, adapter.namespace)
		}
		if tenant.Workspace == nil {
			t.Fatalf("tenant %s has no workspace adapter", want.tenant)
		}
	}
	if tenants[0].Runtime == tenants[1].Runtime {
		t.Fatal("two tenants share one runtime adapter")
	}
	if tenants[0].WorkspacePVC.Namespace == tenants[1].WorkspacePVC.Namespace {
		t.Fatal("two tenants share one workspace claim namespace")
	}
}

// TestBuildProductionKeepsSingletonTenantCompatibility proves the singleton
// KODAMA_TENANT form builds the one-tenant composition without a tenant list.
func TestBuildProductionKeepsSingletonTenantCompatibility(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		if name == "KODAMA_TENANT_NAMESPACE" {
			return "kodama-tenant-explicit"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if len(build.Config.Production.Tenants) != 0 {
		t.Fatalf("singleton composition built %d list tenants", len(build.Config.Production.Tenants))
	}
	if build.Config.WorkspacePVC == nil || build.Config.WorkspacePVC.TenantID != "tenant-a" || build.Config.WorkspacePVC.Namespace != "kodama-tenant-explicit" {
		t.Fatalf("workspace template = %+v; want the single configured tenant and explicit namespace", build.Config.WorkspacePVC)
	}
	if adapter := inspectKubeAdapter(t, build.Config.Production.Runtime); adapter.tenant != "tenant-a" || adapter.namespace != "kodama-tenant-explicit" {
		t.Fatalf("singleton adapter = tenant %q namespace %q", adapter.tenant, adapter.namespace)
	}
}

// TestBuildProductionMapsTenantListSettings proves the configured tenant
// storage settings and workload policy reach every tenant's adapter and claim
// template, not only the first one.
func TestBuildProductionMapsTenantListSettings(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_TENANT":
			return ""
		case "KODAMA_TENANTS":
			return `["tenant-a","tenant-b"]`
		case "KODAMA_WORKSPACE_PVC_STORAGE_CLASS":
			return "storage-class-sentinel"
		case "KODAMA_WORKSPACE_PVC_SIZE":
			return "42Gi"
		case "KODAMA_WORKSPACE_PVC_ACCESS_MODE":
			return "ReadWriteMany"
		case "KODAMA_WORKLOAD_CPU_LIMIT":
			return "cpu-limit-sentinel"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	for _, tenant := range build.Config.Production.Tenants {
		template := tenant.WorkspacePVC
		if template == nil || template.StorageClass != "storage-class-sentinel" || template.Size != "42Gi" || template.AccessMode != "ReadWriteMany" {
			t.Fatalf("tenant %s claim template = %+v; want the configured storage settings", tenant.TenantID, template)
		}
		if adapter := inspectKubeAdapter(t, tenant.Runtime); adapter.cpuLimit != "cpu-limit-sentinel" {
			t.Fatalf("tenant %s cpu limit = %q; want the configured workload policy", tenant.TenantID, adapter.cpuLimit)
		}
	}
}

// TestBuildProductionRefusesUnsafeTenantConfiguration pins the fail-closed
// tenant list: an ambiguous dual configuration, a malformed or duplicated list,
// an invalid tenant id and a singleton namespace override for more than one
// tenant all refuse startup. No refusal renders the operator's configured
// value.
func TestBuildProductionRefusesUnsafeTenantConfiguration(t *testing.T) {
	const sentinel = "tenant-sentinel-do-not-render"
	for _, testCase := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{
			name:   "singleton and list together",
			values: map[string]string{"KODAMA_TENANT": "tenant-a", "KODAMA_TENANTS": `["tenant-b"]`},
			want:   "both KODAMA_TENANT and KODAMA_TENANTS",
		},
		{
			name:   "neither singleton nor list",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": ""},
			want:   "KODAMA_TENANT or KODAMA_TENANTS",
		},
		{
			name:   "malformed list",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant-a",`},
			want:   "KODAMA_TENANTS is not a valid JSON list",
		},
		{
			name:   "empty list",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `[]`},
			want:   "KODAMA_TENANTS is empty",
		},
		{
			name:   "duplicate tenant",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant-a","tenant-a"]`},
			want:   "KODAMA_TENANTS repeats a tenant id",
		},
		{
			name:   "malformed tenant id",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant-a","Not A Tenant tenant-sentinel-do-not-render"]`},
			want:   "KODAMA_TENANTS contains an invalid tenant id",
		},
		{
			name:   "non-string tenant",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `[{"id":"tenant-a"}]`},
			want:   "KODAMA_TENANTS is not a valid JSON list",
		},
		{
			name:   "uppercase list tenant",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant-a","Tenant-B"]`},
			want:   "not a lowercase DNS label",
		},
		{
			name:   "underscore list tenant",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant_a"]`},
			want:   "not a lowercase DNS label",
		},
		{
			name:   "overlong list tenant",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["` + strings.Repeat("a", 50) + `"]`},
			want:   "not a lowercase DNS label",
		},
		{
			name:   "uppercase singleton tenant",
			values: map[string]string{"KODAMA_TENANT": "Tenant-A", "KODAMA_TENANTS": ""},
			want:   "KODAMA_TENANT is not a lowercase DNS label",
		},
		{
			name:   "overlong singleton tenant",
			values: map[string]string{"KODAMA_TENANT": strings.Repeat("a", 50), "KODAMA_TENANTS": ""},
			want:   "KODAMA_TENANT is not a lowercase DNS label",
		},
		{
			name:   "namespace override with several tenants",
			values: map[string]string{"KODAMA_TENANT": "", "KODAMA_TENANTS": `["tenant-a","tenant-b"]`, "KODAMA_TENANT_NAMESPACE": "namespace-sentinel"},
			want:   "KODAMA_TENANT_NAMESPACE",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				if value, ok := testCase.values[name]; ok {
					return value
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil {
				t.Fatalf("buildProduction accepted %s", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("refusal rendered a configured tenant value: %v", err)
			}
		})
	}
}

// TestBuildProductionAcceptsTheTenantNamespaceBoundary pins the exact boundary
// of the derived namespace: a 49-character lowercase DNS label yields a
// 63-character Kubernetes namespace, the maximum.
func TestBuildProductionAcceptsTheTenantNamespaceBoundary(t *testing.T) {
	longest := strings.Repeat("a", 49)
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_TENANT":
			return ""
		case "KODAMA_TENANTS":
			return `["` + longest + `"]`
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction refused a %d-character tenant id: %v", len(longest), err)
	}
	tenants := build.Config.Production.Tenants
	if len(tenants) != 1 || string(tenants[0].TenantID) != longest {
		t.Fatalf("served tenants = %+v; want the boundary tenant", tenants)
	}
	namespace := tenants[0].WorkspacePVC.Namespace
	if namespace != "kodama-tenant-"+longest || len(namespace) != 63 {
		t.Fatalf("derived namespace = %q (len %d); want 63 characters", namespace, len(namespace))
	}
}

// TestBuildProductionRefusesMalformedTenantListValues keeps every invalid
// spelling on the same fixed refusal: array members must be tenant ids, so a
// number, object, null or nested list cannot reach an adapter.
func TestBuildProductionRefusesMalformedTenantListValues(t *testing.T) {
	for _, list := range []string{
		`[1]`,
		`[true]`,
		`[null]`,
		`[["tenant-a"]]`,
		`"tenant-a"`,
		`{"tenant-a":true}`,
		`["tenant-a",]`,
	} {
		t.Run(list, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				switch name {
				case "KODAMA_TENANT":
					return ""
				case "KODAMA_TENANTS":
					return list
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil || !strings.Contains(err.Error(), "KODAMA_TENANTS is not a valid JSON list") {
				t.Fatalf("err = %v; want the fixed tenant list refusal", err)
			}
		})
	}
}

func TestBuildProductionRefusesWhatItCannotRunSafely(t *testing.T) {
	for _, tc := range []struct{ name, key, value, want string }{
		{"missing database", "KODAMA_DATABASE_URL", "", "KODAMA_DATABASE_URL"},
		{"missing controller database", "KODAMA_CONTROLLER_DATABASE_URL", "", "KODAMA_CONTROLLER_DATABASE_URL"},
		{"missing publisher database", "KODAMA_PUBLISHER_DATABASE_URL", "", "KODAMA_PUBLISHER_DATABASE_URL"},
		{"missing auth database", "KODAMA_AUTH_DATABASE_URL", "", "KODAMA_AUTH_DATABASE_URL"},
		{"missing broker URL", "KODAMA_NATS_URL", "", "KODAMA_NATS_URL"},
		{"missing broker CA", "KODAMA_NATS_CA_FILE", "", "KODAMA_NATS_CA_FILE"},
		{"missing broker certificate", "KODAMA_NATS_CERT_FILE", "", "KODAMA_NATS_CERT_FILE"},
		{"missing broker key", "KODAMA_NATS_KEY_FILE", "", "KODAMA_NATS_KEY_FILE"},
		{"missing kms credential", "SCW_SECRET_KEY", "", "SCW_SECRET_KEY"},
		{"missing key reference", "KODAMA_KEY_REF", "", "KODAMA_KEY_REF"},
		{"missing key blobs", "KODAMA_KEY_BLOBS_DIR", "", "KODAMA_KEY_BLOBS_DIR"},
		{"missing TLS directory", "KODAMA_TLS_DIR", "", "KODAMA_TLS_DIR"},
		{"missing tenant", "KODAMA_TENANT", "", "KODAMA_TENANT"},
		{"missing oidc issuer", "KODAMA_OIDC_ISSUER", "", "KODAMA_OIDC_ISSUER"},
		{"missing oidc audience", "KODAMA_OIDC_AUDIENCE", "", "KODAMA_OIDC_AUDIENCE"},
		{"missing oidc jwks", "KODAMA_OIDC_JWKS_URL", "", "KODAMA_OIDC_JWKS_URL"},
		{"missing oidc tenant claim", "KODAMA_OIDC_TENANT_CLAIM", "", "KODAMA_OIDC_TENANT_CLAIM"},
		{"missing workspace storage class", "KODAMA_WORKSPACE_PVC_STORAGE_CLASS", "", "KODAMA_WORKSPACE_PVC_STORAGE_CLASS"},
		{"missing workspace size", "KODAMA_WORKSPACE_PVC_SIZE", "", "KODAMA_WORKSPACE_PVC_SIZE"},
		{"missing workspace access mode", "KODAMA_WORKSPACE_PVC_ACCESS_MODE", "", "KODAMA_WORKSPACE_PVC_ACCESS_MODE"},
		{"other key adapter", "KODAMA_KEY_ADAPTER", "keyfile", "scw-kms"},
		{"empty key adapter", "KODAMA_KEY_ADAPTER", "", "scw-kms"},
		{"bad tenant", "KODAMA_TENANT", "Not A Tenant", "KODAMA_TENANT"},
		{"empty tenant", "KODAMA_TENANT", "   ", "KODAMA_TENANT"},
		{"bad key version", "KODAMA_KEY_CURRENT_VERSION", "0", "KODAMA_KEY_CURRENT_VERSION"},
		{"unparseable key version", "KODAMA_KEY_CURRENT_VERSION", "one", "KODAMA_KEY_CURRENT_VERSION"},
		{"unreadable blob dir", "KODAMA_KEY_BLOBS_DIR", "/nonexistent", "key blob"},
		{"http database URL", "KODAMA_DATABASE_URL", "http://db/kodama", "KODAMA_DATABASE_URL"},
		{"controller URL is not postgres", "KODAMA_CONTROLLER_DATABASE_URL", "mysql://db/kodama", "KODAMA_CONTROLLER_DATABASE_URL"},
		{"publisher URL has no database", "KODAMA_PUBLISHER_DATABASE_URL", "postgres://db/", "KODAMA_PUBLISHER_DATABASE_URL"},
		{"plaintext broker URL", "KODAMA_NATS_URL", "nats://nats:4222", "tls://"},
		{"unreadable broker CA", "KODAMA_NATS_CA_FILE", "/nonexistent/ca.crt", "certificate authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				if name == tc.key {
					return tc.value
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestBuildProductionRefusesUnverifiedProductionTransports pins the
// verified-TLS rule at the command boundary for every production pool
// individually. A command that validated only the runtime DSN would still hand
// the controller, publisher or auth pool a connection that pgx would open
// without verified TLS, and a command that accepted a plaintext broker URL
// would never negotiate the configured mutual TLS. Every refusal is
// fault.InvalidInput and names only the environment setting: the DSN, its
// password and the broker credential never appear.
func TestBuildProductionRefusesUnverifiedProductionTransports(t *testing.T) {
	const password = "SENTINEL-DB-PASSWORD"
	const dsnBase = "postgres://kodama_runtime:" + password + "@database:5432/kodama"
	for _, pool := range []struct{ name, key string }{
		{name: "runtime", key: "KODAMA_DATABASE_URL"},
		{name: "controller", key: "KODAMA_CONTROLLER_DATABASE_URL"},
		{name: "publisher", key: "KODAMA_PUBLISHER_DATABASE_URL"},
		{name: "auth", key: "KODAMA_AUTH_DATABASE_URL"},
	} {
		for _, testCase := range []struct{ name, suffix string }{
			{name: "plaintext", suffix: "?sslmode=disable"},
			{name: "verify-ca", suffix: "?sslmode=verify-ca"},
			{name: "missing sslmode", suffix: ""},
			{name: "require without host verification", suffix: "?sslmode=require"},
		} {
			t.Run(pool.name+" refused "+testCase.name, func(t *testing.T) {
				getenv, kube, options := productionFixture(t)
				dsn := dsnBase + testCase.suffix
				override := func(name string) string {
					if name == pool.key {
						return dsn
					}
					return getenv(name)
				}
				_, err := buildProduction(options, override, kube)
				requireCommandInvalidInput(t, err, pool.name+" "+testCase.name)
				if !strings.Contains(err.Error(), pool.key) {
					t.Fatalf("refusal = %v; want it to name %s", err, pool.key)
				}
				if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), dsn) {
					t.Fatalf("refusal echoed the configured %s transport: %v", pool.name, err)
				}
			})
		}
	}

	for _, testCase := range []struct{ name, url string }{
		{name: "plaintext broker", url: "nats://nats:4222"},
		{name: "plaintext broker with credential", url: "nats://user:" + password + "@nats:4222"},
		{name: "tls broker with userinfo", url: "tls://user:" + password + "@nats:4222"},
		{name: "tls broker comma list", url: "tls://a:4222,b:4222"},
		{name: "tls broker fragment", url: "tls://nats:4222#fragment"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				if name == "KODAMA_NATS_URL" {
					return testCase.url
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			requireCommandInvalidInput(t, err, "broker "+testCase.name)
			if !strings.Contains(err.Error(), "NATS URL") {
				t.Fatalf("refusal = %v; want it to name the NATS URL setting", err)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), testCase.url) {
				t.Fatalf("refusal echoed the configured broker transport: %v", err)
			}
		})
	}
}

// requireCommandInvalidInput fails unless err is a fault.InvalidInput.
func requireCommandInvalidInput(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted; want fault.InvalidInput", context)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("%s refusal is not a fault: %v", context, err)
	}
	if class != fault.InvalidInput {
		t.Fatalf("%s fault class = %s; want invalid_input (%v)", context, class, err)
	}
}

func TestBuildProductionRefusesDevelopmentIdentityAndLogFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*serverOptions)
		want   string
	}{
		{"installation id is empty", func(options *serverOptions) { options.InstallationID = "" }, "installation id"},
		{"installation id is the development default", func(options *serverOptions) { options.InstallationID = "installation-dev" }, "installation id"},
		{"data directory is empty", func(options *serverOptions) { options.DataDir = "" }, "data directory"},
		{"public URL is not https", func(options *serverOptions) { options.PublicURL = "http://127.0.0.1:8080" }, "public URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			tc.mutate(&options)
			_, err := buildProduction(options, getenv, kube)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestBuildProductionRefusalNeverRendersSecrets proves every configuration
// refusal is secret-free: neither a database password nor the KMS credential
// appears in the error text.
func TestBuildProductionRefusalNeverRendersSecrets(t *testing.T) {
	const dsnSecret = "SENTINEL-DB-PASSWORD"
	const kmsSecret = "SENTINEL-SCW-SECRET-KEY"
	for _, tc := range []struct{ name, key, value string }{
		{"database URL", "KODAMA_DATABASE_URL", "postgres://kodama_runtime:" + dsnSecret + "@"},
		{"broker URL", "KODAMA_NATS_URL", "nats://user:" + dsnSecret + "@nats:4222"},
		{"tenant", "KODAMA_TENANT", "Not A Tenant " + dsnSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				if name == "SCW_SECRET_KEY" {
					return kmsSecret
				}
				if name == tc.key {
					return tc.value
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil {
				t.Fatalf("configuration %s was accepted", tc.key)
			}
			if strings.Contains(err.Error(), dsnSecret) || strings.Contains(err.Error(), kmsSecret) {
				t.Fatalf("refusal rendered a credential: %v", err)
			}
		})
	}
}

// TestBuildProductionIgnoresDevelopmentAuthentication proves the production
// profile constructs no development authenticator and no seeded identity even
// when a development token and seed variables are present. The values are
// ignored, not merged into the production composition.
func TestBuildProductionIgnoresDevelopmentAuthentication(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	const devToken = "SENTINEL-DEV-TOKEN"
	options.DevTokens = []string{devToken}
	options.DevPrincipal = "dev-admin"
	options.DevTenant = "tenant-dev"
	options.DevProject = "project-dev"
	options.DevRoles = "admin"
	override := func(name string) string {
		switch name {
		case "KODAMA_DEV_TOKEN":
			return devToken
		case "KODAMA_DEV_PRINCIPAL":
			return "dev-admin"
		case "KODAMA_DEV_TENANT":
			return "tenant-dev"
		case "KODAMA_DEV_PROJECT":
			return "project-dev"
		case "KODAMA_DEV_ROLES":
			return "admin"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if build.Config.Dev != nil {
		t.Fatalf("production composition kept a development authenticator: %+v", build.Config.Dev)
	}
	if build.Config.Production == nil {
		t.Fatal("production composition is missing its production configuration")
	}
	if strings.Contains(build.Config.InstallationID, devToken) {
		t.Fatal("a development token leaked into the installation identity")
	}
}

// TestServerProfileComesFromTheEnvironmentButFlagsWin pins the KODAMA_PROFILE
// handling: the environment selects the profile when no flag is given, and an
// explicit flag still wins. A production selection never falls back to the
// development composition.
func TestServerProfileComesFromTheEnvironmentButFlagsWin(t *testing.T) {
	t.Setenv("KODAMA_PROFILE", "production")
	options, code := parseServerOptions(nil, &lockedBuffer{})
	if code != 0 || options.Profile != "production" {
		t.Fatalf("profile = %q (code %d); want the environment's production", options.Profile, code)
	}
	options, code = parseServerOptions([]string{"--profile", "development"}, &lockedBuffer{})
	if code != 0 || options.Profile != "development" {
		t.Fatalf("profile = %q (code %d); an explicit flag must win", options.Profile, code)
	}

	called := false
	deps := serverDeps{
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			called = true
			return nil, nil
		},
		listen: func(string, string) (net.Listener, error) { return nil, nil },
	}
	var stdout, stderr lockedBuffer
	code = runServer(context.Background(), nil, &stdout, &stderr, deps)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if called {
		t.Fatal("the environment-selected production profile built a composition without configuration")
	}
	if !strings.Contains(stderr.String(), "production requires") || strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("stderr = %q; want a production refusal without the development banner", stderr.String())
	}
}

// certificatePEM writes one server certificate as a PEM trust bundle and
// returns its path.
func certificatePEM(t *testing.T, certificate *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatalf("write the trust bundle: %v", err)
	}
	return path
}

// TestOIDCHTTPClientPinsTLSVerification pins the production identity-provider
// client: it trusts the system roots plus exactly the configured CA bundle (the
// in-cluster installation CA), refuses a host it cannot verify (so
// InsecureSkipVerify can never be enabled), and negotiates TLS 1.2 or newer.
func TestOIDCHTTPClientPinsTLSVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("jwks"))
	}))
	defer server.Close()

	client, err := oidcHTTPClient(certificatePEM(t, server.Certificate()))
	if err != nil {
		t.Fatalf("oidcHTTPClient: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatalf("OIDC client transport = %T; want a TLS-configured transport", client.Transport)
	}
	config := transport.TLSClientConfig
	if config.InsecureSkipVerify {
		t.Fatal("the OIDC client disables certificate verification")
	}
	if config.RootCAs == nil {
		t.Fatal("the OIDC client has no root pool")
	}
	if config.MinVersion < tls.VersionTLS12 {
		t.Fatalf("OIDC client minimum TLS version = %x; want TLS 1.2 or newer", config.MinVersion)
	}
	if client.Timeout != 15*time.Second {
		t.Fatalf("OIDC client timeout = %s; want 15s", client.Timeout)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("the configured CA did not verify the identity provider: %v", err)
	}
	response.Body.Close()

	// Without the installation CA only the system roots are trusted, so the
	// provider's private CA is refused; a MITM could not substitute its JWKS.
	systemOnly, err := oidcHTTPClient("")
	if err != nil {
		t.Fatalf("oidcHTTPClient with system roots: %v", err)
	}
	if response, err := systemOnly.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("the OIDC client trusted a provider without the configured CA")
	}
	if config := systemOnly.Transport.(*http.Transport).TLSClientConfig; config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS12 {
		t.Fatal("the system-root OIDC client weakened verification")
	}

	// A different (wrong) CA must not verify the provider either.
	wrongDir := t.TempDir()
	selfSigned(t, wrongDir)
	wrong, err := oidcHTTPClient(filepath.Join(wrongDir, "ca.crt"))
	if err != nil {
		t.Fatalf("oidcHTTPClient with a wrong CA: %v", err)
	}
	if response, err := wrong.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("the OIDC client accepted an unrelated CA")
	}

	if _, err := oidcHTTPClient(filepath.Join(t.TempDir(), "missing.crt")); err == nil {
		t.Fatal("an unreadable OIDC CA bundle was accepted")
	}
}

// TestBuildProductionPinsTheOIDCVerifier pins the identity provider wiring
// from the environment: issuer and audience are not swapped, the JWKS URL and
// claim names are the configured ones (never a hardcoded tenant claim) and both
// supported signing algorithms stay allowed.
func TestBuildProductionPinsTheOIDCVerifier(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_OIDC_TENANT_CLAIM":
			return "org"
		case "KODAMA_OIDC_SUBJECT_CLAIM":
			return "email"
		case "KODAMA_OIDC_PROJECT_CLAIM":
			return "project"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	verifier := build.Config.Production.OIDC
	if verifier.Issuer != "https://idp.example.com" || verifier.Audience != "https://api.example.com" {
		t.Fatalf("issuer/audience = %q/%q; want the environment's values in their own fields", verifier.Issuer, verifier.Audience)
	}
	if verifier.JWKSURL != "https://idp.example.com/jwks" {
		t.Fatalf("JWKS URL = %q", verifier.JWKSURL)
	}
	if verifier.TenantClaim != "org" || verifier.SubjectClaim != "email" || verifier.ProjectClaim != "project" {
		t.Fatalf("claim names = %q/%q/%q; want org/email/project", verifier.TenantClaim, verifier.SubjectClaim, verifier.ProjectClaim)
	}
	if len(verifier.Algorithms) != 2 || verifier.Algorithms[0] != oidc.AlgorithmRS256 || verifier.Algorithms[1] != oidc.AlgorithmES256 {
		t.Fatalf("algorithms = %v; want exactly RS256 and ES256", verifier.Algorithms)
	}
	if verifier.HTTPClient == nil || verifier.MaxJWKSBytes <= 0 || verifier.MaxTokenBytes <= 0 {
		t.Fatal("the OIDC verifier is missing its bounded client or body bounds")
	}
	// The clock window and cache/body bounds are explicit, bounded values: a
	// single day of skew (or a year-long key cache) would let an expired token
	// or a rotated-out key keep working far past its lifetime.
	if verifier.ClockSkew != time.Minute {
		t.Fatalf("OIDC clock skew = %s; want 1m", verifier.ClockSkew)
	}
	if verifier.CacheTTL != 10*time.Minute {
		t.Fatalf("OIDC key cache TTL = %s; want 10m", verifier.CacheTTL)
	}
	if verifier.MaxJWKSBytes != 1<<20 {
		t.Fatalf("OIDC JWKS body bound = %d; want %d", verifier.MaxJWKSBytes, 1<<20)
	}
	if verifier.MaxTokenBytes != 1<<20 {
		t.Fatalf("OIDC token body bound = %d; want %d", verifier.MaxTokenBytes, 1<<20)
	}
	if verifier.HTTPClient.Timeout != 15*time.Second {
		t.Fatalf("OIDC HTTP client timeout = %s; want 15s", verifier.HTTPClient.Timeout)
	}
}

// TestBuildProductionPinsTheConfiguredBrokerTLSFiles proves the broker client
// certificate, key and CA the environment names are the ones the composition
// dials with (a swapped path would authenticate the wrong identity).
func TestBuildProductionPinsTheConfiguredBrokerTLSFiles(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	build, err := buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	tlsConfig := build.Config.Production.NATSTLS
	if tlsConfig == nil {
		t.Fatal("the production composition has no broker TLS configuration")
	}
	if tlsConfig.CAFile != getenv("KODAMA_NATS_CA_FILE") ||
		tlsConfig.CertFile != getenv("KODAMA_NATS_CERT_FILE") ||
		tlsConfig.KeyFile != getenv("KODAMA_NATS_KEY_FILE") {
		t.Fatalf("broker TLS files = %q/%q/%q; want the environment's paths", tlsConfig.CAFile, tlsConfig.CertFile, tlsConfig.KeyFile)
	}
}

// TestBuildProductionBrokerCertificateMustBeUsable proves a CA that is valid
// but a client certificate/key pair that is not a usable key pair is refused at
// configuration time, never at the first broker dial.
func TestBuildProductionBrokerCertificateMustBeUsable(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"KODAMA_NATS_CERT_FILE", "KODAMA_NATS_KEY_FILE"} {
		t.Run(key, func(t *testing.T) {
			override := func(name string) string {
				if name == key {
					return filepath.Join(dir, filepath.Base(getenv(key)))
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil || !strings.Contains(err.Error(), "client certificate is unusable") {
				t.Fatalf("err = %v; want an unusable-client-certificate refusal", err)
			}
		})
	}
}

// TestBuildProductionReadsTheCurrentKeyBlobVersion proves the key blob file is
// the CURRENT version's file: a stale file under another name is never read.
func TestBuildProductionReadsTheCurrentKeyBlobVersion(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	keysDir := getenv("KODAMA_KEY_BLOBS_DIR")

	t.Run("current version blob is loaded", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(keysDir, "kek-v2.blob"), []byte("wrapped-v2"), 0o600); err != nil {
			t.Fatal(err)
		}
		override := func(name string) string {
			if name == "KODAMA_KEY_CURRENT_VERSION" {
				return "2"
			}
			return getenv(name)
		}
		build, err := buildProduction(options, override, kube)
		if err != nil {
			t.Fatalf("buildProduction: %v", err)
		}
		cloud := build.Config.Production.Cloud
		if cloud.Current != 2 || string(cloud.Versions[2]) != "wrapped-v2" {
			t.Fatalf("cloud key versions = current %d, %v; want version 2 and kek-v2.blob", cloud.Current, cloud.Versions)
		}
		if _, found := cloud.Versions[1]; found {
			t.Fatal("a stale kek-v1.blob was loaded for current version 2")
		}
	})

	t.Run("missing current version blob is refused", func(t *testing.T) {
		override := func(name string) string {
			if name == "KODAMA_KEY_CURRENT_VERSION" {
				return "3"
			}
			return getenv(name)
		}
		if _, err := buildProduction(options, override, kube); err == nil {
			t.Fatal("a missing current-version key blob was accepted")
		}
	})
}

// TestBuildProductionRefusalNeverRendersFilesystemDiagnostics proves every
// filesystem error path renders a fixed diagnostic and never the KMS secret key
// (or a database password) the environment carries.
func TestBuildProductionRefusalNeverRendersFilesystemDiagnostics(t *testing.T) {
	const kmsSecret = "SENTINEL-SCW-SECRET-KEY-FS"
	const dsnSecret = "SENTINEL-DB-PASSWORD-FS"
	for _, tc := range []struct{ name, key, value string }{
		{"missing key blobs directory", "KODAMA_KEY_BLOBS_DIR", "/nonexistent/key-blobs"},
		{"missing TLS directory", "KODAMA_TLS_DIR", "/nonexistent/tls"},
		{"missing broker CA", "KODAMA_NATS_CA_FILE", "/nonexistent/ca.crt"},
		{"missing broker certificate", "KODAMA_NATS_CERT_FILE", "/nonexistent/tls.crt"},
		{"missing broker key", "KODAMA_NATS_KEY_FILE", "/nonexistent/tls.key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			override := func(name string) string {
				if name == "SCW_SECRET_KEY" {
					return kmsSecret
				}
				if name == "KODAMA_DATABASE_URL" {
					return "postgres://kodama_runtime:" + dsnSecret + "@db/kodama"
				}
				if name == tc.key {
					return tc.value
				}
				return getenv(name)
			}
			_, err := buildProduction(options, override, kube)
			if err == nil {
				t.Fatalf("configuration with %s accepted", tc.key)
			}
			if strings.Contains(err.Error(), kmsSecret) || strings.Contains(err.Error(), dsnSecret) {
				t.Fatalf("refusal rendered a credential: %v", err)
			}
		})
	}
}

// TestBuildProductionVerifierAcceptsARealToken builds the production verifier
// exactly as buildProduction wires it and verifies a real ES256 token from a
// local TLS JWKS provider. A swapped issuer/audience, a narrowed algorithm set
// or a hardcoded tenant claim would refuse this token.
func TestBuildProductionVerifierAcceptsARealToken(t *testing.T) {
	keys := newOIDCTestKeys(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(keys.jwks())
	}))
	defer server.Close()

	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		switch name {
		case "KODAMA_OIDC_ISSUER":
			return oidcTestIssuer
		case "KODAMA_OIDC_AUDIENCE":
			return oidcTestAudience
		case "KODAMA_OIDC_JWKS_URL":
			return server.URL + "/jwks"
		case "KODAMA_OIDC_CA_FILE":
			return certificatePEM(t, server.Certificate())
		case "KODAMA_OIDC_TENANT_CLAIM":
			return "org"
		case "KODAMA_OIDC_SUBJECT_CLAIM":
			return "email"
		}
		return getenv(name)
	}
	build, err := buildProduction(options, override, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	verifier, err := oidc.NewVerifier(build.Config.Production.OIDC)
	if err != nil {
		t.Fatalf("NewVerifier from the production configuration: %v", err)
	}
	claims := map[string]any{
		"iss": oidcTestIssuer, "aud": oidcTestAudience,
		"email": oidcTestSubject, "org": oidcTestTenant,
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	token := keys.signToken(t, "ES256", claims)
	principal, err := verifier.Verify(context.Background(), identity.OIDCRequest{AccessToken: token, Now: time.Now()})
	if err != nil {
		t.Fatalf("the production verifier refused a valid ES256 token: %v", err)
	}
	if principal.Scope.TenantID != oidcTestTenant {
		t.Fatalf("verified tenant = %q; want %q", principal.Scope.TenantID, oidcTestTenant)
	}

	// The production verifier's one-minute skew is real: a token that expired
	// 30 seconds ago is still accepted (clock drift), while a token that
	// expired two minutes ago is refused. A build wiring a day-long skew would
	// accept the latter; a build with no skew would refuse the former.
	now := time.Now()
	for _, tc := range []struct {
		name     string
		expiry   time.Time
		accepted bool
	}{
		{name: "expired just inside the skew", expiry: now.Add(-30 * time.Second), accepted: true},
		{name: "expired just outside the skew", expiry: now.Add(-2 * time.Minute), accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{
				"iss": oidcTestIssuer, "aud": oidcTestAudience,
				"email": oidcTestSubject, "org": oidcTestTenant,
				"exp": tc.expiry.Unix(),
			}
			_, err := verifier.Verify(context.Background(), identity.OIDCRequest{
				AccessToken: keys.signToken(t, "ES256", claims), Now: now,
			})
			if tc.accepted && err != nil {
				t.Fatalf("the production verifier refused a token inside its skew: %v", err)
			}
			if !tc.accepted && err == nil {
				t.Fatal("the production verifier accepted a token outside its skew")
			}
		})
	}

	// The same token for another audience is refused through the configured
	// client and CA, not by an unrelated check.
	wrongAudience := map[string]any{
		"iss": oidcTestIssuer, "aud": "another-api",
		"email": oidcTestSubject, "org": oidcTestTenant,
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	if _, err := verifier.Verify(context.Background(), identity.OIDCRequest{
		AccessToken: keys.signToken(t, "ES256", wrongAudience), Now: time.Now(),
	}); err == nil {
		t.Fatal("the production verifier accepted a token for another audience")
	}
}
