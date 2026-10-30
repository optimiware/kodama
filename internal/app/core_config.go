package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Assembly names select what the `server` command composes. The scaffold is the
// T-001 process shell (health only, no dependencies) kept so the pinned smoke
// and chart contracts keep working until T-027 selects the complete assembly by
// default; core is the T-070 slice. Neither is chosen implicitly from the
// presence of other settings.
const (
	AssemblyScaffold = "scaffold"
	AssemblyCore     = "core"
)

// CoreConfig is the complete, explicit configuration of the core assembly.
// Nothing in it has an implicit hosted or development fallback: every
// dependency is named, and a missing one fails startup.
type CoreConfig struct {
	Process Config

	// Env selects every outbound adapter (SD-12). It is required and has no
	// default: the zero value, like an unknown one, refuses to start.
	Env Env

	Database DatabaseConfig
	NATS     NATSConfig
	S3       S3Config
	OIDC     OIDCConfig
	Git      GitConfig
	Secrets  SecretsConfig
	// Runtime selects and bounds container execution: the workload runtime of
	// the environment, the runtime API the workloads call back, and the key
	// that signs their session tokens.
	Runtime RuntimeConfig

	// InstallationID binds envelope encryption contexts; it must be stable for
	// the life of the installation.
	InstallationID string
	// CursorKey signs list/stream continuation tokens and IdempotencyHashKey
	// keys the fingerprint of secret-bearing requests. Both are at least 32
	// bytes and kept outside the database.
	CursorKey          []byte
	IdempotencyHashKey []byte

	// DeliveryTools are the trusted qualifications of the delivery scripts' replay
	// protocol (the tools image's apply and cleanup entrypoints). Empty by
	// default: no uncertain launch is then resolved from a deployment record.
	DeliveryTools []ToolQualification

	// ModelTools is the installation's model gateway, MCP proxy and wrapper
	// catalogue statement (T-039). Empty by default: nothing is approved, no
	// profile registers and no agent step is admitted.
	ModelTools ModelToolsConfig

	// BusinessLoop is the installation's ChatOps destination policy and QA
	// mapping statement (T-075). Empty by default: no notification is planned or
	// sent and no defect is mapped. The dependency graph and the flow summaries
	// need no statement and are always selected.
	BusinessLoop BusinessLoopConfig

	// ActionCeiling, Registry and GitTriggers are the operator statements of the
	// M2 production joins (T-101). Each is empty by default and then leaves its
	// capability disabled: attempts hold the default runtime actions only
	// (action-ceiling.json), no OCI registration is verified (registry.json) and
	// no push hook is received (git-triggers.json).
	ActionCeiling ActionCeilingConfig
	Registry      RegistryConfig
	GitTriggers   GitTriggerConfig

	Tuning Tuning
}

// DatabaseConfig selects the PostgreSQL connection of the serving process. The
// URL must identify the non-owner runtime role: startup refuses a role that
// owns the schema, is a superuser or bypasses row level security.
type DatabaseConfig struct {
	RuntimeURL string
	MaxConns   int32
}

// NATSConfig selects the JetStream stream the event workers publish to and
// consume from. Token may be empty when the URL carries it.
type NATSConfig struct {
	URL       string
	Token     string
	Stream    string
	Namespace string
}

// S3Config selects the artifact object store. AllowInsecureHTTP is for the
// local fixture profile only.
type S3Config struct {
	Endpoint          string
	Region            string
	PathStyle         bool
	Bucket            string
	Prefix            string
	AccessKey         string
	SecretKey         string
	AllowInsecureHTTP bool
}

// OIDCConfig selects the standalone authority's token verifier. The audience
// is the API audience access tokens must carry.
type OIDCConfig struct {
	Issuer            string
	Audience          string
	AllowInsecureHTTP bool
	// HTTPClient performs discovery and JWKS requests (nil: the verifier's
	// default, which follows no redirects).
	HTTPClient *http.Client
}

// GitConfig bounds the Git adapters and lists the installation-level
// credentials workspace repository references may use. A credential is bound
// to the repository URL prefix and tenants it may be sent to: a project cannot
// aim a credential at another host or use a credential of another tenant.
type GitConfig struct {
	CAFile          string
	ScratchRoot     string
	CommandTimeout  time.Duration
	MaxBundleBytes  int64
	MaxScratchBytes int64
	Credentials     []GitCredential
}

// GitCredential is one reference that project settings may name.
type GitCredential struct {
	// Ref is the opaque reference stored on the workspace
	// (secret://installation/<name>).
	Ref      string
	Username string
	Password string
	// URLPrefix is the https prefix repositories using this reference must have.
	URLPrefix string
	// Tenants lists the tenants allowed to use it; "*" allows every tenant of
	// this installation and must be written deliberately.
	Tenants []string
}

// RuntimeConfig configures container execution. The workload backend is not a
// setting: it is the environment's (development: Process, test: Podman,
// production: Kubernetes), and a setting for another backend is ignored by the
// constructor of the selected one, never used as a fallback.
type RuntimeConfig struct {
	// ServerURL is the origin of the runtime API as the workloads reach it. It
	// must be https unless it is a loopback address (development and test).
	ServerURL string
	// CAFile holds extra PEM trust roots workloads use for ServerURL.
	CAFile string
	// StateDir holds the durable workload records of the Process and Podman
	// runtimes.
	StateDir string
	// EntrypointBinary is the host kodama binary the Process runtime runs and
	// the static Linux kodama binary the Podman runtime mounts into the step's
	// image. The Kubernetes runtime copies it from the injector image instead.
	EntrypointBinary string

	Podman     PodmanConfig
	Kubernetes KubernetesConfig

	// SigningKeyFile is the PEM P-256 key that signs runtime session tokens and
	// SigningGeneration its generation (at least 1). A generation is never
	// reused for another key.
	SigningKeyFile    string
	SigningGeneration int64
}

// PodmanConfig bounds the containers of the Podman runtime.
type PodmanConfig struct {
	Binary  string
	Network string
	CPUs    string
	Memory  string
	PIDs    int
}

// KubernetesConfig reaches the cluster the Kubernetes runtime creates Jobs in.
// PolicyFile is a JSON document of the execution policy (kube.PolicyConfig:
// tenant targets, digest-pinned injector image, runtime callback and egress,
// resource bounds). Nothing in it has a default.
type KubernetesConfig struct {
	APIURL     string
	TokenFile  string
	CAFile     string
	PolicyFile string
	// Policy is the execution policy supplied by the rendered settings document
	// (ConfigFileVariable) instead of PolicyFile; at most one of them is set.
	Policy *kube.PolicyConfig
	// RunShared is the installation's run_shared opt-in from the settings
	// document (nil: not configured, new-format admission stays closed). It is
	// composed on Kubernetes only; the other backends refuse it (see validateRuntime).
	RunShared *RunSharedConfig
}

// Tuning holds the operational bounds. The defaults are explicit development
// and test values (decisions O-11 leaves production retention open), not
// production selections.
type Tuning struct {
	Limit execution.Limit

	Events EventsTuning

	// ReconcileInterval is the period of the repair passes (event repair and
	// pruning, cancellation scan, workspace and artifact reconciliation).
	ReconcileInterval time.Duration
	// ReconcileBatch bounds each repair scan.
	ReconcileBatch int
	// DispatchInterval is the period of the dispatch loop that starts runnable
	// attempts. Dispatch reads durable state only, so the loop is a wakeup and
	// never the source of truth.
	DispatchInterval time.Duration

	// AdmissionTTL, TokenTTL and MaxSession are the runtime session lifetimes
	// (runtimeauth.Policy). MaxSession must outlive the longest step timeout.
	AdmissionTTL time.Duration
	TokenTTL     time.Duration
	MaxSession   time.Duration

	// Logs bounds the log API.
	Logs LogsTuning

	RateLimitPerMinute int

	ObservationPageSize       int
	ObservationPollInterval   time.Duration
	ObservationHeartbeat      time.Duration
	ObservationAccessInterval time.Duration
	ObservationWriteTimeout   time.Duration
	ObservationBufferBytes    int
	ObservationMaxResources   int

	ReadinessTimeout time.Duration
	ReadinessTTL     time.Duration

	// HTTPWriteTimeout bounds writing one response. Event streams replace it
	// with their own per-write deadline, so a healthy stream outlives it.
	HTTPWriteTimeout time.Duration

	ArtifactStagingGrace time.Duration
	ArtifactAbandonAfter time.Duration

	// GateTTL is how long a human gate waits for its decision (snapshotted into
	// the gate's deadline). No merged contract defines a gate duration, so the
	// composition injects it; a gate that expires needs a new request.
	GateTTL time.Duration
	// CleanupSweep is the period of the deployment-bound cleanup scheduler's scan
	// of deployment records (never shorter than the scan's own bounds allow).
	CleanupSweep time.Duration

	// ChatOps bounds the notification worker (T-075). It applies only when the
	// installation selected ChatOps (business-loop.json).
	ChatOps ChatOpsTuning
}

// LogsTuning bounds the log read API and the controller's fallback capture.
type LogsTuning struct {
	MaxLines       int
	MaxBytes       int64
	MaxDuration    time.Duration
	MaxStreams     int
	WriteTimeout   time.Duration
	CaptureBytes   int64
	CaptureTimeout time.Duration
	Lease          time.Duration
}

// EventsTuning are the event bus policies. Every value is explicit.
type EventsTuning struct {
	Backlog        events.BacklogLimits
	Retry          events.RetryPolicy
	Retention      events.RetentionPolicy
	ReplayLease    time.Duration
	ReplayBatch    int
	PublishLease   time.Duration
	PublishTimeout time.Duration
	PublishBatch   int
	PublishEvery   time.Duration
	StreamMaxAge   time.Duration
	StreamMaxBytes int64
	StreamMaxMsgs  int64
	StreamDedup    time.Duration
	AckWait        time.Duration
	MaxAckPending  int
	FetchBatch     int
	FetchWait      time.Duration
	RetryDelay     time.Duration
	RepairGrace    time.Duration
}

// DefaultTuning returns the documented development values.
func DefaultTuning() Tuning {
	return Tuning{
		Limit: execution.Limit{
			TenantConcurrency: 10, ProjectConcurrency: 5, StartupBudget: time.Minute,
			MaxOutputBytes: 1 << 20, MaxOutputFiles: 100, MaxDiagnosticBytes: 1 << 20,
			DefaultTimeout: 10 * time.Minute, MaxTimeout: time.Hour,
		},
		Events: EventsTuning{
			Backlog: events.BacklogLimits{MaxEvents: 10000, MaxBytes: 64 << 20},
			Retry:   events.RetryPolicy{Initial: 500 * time.Millisecond, Max: 30 * time.Second, MaxAttempts: 8},
			Retention: events.RetentionPolicy{
				PublishedOutbox: 24 * time.Hour, Observation: 7 * 24 * time.Hour, Inbox: 7 * 24 * time.Hour,
				ResolvedQuarantine: 24 * time.Hour, CompletedReplay: 24 * time.Hour,
				BrokerRedelivery: 24 * time.Hour, ReplayWindow: 24 * time.Hour, MaxClockSkew: time.Minute, BatchSize: 500,
			},
			ReplayLease: time.Minute, ReplayBatch: 10,
			PublishLease: 30 * time.Second, PublishTimeout: 10 * time.Second, PublishBatch: 50, PublishEvery: time.Second,
			StreamMaxAge: 24 * time.Hour, StreamMaxBytes: 1 << 30, StreamMaxMsgs: 1_000_000, StreamDedup: 2 * time.Minute,
			AckWait: 30 * time.Second, MaxAckPending: 64, FetchBatch: 16, FetchWait: time.Second, RetryDelay: time.Second,
			RepairGrace: 5 * time.Second,
		},
		ReconcileInterval: 5 * time.Second, ReconcileBatch: 50, DispatchInterval: time.Second,
		AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour,
		Logs: LogsTuning{
			MaxLines: 5000, MaxBytes: 1 << 20, MaxDuration: 5 * time.Minute, MaxStreams: 32, WriteTimeout: 10 * time.Second,
			CaptureBytes: 8 << 20, CaptureTimeout: time.Minute, Lease: time.Minute,
		},
		RateLimitPerMinute: 1200,

		ObservationPageSize: 100, ObservationPollInterval: 250 * time.Millisecond, ObservationHeartbeat: 15 * time.Second,
		ObservationAccessInterval: 5 * time.Second, ObservationWriteTimeout: 10 * time.Second,
		ObservationBufferBytes: 1 << 20, ObservationMaxResources: 5000,

		ReadinessTimeout: 2 * time.Second, ReadinessTTL: time.Second, HTTPWriteTimeout: 30 * time.Second,

		ArtifactStagingGrace: time.Hour, ArtifactAbandonAfter: 24 * time.Hour,

		GateTTL: 7 * 24 * time.Hour, CleanupSweep: time.Minute,

		ChatOps: DefaultChatOpsTuning(),
	}
}

// Validate checks every required setting. It never reads the network or the
// filesystem and never defaults a missing dependency.
func (c CoreConfig) Validate() error {
	var errs []error
	req := func(ok bool, msg string) {
		if !ok {
			errs = append(errs, errors.New(msg))
		}
	}
	switch {
	case c.Env == "":
		errs = append(errs, ErrEnvRequired)
	case !c.Env.Valid():
		errs = append(errs, fmt.Errorf("%w: %q", ErrEnvUnknown, string(c.Env)))
	default:
		errs = append(errs, c.guardEnv()...)
		errs = append(errs, c.validateRuntime()...)
	}
	req(c.Database.RuntimeURL != "", "the database runtime URL is required (KODAMA_DATABASE_URL or the database-runtime-url secret)")
	req(c.Database.MaxConns >= 0, "the database pool size must not be negative")
	req(c.NATS.URL != "", "the NATS URL is required (KODAMA_NATS_URL)")
	req(c.NATS.Stream != "" && c.NATS.Namespace != "", "the JetStream stream and subject namespace are required")
	req(c.S3.Endpoint != "" && c.S3.Bucket != "", "the S3 endpoint and bucket are required (KODAMA_S3_ENDPOINT, KODAMA_S3_BUCKET)")
	req(c.S3.AccessKey != "" && c.S3.SecretKey != "", "the S3 credentials are required (s3-access-key, s3-secret-key)")
	req(c.S3.Region != "", "the S3 region is required (KODAMA_S3_REGION)")
	req(c.OIDC.Issuer != "" && c.OIDC.Audience != "", "the OIDC issuer and API audience are required (KODAMA_OIDC_ISSUER, KODAMA_OIDC_AUDIENCE)")
	req(c.InstallationID != "", "the installation identifier is required (KODAMA_INSTALLATION_ID)")
	req(len(c.CursorKey) >= 32, "the cursor signing key must be at least 32 bytes")
	req(len(c.IdempotencyHashKey) >= 32, "the idempotency hash key must be at least 32 bytes")
	req(c.Git.ScratchRoot != "", "the Git scratch root is required")
	errs = append(errs, c.Secrets.Validate(c.Env))
	if err := c.Tuning.Limit.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("execution limits: %w", err))
	}
	if c.Tuning.RateLimitPerMinute < 1 {
		errs = append(errs, errors.New("the rate limit must be positive (there is no unlimited default)"))
	}
	if c.Tuning.ReconcileInterval <= 0 || c.Tuning.ReconcileBatch < 1 || c.Tuning.ReadinessTimeout <= 0 || c.Tuning.HTTPWriteTimeout <= 0 {
		errs = append(errs, errors.New("reconcile, readiness and HTTP write bounds must be positive"))
	}
	for _, cred := range c.Git.Credentials {
		if err := cred.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.ModelTools.Validate(c.Env); err != nil {
		errs = append(errs, err)
	}
	if (len(c.ModelTools.Gateway.Origins) == 0) != (len(c.ModelTools.MCP.AllowedHosts) == 0) {
		errs = append(errs, errors.New("the model gateway and the MCP proxy are selected together: name both an approved provider origin and an approved MCP host"))
	}
	if err := c.BusinessLoop.Validate(c.Env, c.Tuning); err != nil {
		errs = append(errs, err)
	}
	if c.Env.Valid() {
		if err := c.ActionCeiling.Validate(c.Env); err != nil {
			errs = append(errs, err)
		}
		if err := c.Registry.Validate(c.Env); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.GitTriggers.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Process.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("the shutdown timeout is required"))
	}
	return errors.Join(errs...)
}

// runSharedConfigured reports whether the installation opted into the run_shared
// model (the settings document's `runShared` section). Configuration validation
// refuses it outside production, where only Kubernetes enforces its policy.
func (c CoreConfig) runSharedConfigured() bool { return c.Runtime.Kubernetes.RunShared != nil }

// guardEnv applies the construction guards of SD-12 to the settings that name
// a development or test adapter: under production the development key file,
// an issuer reached over plain HTTP (the OIDC fixture) and an object store
// reached over plain HTTP (the S3 fixture) are refused, and so is a runtime
// that is not the Kubernetes one. The constructors check the built adapters
// again; this stops startup before anything is built.
func (c CoreConfig) guardEnv() []error {
	if c.Env != Production {
		return nil
	}
	var errs []error
	refuse := func(port Port, adapter, why string) {
		errs = append(errs, &AdapterError{Env: Production, Port: port, Adapter: adapter, Reason: why})
	}
	if c.OIDC.AllowInsecureHTTP {
		refuse(PortIdentity, "oidc-fixture", "an issuer reached over plain HTTP is the test fixture")
	}
	if c.S3.Endpoint != "" {
		if err := refuseObjectFixture(c.S3); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Runtime.StateDir != "" || c.Runtime.EntrypointBinary != "" || c.Runtime.Podman != (PodmanConfig{}) {
		refuse(PortRuntime, "process/podman", "production runs workloads only on Kubernetes")
	}
	return errs
}

// validateRuntime checks the container execution settings of the environment.
func (c CoreConfig) validateRuntime() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	r := c.Runtime
	if u, err := url.Parse(r.ServerURL); err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		add("the runtime API origin is required (KODAMA_RUNTIME_URL)")
	} else if u.Scheme != "https" && !(u.Scheme == "http" && c.Env != Production && isLoopbackHost(u.Hostname())) {
		add("the runtime API origin must be https (plain http only for a loopback address outside production)")
	}
	if r.SigningKeyFile == "" || r.SigningGeneration < 1 {
		add("the runtime signing key and its generation are required (KODAMA_RUNTIME_KEY_FILE, KODAMA_RUNTIME_KEY_GENERATION)")
	}
	switch c.Env {
	case Test:
		if r.StateDir == "" || r.EntrypointBinary == "" {
			add("the Podman runtime needs a state directory and the entrypoint binary (KODAMA_RUNTIME_STATE_DIR, KODAMA_ENTRYPOINT_BINARY)")
		}
	case Production:
		k := r.Kubernetes
		if k.APIURL == "" || k.TokenFile == "" || k.CAFile == "" || (k.PolicyFile == "") == (k.Policy == nil) {
			add("the Kubernetes runtime needs the API URL, token, CA and execution policy files (KODAMA_KUBE_API, KODAMA_KUBE_TOKEN_FILE, KODAMA_KUBE_CA_FILE and exactly one of KODAMA_KUBE_POLICY_FILE or the settings document's execution policy)")
		}
	}
	if rs := r.Kubernetes.RunShared; rs != nil {
		// run_shared is composed on Kubernetes only: the Podman and Process
		// compositions follow with their resource adapters (T-115) and refuse the
		// configuration rather than start a backend that cannot enforce its policy.
		if c.Env != Production {
			add("run_shared is configured but the %s environment's runtime cannot enforce its execution policy: it is composed on Kubernetes only", c.Env)
		} else if err := rs.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	t := c.Tuning
	if err := (runtimeauth.Policy{AdmissionTTL: t.AdmissionTTL, TokenTTL: t.TokenTTL, MaxSession: t.MaxSession}).Validate(); err != nil {
		errs = append(errs, err)
	} else if t.MaxSession < t.Limit.MaxTimeout {
		add("the runtime session lifetime %s must outlive the longest step timeout %s", t.MaxSession, t.Limit.MaxTimeout)
	}
	if t.DispatchInterval <= 0 {
		add("the dispatch interval must be positive")
	}
	if t.GateTTL <= 0 || t.CleanupSweep <= 0 {
		add("the gate deadline and the cleanup sweep interval must be positive")
	}
	l := t.Logs
	if l.MaxLines < 1 || l.MaxBytes < 1 || l.MaxDuration <= 0 || l.MaxStreams < 1 || l.WriteTimeout <= 0 || l.CaptureBytes < 1 || l.CaptureTimeout <= 0 || l.Lease <= 0 {
		add("the log bounds must all be positive")
	}
	return errs
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (g GitCredential) validate() error {
	u, err := url.Parse(g.URLPrefix)
	switch {
	case g.Ref == "" || !strings.HasPrefix(g.Ref, "secret://installation/"):
		return fmt.Errorf("git credential reference %q must have the form secret://installation/<name>", g.Ref)
	case g.Username == "" || g.Password == "":
		return fmt.Errorf("git credential %s needs a username and password", g.Ref)
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("git credential %s must be bound to an https URL prefix", g.Ref)
	case len(g.Tenants) == 0:
		return fmt.Errorf("git credential %s must list its tenants (\"*\" for every tenant)", g.Ref)
	}
	return nil
}

// gitCredentialFile is the on-disk form of GitConfig.Credentials.
type gitCredentialFile struct {
	Ref          string   `json:"ref"`
	Username     string   `json:"username"`
	PasswordFile string   `json:"passwordFile"`
	URLPrefix    string   `json:"urlPrefix"`
	Tenants      []string `json:"tenants"`
}

// LoadCoreConfig reads the core assembly settings from the environment and the
// secrets directory (dev/configuration.md). Environment names:
//
//	KODAMA_SECRETS_DIR           directory of secret files (default /secrets)
//	KODAMA_DATABASE_URL          runtime role URL (else database-runtime-url)
//	KODAMA_NATS_URL              nats://[token@]host:port (token else nats-token)
//	KODAMA_EVENT_STREAM          JetStream stream (default kodama-events)
//	KODAMA_EVENT_NAMESPACE       subject namespace (default kodama)
//	KODAMA_S3_ENDPOINT, _BUCKET, _PREFIX; s3-access-key and s3-secret-key files
//	KODAMA_S3_REGION (default us-east-1), KODAMA_S3_PATH_STYLE (default true)
//	KODAMA_S3_ALLOW_INSECURE_HTTP=true  local fixture only
//	KODAMA_OIDC_ISSUER, KODAMA_OIDC_AUDIENCE, KODAMA_OIDC_ALLOW_INSECURE_HTTP
//	KODAMA_INSTALLATION_ID       stable installation identifier
//	KODAMA_ENV                   development, test or production (required, no default)
//	KODAMA_RUNTIME_URL           origin of the runtime API as workloads reach it
//	KODAMA_RUNTIME_CA_FILE, KODAMA_RUNTIME_STATE_DIR, KODAMA_ENTRYPOINT_BINARY
//	KODAMA_PODMAN_BINARY, _NETWORK, _CPUS, _MEMORY, _PIDS (test)
//	KODAMA_KUBE_API (default: the in-cluster API), KODAMA_KUBE_TOKEN_FILE, KODAMA_KUBE_CA_FILE, KODAMA_KUBE_POLICY_FILE (production)
//	KODAMA_CONFIG_FILE           rendered settings document (the chart's config.yaml): defaults for the
//	                             settings it covers and the execution policy; variables that are set win
//	KODAMA_RUNTIME_KEY_FILE      PEM P-256 runtime signing key (else runtime-signing-key)
//	KODAMA_RUNTIME_KEY_GENERATION  its generation (at least 1)
//	KODAMA_SECRETS_ADAPTER=keyfile (master-key file); KODAMA_SECRETS_CONFIG is the full JSON settings object
//	KODAMA_GIT_CA_FILE, KODAMA_GIT_SCRATCH_DIR, KODAMA_GIT_CREDENTIALS_FILE
//	KODAMA_ACTION_CEILING_FILE   per-kind runtime action ceiling (default action-ceiling.json; absent: defaults only)
//	KODAMA_REGISTRY_FILE         OCI registries and who may register from them (default registry.json; absent: refused)
//	KODAMA_GIT_TRIGGERS_FILE     native Git push trigger selection (default git-triggers.json; absent: disabled)
//	cursor-key and idempotency-hash-key files, raw bytes, at least 32 (or
//	KODAMA_CURSOR_KEY_FILE, KODAMA_IDEMPOTENCY_KEY_FILE)
//
// Process settings keep their existing names (KODAMA_HTTP_ADDR and the others).
// Unset names have no implicit value except where stated above.
func LoadCoreConfig(getenv func(string) string, process Config) (CoreConfig, error) {
	dir := "/secrets"
	if v := getenv("KODAMA_SECRETS_DIR"); v != "" {
		dir = v
	}
	var errs []error
	// The rendered settings document (the chart's config.yaml) supplies defaults
	// under the environment; anything it cannot serve is a startup error.
	getenv, document, cfgErr := withConfigDocument(getenv, dir)
	policy := document.Policy
	if cfgErr != nil {
		errs = append(errs, cfgErr)
	}
	setting := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, fmt.Errorf("secret file %s: %w", name, err))
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	// Signing and fingerprint keys are raw bytes: a key may begin or end with a
	// byte that looks like whitespace, so it is never trimmed.
	readKey := func(env, def string) []byte {
		p := setting(env, filepath.Join(dir, def))
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("secret file %s: %w", p, err))
			return nil
		}
		return b
	}
	boolean := func(name string) bool { return strings.EqualFold(getenv(name), "true") }

	c := CoreConfig{Process: process, Tuning: DefaultTuning()}
	env, err := ParseEnv(getenv)
	if err != nil {
		return c, err // nothing else is read: the environment decides what would be
	}
	c.Env = env
	c.Database.RuntimeURL = getenv("KODAMA_DATABASE_URL")
	if c.Database.RuntimeURL == "" {
		c.Database.RuntimeURL = read("database-runtime-url")
	}
	c.NATS = NATSConfig{URL: getenv("KODAMA_NATS_URL"), Stream: setting("KODAMA_EVENT_STREAM", "kodama-events"), Namespace: setting("KODAMA_EVENT_NAMESPACE", "kodama")}
	if u, err := url.Parse(c.NATS.URL); err == nil && u.User == nil && c.NATS.URL != "" {
		c.NATS.Token = read("nats-token")
	}
	c.S3 = S3Config{
		Endpoint: getenv("KODAMA_S3_ENDPOINT"), Region: setting("KODAMA_S3_REGION", "us-east-1"), PathStyle: !strings.EqualFold(getenv("KODAMA_S3_PATH_STYLE"), "false"), Bucket: getenv("KODAMA_S3_BUCKET"), Prefix: getenv("KODAMA_S3_PREFIX"),
		AccessKey: read("s3-access-key"), SecretKey: read("s3-secret-key"), AllowInsecureHTTP: boolean("KODAMA_S3_ALLOW_INSECURE_HTTP"),
	}
	c.OIDC = OIDCConfig{Issuer: getenv("KODAMA_OIDC_ISSUER"), Audience: getenv("KODAMA_OIDC_AUDIENCE"), AllowInsecureHTTP: boolean("KODAMA_OIDC_ALLOW_INSECURE_HTTP")}
	c.InstallationID = getenv("KODAMA_INSTALLATION_ID")
	c.Secrets, err = loadSecretsConfig(getenv, dir)
	if err != nil {
		errs = append(errs, err)
	}
	c.Runtime = RuntimeConfig{
		ServerURL: getenv("KODAMA_RUNTIME_URL"), CAFile: getenv("KODAMA_RUNTIME_CA_FILE"),
		StateDir: getenv("KODAMA_RUNTIME_STATE_DIR"), EntrypointBinary: getenv("KODAMA_ENTRYPOINT_BINARY"),
		Podman: PodmanConfig{
			Binary: getenv("KODAMA_PODMAN_BINARY"), Network: getenv("KODAMA_PODMAN_NETWORK"),
			CPUs: getenv("KODAMA_PODMAN_CPUS"), Memory: getenv("KODAMA_PODMAN_MEMORY"),
		},
		Kubernetes:     KubernetesConfig{APIURL: getenv("KODAMA_KUBE_API"), PolicyFile: getenv("KODAMA_KUBE_POLICY_FILE")},
		SigningKeyFile: setting("KODAMA_RUNTIME_KEY_FILE", filepath.Join(dir, "runtime-signing-key")),
	}
	if v := getenv("KODAMA_PODMAN_PIDS"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			errs = append(errs, errors.New("KODAMA_PODMAN_PIDS must be a positive integer"))
		}
		c.Runtime.Podman.PIDs = n
	}
	if v := getenv("KODAMA_RUNTIME_KEY_GENERATION"); v != "" {
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || n < 1 {
			errs = append(errs, errors.New("KODAMA_RUNTIME_KEY_GENERATION must be a positive integer"))
		}
		c.Runtime.SigningGeneration = n
	}
	c.Runtime.Kubernetes.Policy = policy
	c.Runtime.Kubernetes.RunShared = document.RunShared
	if env == Production {
		if c.Runtime.Kubernetes.APIURL == "" {
			c.Runtime.Kubernetes.APIURL = inClusterKubeAPI(getenv)
		}
		// The in-cluster service account paths are the Kubernetes convention.
		c.Runtime.Kubernetes.TokenFile = setting("KODAMA_KUBE_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")
		c.Runtime.Kubernetes.CAFile = setting("KODAMA_KUBE_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	}
	c.CursorKey = readKey("KODAMA_CURSOR_KEY_FILE", "cursor-key")
	c.IdempotencyHashKey = readKey("KODAMA_IDEMPOTENCY_KEY_FILE", "idempotency-hash-key")
	if p := setting("KODAMA_DELIVERY_TOOLS_FILE", filepath.Join(dir, "delivery-tools.json")); fileExists(p) {
		tools, err := loadDeliveryTools(p)
		if err != nil {
			errs = append(errs, err)
		}
		c.DeliveryTools = tools
	}
	if p := setting("KODAMA_MODEL_TOOLS_FILE", filepath.Join(dir, "model-tools.json")); fileExists(p) {
		mt, err := LoadModelTools(p)
		if err != nil {
			errs = append(errs, err)
		}
		c.ModelTools = mt
	}
	if p := setting("KODAMA_BUSINESS_LOOP_FILE", filepath.Join(dir, "business-loop.json")); fileExists(p) {
		bl, err := LoadBusinessLoop(p)
		if err != nil {
			errs = append(errs, err)
		}
		c.BusinessLoop = bl
	}
	if p := setting("KODAMA_ACTION_CEILING_FILE", filepath.Join(dir, "action-ceiling.json")); fileExists(p) {
		ac, err := LoadActionCeiling(p)
		if err != nil {
			errs = append(errs, err)
		}
		c.ActionCeiling = ac
	}
	if p := setting("KODAMA_REGISTRY_FILE", filepath.Join(dir, "registry.json")); fileExists(p) {
		reg, err := LoadRegistry(p, dir)
		if err != nil {
			errs = append(errs, err)
		}
		c.Registry = reg
	}
	if p := setting("KODAMA_GIT_TRIGGERS_FILE", filepath.Join(dir, "git-triggers.json")); fileExists(p) {
		gt, err := LoadGitTriggers(p)
		if err != nil {
			errs = append(errs, err)
		}
		c.GitTriggers = gt
	}
	c.Git = GitConfig{
		CAFile: getenv("KODAMA_GIT_CA_FILE"), ScratchRoot: setting("KODAMA_GIT_SCRATCH_DIR", filepath.Join(os.TempDir(), "kodama-git")),
		CommandTimeout: time.Minute, MaxBundleBytes: 256 << 20, MaxScratchBytes: 1 << 30,
	}
	if p := setting("KODAMA_GIT_CREDENTIALS_FILE", filepath.Join(dir, "git-credentials.json")); fileExists(p) {
		creds, err := loadGitCredentials(p, dir)
		if err != nil {
			errs = append(errs, err)
		}
		c.Git.Credentials = creds
	}
	if len(errs) > 0 {
		return c, errors.Join(errs...)
	}
	return c, c.Validate()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// loadDeliveryTools reads the strict JSON list of tool qualifications.
func loadDeliveryTools(path string) ([]ToolQualification, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tools []ToolQualification
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tools); err != nil {
		return nil, fmt.Errorf("delivery tools file: %w", err)
	}
	if _, err := NewToolQualifications(tools); err != nil {
		return nil, err
	}
	return tools, nil
}

func loadGitCredentials(path, dir string) ([]GitCredential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []gitCredentialFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("git credentials file: %w", err)
	}
	out := make([]GitCredential, 0, len(entries))
	for _, e := range entries {
		p := e.PasswordFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		pw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("git credential %s: %w", e.Ref, err)
		}
		out = append(out, GitCredential{Ref: e.Ref, Username: e.Username, Password: strings.TrimSpace(string(pw)), URLPrefix: e.URLPrefix, Tenants: e.Tenants})
	}
	return out, nil
}
