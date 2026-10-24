// Package kit is the shared fixture of T-038's M2 acceptance: the actual core
// assembly (internal/app, KODAMA_ENV=test) of T-026's M1 kit, started with the
// installation statement of the model gateway, the MCP proxy and a qualified
// wrapper tuple, over the same real services (PostgreSQL, JetStream, S3, HTTPS
// Git, oauthsonas) and the Podman runtime.
//
// What is real: the server and every module it wires, the database, the broker,
// the object store, Git, identity, the Podman containers and the step
// entrypoint. What is deterministic and test-owned: the model provider (an
// OpenAI-compatible TLS server whose replies are scripted), the remote MCP
// server (the T-033 fixture) and the agent itself (a /bin/sh program run as the
// profile's wrapper, see agent.go). None of it is a live-provider, wrapper-image
// or release qualification (T-085).
//
// Resource prefix t038 (the M1 kit's per-process suffix keeps containers apart).
package kit

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
)

// Env is one installation with the M2 slices selected.
type Env struct {
	*Stack
	World *World

	// Provider is the deterministic model provider; MCP the deterministic remote
	// MCP server.
	Provider *gatewaytest.Provider
	MCP      *mcpfixture.Server

	// Front is the plain-HTTP origin a container reaches the gateway and MCP
	// paths through (nothing else of the server).
	Front *WorkloadFront

	// Registration is the MCP registration the profile may use; Profile the
	// registered agent profile reference (name@version).
	Registration string
	Profile      string

	// Tools is the real tools image (Options.Delivery).
	Tools Image

	cfgTweak func(*app.CoreConfig)
	retune   func(*app.Tuning)
	modelCA  string
	mcpCA    string
}

// Options select what a test needs.
type Options struct {
	Stack StackOptions
	// Tune adjusts the operational bounds.
	Tune func(*app.Tuning)
	// Config adjusts the configuration before the server starts.
	Config func(*app.CoreConfig)
	// Limits are the project's model limits (default DefaultLimits).
	Limits *ModelLimits
	// ProfileLimits are the limits the registered profile claims (default
	// generous).
	ProfileLimits *agents.Limits
	// Delivery builds the real deployment-tools image, pushes it to the scope
	// registry, qualifies its apply/delete scripts as the installation's delivery
	// tools and its image as a second wrapper tuple (the ERP agent's), and
	// shortens the cleanup sweep. It needs the scope's registry; the scenarios
	// that deploy also need its kind cluster.
	Delivery bool
	// NoProfile skips registering the agent profile.
	NoProfile bool
}

// ModelLimits is the project's model settings limits.
type ModelLimits struct {
	MaxRequests     int64 `json:"maxRequests"`
	TokenAllowance  int64 `json:"tokenAllowance"`
	MaxRequestBytes int64 `json:"maxRequestBytes"`
	MaxInputTokens  int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
}

// DefaultLimits are generous enough for every scenario that is not about limits.
var DefaultLimits = ModelLimits{MaxRequests: 40, TokenAllowance: 100000, MaxRequestBytes: 8192, MaxInputTokens: 4096, MaxOutputTokens: 256}

// New provisions the isolated dependencies, starts the deterministic provider
// and remote, composes and starts the application with the M2 installation
// statement and seeds tenant A. The profile is registered through the public
// API unless NoProfile.
func New(t *testing.T, o Options) *Env {
	t.Helper()
	so := o.Stack
	so.Tune = o.Tune
	s := NewStack(t, so)
	e := &Env{Stack: s, cfgTweak: o.Config}
	if o.Delivery {
		e.Tools = ToolsImage(t)
	}

	e.Provider = gatewaytest.NewProvider()
	t.Cleanup(e.Provider.Close)
	e.MCP = mcpfixture.New(t, mcpfixture.Options{})

	dir := t.TempDir()
	e.modelCA = writeCert(t, filepath.Join(dir, "model-ca.pem"), e.Provider.Srv.Certificate())
	e.mcpCA = writeCert(t, filepath.Join(dir, "mcp-ca.pem"), e.MCP.Srv.Certificate())
	e.Front = newWorkloadFront(t, "127.0.0.1", s)

	cfg := e.Config()
	if _, err := s.TryStart(cfg); err != nil {
		t.Fatalf("start the core assembly: %v", err)
	}
	s.WaitReady()
	e.World = s.Seed(true)
	e.AddMember("reviewer", "reviewer")
	e.AddMember("analyst", "reviewer") // a second reviewer: analyst the persona, reviewer the role
	e.configureModelTools(o)
	if !o.NoProfile {
		e.RegisterProfile(o.ProfileLimits)
	}
	return e
}

// Config is the configuration the real process would load with this
// installation statement.
func (e *Env) Config() app.CoreConfig {
	cfg := e.Stack.CoreConfig()
	tune := &cfg.Tuning
	tune.GateTTL = 10 * time.Minute
	// A shared host starts many containers at once (checkout, image mount); the
	// default one-minute startup budget measures the host, not the product.
	tune.Limit.StartupBudget = 4 * time.Minute
	cfg.ModelTools = app.ModelToolsConfig{
		Gateway: app.GatewayConfig{
			Origins:       []app.OriginConfig{{Origin: e.Provider.Origin(), PathPrefix: "/v1"}},
			CAFile:        e.modelCA,
			AllowNetworks: []string{"127.0.0.0/8"},
			Models:        map[string]app.ModelAccountingConfig{ModelName: {}},
		},
		MCP: app.MCPConfig{AllowedHosts: []string{e.MCP.HostPort()}, AllowPrivate: true, CAFile: e.mcpCA},
		Catalogue: []app.CatalogueConfig{{
			Image: PodmanImage, WrapperID: WrapperID, WrapperVersion: WrapperVersion, Command: WrapperCommand(),
			HarnessID: HarnessID, HarnessVersion: HarnessVersion,
			// requests and tokens are NOT qualified: the gateway enforces the project's
			// settings limits, not an allowance a profile claims (docs/acceptance/m2.md).
			Controls: []string{"model", "tool_calls", "tool_allowlist", "transcript"},
			Evidence: "t038-deterministic-stub-smoke",
		}},
	}
	// The operator statements of the M2 production joins (T-096, T-067's registry,
	// T-100): the action ceiling per step kind, the scope registry the build's
	// image is verified against, and the native Git trigger.
	cfg.ActionCeiling = app.ActionCeilingConfig{Kinds: map[string]app.KindCeilingConfig{
		"agent":     {Grants: []string{"model.invoke", "tool.invoke"}},
		"container": {Grants: []string{"deployment.call"}},
	}}
	cfg.Registry = e.registryConfig()
	if e.Tools.Digest != "" {
		cfg.DeliveryTools = []app.ToolQualification{
			{Protocol: app.ToolApply, Image: e.Tools.Ref(), Command: []string{"/opt/kodama/deploy/apply.sh"}},
			{Protocol: app.ToolCleanup, Image: e.Tools.Ref(), Command: []string{"/opt/kodama/deploy/delete.sh"}},
		}
		cfg.ModelTools.Catalogue = append(cfg.ModelTools.Catalogue, app.CatalogueConfig{
			Image: e.Tools.Ref(), WrapperID: WrapperID, WrapperVersion: WrapperVersion, Command: WrapperCommand(),
			HarnessID: HarnessID, HarnessVersion: HarnessVersion,
			Controls: []string{"model", "tool_calls", "tool_allowlist", "transcript"}, Evidence: "t038-deterministic-stub-smoke-tools-image",
		})
		cfg.Tuning.CleanupSweep = 2 * time.Second
	}
	cfg.GitTriggers = app.GitTriggerConfig{RateLimitPerMinute: 6000}
	if e.retune != nil {
		e.retune(&cfg.Tuning)
	}
	if e.cfgTweak != nil {
		e.cfgTweak(&cfg)
	}
	return cfg
}

// Retune changes the operational bounds of the NEXT process (see Restart); nil
// restores the kit's defaults.
func (e *Env) Retune(f func(*app.Tuning)) { e.retune = f }

// Restart stops the process and starts a new one over the same state and the
// same installation statement.
func (e *Env) Restart() {
	e.T.Helper()
	e.Stop()
	if _, err := e.TryStart(e.Config()); err != nil {
		e.T.Fatalf("restart: %v", err)
	}
	e.WaitReady()
}

func writeCert(t testing.TB, path string, cert *x509.Certificate) string {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// WorkloadFront is the plain-HTTP origin of the workload-facing paths for the
// containers: the gateway, the MCP proxy and (so a test can show what the
// server answers a workload that reaches it with its own runtime token) the
// public API. It forwards those prefixes to the running process only and counts
// what it forwarded.
type WorkloadFront struct {
	Origin string
	n      atomic.Int64
	srv    *http.Server
}

// Requests counts forwarded requests.
func (f *WorkloadFront) Requests() int64 { return f.n.Load() }

func newWorkloadFront(t testing.TB, host string, s *Stack) *WorkloadFront {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	f := &WorkloadFront{Origin: "http://" + net.JoinHostPort(host, port)}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			u, _ := url.Parse(s.Base())
			r.SetURL(u)
			r.Out.Host = r.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "the server is not serving", http.StatusBadGateway)
		},
	}
	mux := http.NewServeMux()
	for _, p := range []string{"/gateway/v1/", "/internal/mcp/v1/", "/api/v1/"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			f.n.Add(1)
			proxy.ServeHTTP(w, r)
		})
	}
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

// HoldProgress restarts the process with a reconcile interval of an hour and
// stops the broker's proxy, so a committed gate decision is neither consumed by
// the scan nor woken by its event: the window between a decision and its
// consumption is as long as the test needs. The decision and every other
// request still commit (the event is only a wakeup; the outbox keeps it).
func (e *Env) HoldProgress() {
	e.T.Helper()
	e.Retune(func(t *app.Tuning) { t.ReconcileInterval = time.Hour })
	e.Restart()
	e.NATS.Stop()
}

// ResumeProgress ends the window: the broker is reachable again and a new
// process runs at the kit's normal pace, which recovers from durable state.
func (e *Env) ResumeProgress() {
	e.T.Helper()
	e.Retune(nil)
	e.NATS.Start()
	e.Restart()
}

// RegistryHost is the scope registry as repository references spell it and as
// the host, the server and the host-networked containers all reach it.
func RegistryHost() string {
	u, err := url.Parse(ScopeEnv("KODAMA_REGISTRY_URL"))
	if err != nil || u.Host == "" {
		panic("KODAMA_REGISTRY_URL is not an origin")
	}
	return u.Host
}

// RegistryRepositoryPrefix is the scope's registry namespace: images of this
// installation live under it.
func RegistryRepositoryPrefix() string {
	for _, k := range []string{"DEV_SCOPE", "KODAMA_RESOURCE_PREFIX"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return Prefix
}

func (e *Env) registryConfig() app.RegistryConfig {
	h := RegistryHost()
	return app.RegistryConfig{Endpoints: []app.RegistryEndpoint{{
		Host: h, URL: "http://" + h, AllowInsecureHTTP: true, Tenants: []string{"*"}, Repositories: []string{RegistryRepositoryPrefix()},
	}}}
}

// StartReplica starts a second process of the installation over the same state
// and the same installation statement (one more replica of the deployment).
func (e *Env) StartReplica() *app.Server {
	e.T.Helper()
	cfg := e.Config()
	cfg.Process.HTTPAddr = "127.0.0.1:0"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := app.Start(ctx, cfg, e.Logger())
	if err != nil {
		e.T.Fatalf("start a replica: %v", err)
	}
	e.T.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
	})
	return srv
}
