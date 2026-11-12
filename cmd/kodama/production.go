package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// productionEnv is the environment the Helm chart renders for the server. Every
// value is trusted operator configuration; secrets arrive as environment
// variables from Secrets or as files, never as flags.
type productionEnv func(string) string

// productionBuild is the result of reading the environment: the composition
// input and the server certificate to serve it with.
type productionBuild struct {
	Config shellapp.Config
	TLS    *tls.Config
}

// natsAuthFromEnv reads the optional per-credential broker settings. The
// control plane's bypass user and the callout service are both opt-in together:
// setting only half of either pair is a configuration refusal rather than a
// silently weaker broker path. No value is ever rendered in an error.
func natsAuthFromEnv(getenv productionEnv) (*shellapp.NATSAuthConfig, error) {
	user := strings.TrimSpace(getenv("KODAMA_NATS_USER"))
	password := strings.TrimSpace(getenv("KODAMA_NATS_PASSWORD"))
	calloutUser := strings.TrimSpace(getenv("KODAMA_NATS_CALLOUT_USER"))
	calloutPassword := strings.TrimSpace(getenv("KODAMA_NATS_CALLOUT_PASSWORD"))
	issuerSeed := strings.TrimSpace(getenv("KODAMA_NATS_CALLOUT_ISSUER_SEED"))
	account := strings.TrimSpace(getenv("KODAMA_NATS_CALLOUT_ACCOUNT"))

	anyCallout := calloutUser != "" || calloutPassword != "" || issuerSeed != "" || account != ""
	anyBypass := user != "" || password != ""
	if !anyCallout && !anyBypass {
		return nil, nil
	}
	if anyBypass && (user == "" || password == "") {
		return nil, fmt.Errorf("KODAMA_NATS_USER and KODAMA_NATS_PASSWORD must be set together")
	}
	if anyCallout {
		for _, pair := range []struct {
			name  string
			value string
		}{
			{"KODAMA_NATS_CALLOUT_USER", calloutUser},
			{"KODAMA_NATS_CALLOUT_PASSWORD", calloutPassword},
			{"KODAMA_NATS_CALLOUT_ISSUER_SEED", issuerSeed},
			{"KODAMA_NATS_CALLOUT_ACCOUNT", account},
		} {
			if pair.value == "" {
				return nil, fmt.Errorf("per-credential NATS access requires %s", pair.name)
			}
		}
		if user == "" || password == "" {
			return nil, fmt.Errorf("per-credential NATS access requires KODAMA_NATS_USER and KODAMA_NATS_PASSWORD")
		}
	}
	config := &shellapp.NATSAuthConfig{User: user, Password: password}
	if anyCallout {
		config.Callout = &shellapp.NATSCalloutConfig{
			User:       calloutUser,
			Password:   calloutPassword,
			IssuerSeed: issuerSeed,
			Account:    account,
		}
	}
	return config, nil
}

// buildProduction assembles the production composition from the environment.
// kube selects how the in-cluster API client authenticates; production passes
// the conventional service-account paths.
func buildProduction(options serverOptions, getenv productionEnv, kube driverkube.InClusterConfig) (productionBuild, error) {
	need := func(name string) (string, error) {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return "", fmt.Errorf("production requires %s", name)
		}
		return value, nil
	}
	// The optional private-registry credential file is trusted operator
	// configuration named by an explicit environment setting. It is loaded
	// before any other dependency so a missing or malformed file is a
	// configuration refusal, never a partially built composition.
	registryCredentials, err := productionRegistryCredentials(getenv)
	if err != nil {
		return productionBuild{}, err
	}
	values := map[string]string{}
	for _, name := range []string{
		"KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL", "KODAMA_PUBLISHER_DATABASE_URL",
		"KODAMA_AUTH_DATABASE_URL", "KODAMA_NATS_URL", "KODAMA_NATS_CA_FILE", "KODAMA_NATS_CERT_FILE", "KODAMA_NATS_KEY_FILE",
		"KODAMA_KEY_REF", "KODAMA_KEY_BLOBS_DIR",
		"KODAMA_TLS_DIR",
		"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
		"KODAMA_WORKSPACE_PVC_STORAGE_CLASS", "KODAMA_WORKSPACE_PVC_SIZE", "KODAMA_WORKSPACE_PVC_ACCESS_MODE",
	} {
		value, err := need(name)
		if err != nil {
			return productionBuild{}, err
		}
		values[name] = value
	}
	// The development defaults are not an installation identity or a durable
	// log location: production refuses to fall back to them.
	if strings.TrimSpace(options.InstallationID) == "" || options.InstallationID == "installation-dev" {
		return productionBuild{}, fmt.Errorf("production requires an explicit installation id (--installation-id or KODAMA_INSTALLATION_ID)")
	}
	if strings.TrimSpace(options.DataDir) == "" {
		return productionBuild{}, fmt.Errorf("production requires an explicit data directory for retained logs (--data-dir or KODAMA_DATA_DIR)")
	}
	tenants, err := parseProductionTenants(getenv)
	if err != nil {
		return productionBuild{}, err
	}
	loopInterval, err := resolvedLoopInterval(options.LoopInterval, productionLoopInterval)
	if err != nil {
		return productionBuild{}, err
	}
	gitStorageRoot, err := resolveGitStorageRoot(options, getenv)
	if err != nil {
		return productionBuild{}, err
	}

	keyAdapter, err := productionKeyAdapter(getenv)
	if err != nil {
		return productionBuild{}, err
	}

	installationConfig := installation.Config{
		Environment: installation.EnvironmentProduction,
		State:       installation.StatePostgres,
		Messaging:   installation.MessagingNATS,
		Runtime:     installation.RuntimeKubernetes,
		Keys:        keyAdapter,
		DatabaseURL: values["KODAMA_DATABASE_URL"],
		NATSURL:     values["KODAMA_NATS_URL"],
		KeyRef:      values["KODAMA_KEY_REF"],
		PublicURL:   options.PublicURL,
	}
	// Adapter selection, endpoint schemes and the public URL are validated
	// before any client is constructed, so a wrong installation is an exit-2
	// configuration refusal instead of a partially built composition. Every
	// one of the four pools is checked with the production validator: the
	// runtime DSN is also checked by Preflight, and the controller, publisher
	// and auth DSNs are checked here so a command that never constructs the
	// full composition cannot accept a pool that would connect without
	// verified TLS. Preflight owns the production NATS/TLS rule.
	for _, name := range []string{"KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL", "KODAMA_PUBLISHER_DATABASE_URL", "KODAMA_AUTH_DATABASE_URL"} {
		if err := installation.ValidateProductionPostgresURL(values[name]); err != nil {
			// Wrap with the environment setting only: the validator never
			// echoes the DSN or its credential, and %w keeps the
			// fault.InvalidInput class for the caller.
			return productionBuild{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := installationConfig.Preflight(); err != nil {
		return productionBuild{}, fmt.Errorf("production configuration: %w", err)
	}
	if err := validateBrokerTLS(values["KODAMA_NATS_CA_FILE"], values["KODAMA_NATS_CERT_FILE"], values["KODAMA_NATS_KEY_FILE"]); err != nil {
		return productionBuild{}, err
	}

	keyset, err := loadProductionKeys(getenv)
	if err != nil {
		return productionBuild{}, err
	}
	kms := keyset.kms

	kubeConfig := kube
	if kubeConfig.Server == "" {
		host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return productionBuild{}, fmt.Errorf("production runs inside the cluster: KUBERNETES_SERVICE_HOST/PORT are unset")
		}
		kubeConfig.Server = "https://" + net.JoinHostPort(host, port)
	}
	cluster, err := driverkube.NewCluster(driverkube.ClusterConfig{InCluster: &kubeConfig, Timeout: 2 * time.Minute})
	if err != nil {
		return productionBuild{}, fmt.Errorf("kubernetes client: %w", err)
	}

	oidcClient, err := oidcHTTPClient(getenv("KODAMA_OIDC_CA_FILE"))
	if err != nil {
		return productionBuild{}, err
	}
	discovery, err := options.discoveryConfig()
	if err != nil {
		return productionBuild{}, err
	}
	models, toolProxy, err := options.gatewayConfigurations(shellapp.ProfileProduction)
	if err != nil {
		return productionBuild{}, err
	}
	natsAuth, err := natsAuthFromEnv(getenv)
	if err != nil {
		return productionBuild{}, err
	}

	cert, err := tls.LoadX509KeyPair(filepath.Join(values["KODAMA_TLS_DIR"], "tls.crt"), filepath.Join(values["KODAMA_TLS_DIR"], "tls.key"))
	if err != nil {
		return productionBuild{}, fmt.Errorf("server certificate: %w", err)
	}

	template := func(namespace string, tenant id.TenantID) *shellapp.WorkspacePVC {
		return &shellapp.WorkspacePVC{
			Namespace:    namespace,
			TenantID:     tenant,
			StorageClass: values["KODAMA_WORKSPACE_PVC_STORAGE_CLASS"],
			Size:         values["KODAMA_WORKSPACE_PVC_SIZE"],
			AccessMode:   values["KODAMA_WORKSPACE_PVC_ACCESS_MODE"],
		}
	}
	config := shellapp.Config{
		Profile:        shellapp.ProfileProduction,
		Installation:   installationConfig,
		InstallationID: options.InstallationID,
		DataDir:        options.DataDir,
		MCPPath:        options.MCPPath,
		LoopInterval:   loopInterval,
		GitStorageRoot: gitStorageRoot,
		Models:         models,
		ToolProxy:      toolProxy,
		NATSAuth:       natsAuth,
		Production: &shellapp.ProductionConfig{
			RuntimeCloser:    func(context.Context) error { return nil },
			KMS:              kms,
			Cloud:            keyset.cloud,
			ClaimProvisioner: cluster,
			NATSTLS:          &natsdriver.TLSConfig{CAFile: values["KODAMA_NATS_CA_FILE"], CertFile: values["KODAMA_NATS_CERT_FILE"], KeyFile: values["KODAMA_NATS_KEY_FILE"]},
			ControllerURL:    values["KODAMA_CONTROLLER_DATABASE_URL"],
			PublisherURL:     values["KODAMA_PUBLISHER_DATABASE_URL"],
			AuthURL:          values["KODAMA_AUTH_DATABASE_URL"],
			OIDC: oidc.Config{
				OIDCConfig:         identity.OIDCConfig{Issuer: values["KODAMA_OIDC_ISSUER"], Audience: values["KODAMA_OIDC_AUDIENCE"], ClockSkew: time.Minute},
				JWKSURL:            values["KODAMA_OIDC_JWKS_URL"],
				Algorithms:         []string{oidc.AlgorithmRS256, oidc.AlgorithmES256},
				HTTPClient:         oidcClient,
				MaxJWKSBytes:       1 << 20,
				MaxTokenBytes:      1 << 20,
				CacheTTL:           10 * time.Minute,
				MinRefreshInterval: 10 * time.Second,
				SubjectClaim:       strings.TrimSpace(getenv("KODAMA_OIDC_SUBJECT_CLAIM")),
				TenantClaim:        values["KODAMA_OIDC_TENANT_CLAIM"],
				ProjectClaim:       strings.TrimSpace(getenv("KODAMA_OIDC_PROJECT_CLAIM")),
			},
			Registry:  registry.Config{Transport: http.DefaultTransport, Credentials: registryCredentials},
			Discovery: discovery,
		},
	}
	// One runtime adapter and one workspace claim template per tenant. The
	// singleton KODAMA_TENANT form builds the single-tenant
	// composition; the explicit list form serves every listed tenant.
	if tenants.list {
		config.Production.Tenants = make([]shellapp.ProductionTenant, 0, len(tenants.entries))
		for _, entry := range tenants.entries {
			adapter, err := newProductionTenantAdapter(getenv, cluster, entry, options.PublicURL)
			if err != nil {
				return productionBuild{}, err
			}
			config.Production.Tenants = append(config.Production.Tenants, shellapp.ProductionTenant{
				TenantID:     entry.ID,
				Runtime:      adapter,
				Workspace:    adapter,
				WorkspacePVC: template(entry.Namespace, entry.ID),
			})
		}
	} else {
		entry := tenants.entries[0]
		adapter, err := newProductionTenantAdapter(getenv, cluster, entry, options.PublicURL)
		if err != nil {
			return productionBuild{}, err
		}
		config.Production.Runtime = adapter
		config.Production.Workspace = adapter
		config.WorkspacePVC = template(entry.Namespace, entry.ID)
	}
	return productionBuild{Config: config, TLS: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}, nil
}

// maxTenantNamespaceIDLength bounds a tenant id so the derived namespace
// kodama-tenant-<tenant> stays within the 63-character Kubernetes namespace
// limit (the prefix is 14 characters). The Helm chart enforces the same bound
// and grammar.
const maxTenantNamespaceIDLength = 49

// tenantNamespaceIDPattern is the lowercase DNS-label grammar a tenant id must
// satisfy for the derived namespace to be valid. id.ParseTenantID is looser
// (uppercase, underscore), so a non-chart deployment would otherwise fail only
// at the first namespace or claim use.
var tenantNamespaceIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validTenantNamespaceID reports whether a tenant id can be derived into a
// valid Kubernetes namespace.
func validTenantNamespaceID(tenant string) bool {
	return len(tenant) <= maxTenantNamespaceIDLength && tenantNamespaceIDPattern.MatchString(tenant)
}

// productionTenantEntry is one validated tenant of the production entry and
// the namespace its adapters and claim templates are bound to.
type productionTenantEntry struct {
	ID        id.TenantID
	Namespace string
}

// productionTenantSet is the validated tenant configuration: either the
// explicit KODAMA_TENANTS list or the singleton KODAMA_TENANT compatibility
// form.
type productionTenantSet struct {
	entries []productionTenantEntry
	list    bool
}

// parseProductionTenants validates the installation's tenant configuration.
// The two forms are mutually exclusive, the list is complete JSON, every id is
// a tenant identity, duplicates are refused, and the singleton namespace
// override is refused for more than one tenant. Every refusal uses fixed text
// and never renders a configured value.
func parseProductionTenants(getenv productionEnv) (productionTenantSet, error) {
	listValue := strings.TrimSpace(getenv("KODAMA_TENANTS"))
	singletonValue := strings.TrimSpace(getenv("KODAMA_TENANT"))
	if listValue != "" && singletonValue != "" {
		return productionTenantSet{}, fmt.Errorf("production refuses both KODAMA_TENANT and KODAMA_TENANTS; set exactly one")
	}
	if listValue == "" && singletonValue == "" {
		return productionTenantSet{}, fmt.Errorf("production requires KODAMA_TENANT or KODAMA_TENANTS")
	}
	namespaceOverride := strings.TrimSpace(getenv("KODAMA_TENANT_NAMESPACE"))

	if listValue == "" {
		tenant, err := id.ParseTenantID(singletonValue)
		if err != nil {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANT is not a valid tenant id")
		}
		if !validTenantNamespaceID(string(tenant)) {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANT is not a lowercase DNS label of at most %d characters", maxTenantNamespaceIDLength)
		}
		namespace := namespaceOverride
		if namespace == "" {
			namespace = "kodama-tenant-" + string(tenant)
		}
		return productionTenantSet{entries: []productionTenantEntry{{ID: tenant, Namespace: namespace}}}, nil
	}

	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(listValue), &raw); err != nil {
		return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS is not a valid JSON list of tenant ids")
	}
	list := make([]string, 0, len(raw))
	for _, entry := range raw {
		var decoded any
		if err := json.Unmarshal(entry, &decoded); err != nil {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS is not a valid JSON list of tenant ids")
		}
		value, ok := decoded.(string)
		if !ok {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS is not a valid JSON list of tenant ids")
		}
		list = append(list, value)
	}
	if len(list) == 0 {
		return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS is empty; a production installation serves at least one tenant")
	}
	if len(list) > 1 && namespaceOverride != "" {
		return productionTenantSet{}, fmt.Errorf("production refuses KODAMA_TENANT_NAMESPACE with more than one KODAMA_TENANTS entry; each tenant derives its own namespace")
	}
	entries := make([]productionTenantEntry, 0, len(list))
	seen := make(map[id.TenantID]bool, len(list))
	for _, value := range list {
		tenant, err := id.ParseTenantID(strings.TrimSpace(value))
		if err != nil {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS contains an invalid tenant id")
		}
		if !validTenantNamespaceID(string(tenant)) {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS contains a tenant id that is not a lowercase DNS label of at most %d characters", maxTenantNamespaceIDLength)
		}
		if seen[tenant] {
			return productionTenantSet{}, fmt.Errorf("KODAMA_TENANTS repeats a tenant id")
		}
		seen[tenant] = true
		namespace := namespaceOverride
		if namespace == "" {
			namespace = "kodama-tenant-" + string(tenant)
		}
		entries = append(entries, productionTenantEntry{ID: tenant, Namespace: namespace})
	}
	return productionTenantSet{entries: entries, list: true}, nil
}

// newProductionTenantAdapter builds one tenant's Kubernetes runtime and
// workspace adapter from the shared cluster client and the installation
// workload policy.
func newProductionTenantAdapter(getenv productionEnv, cluster *driverkube.Cluster, tenant productionTenantEntry, publicURL string) (*runtimekube.Adapter, error) {
	imagePullSecrets, err := parseImagePullSecrets(getenv("KODAMA_WORKLOAD_IMAGE_PULL_SECRETS"))
	if err != nil {
		return nil, err
	}
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   cluster,
		Namespace: tenant.Namespace,
		TenantID:  tenant.ID,
		KodamaURL: publicURL,
		Policy: runtimekube.Policy{
			CPURequest:    envOrGetenv(getenv, "KODAMA_WORKLOAD_CPU_REQUEST", "250m"),
			CPULimit:      envOrGetenv(getenv, "KODAMA_WORKLOAD_CPU_LIMIT", "1"),
			MemoryRequest: envOrGetenv(getenv, "KODAMA_WORKLOAD_MEMORY_REQUEST", "256Mi"),
			MemoryLimit:   envOrGetenv(getenv, "KODAMA_WORKLOAD_MEMORY_LIMIT", "1Gi"),
			JobTTLSeconds: 600,
		},
		Cleanup:          runtimekube.CleanupPolicy{Attempts: 3, Backoff: time.Second},
		WorkloadCA:       runtimekube.WorkloadCAConfig{SecretName: strings.TrimSpace(getenv("KODAMA_WORKLOAD_CA_SECRET"))},
		ImagePullSecrets: imagePullSecrets,
	})
	if err != nil {
		return nil, fmt.Errorf("kubernetes runtime: %w", err)
	}
	return adapter, nil
}

// resolveGitStorageRoot reads the optional mounted Git storage root the
// retained-commit keeper enumerates. The keeper never creates the root: an
// unmounted PVC would otherwise look like an empty installation and silently
// disable keep refs. A configured root must therefore be an absolute existing
// directory, and a partial or unusable value is a named configuration refusal.
func resolveGitStorageRoot(options serverOptions, getenv productionEnv) (string, error) {
	root := strings.TrimSpace(options.GitStorageRoot)
	if root == "" {
		root = strings.TrimSpace(getenv("KODAMA_GIT_STORAGE_ROOT"))
	}
	if root == "" {
		return "", nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: KODAMA_GIT_STORAGE_ROOT must be an absolute mounted path",
			shellapp.ErrKeepRefsConfiguration)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: KODAMA_GIT_STORAGE_ROOT is not a mounted directory",
			shellapp.ErrKeepRefsConfiguration)
	}
	return root, nil
}

func envOrGetenv(getenv productionEnv, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

// productionRegistryCredentials loads the optional docker-config credential
// file named by KODAMA_REGISTRY_CONFIG_FILE. An unset setting leaves the
// installation anonymous (public registries); a configured file must be
// readable and well formed, so the entry never silently drops a private
// registry credential.
func productionRegistryCredentials(getenv productionEnv) (registry.CredentialSource, error) {
	path := strings.TrimSpace(getenv("KODAMA_REGISTRY_CONFIG_FILE"))
	if path == "" {
		return nil, nil
	}
	source, err := registry.LoadDockerConfig(path)
	if err != nil {
		return nil, fmt.Errorf("KODAMA_REGISTRY_CONFIG_FILE: %w", err)
	}
	return source, nil
}

// parseImagePullSecrets reads the optional comma-separated
// KODAMA_WORKLOAD_IMAGE_PULL_SECRETS list. The adapter validates every name as
// a Kubernetes DNS-1123 subdomain before the first Job; this function only
// rejects an empty list entry.
func parseImagePullSecrets(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("KODAMA_WORKLOAD_IMAGE_PULL_SECRETS contains an empty name")
		}
		names = append(names, name)
	}
	return names, nil
}

// productionKeys is the validated key material the server and the one-shot
// first-admin bootstrap share. Both commands read the same chart environment;
// keeping one loader means neither can silently select a different key adapter
// or key version from the other.
type productionKeys struct {
	kms      keys.KMSClient
	provider keys.Provider
	cloud    keys.CloudConfig
	version  secret.KeyVersion
	blob     []byte
}

// productionKeyAdapter reads the explicitly selected cloud KMS adapter. There
// is no default: an empty or non-cloud value is a configuration refusal, never
// a weaker key path.
func productionKeyAdapter(getenv productionEnv) (installation.KeyAdapter, error) {
	adapter := installation.KeyAdapter(strings.TrimSpace(getenv("KODAMA_KEY_ADAPTER")))
	switch adapter {
	case installation.KeyScalewayKMS, installation.KeyAWSKMS, installation.KeyGCPKMS:
		return adapter, nil
	default:
		return "", fmt.Errorf("production supports KODAMA_KEY_ADAPTER=%s, %s or %s only",
			installation.KeyScalewayKMS, installation.KeyAWSKMS, installation.KeyGCPKMS)
	}
}

// loadProductionKeys validates the cloud key configuration, builds the KMS
// client of the selected adapter and loads the current wrapped key blob. Each
// adapter has exactly one credential mode: Scaleway reads the static
// SCW_SECRET_KEY, AWS uses the IRSA role and projected token the EKS pod
// identity webhook injects, and GCP uses the GKE metadata server. A missing
// credential setting or an unreadable blob is a configuration refusal, never a
// weaker ephemeral key path.
func loadProductionKeys(getenv productionEnv) (productionKeys, error) {
	adapter, err := productionKeyAdapter(getenv)
	if err != nil {
		return productionKeys{}, err
	}
	need := func(name string) (string, error) {
		value := strings.TrimSpace(getenv(name))
		if value == "" {
			return "", fmt.Errorf("production requires %s", name)
		}
		return value, nil
	}
	keyRef, err := need("KODAMA_KEY_REF")
	if err != nil {
		return productionKeys{}, err
	}
	blobsDir, err := need("KODAMA_KEY_BLOBS_DIR")
	if err != nil {
		return productionKeys{}, err
	}
	kms, newProvider, err := productionKMS(adapter, secret.KeyRef(keyRef), getenv, need)
	if err != nil {
		return productionKeys{}, err
	}
	version, err := strconv.ParseUint(strings.TrimSpace(getenv("KODAMA_KEY_CURRENT_VERSION")), 10, 32)
	if err != nil || version < 1 {
		return productionKeys{}, fmt.Errorf("KODAMA_KEY_CURRENT_VERSION must be a positive integer")
	}
	keyVersion := secret.KeyVersion(version)
	blob, err := os.ReadFile(filepath.Join(blobsDir, fmt.Sprintf("kek-v%d.blob", version)))
	if err != nil {
		return productionKeys{}, fmt.Errorf("read the current key blob: %w", err)
	}
	cloud := keys.CloudConfig{
		KeyRef:   secret.KeyRef(keyRef),
		Current:  keyVersion,
		Versions: map[secret.KeyVersion][]byte{keyVersion: blob},
	}
	provider, err := newProvider(kms, cloud, rand.Reader)
	if err != nil {
		return productionKeys{}, err
	}
	return productionKeys{kms: kms, provider: provider, cloud: cloud, version: keyVersion, blob: blob}, nil
}

// cloudProviderFactory is the shared shape of keys.NewScaleway, keys.NewAWS and
// keys.NewGCP.
type cloudProviderFactory func(keys.KMSClient, keys.CloudConfig, io.Reader) (keys.Provider, error)

// productionKMS builds the KMS client of one adapter from its own credential
// settings. No client makes a request at construction. The client endpoints are
// never taken from the environment: an operator-selected origin would receive
// the credential.
func productionKMS(adapter installation.KeyAdapter, keyRef secret.KeyRef, getenv productionEnv,
	need func(string) (string, error)) (keys.KMSClient, cloudProviderFactory, error) {
	switch adapter {
	case installation.KeyScalewayKMS:
		secretKey, err := need("SCW_SECRET_KEY")
		if err != nil {
			return nil, nil, err
		}
		kms, err := keys.NewScalewayKMS(keys.ScalewayKMSConfig{SecretKey: secretKey})
		if err != nil {
			return nil, nil, err
		}
		return kms, keys.NewScaleway, nil
	case installation.KeyAWSKMS:
		// The EKS pod identity webhook injects all three settings into a pod
		// whose ServiceAccount carries the role annotation. A full key ARN
		// names its own region, so AWS_REGION is required only for an alias
		// reference; when both are present they must agree.
		reference, err := keys.ParseAWSReference(keyRef)
		if err != nil {
			return nil, nil, err
		}
		region := strings.TrimSpace(getenv("AWS_REGION"))
		if region == "" {
			region = reference.Region
		}
		if region == "" {
			return nil, nil, fmt.Errorf("production requires AWS_REGION for an alias key reference")
		}
		if err := reference.ValidateRegion(region); err != nil {
			return nil, nil, err
		}
		roleARN, err := need("AWS_ROLE_ARN")
		if err != nil {
			return nil, nil, err
		}
		tokenFile, err := need("AWS_WEB_IDENTITY_TOKEN_FILE")
		if err != nil {
			return nil, nil, err
		}
		kms, err := keys.NewAWSKMS(keys.AWSKMSConfig{Region: region, RoleARN: roleARN, WebIdentityTokenFile: tokenFile})
		if err != nil {
			return nil, nil, err
		}
		return kms, keys.NewAWS, nil
	case installation.KeyGCPKMS:
		kms, err := keys.NewGCPKMS(keys.GCPKMSConfig{})
		if err != nil {
			return nil, nil, err
		}
		return kms, keys.NewGCP, nil
	default:
		return nil, nil, fmt.Errorf("production supports KODAMA_KEY_ADAPTER=%s, %s or %s only",
			installation.KeyScalewayKMS, installation.KeyAWSKMS, installation.KeyGCPKMS)
	}
}

// validateBrokerTLS reads the pinned broker trust material once at startup so
// a missing or malformed CA, client certificate or key is an exit-2
// configuration refusal. The messages never render file content.
func validateBrokerTLS(caFile, certFile, keyFile string) error {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("broker TLS certificate authority is unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("broker TLS certificate authority is not a usable certificate")
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return fmt.Errorf("broker TLS client certificate is unusable")
	}
	return nil
}

// oidcHTTPClient trusts the system roots plus, optionally, one extra CA bundle
// (the installation CA, for an identity provider served inside the cluster).
func oidcHTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(caFile) != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read OIDC CA bundle: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("OIDC CA bundle has no usable certificate")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}, nil
}
