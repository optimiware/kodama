package entrypoint

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Default runtime layout (docs/implementation/step-entrypoint.md).
const (
	DefaultWorkspaceDir = "/workspace"
	DefaultOutputsDir   = "/kodama/outputs"
	DefaultSecretsDir   = "/kodama/secrets"
	DefaultIdentityDir  = "/kodama/identity"
	DefaultResultsFile  = "/kodama/results.json"

	// OutputsPrefix is the path prefix every declared file output carries in
	// the Context; it is mapped onto Config.OutputsDir.
	OutputsPrefix = "/kodama/outputs"
)

// Runtime wires. `kodama step` speaks exactly one: v1 (attempt_private, the
// default) or v2 (run_shared), selected by the controller through
// KODAMA_RUNTIME_API. The entrypoint never negotiates: a context of the other
// version is refused before any command.
const (
	RuntimeAPIv1 = "v1"
	RuntimeAPIv2 = "v2"
)

// Version is the entrypoint version sent as AdmitRequest.ClientVersion. The
// release build may override it with -ldflags.
var Version = "0.1.0"

// Config is the complete bootstrap input of one step. Zero fields take the
// defaults of applyDefaults; flags and environment map onto it in ParseConfig.
type Config struct {
	ServerURL string // origin of the runtime API; https, or http on loopback only
	AttemptID string

	AdmissionCredentialFile string // default <IdentityDir>/admission-credential
	CheckoutCredentialFile  string // default <IdentityDir>/checkout-credential; "username:password"; absent = anonymous
	CAFile                  string // extra PEM roots for the runtime API and uploads
	GitCAFile               string // GIT_SSL_CAINFO for clone; empty = system roots

	// RuntimeAPI selects the runtime wire: "" and "v1" keep the frozen v1 routes and
	// the per-attempt checkout; "v2" uses /runtime/v2/attempts, verifies the
	// controller-prepared /workspace and calls begin before the command. A
	// run_shared attempt never has a checkout credential.
	RuntimeAPI string

	WorkspaceDir string
	OutputsDir   string
	SecretsDir   string
	IdentityDir  string
	ResultsFile  string
	TokenFile    string // current runtime token, written 0600 after admit/renewal; default <IdentityDir>/token
	ScratchDir   string // diagnostics log and bundle; default os.TempDir()

	Job           runtimev1.JobIdentity
	ClientVersion string
	GitBinary     string
	LogLevel      slog.Level

	// ChildEnv is the base environment of the command (PATH, HOME, ...);
	// Context env and secrets are layered on top. Run fills it from a small
	// allowlist of the process environment.
	ChildEnv map[string]string

	Retry            RetryPolicy
	KillGrace        time.Duration // SIGTERM to SIGKILL
	FinalizeTimeout  time.Duration // bound for uploading diagnostics and submitting the result
	SettleTimeout    time.Duration // bound for a pending publication to settle
	SettleBackoff    time.Duration // first re-proposal delay (doubles, capped at 5s)
	RenewMinInterval time.Duration // lower bound of the token renewal period
	RenewReserve     time.Duration // stop the child this long before maxExpiresAt
	InterruptTimeout time.Duration // finalization budget after SIGTERM of the entrypoint

	Logger *slog.Logger // optional; default JSON to stderr

	// ContainerLog receives the redacted, bounded mirror of the command's
	// output (see mirror.go). Run sets it to the container's stderr; nil
	// disables the mirror.
	ContainerLog io.Writer
}

// SharedRun reports whether the run_shared (v2) wire is selected.
func (c *Config) SharedRun() bool { return c.RuntimeAPI == RuntimeAPIv2 }

func (c *Config) applyDefaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.WorkspaceDir, DefaultWorkspaceDir)
	def(&c.OutputsDir, DefaultOutputsDir)
	def(&c.SecretsDir, DefaultSecretsDir)
	def(&c.IdentityDir, DefaultIdentityDir)
	def(&c.ResultsFile, DefaultResultsFile)
	def(&c.AdmissionCredentialFile, filepath.Join(c.IdentityDir, "admission"))
	def(&c.CheckoutCredentialFile, filepath.Join(c.IdentityDir, "checkout-credential"))
	def(&c.TokenFile, filepath.Join(c.IdentityDir, "token"))
	def(&c.ScratchDir, os.TempDir())
	def(&c.ClientVersion, Version)
	def(&c.GitBinary, "git")
	def(&c.RuntimeAPI, RuntimeAPIv1)
	if c.Retry.MaxAttempts < 1 {
		c.Retry = DefaultRetry
	}
	dur := func(p *time.Duration, v time.Duration) {
		if *p <= 0 {
			*p = v
		}
	}
	dur(&c.KillGrace, 10*time.Second)
	dur(&c.FinalizeTimeout, 5*time.Minute)
	dur(&c.SettleTimeout, 2*time.Minute)
	dur(&c.SettleBackoff, 250*time.Millisecond)
	dur(&c.RenewMinInterval, time.Second)
	dur(&c.RenewReserve, 30*time.Second)
	dur(&c.InterruptTimeout, 10*time.Second)
}

// Validate checks the bootstrap inputs and the layout invariant that the
// secrets directory is disjoint from the workspace, outputs and identity roots.
func (c *Config) Validate() error {
	switch c.RuntimeAPI {
	case "", RuntimeAPIv1, RuntimeAPIv2:
	default:
		return fmt.Errorf("unsupported runtime API %q: use %s or %s", c.RuntimeAPI, RuntimeAPIv1, RuntimeAPIv2)
	}
	u, err := url.Parse(c.ServerURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("server URL must be an absolute URL without credentials, query or fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); !(u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
			return errors.New("plain http is allowed only for loopback servers")
		}
	default:
		return errors.New("server URL must use https")
	}
	if err := domain.AttemptID(c.AttemptID).Validate(); err != nil {
		return err
	}
	if err := c.Job.Validate(); err != nil {
		return fmt.Errorf("job identity: %w", err)
	}
	roots := map[string]string{"workspace": c.WorkspaceDir, "outputs": c.OutputsDir, "secrets": c.SecretsDir, "identity": c.IdentityDir}
	abs := map[string]string{}
	for name, p := range roots {
		a, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%s dir: %w", name, err)
		}
		abs[name] = filepath.Clean(a)
	}
	names := []string{"workspace", "outputs", "secrets", "identity"}
	for i, a := range names {
		for _, b := range names[i+1:] {
			if within(abs[a], abs[b]) || within(abs[b], abs[a]) {
				return fmt.Errorf("%s and %s directories must not overlap", a, b)
			}
		}
	}
	for name, p := range map[string]string{"results file": c.ResultsFile, "token file": c.TokenFile} {
		a, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		for _, root := range []string{"workspace", "outputs", "secrets"} {
			if within(abs[root], a) {
				return fmt.Errorf("%s must be outside the %s directory", name, root)
			}
		}
	}
	return nil
}

// within reports whether path is root or inside it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// childEnvAllowlist is the part of the process environment a command inherits.
var childEnvAllowlist = []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "USER"}

// ParseArgs is ParseConfig with help and usage output discarded.
func ParseArgs(args []string, getenv func(string) string) (Config, error) {
	return ParseConfig(args, getenv, io.Discard)
}

// ParseConfig maps flags (which override) and environment onto a Config. Help
// text and parse errors go to output.
//
// Every setting is read from KODAMA_STEP_<NAME> first and from the shorter
// KODAMA_<NAME> alias second (for example KODAMA_STEP_SERVER_URL, then
// KODAMA_SERVER_URL); an explicit flag overrides both.
func ParseConfig(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	rawGetenv := getenv
	getenv = func(name string) string {
		if rest, ok := strings.CutPrefix(name, "KODAMA_"); ok {
			if v := rawGetenv("KODAMA_STEP_" + rest); v != "" {
				return v
			}
		}
		return rawGetenv(name)
	}
	setting := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	fs := flag.NewFlagSet("kodama step", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: kodama step [flags]\n\nRuns one attempt's command through the runtime lifecycle. Flags override environment settings:")
		fs.PrintDefaults()
	}
	var c Config
	str := func(p *string, flagName, env, usage string) {
		fs.StringVar(p, flagName, setting(env, ""), usage+" (env KODAMA_STEP_"+strings.TrimPrefix(env, "KODAMA_")+" or "+env+")")
	}
	str(&c.ServerURL, "server-url", "KODAMA_SERVER_URL", "runtime API origin")
	str(&c.RuntimeAPI, "runtime-api", "KODAMA_RUNTIME_API", "runtime wire: v1 (default) or v2 (run_shared)")
	str(&c.AttemptID, "attempt-id", "KODAMA_ATTEMPT_ID", "attempt ID")
	str(&c.IdentityDir, "identity-dir", "KODAMA_IDENTITY_DIR", "identity directory, default "+DefaultIdentityDir)
	str(&c.AdmissionCredentialFile, "admission-credential-file", "KODAMA_ADMISSION_CREDENTIAL_FILE", "one-time admission credential file")
	str(&c.CheckoutCredentialFile, "checkout-credential-file", "KODAMA_CHECKOUT_CREDENTIAL_FILE", "read-only Git credential file (username:password)")
	str(&c.CAFile, "ca-file", "KODAMA_CA_FILE", "extra PEM CA roots for the runtime API and uploads")
	str(&c.GitCAFile, "git-ca-file", "KODAMA_GIT_CA_FILE", "PEM CA bundle for Git over HTTPS (replaces system roots for Git)")
	str(&c.WorkspaceDir, "workspace-dir", "KODAMA_WORKSPACE_DIR", "checkout directory, default "+DefaultWorkspaceDir)
	str(&c.OutputsDir, "outputs-dir", "KODAMA_OUTPUTS_DIR", "outputs root, default "+DefaultOutputsDir)
	str(&c.SecretsDir, "secrets-dir", "KODAMA_SECRETS_DIR", "memory-backed secrets directory, default "+DefaultSecretsDir)
	str(&c.ResultsFile, "results-file", "KODAMA_RESULTS_FILE", "result manifest path, default "+DefaultResultsFile)
	str(&c.TokenFile, "token-file", "KODAMA_TOKEN_FILE", "file receiving the current runtime token")
	str(&c.ScratchDir, "scratch-dir", "KODAMA_SCRATCH_DIR", "scratch directory for logs and bundles")
	str(&c.Job.Name, "job-name", "KODAMA_JOB_NAME", "Job name")
	str(&c.Job.UID, "job-uid", "KODAMA_JOB_UID", "Job UID")
	str(&c.Job.PodName, "pod-name", "KODAMA_POD_NAME", "Pod name")
	str(&c.Job.PodUID, "pod-uid", "KODAMA_POD_UID", "Pod UID")
	str(&c.GitBinary, "git-binary", "KODAMA_GIT_BINARY", "git executable, default git")
	level := fs.String("log-level", setting("KODAMA_LOG_LEVEL", "INFO"), "slog level (KODAMA_LOG_LEVEL)")
	duration := func(p *time.Duration, flagName, env, usage string) {
		def := time.Duration(0)
		if v := getenv(env); v != "" {
			d, err := time.ParseDuration(v)
			if err == nil {
				def = d
			}
		}
		fs.DurationVar(p, flagName, def, usage+" ("+env+")")
	}
	duration(&c.KillGrace, "kill-grace", "KODAMA_KILL_GRACE", "SIGTERM to SIGKILL delay, default 10s")
	duration(&c.FinalizeTimeout, "finalize-timeout", "KODAMA_FINALIZE_TIMEOUT", "bound for diagnostics upload and result submission, default 5m")
	duration(&c.SettleTimeout, "settle-timeout", "KODAMA_SETTLE_TIMEOUT", "bound for a pending publication to settle, default 2m")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, errors.New("kodama step accepts flags only, not positional arguments")
	}
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return Config{}, fmt.Errorf("invalid log level %q", *level)
	}
	c.ChildEnv = map[string]string{}
	for _, name := range childEnvAllowlist {
		if v := getenv(name); v != "" {
			c.ChildEnv[name] = v
		}
	}
	c.applyDefaults()
	// Job identity fallback: files in the identity directory.
	for _, f := range []struct {
		dst  *string
		file string
	}{{&c.Job.Name, "job-name"}, {&c.Job.UID, "job-uid"}, {&c.Job.PodName, "pod-name"}, {&c.Job.PodUID, "pod-uid"}} {
		if *f.dst == "" {
			if b, err := os.ReadFile(filepath.Join(c.IdentityDir, f.file)); err == nil {
				*f.dst = strings.TrimSpace(string(b))
			}
		}
	}
	if c.ServerURL == "" || c.AttemptID == "" {
		return Config{}, errors.New("--server-url and --attempt-id are required")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
