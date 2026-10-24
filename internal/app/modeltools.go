package app

// The model gateway and the MCP proxy (T-032, T-033), joined by T-039.
//
// Three pieces, kept apart so each is tested without a service:
//
//   - ModelToolsConfig is the installation's own statement of what a model
//     gateway and an MCP proxy may reach and which wrapper images are supported.
//     It is strict JSON read from the secrets directory (model-tools.json), empty
//     by default: without it nothing is approved, no profile can be registered
//     and no agent step admitted (fail closed, never an allow-all default).
//   - AssembleModelTools is a pure composition over injected services: the
//     management routes join the public router, the workload routes of the
//     gateway (/gateway/v1) and of the MCP proxy (/internal/mcp/v1) are plain
//     http.Handlers authenticated by the attempt's runtime token, and the workers
//     recover abandoned requests and stale tool calls.
//   - ModelApprovals/ToolApprovals decide which models and tools an agent
//     profile may name, from the project's stored gateway settings and MCP
//     registrations. Approval only narrows what a profile may say; it grants no
//     authority.
//
// NewCore selects the PostgreSQL-backed adapters (modeltools_core.go). Doubles
// are injected only by tests. Live provider and wrapper-image smoke is T-085.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gateway"
	gatewayhttp "github.com/optimiware/kodama/internal/gateway/http"
	"github.com/optimiware/kodama/internal/gateway/openai"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpproxyhttp "github.com/optimiware/kodama/internal/mcpproxy/http"
	mcpprotocol "github.com/optimiware/kodama/internal/mcpproxy/protocol"
)

// ---- installation configuration -----------------------------------------------

// ModelToolsConfig is the installation's model/tool statement. The zero value
// configures nothing.
type ModelToolsConfig struct {
	// Gateway selects the one OpenAI-compatible provider destination policy.
	Gateway GatewayConfig `json:"gateway"`
	// MCP selects the approved remote MCP destinations.
	MCP MCPConfig `json:"mcp"`
	// Catalogue lists the wrapper/harness/image tuples with compatibility
	// evidence. An empty catalogue qualifies nothing, so no profile registers.
	Catalogue []CatalogueConfig `json:"catalogue"`
}

// GatewayConfig is the gateway half.
type GatewayConfig struct {
	// Origins are the approved provider origins and optional path prefixes. An
	// empty list approves nothing and disables the gateway.
	Origins []OriginConfig `json:"origins"`
	// CAFile adds private trust roots for the provider (PEM).
	CAFile string `json:"caFile,omitempty"`
	// AllowNetworks lists CIDR ranges a provider name may resolve into besides
	// publicly routable unicast (a private provider on the operator's network).
	// Loopback and unspecified ranges are refused under production.
	AllowNetworks []string `json:"allowNetworks,omitempty"`
	// Models is the accounting table of the models the installation can estimate;
	// a model absent from it cannot be admitted. Zero members select the
	// conservative accounting.
	Models map[string]ModelAccountingConfig `json:"models"`
}

// OriginConfig is one approved provider origin.
type OriginConfig struct {
	Origin     string `json:"origin"`
	PathPrefix string `json:"pathPrefix,omitempty"`
}

// ModelAccountingConfig is one model's conservative token accounting.
type ModelAccountingConfig struct {
	BytesPerToken   int64 `json:"bytesPerToken,omitempty"`
	RequestOverhead int64 `json:"requestOverhead,omitempty"`
	MessageOverhead int64 `json:"messageOverhead,omitempty"`
	ToolOverhead    int64 `json:"toolOverhead,omitempty"`
}

func (m ModelAccountingConfig) accounting() openai.ModelAccounting {
	if m == (ModelAccountingConfig{}) {
		return openai.ConservativeAccounting
	}
	return openai.ModelAccounting{BytesPerToken: m.BytesPerToken, RequestOverhead: m.RequestOverhead, MessageOverhead: m.MessageOverhead, ToolOverhead: m.ToolOverhead}
}

// MCPConfig is the MCP half.
type MCPConfig struct {
	// AllowedHosts lists approved "host" (port 443) or "host:port" entries.
	AllowedHosts []string `json:"allowedHosts"`
	// AllowPrivate permits loopback and private addresses for approved hosts (an
	// in-cluster MCP server). An explicit installation decision.
	AllowPrivate bool `json:"allowPrivate,omitempty"`
	// CAFile adds private trust roots for the remote servers (PEM).
	CAFile string `json:"caFile,omitempty"`
}

// CatalogueConfig is one qualified tuple. Evidence is mandatory.
type CatalogueConfig struct {
	Image          string   `json:"image"`
	WrapperID      string   `json:"wrapperId"`
	WrapperVersion string   `json:"wrapperVersion"`
	Command        []string `json:"command"`
	HarnessID      string   `json:"harnessId"`
	HarnessVersion string   `json:"harnessVersion"`
	Controls       []string `json:"controls"`
	Evidence       string   `json:"evidence"`
}

// Configured reports whether the installation selected any model or tool
// destination. A catalogue alone does not enable anything.
func (c ModelToolsConfig) Configured() bool {
	return len(c.Gateway.Origins) > 0 || len(c.MCP.AllowedHosts) > 0
}

// LoadModelTools reads and validates the strict JSON document.
func LoadModelTools(path string) (ModelToolsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ModelToolsConfig{}, err
	}
	var c ModelToolsConfig
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return ModelToolsConfig{}, fmt.Errorf("model tools file: %w", err)
	}
	if dec.More() {
		return ModelToolsConfig{}, errors.New("model tools file: trailing content")
	}
	return c, nil
}

// Validate checks the document against the environment: nothing is guessed, a
// malformed entry stops startup, and production refuses loopback allowances.
func (c ModelToolsConfig) Validate(env Env) error {
	var errs []error
	if len(c.Gateway.Origins) == 0 {
		if len(c.Gateway.Models) > 0 || c.Gateway.CAFile != "" || len(c.Gateway.AllowNetworks) > 0 {
			errs = append(errs, errors.New("the gateway has settings but no approved origin"))
		}
	} else {
		if len(c.Gateway.Models) == 0 {
			errs = append(errs, errors.New("the gateway needs an accounting entry for each model it may admit"))
		}
		if _, err := c.gatewayPolicy(env); err != nil {
			errs = append(errs, err)
		}
		if _, err := c.estimator(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(c.MCP.AllowedHosts) == 0 && (c.MCP.AllowPrivate || c.MCP.CAFile != "") {
		errs = append(errs, errors.New("the MCP proxy has settings but no approved host"))
	}
	if len(c.MCP.AllowedHosts) > 0 {
		if _, err := mcpprotocol.NewDestinations(mcpprotocol.DestinationConfig{Allowed: c.MCP.AllowedHosts, AllowPrivate: c.MCP.AllowPrivate}); err != nil {
			errs = append(errs, err)
		}
	}
	if len(c.Catalogue) > 0 && len(c.Gateway.Origins) == 0 {
		errs = append(errs, errors.New("a wrapper catalogue needs the model gateway: its controls route provider traffic through it"))
	}
	if _, err := c.catalogue(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c ModelToolsConfig) gatewayPolicy(env Env) (*openai.Policy, error) {
	origins := make([]openai.ApprovedOrigin, 0, len(c.Gateway.Origins))
	for _, o := range c.Gateway.Origins {
		origins = append(origins, openai.ApprovedOrigin{Origin: o.Origin, PathPrefix: o.PathPrefix})
	}
	cfg := openai.PolicyConfig{Origins: origins}
	if len(c.Gateway.AllowNetworks) > 0 {
		var prefixes []netip.Prefix
		for _, s := range c.Gateway.AllowNetworks {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("gateway allowed network %q: %w", s, err)
			}
			p = p.Masked()
			if env == Production && (p.Contains(netip.MustParseAddr("127.0.0.1")) || p.Contains(netip.IPv6Loopback()) ||
				p.Contains(netip.IPv4Unspecified()) || p.Contains(netip.IPv6Unspecified()) || p.Bits() == 0) {
				return nil, &AdapterError{Env: Production, Port: PortStore, Adapter: "gateway-loopback-network", Reason: "a loopback or unspecified provider network is a test fixture"}
			}
			prefixes = append(prefixes, p)
		}
		cfg.Addresses = openai.AllowPrefixes(openai.PublicOnly, prefixes...)
	}
	return openai.NewPolicy(cfg)
}

func (c ModelToolsConfig) estimator() (*openai.Estimator, error) {
	table := make(map[string]openai.ModelAccounting, len(c.Gateway.Models))
	for name, m := range c.Gateway.Models {
		table[name] = m.accounting()
	}
	return openai.NewEstimator(table)
}

func (c ModelToolsConfig) catalogue() (*agents.Catalogue, error) {
	entries := make([]agents.CatalogueEntry, 0, len(c.Catalogue))
	for _, e := range c.Catalogue {
		var controls []agents.Control
		for _, k := range e.Controls {
			controls = append(controls, agents.Control(k))
		}
		entries = append(entries, agents.CatalogueEntry{
			Tuple: agents.Tuple{
				Image: e.Image, WrapperID: e.WrapperID, WrapperVersion: e.WrapperVersion, Command: slices.Clone(e.Command),
				HarnessID: e.HarnessID, HarnessVersion: e.HarnessVersion,
			},
			Controls: controls, Evidence: e.Evidence,
		})
	}
	return agents.NewCatalogue(entries...)
}

// ---- approvals ------------------------------------------------------------------

// SettingsReader is the gateway's read of a project's stored model settings.
type SettingsReader interface {
	Settings(ctx context.Context, scope domain.Scope) (gateway.ModelSettings, error)
}

// ModelApprovals approves exactly the model and endpoint a project's stored
// gateway settings enable: the settings must be enabled, the endpoint equal and
// the model on the allow-list. It does not approve tools; compose it with the
// MCP tool approvals (ComposeApprovals).
type ModelApprovals struct{ settings SettingsReader }

// NewModelApprovals requires the settings reader.
func NewModelApprovals(s SettingsReader) (*ModelApprovals, error) {
	if s == nil {
		return nil, errors.New("app: a gateway settings reader is required")
	}
	return &ModelApprovals{settings: s}, nil
}

// ApproveModel implements the model half of agents.Approvals.
func (a *ModelApprovals) ApproveModel(ctx context.Context, scope domain.Scope, m agents.Model) error {
	st, err := a.settings.Settings(ctx, scope)
	switch {
	case err == nil:
	case errors.Is(err, gateway.ErrNotFound), errors.Is(err, gateway.ErrHidden):
		return agents.ErrDenied
	default:
		return agents.Unavailablef(err, "the model settings are unavailable")
	}
	if !st.Enabled || st.Endpoint.IsZero() || st.Endpoint.String() != strings.TrimRight(m.Endpoint, "/") || !slices.Contains(st.Models, m.Name) {
		return agents.ErrDenied
	}
	return nil
}

// ComposedApprovals is agents.Approvals from a model half and a tool half.
type ComposedApprovals struct {
	Models interface {
		ApproveModel(context.Context, domain.Scope, agents.Model) error
	}
	Tools interface {
		ApproveTools(context.Context, domain.Scope, []string) error
	}
}

var _ agents.Approvals = ComposedApprovals{}

// ApproveModel implements agents.Approvals; no model half denies.
func (c ComposedApprovals) ApproveModel(ctx context.Context, scope domain.Scope, m agents.Model) error {
	if c.Models == nil {
		return agents.ErrDenied
	}
	return c.Models.ApproveModel(ctx, scope, m)
}

// ApproveTools implements agents.Approvals; no tool half denies.
func (c ComposedApprovals) ApproveTools(ctx context.Context, scope domain.Scope, tools []string) error {
	if c.Tools == nil {
		if len(tools) == 0 {
			return nil
		}
		return agents.ErrDenied
	}
	return c.Tools.ApproveTools(ctx, scope, tools)
}

// LateApprovals breaks the construction cycle between the agent profile service
// (which needs approvals) and the gateway and MCP stores (which need the
// runtime authority built after it). Until bound it approves nothing.
type LateApprovals struct {
	mu sync.RWMutex
	a  agents.Approvals
}

var _ agents.Approvals = (*LateApprovals)(nil)

// Bind sets the approvals once the gateway and MCP stores exist.
func (l *LateApprovals) Bind(a agents.Approvals) {
	l.mu.Lock()
	l.a = a
	l.mu.Unlock()
}

func (l *LateApprovals) current() agents.Approvals {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.a
}

// ApproveModel implements agents.Approvals.
func (l *LateApprovals) ApproveModel(ctx context.Context, scope domain.Scope, m agents.Model) error {
	if a := l.current(); a != nil {
		return a.ApproveModel(ctx, scope, m)
	}
	return agents.ErrDenied
}

// ApproveTools implements agents.Approvals.
func (l *LateApprovals) ApproveTools(ctx context.Context, scope domain.Scope, tools []string) error {
	if a := l.current(); a != nil {
		return a.ApproveTools(ctx, scope, tools)
	}
	return agents.ErrDenied
}

// ---- the slice --------------------------------------------------------------------

// MCPService is what the MCP management routes and the workload endpoint need
// from the proxy; *mcpproxy.Service implements both.
type MCPService interface {
	mcpproxyhttp.Manager
	mcpprotocol.Runtime
	Sweep(ctx context.Context, tenant domain.TenantID, limit int) error
	Shutdown()
}

var _ MCPService = (*mcpproxy.Service)(nil)

// GatewayService is the gateway's use-case layer as the slice needs it.
type GatewayService interface {
	gatewayhttp.Service
	Reconcile(ctx context.Context, quiet time.Duration) (gateway.ReconcileReport, error)
}

var _ GatewayService = (*gateway.Service)(nil)

// DefaultGatewayQuiet is how long a request must be silent before reconciliation
// abandons it; it must cover several heartbeat intervals (gateway.minQuiet).
const DefaultGatewayQuiet = 5 * time.Minute

// mcpSweepBatch bounds one tenant's recovery per pass.
const mcpSweepBatch = 100

// ModelToolInputs are the collaborators of the model/tool slice.
type ModelToolInputs struct {
	Env     Env
	Log     *slog.Logger
	Tuning  Tuning
	Cursors *httpapi.CursorCodec
	Tenants execution.Tenants

	Gateway GatewayService
	// GatewayWriteTimeout bounds one write of the response stream (default: the
	// handler's).
	GatewayWriteTimeout time.Duration
	// GatewayQuiet overrides DefaultGatewayQuiet.
	GatewayQuiet time.Duration

	MCP MCPService
	// MCPIdleTimeout and MCPMaxRequestBytes bound the workload endpoint (defaults
	// one hour and 1 MiB).
	MCPIdleTimeout     time.Duration
	MCPMaxRequestBytes int64

	// Adapters lists every adapter below the services (stores, authorities,
	// credential sources, provider transport, remote client), for the
	// construction guard of the environment: production refuses a development or
	// test one.
	Adapters []any
}

// ModelToolSlice is the assembled slice.
type ModelToolSlice struct {
	// RegisterPublic adds the model-settings and MCP-server management routes.
	RegisterPublic func(*httpapi.Router) error
	// GatewayHandler serves /gateway/v1/…; MCPHandler serves /internal/mcp/v1/….
	// Both are authenticated by the attempt's runtime token, not by the public
	// router.
	GatewayHandler http.Handler
	MCPHandler     http.Handler
	// Workers are the recovery loops, in start order.
	Workers []NamedWorker
	// Shutdown releases the live MCP sessions; call it after the workers stopped.
	Shutdown func()
	// Routes names what is registered.
	Routes []string
}

// Paths the process handler mounts the workload handlers at.
const (
	GatewayWorkloadPrefix = "/gateway/v1/"
	MCPWorkloadPrefix     = "/internal/mcp/v1/"
)

// AssembleModelTools validates the inputs and builds the slice. Both services
// are required together: a gateway without tool approvals, or the reverse, is
// not a configuration this assembly supports (the caller leaves both out and
// records them as disabled).
func AssembleModelTools(in ModelToolInputs) (*ModelToolSlice, error) {
	missing := func(what string) error { return fmt.Errorf("%w: %s is required", execution.ErrNotConfigured, what) }
	if !in.Env.Valid() {
		return nil, ErrEnvUnknown
	}
	if in.Log == nil {
		in.Log = slog.New(slog.DiscardHandler)
	}
	switch {
	case in.Gateway == nil:
		return nil, missing("the model gateway service")
	case in.MCP == nil:
		return nil, missing("the MCP proxy service")
	case in.Cursors == nil:
		return nil, missing("the cursor codec")
	case in.Tenants == nil:
		return nil, missing("the tenant source of the recovery loops")
	case in.Tuning.ReconcileInterval <= 0:
		return nil, missing("a positive reconcile interval")
	}
	for _, a := range append([]any{in.Gateway, in.MCP}, in.Adapters...) {
		if a == nil {
			continue
		}
		if _, err := Admit(in.Env, PortStore, a); err != nil {
			return nil, err
		}
	}

	gwHandler, err := gatewayhttp.NewHandler(gatewayhttp.Config{Service: in.Gateway, WriteTimeout: in.GatewayWriteTimeout, Logger: in.Log})
	if err != nil {
		return nil, fmt.Errorf("model gateway routes: %w", err)
	}
	mcpManage, err := mcpproxyhttp.NewHandler(in.MCP, in.Cursors)
	if err != nil {
		return nil, fmt.Errorf("MCP management routes: %w", err)
	}
	idle, maxBody := in.MCPIdleTimeout, in.MCPMaxRequestBytes
	if idle == 0 {
		idle = time.Hour
	}
	if maxBody == 0 {
		maxBody = 1 << 20
	}
	mcpWorkload, err := mcpprotocol.NewHandler(mcpprotocol.ServerConfig{Runtime: in.MCP, IdleTimeout: idle, MaxRequestBytes: maxBody, Logger: in.Log})
	if err != nil {
		return nil, fmt.Errorf("MCP proxy endpoint: %w", err)
	}
	quiet := in.GatewayQuiet
	if quiet == 0 {
		quiet = DefaultGatewayQuiet
	}
	s := &ModelToolSlice{
		RegisterPublic: func(rt *httpapi.Router) error {
			for _, h := range []routeRegistrar{gwHandler, mcpManage} {
				if err := h.RegisterRoutes(rt); err != nil {
					return err
				}
			}
			return nil
		},
		GatewayHandler: gwHandler.Workload(),
		MCPHandler:     mcpWorkload,
		Shutdown:       in.MCP.Shutdown,
		Routes:         []string{"model-settings", "model-gateway", "mcp-servers", "mcp-proxy"},
	}
	s.Workers = []NamedWorker{
		{Name: "model-gateway-reconciler", Run: func(ctx context.Context) error {
			return loop(ctx, in.Tuning.ReconcileInterval, func(ctx context.Context) error {
				_, err := in.Gateway.Reconcile(ctx, quiet)
				return err
			}, in.Log, "model gateway reconciliation")
		}},
		{Name: "mcp-proxy-sweeper", Run: func(ctx context.Context) error {
			return loop(ctx, in.Tuning.ReconcileInterval, func(ctx context.Context) error {
				ids, err := in.Tenants.TenantIDs(ctx)
				if err != nil {
					return fmt.Errorf("tenant enumeration: %w", err)
				}
				var errs []error
				for _, t := range ids {
					if err := in.MCP.Sweep(ctx, t, mcpSweepBatch); err != nil {
						errs = append(errs, err)
					}
				}
				return errors.Join(errs...)
			}, in.Log, "MCP proxy recovery")
		}},
	}
	return s, nil
}
