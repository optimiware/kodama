package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// fakeKMS returns a fixed key-encryption key without any network call.
type fakeKMS struct{}

func (fakeKMS) Decrypt(context.Context, secret.KeyRef, []byte) ([]byte, error) {
	return make([]byte, secret.DataKeyBytes), nil
}

// noNetworkTransport refuses every registry request.
type noNetworkTransport struct{}

func (noNetworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("registry network is not available in unit tests")
}

func validProductionInstallation() installation.Config {
	return installation.Config{
		Environment: installation.EnvironmentProduction,
		State:       installation.StatePostgres,
		Messaging:   installation.MessagingNATS,
		Runtime:     installation.RuntimeKubernetes,
		Keys:        installation.KeyAWSKMS,
		DatabaseURL: "postgres://runtime@localhost:5432/kodama?sslmode=verify-full",
		NATSURL:     "tls://localhost:4222",
		PublicURL:   "https://kodama.example",
		KeyRef:      "aws-kms:alias/kodama",
	}
}

func validProductionConfig(t *testing.T) Config {
	t.Helper()
	runtime := &fakeRuntime{}
	return Config{
		Profile:        ProfileProduction,
		Installation:   validProductionInstallation(),
		InstallationID: "installation-1",
		DataDir:        t.TempDir(),
		LoopInterval:   time.Millisecond,
		Logger:         nil,
		Production: &ProductionConfig{
			Runtime:   runtime,
			Workspace: runtime,
			KMS:       fakeKMS{},
			Cloud:     keys.CloudConfig{KeyRef: "aws-kms:alias/kodama", Current: secret.MinKeyVersion, Versions: map[secret.KeyVersion][]byte{secret.MinKeyVersion: make([]byte, 32)}},
			OIDC: oidc.Config{
				OIDCConfig:    identity.OIDCConfig{Issuer: "https://issuer.example", Audience: "kodama"},
				JWKSURL:       "https://issuer.example/jwks",
				Algorithms:    []string{oidc.AlgorithmRS256},
				HTTPClient:    &http.Client{Transport: noNetworkTransport{}},
				MaxJWKSBytes:  1 << 20,
				MaxTokenBytes: 1 << 20,
				TenantClaim:   "tenant",
				Clock:         time.Now,
			},
			Registry:      registry.Config{Transport: noNetworkTransport{}},
			NATSTLS:       &natsdriver.TLSConfig{CAFile: "/etc/kodama/broker-tls/ca.crt", CertFile: "/etc/kodama/broker-tls/tls.crt", KeyFile: "/etc/kodama/broker-tls/tls.key"},
			ControllerURL: "postgres://controller@localhost:5432/kodama?sslmode=verify-full",
			PublisherURL:  "postgres://publisher@localhost:5432/kodama?sslmode=verify-full",
			AuthURL:       "postgres://auth@localhost:5432/kodama?sslmode=verify-full",
		},
	}
}

func TestEnvironmentAndPreflightMatrix(t *testing.T) {
	development := func() Config {
		environment := newTestEnvironment(t)
		return environment.config
	}
	cases := []struct {
		name string
		cfg  func() Config
		want string
	}{
		{"unknown profile", func() Config { cfg := development(); cfg.Profile = "staging"; return cfg }, "composition profile"},
		{"missing installation id", func() Config { cfg := development(); cfg.InstallationID = ""; return cfg }, "installation id"},
		{"development wrong state", func() Config { cfg := development(); cfg.Installation.State = installation.StatePostgres; return cfg }, "development requires the memory state adapter"},
		{"development wrong messaging", func() Config {
			cfg := development()
			cfg.Installation.Messaging = installation.MessagingNATS
			return cfg
		}, "development requires the memory messaging adapter"},
		{"development wrong runtime", func() Config { cfg := development(); cfg.Installation.Runtime = installation.RuntimePodman; return cfg }, "development requires the process runtime adapter"},
		{"development wrong keys", func() Config { cfg := development(); cfg.Installation.Keys = installation.KeyKeyfile; return cfg }, "development requires the ephemeral key adapter"},
		{"development missing dev config", func() Config { cfg := development(); cfg.Dev = nil; return cfg }, "explicit development authentication"},
		{"development no tokens", func() Config { cfg := development(); cfg.Dev.Tokens = nil; return cfg }, "at least one development token"},
		{"test missing injected adapters", func() Config {
			cfg := development()
			cfg.Profile = ProfileTest
			cfg.Adapters = &Adapters{}
			return cfg
		}, "requires an injected"},
		{"invalid MCP path", func() Config { cfg := development(); cfg.MCPPath = "mcp"; return cfg }, "MCP path"},
		{"negative loop interval", func() Config { cfg := development(); cfg.LoopInterval = -time.Second; return cfg }, "positive loop interval"},
		{"production missing production adapters", func() Config { cfg := validProductionConfig(t); cfg.Production = nil; return cfg }, "explicit production adapters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testCase.cfg()
			err := cfg.resolved().validate()
			if err == nil {
				t.Fatalf("validate accepted the configuration")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestProductionRefusesEveryWeakerAdapter(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"development environment", func(cfg *Config) { cfg.Installation.Environment = installation.EnvironmentDevelopment }, "development requires the memory state adapter"},
		{"memory state", func(cfg *Config) { cfg.Installation.State = installation.StateMemory }, "production requires the postgres state adapter"},
		{"memory messaging", func(cfg *Config) { cfg.Installation.Messaging = installation.MessagingMemory }, "production requires the NATS messaging adapter"},
		{"process runtime", func(cfg *Config) { cfg.Installation.Runtime = installation.RuntimeProcess }, "production requires the kubernetes runtime adapter"},
		{"podman runtime", func(cfg *Config) { cfg.Installation.Runtime = installation.RuntimePodman }, "production requires the kubernetes runtime adapter"},
		{"ephemeral keys", func(cfg *Config) { cfg.Installation.Keys = installation.KeyEphemeral }, "production requires exactly one explicitly selected cloud KMS"},
		{"keyfile keys", func(cfg *Config) { cfg.Installation.Keys = installation.KeyKeyfile }, "production requires exactly one explicitly selected cloud KMS"},
		{"static authenticator and seed", func(cfg *Config) {
			cfg.Dev = &DevelopmentConfig{Tokens: []DevToken{{Credential: "x", Principal: identity.Principal{ID: "admin-1", Kind: identity.KindHuman, Scope: tenancy.Scope{TenantID: "tenant-1"}, Enabled: true, Version: 1}, ExpiresAt: time.Now().Add(time.Hour)}}}
		}, "refuses a development authenticator"},
		{"in-memory adapter override", func(cfg *Config) { cfg.Adapters = &Adapters{Store: &noopExecutor{}} }, "refuses a development or test adapter"},
		{"live log publisher override", func(cfg *Config) { cfg.Adapters = &Adapters{LivePublisher: &recordingLivePublisher{}} }, "refuses a development or test adapter"},
		// The fresh-only workspace decorator's two adapter seams are only
		// meaningful in a test/development composition. Each supplied alone
		// must be refused like every other explicit override, so neither term
		// of hasDevelopmentAdapter can silently regress.
		{"volume provisioner override", func(cfg *Config) { cfg.Adapters = &Adapters{VolumeProvisioner: &recordingProvisioner{}} }, "refuses a development or test adapter"},
		{"workspace activation override", func(cfg *Config) { cfg.Adapters = &Adapters{WorkspaceActivation: &fakeActivation{}} }, "refuses a development or test adapter"},
		{"claim provisioner override", func(cfg *Config) { cfg.Adapters = &Adapters{ClaimProvisioner: &recordingClaimProvisioner{}} }, "refuses a development or test adapter"},
		// Kubernetes claim provisioning is composed only when both the trusted
		// template and the cluster client are supplied. Production accepts the
		// pair; either half alone is refused before startup.
		{"workspace claim template", func(cfg *Config) { template := kubeTestTemplate(); cfg.WorkspacePVC = &template }, "workspace claim template"},
		{"claim provisioner without template", func(cfg *Config) { cfg.Production.ClaimProvisioner = &recordingClaimProvisioner{} }, "needs both"},
		{"missing broker TLS", func(cfg *Config) { cfg.Production.NATSTLS = nil }, "broker TLS"},
		{"missing broker certificate authority", func(cfg *Config) { cfg.Production.NATSTLS.CAFile = "" }, "broker TLS"},
		{"whitespace broker certificate authority", func(cfg *Config) { cfg.Production.NATSTLS.CAFile = "   " }, "broker TLS"},
		{"missing broker client certificate", func(cfg *Config) { cfg.Production.NATSTLS.CertFile = "" }, "broker TLS"},
		{"whitespace broker client certificate", func(cfg *Config) { cfg.Production.NATSTLS.CertFile = "\t " }, "broker TLS"},
		{"missing broker client key", func(cfg *Config) { cfg.Production.NATSTLS.KeyFile = "" }, "broker TLS"},
		{"whitespace broker client key", func(cfg *Config) { cfg.Production.NATSTLS.KeyFile = "\n " }, "broker TLS"},
		{"missing kubernetes runtime", func(cfg *Config) { cfg.Production.Runtime = nil }, "requires the kubernetes runtime adapter"},
		{"missing workspace", func(cfg *Config) { cfg.Production.Workspace = nil }, "requires the kubernetes workspace adapter"},
		{"missing KMS client", func(cfg *Config) { cfg.Production.KMS = nil }, "requires a cloud KMS client"},
		{"missing OIDC", func(cfg *Config) { cfg.Production.OIDC.JWKSURL = "" }, "requires an OIDC configuration"},
		{"missing registry transport", func(cfg *Config) { cfg.Production.Registry.Transport = nil }, "requires a registry transport"},
		{"missing controller URL", func(cfg *Config) { cfg.Production.ControllerURL = "" }, "requires a controller database URL"},
		{"missing publisher URL", func(cfg *Config) { cfg.Production.PublisherURL = "" }, "requires a publisher database URL"},
		{"missing auth URL", func(cfg *Config) { cfg.Production.AuthURL = "" }, "requires an auth database URL"},
		{"foreign KMS key reference", func(cfg *Config) {
			cfg.Production.Cloud.KeyRef = "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek"
		}, "does not belong to the selected cloud KMS adapter"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := validProductionConfig(t)
			testCase.mutate(&cfg)
			err := cfg.resolved().validate()
			if err == nil {
				t.Fatalf("validate accepted the weaker production composition")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestProductionCompositionBuildsWithoutDialing(t *testing.T) {
	cfg := validProductionConfig(t)
	// Production startup now provisions the JetStream streams through the
	// driver seam. The fake dial keeps this a construction-only proof: no real
	// NATS socket is opened and no stream is created.
	cfg.Production.dialNATS = (&fakeNATSServer{}).dial
	app, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// noopExecutor is a deliberately incomplete adapter used only to prove the
// production profile refuses any caller-supplied development adapter set.
type noopExecutor struct{}

func (*noopExecutor) Execute(context.Context, tenancy.Scope, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, nil
}

func (*noopExecutor) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, nil
}

// validMultiTenantProductionConfig builds a production composition whose
// adapters are one set per tenant. The claim provisioner and every tenant's
// template are present so the baseline is valid.
func validMultiTenantProductionConfig(t *testing.T) Config {
	t.Helper()
	config := validProductionConfig(t)
	config.Production.Runtime = nil
	config.Production.Workspace = nil
	config.Production.ClaimProvisioner = &recordingClaimProvisioner{}
	template := func(tenant id.TenantID) *WorkspacePVC {
		value := kubeTestTemplate()
		value.TenantID = tenant
		value.Namespace = "kodama-tenant-" + string(tenant)
		return &value
	}
	config.Production.Tenants = []ProductionTenant{
		{TenantID: "tenant-a", Runtime: &fakeRuntime{}, Workspace: &fakeRuntime{}, WorkspacePVC: template("tenant-a")},
		{TenantID: "tenant-b", Runtime: &fakeRuntime{}, Workspace: &fakeRuntime{}, WorkspacePVC: template("tenant-b")},
	}
	return config
}

// TestProductionAcceptsOneAdapterSetPerTenant proves a multi-tenant
// production composition with a distinct runtime, workspace and workspace
// claim template per tenant is a valid production configuration.
func TestProductionAcceptsOneAdapterSetPerTenant(t *testing.T) {
	config := validMultiTenantProductionConfig(t)
	if err := config.resolved().validate(); err != nil {
		t.Fatalf("validate refused a multi-tenant production composition: %v", err)
	}
}

// TestProductionRefusesAmbiguousOrUnsafeTenantSets pins the configuration
// boundary of the multi-tenant composition: a dual singleton/list
// configuration, duplicate tenants, an unserved adapter half, a crossed or
// duplicated workspace claim namespace and a half-configured claim provisioner
// all refuse startup. No refusal renders a configured tenant value.
func TestProductionRefusesAmbiguousOrUnsafeTenantSets(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "singleton and tenant list together",
			mutate: func(config *Config) {
				config.Production.Runtime = &fakeRuntime{}
				config.Production.Workspace = &fakeRuntime{}
			},
			want: "single-tenant runtime and a tenant list",
		},
		{
			name: "no singleton and no tenant list",
			mutate: func(config *Config) {
				config.Production.Tenants = nil
			},
			want: "kubernetes runtime adapter",
		},
		{
			name: "duplicate tenant",
			mutate: func(config *Config) {
				config.Production.Tenants[1].TenantID = config.Production.Tenants[0].TenantID
				template := kubeTestTemplate()
				template.TenantID = config.Production.Tenants[0].TenantID
				config.Production.Tenants[1].WorkspacePVC = &template
			},
			want: "repeats an earlier tenant",
		},
		{
			name: "malformed tenant id",
			mutate: func(config *Config) {
				config.Production.Tenants[0].TenantID = "Not A Tenant"
			},
			want: "tenant id is invalid",
		},
		{
			name: "uppercase tenant id",
			mutate: func(config *Config) {
				config.Production.Tenants[0].TenantID = "Tenant-A"
			},
			want: "lowercase DNS label",
		},
		{
			name: "underscore tenant id",
			mutate: func(config *Config) {
				config.Production.Tenants[0].TenantID = "tenant_a"
			},
			want: "lowercase DNS label",
		},
		{
			name: "overlong tenant id",
			mutate: func(config *Config) {
				config.Production.Tenants[0].TenantID = id.TenantID(strings.Repeat("a", 50))
			},
			want: "lowercase DNS label",
		},
		{
			name: "missing tenant runtime",
			mutate: func(config *Config) {
				config.Production.Tenants[0].Runtime = nil
			},
			want: "runtime adapter",
		},
		{
			name: "missing tenant workspace",
			mutate: func(config *Config) {
				config.Production.Tenants[1].Workspace = nil
			},
			want: "workspace adapter",
		},
		{
			name: "claim template of another tenant",
			mutate: func(config *Config) {
				template := kubeTestTemplate()
				template.TenantID = config.Production.Tenants[1].TenantID
				config.Production.Tenants[0].WorkspacePVC = &template
			},
			want: "template tenant does not match",
		},
		{
			name: "two tenants share one claim namespace",
			mutate: func(config *Config) {
				config.Production.Tenants[1].WorkspacePVC.Namespace = config.Production.Tenants[0].WorkspacePVC.Namespace
			},
			want: "share a workspace claim namespace",
		},
		{
			name: "single-tenant template with a tenant list",
			mutate: func(config *Config) {
				template := kubeTestTemplate()
				config.WorkspacePVC = &template
			},
			want: "single-tenant workspace claim template",
		},
		{
			name: "tenant templates without a claim provisioner",
			mutate: func(config *Config) {
				config.Production.ClaimProvisioner = nil
			},
			want: "tenant workspace claim",
		},
		{
			name: "claim provisioner without tenant templates",
			mutate: func(config *Config) {
				config.Production.Tenants[0].WorkspacePVC = nil
				config.Production.Tenants[1].WorkspacePVC = nil
			},
			want: "tenant workspace claim",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := validMultiTenantProductionConfig(t)
			testCase.mutate(&config)
			err := config.resolved().validate()
			if err == nil {
				t.Fatal("validate accepted an ambiguous or unsafe tenant configuration")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

// TestProductionTenantNamespaceIDGrammar pins the tenant-id grammar the chart
// uses to derive kodama-tenant-<tenant>: a lowercase DNS label no longer than
// 49 characters. id.ParseTenantID alone is looser, so startup refuses what
// would otherwise fail only at the first namespace or claim use.
func TestProductionTenantNamespaceIDGrammar(t *testing.T) {
	for _, valid := range []id.TenantID{
		"a",
		"tenant-a",
		"a-b-c",
		"t1",
		id.TenantID(strings.Repeat("a", 49)),
	} {
		if err := validateTenantNamespaceID(valid); err != nil {
			t.Errorf("refused valid tenant id %q: %v", valid, err)
		}
	}
	for _, invalid := range []id.TenantID{
		"",
		"Tenant-A",
		"tenant_a",
		"tenant.a",
		"-tenant",
		"tenant-",
		"tenant a",
		id.TenantID(strings.Repeat("a", 50)),
	} {
		if err := validateTenantNamespaceID(invalid); err == nil {
			t.Errorf("accepted invalid tenant id %q", invalid)
		}
	}
}
