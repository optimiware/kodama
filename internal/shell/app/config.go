package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/token"
)

var (
	// ErrKeepRefsConfiguration reports a retained-commit keep-ref configuration
	// that cannot be honoured, for example a repository list without the
	// storage root it resolves against.
	ErrKeepRefsConfiguration = errors.New("retained-commit keep refs are misconfigured")
	// ErrKeepRefsWithoutRetainedCommits reports keep refs configured in a
	// composition whose durable store cannot supply the authoritative retained
	// commit set. The sweep must refuse to start instead of releasing every ref
	// or silently doing nothing.
	ErrKeepRefsWithoutRetainedCommits = errors.New("retained-commit keep refs require a durable retained-commit source")
)

// Profile is one explicit Shell composition profile.
type Profile string

const (
	// ProfileDevelopment is the local development composition.
	ProfileDevelopment Profile = "development"
	// ProfileTest is the test composition: either the fully injected harness
	// form or, with Config.Test, the durable single-host test environment.
	ProfileTest Profile = "test"
	// ProfileProduction is the real-adapter production composition.
	ProfileProduction Profile = "production"
)

// Valid reports whether the profile is one of the declared profiles.
func (profile Profile) Valid() bool {
	return profile == ProfileDevelopment || profile == ProfileTest || profile == ProfileProduction
}

// DefaultLoopInterval is the default background scan cadence.
const DefaultLoopInterval = 20 * time.Millisecond

// DefaultActionTimeout is the default admitted action timeout.
const DefaultActionTimeout = 5 * time.Minute

// DefaultMCPPath is the composition HTTP path of the MCP JSON-RPC edge.
//
// The public API declares the REST route inventory and the MCP tool mapping,
// not an HTTP path for the JSON-RPC edge. The composition therefore chooses one
// explicit path instead of guessing a route.
const DefaultMCPPath = "/mcp"

// Clock is the authoritative time source and cancellable sleeper every
// capability and background loop consumes.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// DevToken is one explicitly configured development bearer credential.
//
// It is a static token mapped to a principal. It is refused in production.
type DevToken struct {
	// Credential is the bearer token value. It is never logged.
	Credential string
	// Principal is the current principal the credential resolves to.
	Principal identity.Principal
	// ExpiresAt is the credential/session expiry. A human session expiry is
	// also exposed through event.subscribe.
	ExpiresAt time.Time
}

// Bootstrap is the first-administrator seed performed through the store seam
// before serving. It is idempotent and is refused in production.
type Bootstrap struct {
	TenantID       id.TenantID
	TenantName     string
	ProjectID      id.ProjectID
	ProjectName    string
	AdminPrincipal id.PrincipalID
	AdminRoles     []string
}

// DevelopmentConfig is the explicit development/test authentication and
// bootstrap configuration.
type DevelopmentConfig struct {
	Tokens    []DevToken
	Bootstrap *Bootstrap
}

// maxTenantNamespaceIDLength bounds a tenant id so the chart's derived
// namespace kodama-tenant-<tenant> stays within the 63-character Kubernetes
// namespace limit (the prefix is 14 characters).
const maxTenantNamespaceIDLength = 49

// tenantNamespaceIDPattern is the tenant id grammar the Helm chart derives a
// tenant namespace from. Kubernetes namespace names are lowercase DNS labels,
// so an uppercase, underscore, dotted or otherwise non-DNS id would otherwise
// fail only at the first namespace or claim use.
var tenantNamespaceIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validateTenantNamespaceID refuses a tenant id the installation's own
// namespace derivation cannot turn into a valid Kubernetes namespace. The
// refusal never renders the configured value.
func validateTenantNamespaceID(value id.TenantID) error {
	text := string(value)
	if len(text) > maxTenantNamespaceIDLength || !tenantNamespaceIDPattern.MatchString(text) {
		return fmt.Errorf("tenant id is not a lowercase DNS label of at most %d characters", maxTenantNamespaceIDLength)
	}
	return nil
}

// ProductionTenant is one tenant's production adapter set. An installation
// with more than one tenant composes one of these per tenant and routes every
// Runtime and Workspace operation by the durable owning tenant.
type ProductionTenant struct {
	// TenantID is the tenant this adapter set serves. Required and validated.
	TenantID id.TenantID
	// Runtime and Workspace are the tenant-bound Kubernetes adapters. Both are
	// required; a single-tenant adapter pair is never reused across tenants.
	Runtime   kernelengine.Runtime
	Workspace kernelengine.Workspace
	// WorkspacePVC is this tenant's trusted claim template. It is required
	// exactly when ProductionConfig.ClaimProvisioner is set and nil otherwise.
	WorkspacePVC *WorkspacePVC
}

// ProductionConfig is the explicit real-adapter production configuration.
//
// The Kubernetes runtime and the cloud KMS client are injected: the command
// entry builds them (the KMS clients are standard library only, with no cloud
// SDK) and this composition depends only on their ports.
type ProductionConfig struct {
	// Runtime and Workspace are the single-tenant adapter pair. They are
	// mutually exclusive with Tenants: one of the two forms is required, and a
	// dual configuration is refused as ambiguous.
	Runtime   kernelengine.Runtime
	Workspace kernelengine.Workspace
	// Tenants is the explicit multi-tenant adapter set of one installation.
	// Every listed tenant is served; an execution whose persisted tenant is not
	// in this set is refused without an effect. Empty selects the single-tenant
	// Runtime/Workspace pair.
	Tenants       []ProductionTenant
	RuntimeCloser func(context.Context) error
	KMS           keys.KMSClient
	Cloud         keys.CloudConfig
	OIDC          oidc.Config
	Registry      registry.Config
	// ClaimProvisioner, with Config.WorkspacePVC, makes the composition create
	// each execution's workspace claim before its first activation. Without
	// both, claims must already exist and a missing one fails visibly.
	ClaimProvisioner ClaimProvisioner
	// NATSTLS verifies the broker against a private CA (and optionally presents
	// a client certificate). Nil dials without a custom CA, so a tls:// URL then
	// relies on the system roots.
	NATSTLS       *natsdriver.TLSConfig
	ControllerURL string
	PublisherURL  string
	AuthURL       string

	// Streams configures the JetStream streams the composition provisions at
	// startup. A nil value selects the driver defaults.
	Streams *StreamConfig

	// Discovery is the optional public login discovery document. It is the
	// production placement for the same document Config.Discovery declares;
	// the issuer and audience are reused from OIDC, so a differing explicit
	// copy is refused at startup. A nil value keeps the discovery route a
	// clean 404 and interactive login unavailable.
	Discovery *DiscoveryConfig

	// dialNATS is a same-package test seam for the NATS driver dial. A real
	// installation leaves it nil and the composition uses the real driver.
	dialNATS natsDial
}

// Adapters is the fully injected adapter set of ProfileTest and the optional
// deterministic overrides of ProfileDevelopment. A nil field uses the
// development default where one exists.
type Adapters struct {
	Store       store.Executor
	Scanner     store.Scanner
	Controller  store.Controller
	Outbox      event.Outbox
	Credentials store.CredentialLookup
	Runtime     kernelengine.Runtime
	Workspace   kernelengine.Workspace
	Keys        keys.Provider
	Publisher   event.Publisher
	Invalidator token.InvalidationPort
	// LivePublisher overrides the live log downstream explicitly. A nil value
	// still uses the Publisher when it implements the live publisher seam.
	LivePublisher      shelllog.LivePublisher
	Clock              Clock
	IDs                *bridges.IDs
	Principal          authorize.CurrentPrincipal
	CredentialValidity event.CredentialValidity
	Registry           pipeline.Resolver
	LogSink            shelllog.RetainedSink
	RuntimeCloser      func(context.Context) error
	PublisherCloser    func(context.Context) error

	// VolumeProvisioner enables the fresh-only workspace provisioning
	// decorator for the Podman runtime. A nil value leaves the workspace
	// adapter unwrapped.
	VolumeProvisioner VolumeProvisioner
	// WorkspaceActivation supplies the durable activation fact the decorator
	// consults. A nil value builds the composition's store-backed fact source.
	WorkspaceActivation WorkspaceActivation
	// ClaimProvisioner enables the fresh-only Kubernetes workspace claim
	// decorator. A nil value leaves the workspace adapter unwrapped; claims are
	// then provisioned externally and Retain still fails visibly for a missing
	// claim.
	ClaimProvisioner ClaimProvisioner
}

// NATSAuthConfig is the per-credential broker access configuration. The
// control plane connects with User/Password (the broker's bypass user);
// external clients present their Kodama credential to the Callout service.
type NATSAuthConfig struct {
	// User and Password are the control-plane bypass credentials. They are
	// required together and are never a per-user credential: the broker lists
	// this user in auth_users so the control plane can receive and answer.
	User     string
	Password string
	// Callout enables the authorization service. Nil leaves external
	// per-credential broker access off.
	Callout *NATSCalloutConfig
}

// NATSCalloutConfig is the broker authorization-callout service configuration.
type NATSCalloutConfig struct {
	// User and Password authenticate the callout service's own broker
	// connection. The broker lists this user in auth_users.
	User     string
	Password string
	// IssuerSeed is the account seed that signs grants. The broker is
	// configured with the matching public key as auth_callout.issuer.
	IssuerSeed string
	// Account is the broker account admitted clients are bound to.
	Account string
}

// Config is the complete composition input.
type Config struct {
	Profile              Profile
	Installation         installation.Config
	InstallationID       string
	DataDir              string
	DefaultActionTimeout time.Duration
	LoopInterval         time.Duration
	MCPPath              string

	// RenewBefore is the workload credential renewal margin. A running
	// execution's committed credential is renewed once its committed expiry is
	// at or before now+RenewBefore and the current action deadline is still in
	// the future. Zero selects DefaultRenewBefore; a negative value is refused
	// at construction.
	RenewBefore time.Duration

	// RenewInterval is the renewal scan cadence. Zero selects
	// DefaultRenewInterval. It is independent of LoopInterval so renewal can
	// run at its own pace.
	RenewInterval time.Duration

	Logger     *slog.Logger
	Dev        *DevelopmentConfig
	Production *ProductionConfig
	Adapters   *Adapters

	// NATSAuth is the optional per-credential broker access configuration.
	// A nil value keeps the broker authenticated only by TLS and leaves
	// external per-credential access off.
	NATSAuth *NATSAuthConfig
	// Test selects the durable form of ProfileTest. It is refused by every
	// other profile and never mixed with Dev, Production or Adapters.
	Test *TestConfig

	// WorkspacePVC is the trusted Kubernetes workspace claim template. It is
	// required when a ClaimProvisioner is injected for the Kubernetes runtime.
	// Production accepts it only together with a claim provisioner; either half
	// alone is refused at construction.
	WorkspacePVC *WorkspacePVC

	// SecretRules is the trusted workload-consumer secret-name allow-list.
	// The composition builds the workload name policy from it and resolves
	// only declared names that an explicit rule authorizes in the execution's
	// scope. An empty list authorizes no activation secret, so a missing
	// allow-list fails closed. Rules never carry secret values.
	SecretRules []secret.NameRule

	// ToolProxy is the optional external MCP tool proxy configuration. A nil
	// value keeps the optional surface disabled with the not_found
	// refusal and constructs no upstream dependency. An enabled configuration
	// maps every public tool alias to exactly one project, upstream server and
	// upstream tool; its MCP secret allow-list is separate from SecretRules.
	ToolProxy *ToolProxyConfig

	// Models is the optional configured model gateway and workload LLM proxy. A
	// nil value keeps the optional surface disabled with the not_found refusal
	// and constructs no provider dependency. An enabled configuration maps every
	// caller alias or provider slug to trusted endpoints; its gateway secret
	// allow-list is separate from SecretRules.
	Models *ModelsConfig

	// WorkloadURL optionally specifies the base URL workloads inside containers
	// use to reach this Kodama server when it differs from Installation.PublicURL
	// (such as https://host.containers.internal:8443 in the Podman test profile).
	WorkloadURL string

	// GitStorageRoot enables retained-commit keep refs. The retention loop
	// keeps one ref per commit named by a retained execution under
	// refs/kodama/keep/<sha> in every bare repository under this root's tenant
	// directories. GitRepositories optionally narrows that set to explicit
	// tenant-relative names; an empty list enumerates storage. Leaving the root
	// empty keeps the sweep disabled, and a repository list without a root is a
	// configuration refusal.
	GitStorageRoot  string
	GitRepositories []string

	// Discovery is the optional public login discovery document served at
	// api.DiscoveryPath. It is validated at construction; a nil value keeps the
	// route a clean 404 so kodactl login reports that the login configuration
	// is unavailable. The production profile may also (or instead) set
	// ProductionConfig.Discovery next to the OIDC verifier it reuses.
	Discovery *DiscoveryConfig

	// hooks is a same-package test seam for partial-start failure injection.
	hooks *startupHook
	// dispatchDisabled is a same-package test seam that starts the composition
	// with the installation dispatch gate closed.
	dispatchDisabled bool
}

// startupHook records a named startup step and can fail exactly one step.
type startupHook struct {
	failAt  string
	stopped []string
}

func (hook *startupHook) before(name string) error {
	if hook != nil && hook.failAt == name {
		return fault.New(fault.InternalFailure, fmt.Sprintf("injected startup failure at %q", name))
	}
	return nil
}

func (hook *startupHook) stoppedStep(name string) {
	if hook != nil {
		hook.stopped = append(hook.stopped, name)
	}
}

// logger returns the configured logger or the default one.
func (config Config) logger() *slog.Logger {
	if config.Logger != nil {
		return config.Logger
	}
	return slog.Default()
}

// workloadBaseURL returns the base URL workloads reach this server through,
// without a trailing slash.
func (config Config) workloadBaseURL() string {
	base := strings.TrimSpace(config.WorkloadURL)
	if base == "" {
		base = strings.TrimSpace(config.Installation.PublicURL)
	}
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return strings.TrimRight(base, "/")
}

// resolved fills the explicit defaults.
func (config Config) resolved() Config {
	if config.DefaultActionTimeout == 0 {
		config.DefaultActionTimeout = DefaultActionTimeout
	}
	if config.LoopInterval == 0 {
		config.LoopInterval = DefaultLoopInterval
	}
	if config.RenewBefore == 0 {
		config.RenewBefore = DefaultRenewBefore
	}
	if config.RenewInterval <= 0 {
		config.RenewInterval = DefaultRenewInterval
	}
	if config.MCPPath == "" {
		config.MCPPath = DefaultMCPPath
	}
	if config.Adapters == nil {
		config.Adapters = &Adapters{}
	}
	return config
}

// validate checks the profile-specific composition before any adapter is
// constructed. Every refusal names the missing or weaker setting.
func (config Config) validate() error {
	if !config.Profile.Valid() {
		return fault.New(fault.InvalidInput, "composition profile is not one of the declared profiles")
	}
	if strings.TrimSpace(config.InstallationID) == "" {
		return fault.New(fault.InvalidInput, "composition requires an installation id")
	}
	if config.DefaultActionTimeout <= 0 {
		return fault.New(fault.InvalidInput, "composition requires a positive default action timeout")
	}
	if config.LoopInterval <= 0 {
		return fault.New(fault.InvalidInput, "composition requires a positive loop interval")
	}
	if config.RenewBefore < 0 {
		return fault.New(fault.InvalidInput, "composition requires a non-negative renewal margin")
	}
	if !strings.HasPrefix(config.MCPPath, "/") || strings.HasSuffix(config.MCPPath, "/") {
		return fault.New(fault.InvalidInput, "MCP path must start with / and have no trailing slash")
	}
	if strings.TrimSpace(config.GitStorageRoot) == "" && len(config.GitRepositories) > 0 {
		return fault.Wrap(fault.InvalidInput,
			"retained-commit keep refs name repositories without a Git storage root", ErrKeepRefsConfiguration)
	}
	for _, repository := range config.GitRepositories {
		if strings.TrimSpace(repository) == "" {
			return fault.Wrap(fault.InvalidInput,
				"retained-commit keep refs require a non-empty repository name", ErrKeepRefsConfiguration)
		}
	}
	if config.Test != nil && config.Profile != ProfileTest {
		return fault.New(fault.InvalidInput, "only the test profile accepts a durable test configuration")
	}
	switch config.Profile {
	case ProfileDevelopment:
		if err := config.Installation.Preflight(); err != nil {
			return err
		}
		if config.Dev == nil {
			return fault.New(fault.InvalidInput, "development composition requires explicit development authentication and bootstrap")
		}
		if len(config.Dev.Tokens) == 0 {
			return fault.New(fault.InvalidInput, "development composition requires at least one development token")
		}
		if err := validateDevTokens(config.Dev.Tokens); err != nil {
			return err
		}
		if config.Production != nil {
			return fault.New(fault.InvalidInput, "development composition refuses a production adapter configuration")
		}
	case ProfileTest:
		if config.Test != nil {
			if err := config.validateDurableTest(); err != nil {
				return err
			}
			break
		}
		if config.Dev == nil {
			return fault.New(fault.InvalidInput, "test composition requires explicit development authentication and bootstrap")
		}
		if err := validateDevTokens(config.Dev.Tokens); err != nil {
			return err
		}
		if err := config.Adapters.requireInjected(); err != nil {
			return err
		}
		if config.Production != nil {
			return fault.New(fault.InvalidInput, "test composition refuses a production adapter configuration")
		}
	case ProfileProduction:
		if err := config.Installation.Preflight(); err != nil {
			return err
		}
		if config.Installation.Environment != installation.EnvironmentProduction {
			return fault.New(fault.InvalidInput, "production composition requires the production installation environment")
		}
		if config.Dev != nil {
			return fault.New(fault.InvalidInput, "production composition refuses a development authenticator and first-admin seed")
		}
		if config.Adapters != nil && config.Adapters.hasDevelopmentAdapter() {
			return fault.New(fault.InvalidInput, "production composition refuses a development or test adapter")
		}
		if config.Production == nil {
			return fault.New(fault.InvalidInput, "production composition requires explicit production adapters")
		}
		if strings.TrimSpace(config.DataDir) == "" {
			return fault.New(fault.InvalidInput, "production composition requires a data directory for retained logs")
		}
		if err := config.Production.validate(config.Installation); err != nil {
			return err
		}
	}
	// The trusted Kubernetes workspace claim template is validated before any
	// adapter exists.
	if config.WorkspacePVC != nil {
		if err := validateWorkspacePVC(*config.WorkspacePVC); err != nil {
			return err
		}
	}
	// Production provisions the workspace claim only when both halves are given:
	// the trusted template and the cluster client that creates the claim. A
	// multi-tenant installation supplies one template per tenant instead of the
	// single-tenant Config.WorkspacePVC.
	if config.Profile == ProfileProduction && config.Production != nil {
		if len(config.Production.Tenants) == 0 {
			if (config.WorkspacePVC == nil) != (config.Production.ClaimProvisioner == nil) {
				return fault.New(fault.InvalidInput,
					"production workspace claim provisioning needs both a workspace claim template and a claim provisioner")
			}
		} else {
			if config.WorkspacePVC != nil {
				return fault.New(fault.InvalidInput,
					"production refuses a single-tenant workspace claim template with a tenant list")
			}
			for index, tenant := range config.Production.Tenants {
				if (tenant.WorkspacePVC == nil) != (config.Production.ClaimProvisioner == nil) {
					return fault.New(fault.InvalidInput,
						fmt.Sprintf("production tenant workspace claim provisioning needs both a template per tenant and a claim provisioner (entry %d)", index+1))
				}
			}
		}
	}
	// The public discovery document is validated before any adapter or
	// goroutine exists; a bad document is a startup failure, never a served
	// partial or invalid one.
	if _, _, err := config.discoveryDocument(); err != nil {
		return err
	}
	// The optional tool proxy configuration is trusted input. An incomplete or
	// ambiguous enabled configuration fails before any adapter is constructed;
	// a nil configuration keeps the optional surface disabled.
	if err := config.ToolProxy.validate(); err != nil {
		return err
	}
	// The optional model gateway configuration is trusted input. An incomplete
	// or ambiguous enabled configuration fails before any adapter is
	// constructed; a nil configuration keeps the optional surface disabled.
	if err := config.Models.validate(config.Profile); err != nil {
		return err
	}
	return nil
}

func validateDevTokens(tokens []DevToken) error {
	seen := make(map[string]bool, len(tokens))
	for index, token := range tokens {
		if strings.TrimSpace(token.Credential) == "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("development token %d has an empty credential", index+1))
		}
		if seen[token.Credential] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("development token %d repeats an earlier credential", index+1))
		}
		seen[token.Credential] = true
		if err := token.Principal.Validate(); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("development token %d principal is invalid", index+1), err)
		}
		if token.ExpiresAt.IsZero() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("development token %d has no expiry", index+1))
		}
	}
	return nil
}

// requireInjected refuses a partially injected test composition.
func (adapters *Adapters) requireInjected() error {
	required := []struct {
		name  string
		value any
	}{
		{"store executor", adapters.Store},
		{"store scanner", adapters.Scanner},
		{"store controller", adapters.Controller},
		{"event outbox", adapters.Outbox},
		{"credential lookup", adapters.Credentials},
		{"runtime", adapters.Runtime},
		{"workspace", adapters.Workspace},
		{"key provider", adapters.Keys},
		{"event publisher", adapters.Publisher},
		{"token invalidator", adapters.Invalidator},
		{"clock", adapters.Clock},
		{"identity source", adapters.IDs},
		{"principal resolver", adapters.Principal},
		{"credential validity", adapters.CredentialValidity},
		{"registry resolver", adapters.Registry},
		{"retained log sink", adapters.LogSink},
	}
	for _, dependency := range required {
		if dependency.value == nil {
			return fault.New(fault.InvalidInput, fmt.Sprintf("test composition requires an injected %s", dependency.name))
		}
	}
	return nil
}

// hasDevelopmentAdapter reports whether an explicitly supplied adapter is one
// of the development families. The production path additionally never consults
// the environment or auto-detects a weaker adapter.
func (adapters *Adapters) hasDevelopmentAdapter() bool {
	if adapters == nil {
		return false
	}
	// A production caller supplies only ProductionConfig. Any explicit Adapters
	// override is treated as a weaker/test composition and refused.
	return adapters.Store != nil || adapters.Runtime != nil || adapters.Keys != nil ||
		adapters.Principal != nil || adapters.Publisher != nil || adapters.LivePublisher != nil ||
		adapters.Clock != nil ||
		adapters.VolumeProvisioner != nil || adapters.WorkspaceActivation != nil ||
		adapters.ClaimProvisioner != nil
}

func (config *ProductionConfig) validate(installationConfig installation.Config) error {
	if len(config.Tenants) == 0 {
		if config.Runtime == nil {
			return fault.New(fault.InvalidInput, "production composition requires the kubernetes runtime adapter")
		}
		if config.Workspace == nil {
			return fault.New(fault.InvalidInput, "production composition requires the kubernetes workspace adapter")
		}
	} else {
		// One installation serves either the explicit singleton adapter pair or
		// an explicit tenant list, never both: a dual configuration is
		// ambiguous about which adapter serves which tenant.
		if config.Runtime != nil || config.Workspace != nil {
			return fault.New(fault.InvalidInput,
				"production refuses both a single-tenant runtime and a tenant list")
		}
		seenTenants := make(map[id.TenantID]bool, len(config.Tenants))
		seenNamespaces := make(map[string]int, len(config.Tenants))
		for index, tenant := range config.Tenants {
			if _, err := id.ParseTenantID(string(tenant.TenantID)); err != nil {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d tenant id is invalid", index+1))
			}
			if err := validateTenantNamespaceID(tenant.TenantID); err != nil {
				return fault.Wrap(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d tenant id is not a lowercase DNS label of at most %d characters", index+1, maxTenantNamespaceIDLength), err)
			}
			if seenTenants[tenant.TenantID] {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d repeats an earlier tenant id", index+1))
			}
			seenTenants[tenant.TenantID] = true
			if tenant.Runtime == nil {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d requires a runtime adapter", index+1))
			}
			if tenant.Workspace == nil {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d requires a workspace adapter", index+1))
			}
			if tenant.WorkspacePVC == nil {
				continue
			}
			if err := validateWorkspacePVC(*tenant.WorkspacePVC); err != nil {
				return fault.Wrap(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d workspace claim template is invalid", index+1), err)
			}
			if tenant.WorkspacePVC.TenantID != tenant.TenantID {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entry %d workspace claim template tenant does not match its tenant", index+1))
			}
			if other, ok := seenNamespaces[tenant.WorkspacePVC.Namespace]; ok {
				return fault.New(fault.InvalidInput,
					fmt.Sprintf("production tenant list entries %d and %d share a workspace claim namespace", other+1, index+1))
			}
			seenNamespaces[tenant.WorkspacePVC.Namespace] = index
		}
	}
	if config.KMS == nil {
		return fault.New(fault.InvalidInput, "production composition requires a cloud KMS client")
	}
	if config.Cloud.KeyRef == "" {
		return fault.New(fault.InvalidInput, "production composition requires a cloud KMS key reference")
	}
	if !exactKeyReference(string(config.Cloud.KeyRef), productionKeyPrefix(installationConfig.Keys)) {
		return fault.New(fault.InvalidInput, "production KMS key reference does not belong to the selected cloud KMS adapter")
	}
	if config.OIDC.JWKSURL == "" {
		return fault.New(fault.InvalidInput, "production composition requires an OIDC configuration")
	}
	if config.Registry.Transport == nil {
		return fault.New(fault.InvalidInput, "production composition requires a registry transport")
	}
	if strings.TrimSpace(config.ControllerURL) == "" {
		return fault.New(fault.InvalidInput, "production composition requires a controller database URL")
	}
	if strings.TrimSpace(config.PublisherURL) == "" {
		return fault.New(fault.InvalidInput, "production composition requires a publisher database URL")
	}
	if strings.TrimSpace(config.AuthURL) == "" {
		return fault.New(fault.InvalidInput, "production composition requires an auth database URL")
	}
	// Every production PostgreSQL DSN requires verified TLS.
	// The runtime DSN is Installation.DatabaseURL and Preflight already checked
	// it; these three are the remaining production connections.
	for _, database := range []struct {
		name string
		url  string
	}{
		{"controller", config.ControllerURL},
		{"publisher", config.PublisherURL},
		{"auth", config.AuthURL},
	} {
		if err := installation.ValidateProductionPostgresURL(database.url); err != nil {
			return fault.Wrap(fault.InvalidInput, "production "+database.name+" database URL must require verified TLS", err)
		}
	}
	// The chart's broker configuration verifies a client certificate on every
	// connection (nats.auth mode mtls). A production composition without the
	// pinned CA and client certificate would fail at the first dial, so it is
	// refused as configuration instead of building a weaker broker path.
	if config.NATSTLS == nil ||
		strings.TrimSpace(config.NATSTLS.CAFile) == "" ||
		strings.TrimSpace(config.NATSTLS.CertFile) == "" ||
		strings.TrimSpace(config.NATSTLS.KeyFile) == "" {
		return fault.New(fault.InvalidInput, "production composition requires broker TLS with a CA bundle and a client certificate")
	}
	if err := config.Streams.validate(); err != nil {
		return err
	}
	return nil
}

func productionKeyPrefix(adapter installation.KeyAdapter) string {
	switch adapter {
	case installation.KeyAWSKMS:
		return installation.KeyRefAWSKMSPrefix
	case installation.KeyGCPKMS:
		return installation.KeyRefGCPKMSPrefix
	case installation.KeyScalewayKMS:
		return installation.KeyRefScalewayKMSPrefix
	default:
		return "\x00"
	}
}

// exactKeyReference requires prefix to be an exact prefix of value and the
// remaining key name to be non-empty and free of whitespace or control bytes.
// A Contains, lower-case or Trim-style match would attribute a padded or
// foreign reference to the selected cloud KMS adapter.
func exactKeyReference(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	name := strings.TrimPrefix(value, prefix)
	if name == "" {
		return false
	}
	return strings.IndexFunc(name, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) < 0
}
