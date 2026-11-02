package entrypoint

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// The resource subjects of a run_shared run: the one-shot preparation workload
// (`kodama prepare`) and the supervisor of a service with pinned secrets
// (`kodama service-run`). They are not attempts: they speak
// /runtime/v2/runs/{runId}/resources/{resourceKey}, hold a resource token of the
// kodama-runtime-resource audience, and have no source, result, artifact, begin or
// model authority (docs/contracts/runtime-v2.md, "Resource subjects").

// Exit codes of the resource commands, beyond the shared ones of step.go.
const (
	// ExitCheckout: the preparation exhausted its checkout tries or its deadline;
	// no receipt was recorded and the run fails with preparation_failed.
	ExitCheckout = 14
	// ExitServiceFailed: the supervisor could not deliver the service's secrets or
	// start its process; the service never ran.
	ExitServiceFailed = 15
)

// DefaultSecretsRoot is where a service's pinned file secrets are delivered: a
// memory-backed volume private to the service, never a shared mount.
const DefaultSecretsRoot = "/kodama/secrets"

// ResourceConfig is the bootstrap input of a preparation or service supervisor,
// the environment contract of seam S4.
type ResourceConfig struct {
	ServerURL string
	CAFile    string
	RunID     string
	// Key is "preparation" or "service.NAME".
	Key                     string
	AdmissionCredentialFile string
	Identity                runtimev2.ResourceIdentity
	ClientVersion           string

	// RuntimeAPI selects the wire of the resource routes: "" and "v2" keep
	// /runtime/v2; "v3" is the preparation of a source set (`kodama prepare` only).
	RuntimeAPI string
	// GitDir and GitHelper: v3 preparation. The context names its credential and CA
	// files as /kodama/git/<file>; GitDir maps them (default /kodama/git), and
	// GitHelper overrides the argv that runs the credential helper.
	GitDir    string
	GitHelper []string

	// Preparation only.
	WorkspaceDir           string
	CheckoutCredentialFile string
	GitCAFile              string
	GitBinary              string
	// ScratchDir holds Git's private home and nothing else (default os.TempDir()).
	ScratchDir string

	// Service only: where file secrets are delivered.
	SecretsDir string

	Retry    RetryPolicy
	LogLevel slog.Level
	Logger   *slog.Logger
	// ChildEnv is the base environment of a supervised service (PATH, HOME, ...);
	// Run fills it from the process environment minus every KODAMA_ variable.
	ChildEnv []string
	// KillGrace bounds how long a stopping service gets between the forwarded
	// SIGTERM and SIGKILL.
	KillGrace time.Duration
	// AdmissionWait bounds how long admission is retried while execution has not yet
	// bound the workload (resource_unbound) or the server is unreachable.
	AdmissionWait time.Duration
}

func (c *ResourceConfig) applyDefaults() {
	if c.WorkspaceDir == "" {
		c.WorkspaceDir = DefaultWorkspaceDir
	}
	if c.GitDir == "" {
		c.GitDir = DefaultGitDir
	}
	if c.SecretsDir == "" {
		c.SecretsDir = DefaultSecretsRoot
	}
	if c.AdmissionCredentialFile == "" {
		c.AdmissionCredentialFile = filepath.Join(DefaultIdentityDir, "admission")
	}
	if c.ClientVersion == "" {
		c.ClientVersion = Version
	}
	if c.GitBinary == "" {
		c.GitBinary = "git"
	}
	if c.ScratchDir == "" {
		c.ScratchDir = os.TempDir()
	}
	if c.Retry.MaxAttempts < 1 {
		c.Retry = DefaultRetry
	}
	if c.KillGrace <= 0 {
		c.KillGrace = 10 * time.Second
	}
	if c.AdmissionWait <= 0 {
		c.AdmissionWait = 2 * time.Minute
	}
}

// Validate checks the identity of the resource and the server URL (https, or
// plain http on loopback only).
func (c *ResourceConfig) Validate() error {
	switch c.RuntimeAPI {
	case "", RuntimeAPIv2, RuntimeAPIv3:
	default:
		return fmt.Errorf("unsupported runtime API %q for a resource workload: use %s or %s", c.RuntimeAPI, RuntimeAPIv2, RuntimeAPIv3)
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
	if err := domain.RunID(c.RunID).Validate(); err != nil {
		return err
	}
	if _, ok := resourceKindOfKey(c.Key); !ok {
		return fmt.Errorf("resource key %q is not preparation or service.NAME", c.Key)
	}
	if err := c.Identity.Validate(); err != nil {
		return fmt.Errorf("workload identity: %w", err)
	}
	return nil
}

// checkSecretsRoot requires the service's secrets directory to be a clean absolute
// path that neither contains nor lies inside the run workspace or the directory of
// the admission or checkout credential, comparing real paths where they exist (a
// link cannot hide an overlap). The directory itself is opened without following
// links when the first file secret is written (openSecretsRoot).
func (c ResourceConfig) checkSecretsRoot() error {
	root := c.SecretsDir
	if root == "" {
		root = DefaultSecretsRoot
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%w: %q is not a clean absolute path", errSecretsRoot, root)
	}
	others := map[string]string{"the run workspace": c.WorkspaceDir}
	if c.AdmissionCredentialFile != "" {
		others["the admission credential's directory"] = filepath.Dir(c.AdmissionCredentialFile)
	}
	if c.CheckoutCredentialFile != "" {
		others["the checkout credential's directory"] = filepath.Dir(c.CheckoutCredentialFile)
	}
	realRoot := realPath(root)
	for what, p := range others {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("%w: %v", errSecretsRoot, err)
		}
		if other := realPath(abs); within(realRoot, other) || within(other, realRoot) {
			return fmt.Errorf("%w: it overlaps %s", errSecretsRoot, what)
		}
	}
	return nil
}

// realPath resolves the symlinks of the longest existing prefix of path.
func realPath(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for p := path; ; {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func resourceKindOfKey(key string) (runtimev2.ResourceKind, bool) {
	if key == string(runtimev2.ResourcePreparation) {
		return runtimev2.ResourcePreparation, true
	}
	if _, ok := runtimev2.ServiceNameOf(key); ok {
		return runtimev2.ResourceService, true
	}
	return "", false
}

// parseResourceConfig maps flags (which override) and the S4 environment onto a
// ResourceConfig. The positional arguments after the flags (a `--` separated argv
// for service-run) are returned.
func parseResourceConfig(name string, args []string, getenv func(string) string, output io.Writer) (ResourceConfig, []string, error) {
	setting := func(env string) string { return getenv(env) }
	fs := flag.NewFlagSet("kodama "+name, flag.ContinueOnError)
	fs.SetOutput(output)
	var c ResourceConfig
	str := func(p *string, flagName, env, usage string) {
		fs.StringVar(p, flagName, setting(env), usage+" (env "+env+")")
	}
	var podName, podUID string
	str(&c.ServerURL, "server-url", "KODAMA_SERVER_URL", "runtime API origin")
	str(&c.CAFile, "ca-file", "KODAMA_CA_FILE", "extra PEM CA roots for the runtime API")
	str(&c.RunID, "run-id", "KODAMA_RUN_ID", "run ID")
	str(&c.RuntimeAPI, "runtime-api", "KODAMA_RUNTIME_API", "runtime wire of the resource routes: v2 (default) or v3 (source set; prepare only)")
	str(&c.GitDir, "git-dir", "KODAMA_GIT_DIR", "v3 preparation: directory of the Git credential and CA files, default "+DefaultGitDir)
	str(&c.Key, "resource-key", "KODAMA_RESOURCE_KEY", "preparation or service.NAME")
	str(&c.AdmissionCredentialFile, "admission-credential-file", "KODAMA_ADMISSION_CREDENTIAL_FILE", "one-time admission credential file")
	str(&c.Identity.WorkloadName, "workload-name", "KODAMA_WORKLOAD_NAME", "committed workload name")
	str(&c.Identity.WorkloadUID, "workload-uid", "KODAMA_WORKLOAD_UID", "bound workload instance UID")
	str(&podName, "pod-name", "KODAMA_POD_NAME", "Pod name (a Job-owned Pod only)")
	str(&podUID, "pod-uid", "KODAMA_POD_UID", "Pod UID (a Job-owned Pod only)")
	str(&c.WorkspaceDir, "workspace-dir", "KODAMA_WORKSPACE_DIR", "preparation: the empty run workspace, default "+DefaultWorkspaceDir)
	str(&c.CheckoutCredentialFile, "checkout-credential-file", "KODAMA_CHECKOUT_CREDENTIAL_FILE", "preparation: read-only Git credential file (username:password)")
	str(&c.GitCAFile, "git-ca-file", "KODAMA_GIT_CA_FILE", "preparation: PEM CA bundle for Git over HTTPS")
	str(&c.GitBinary, "git-binary", "KODAMA_GIT_BINARY", "preparation: git executable, default git")
	str(&c.ScratchDir, "scratch-dir", "KODAMA_SCRATCH_DIR", "preparation: Git's private home, default the temporary directory")
	str(&c.SecretsDir, "secrets-dir", "KODAMA_SECRETS_DIR", "service-run: memory-backed secrets directory, default "+DefaultSecretsRoot)
	level := fs.String("log-level", func() string {
		if v := setting("KODAMA_LOG_LEVEL"); v != "" {
			return v
		}
		return "INFO"
	}(), "slog level (KODAMA_LOG_LEVEL)")
	fs.Usage = func() {
		fmt.Fprintf(output, "Usage: kodama %s [flags]%s\n", name, map[bool]string{true: " -- argv...", false: ""}[name == "service-run"])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ResourceConfig{}, nil, err
	}
	rest := fs.Args()
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return ResourceConfig{}, nil, fmt.Errorf("invalid log level %q", *level)
	}
	if (podName == "") != (podUID == "") {
		return ResourceConfig{}, nil, errors.New("the Pod name and UID are both set or both empty")
	}
	if podName != "" {
		c.Identity.PodName, c.Identity.PodUID = &podName, &podUID
	}
	c.applyDefaults()
	if c.ServerURL == "" || c.RunID == "" || c.Key == "" {
		return ResourceConfig{}, nil, errors.New("--server-url, --run-id and --resource-key are required")
	}
	if err := c.Validate(); err != nil {
		return ResourceConfig{}, nil, err
	}
	return c, rest, nil
}

// ResourceClient is the HTTP adapter of one resource's routes. The bearer token is
// held in memory and never logged; request bodies and retries are the attempt
// client's (the same bounded, idempotent exchange).
type ResourceClient struct{ c *Client }

// NewResourceClient builds the client of /runtime/v2/runs/{runId}/resources/{key}.
func NewResourceClient(serverURL string, run domain.RunID, key, caFile string, retry RetryPolicy, log *slog.Logger) (*ResourceClient, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if _, ok := resourceKindOfKey(key); !ok {
		return nil, fmt.Errorf("resource key %q is not preparation or service.NAME", key)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	base := strings.TrimRight(serverURL, "/") + "/runtime/v2/runs/" + string(run) + "/resources/" + key
	c, err := newClientAt(base, caFile, retry, log, true)
	if err != nil {
		return nil, err
	}
	return &ResourceClient{c: c}, nil
}

// resourceRuntime is the consumer-owned port of the resource commands over the
// resource routes. Unit tests substitute a fake.
type resourceRuntime interface {
	AdmitResource(ctx context.Context, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error)
	RenewResourceToken(ctx context.Context) (runtimev2.ResourceTokenResponse, error)
	ResourceContext(ctx context.Context) (runtimev2.ResourceContext, error)
	ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error)
	CompletePreparation(ctx context.Context, req runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error)
	// Dispose drops every credential the port holds (the bearer token) and makes
	// it refuse all later calls. It is idempotent and irreversible.
	Dispose()
}

var _ resourceRuntime = (*ResourceClient)(nil)

// Dispose clears the bearer token; the client then sends nothing.
func (r *ResourceClient) Dispose() { r.c.dispose() }

// AdmitResource exchanges the one-time admission credential and stores the token.
func (r *ResourceClient) AdmitResource(ctx context.Context, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev2.ResourceAdmitResponse{}, &ProtocolError{Op: "admitResource", Err: err}
	}
	res, err := call[runtimev2.ResourceAdmitResponse](ctx, r.c, "admitResource", http.MethodPost, "/admit", req, false, http.StatusOK)
	if err != nil {
		return runtimev2.ResourceAdmitResponse{}, err
	}
	r.c.setToken(res.value.Token.Token)
	return res.value, nil
}

// RenewResourceToken renews the bearer token and stores the new one.
func (r *ResourceClient) RenewResourceToken(ctx context.Context) (runtimev2.ResourceTokenResponse, error) {
	res, err := call[runtimev2.ResourceTokenResponse](ctx, r.c, "renewResourceToken", http.MethodPost, "/token", runtimev2.ResourceTokenRequest{}, true, http.StatusOK)
	if err != nil {
		return runtimev2.ResourceTokenResponse{}, err
	}
	r.c.setToken(res.value.Token.Token)
	return res.value, nil
}

// ResourceContext fetches the resource's committed context.
func (r *ResourceClient) ResourceContext(ctx context.Context) (runtimev2.ResourceContext, error) {
	res, err := call[runtimev2.ResourceContext](ctx, r.c, "getResourceContext", http.MethodGet, "/context", nil, true, http.StatusOK)
	return res.value, err
}

// ResolveSecret resolves one pinned binding of the service.
func (r *ResourceClient) ResolveSecret(ctx context.Context, req runtimev1.SecretResolveRequest) (runtimev1.SecretResolveResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev1.SecretResolveResponse{}, &ProtocolError{Op: "resolveResourceSecret", Err: err}
	}
	res, err := call[runtimev1.SecretResolveResponse](ctx, r.c, "resolveResourceSecret", http.MethodPost, "/secrets/resolve", req, true, http.StatusOK)
	return res.value, err
}

// CompletePreparation records the trusted checkout receipt (idempotent: an
// identical repeat returns the original).
func (r *ResourceClient) CompletePreparation(ctx context.Context, req runtimev2.PreparationReceiptRequest) (runtimev2.PreparationReceiptResponse, error) {
	if err := req.Validate(); err != nil {
		return runtimev2.PreparationReceiptResponse{}, &ProtocolError{Op: "completePreparation", Err: err}
	}
	res, err := call[runtimev2.PreparationReceiptResponse](ctx, r.c, "completePreparation", http.MethodPost, "/preparation/complete", req, true, http.StatusOK)
	return res.value, err
}

func isResourceRevoked(err error) bool {
	c := apiCode(err)
	return c == runtimev2.CodeResourceRevoked || c == runtimev1.CodeAttemptRevoked
}

func isUnbound(err error) bool { return apiCode(err) == runtimev2.CodeResourceUnbound }

// admitResource performs the resource admission with a fresh nonce, retrying only
// what is safe: an unbound resource (execution has not yet bound the workload's
// UID) and an unknown outcome (transport), both with the SAME nonce so a lost
// response replays to a fresh token under the same maximum. Every other refusal is
// final. The context of the response is the committed one and is checked against
// the resource this process was started for before anything is done with it.
func admitResource(ctx context.Context, rt resourceRuntime, cfg ResourceConfig, credential string, log *slog.Logger, want runtimev2.ResourceKind) (runtimev2.ResourceAdmitResponse, error) {
	nonce, err := newNonce()
	if err != nil {
		return runtimev2.ResourceAdmitResponse{}, err
	}
	req := runtimev2.ResourceAdmitRequest{AdmissionCredential: credential, Nonce: nonce, ClientVersion: cfg.ClientVersion, Identity: cfg.Identity}
	wait, cancel := context.WithTimeout(ctx, cfg.AdmissionWait)
	defer cancel()
	backoff := 250 * time.Millisecond
	for {
		resp, err := rt.AdmitResource(wait, req)
		switch {
		case err == nil:
			c := resp.Context
			if err := resp.Validate(); err != nil {
				return runtimev2.ResourceAdmitResponse{}, &ProtocolError{Op: "admitResource", Err: err}
			}
			if c.ResourceKind != want || c.ResourceKey != cfg.Key || string(c.RunID) != cfg.RunID {
				return runtimev2.ResourceAdmitResponse{}, &ProtocolError{Op: "admitResource", Err: errors.New("the committed context is another resource's")}
			}
			return resp, nil
		case isUnbound(err) || isTransport(err):
			log.Warn("admission not yet possible; will retry", "error", redactErr(err))
		default:
			return runtimev2.ResourceAdmitResponse{}, err
		}
		if err := sleep(wait, backoff); err != nil {
			return runtimev2.ResourceAdmitResponse{}, fmt.Errorf("admission did not succeed within %s: %w", cfg.AdmissionWait, err)
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// tokenRenewer is the one operation the renewal loop needs, so the v2 and v3
// resource ports share it.
type tokenRenewer interface {
	RenewResourceToken(ctx context.Context) (runtimev2.ResourceTokenResponse, error)
}

// renewResourceLoop renews the resource token at half its remaining life until ctx
// ends; a denied renewal, or a token that reached its maximum (the resource's
// deadline), cancels the work with errAuthorityLost.
func renewResourceLoop(ctx context.Context, cancel context.CancelCauseFunc, rt tokenRenewer, tok runtimev2.ResourceTokenInfo, minInterval time.Duration, log *slog.Logger) {
	lose := func(why error) { cancel(fmt.Errorf("%w: %v", errAuthorityLost, why)) }
	for {
		exp, _ := tok.ExpiresAt.Time()
		maxAt, _ := tok.MaxExpiresAt.Time()
		if !exp.Before(maxAt) {
			if sleep(ctx, time.Until(maxAt)) == nil {
				lose(errors.New("the resource deadline was reached"))
			}
			return
		}
		if sleep(ctx, max(time.Until(exp)/2, minInterval)) != nil {
			return
		}
		resp, err := rt.RenewResourceToken(ctx)
		switch {
		case err == nil:
			tok = resp.Token
		case ctx.Err() != nil:
			return
		case isTransport(err):
			if time.Now().After(exp) {
				lose(errors.New("token expired while renewal was unreachable"))
				return
			}
			log.Warn("token renewal unreachable; will retry", "error", err.Error())
			if sleep(ctx, minInterval) != nil {
				return
			}
		default:
			log.Error("token renewal denied", "error", err.Error())
			lose(err)
			return
		}
	}
}
