package app

// Fixtures of the M2 composition (T-039): the model gateway and MCP proxy over
// their in-memory fixtures with deterministic provider and tool responses, the
// native Git trigger over its in-memory store with the real local Git reader,
// and the profile approvals that read both. Every double flags itself
// development-only so `production` refuses it at construction. None of it is
// evidence of SQL, transaction, network or container behavior (T-073, T-067,
// T-038) or of a live provider or wrapper image (T-085).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gateway"
	gwadapters "github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/gateway/openai"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/mcpproxy"
	mcpadapters "github.com/optimiware/kodama/internal/mcpproxy/adapters"
	mcpmem "github.com/optimiware/kodama/internal/mcpproxy/mcptest/memory"
	"github.com/optimiware/kodama/internal/workspace"
)

// The fixture's model: one approved provider origin and one estimable model.
const (
	devModel       = "gpt-a"
	devProviderKey = "sk-t039-PROVIDER-KEY-SENTINEL"
)

// devModelTools is the installation statement of the fixture: the gateway
// fixture's provider origin, one model, the MCP fixture's host, and the tuple
// the agents fixture qualifies.
func devModelTools() ModelToolsConfig {
	d := agentstest.Draft(&testing.T{}, "coding")
	return ModelToolsConfig{
		Gateway: GatewayConfig{
			Origins: []OriginConfig{{Origin: "https://models.t083.example.invalid", PathPrefix: "/v1"}},
			Models:  map[string]ModelAccountingConfig{devModel: {}},
		},
		MCP: MCPConfig{AllowedHosts: []string{"mcp.example.test"}},
		Catalogue: []CatalogueConfig{{
			Image: d.Image, WrapperID: d.Wrapper.ID, WrapperVersion: d.Wrapper.Version, Command: d.Wrapper.Command,
			HarnessID: d.Wrapper.Harness.ID, HarnessVersion: d.Wrapper.Harness.Version,
			Controls: []string{"model", "requests", "tokens", "tool_calls", "tool_allowlist", "transcript"}, Evidence: "t039-fixture-smoke",
		}},
	}
}

// ---- flagged doubles ------------------------------------------------------------

type devGWStore struct{ *gatewaytest.MemStore }
type devGWWorkloads struct{ *gatewaytest.MemAuthority }
type devGWCredentials struct{ *gatewaytest.MemCredentials }
type devGWProvider struct{ *gatewaytest.ScriptedProvider }
type devMCPStore struct{ *mcpmem.Store }
type devMCPRemote struct{ *mcpmem.Remote }
type devHookStore struct{ *gitwebhooktest.Store }
type devHookSecrets struct{ *gitwebhooktest.Secrets }
type devHookDefinitions struct{ *gitwebhooktest.Definitions }
type devHookRuns struct{ *gitwebhooktest.Runs }

func (devGWStore) DevelopmentOnly() bool       { return true }
func (devGWWorkloads) DevelopmentOnly() bool   { return true }
func (devGWCredentials) DevelopmentOnly() bool { return true }
func (devGWProvider) DevelopmentOnly() bool    { return true }
func (devMCPStore) DevelopmentOnly() bool      { return true }
func (devMCPRemote) DevelopmentOnly() bool     { return true }
func (devHookStore) DevelopmentOnly() bool     { return true }
func (devHookSecrets) DevelopmentOnly() bool   { return true }
func (devHookDefinitions) DevelopmentOnly() bool {
	return true
}
func (devHookRuns) DevelopmentOnly() bool { return true }

// ---- the composition ----------------------------------------------------------------

type devM2Inputs struct {
	Log       *slog.Logger
	Tuning    Tuning
	Cursors   *httpapi.CursorCodec
	Tenants   execution.Tenants
	Authority identity.Authority
	Access    identity.AccessReader
	Repos     workspace.Repositories
	Git       workspace.RefReader
}

type devM2 struct {
	In        devM2Inputs
	Config    ModelToolsConfig
	Catalogue *agents.Catalogue
	Approvals agents.Approvals
	// Profiles is the agent profile service whose compatibility catalogue and
	// approvals are the ones above (set by the composition that builds it).
	Profiles *agents.Service
	Slice    *ModelToolSlice
	Triggers *TriggerSlice

	Gateway   *gateway.Service
	GWStore   *gatewaytest.MemStore
	Workloads *gatewaytest.MemAuthority
	Creds     *gatewaytest.MemCredentials
	Provider  *gatewaytest.ScriptedProvider

	MCP      *mcpproxy.Service
	MCPWorld *mcpmem.World

	Hook        *gitwebhook.Service
	HookStore   *gitwebhooktest.Store
	HookRuns    *gitwebhooktest.Runs
	HookSecrets *gitwebhooktest.Secrets
	HookDefs    *gitwebhooktest.Definitions
}

// newDevM2 composes the M2 slices exactly as NewCore does apart from the
// adapters: AssembleModelTools and AssembleTriggers are the production
// compositions; the stores, authority stand-ins, provider and remote are the
// modules' own deterministic fixtures. It reports an error (a construction
// refusal) rather than failing, so the production environment's refusal is
// observable.
func newDevM2(t *testing.T, env Env, in devM2Inputs) (*devM2, error) {
	t.Helper()
	m := &devM2{In: in, Config: devModelTools()}
	if err := m.Config.Validate(env); err != nil {
		return nil, err
	}
	var err error
	if m.Catalogue, err = m.Config.catalogue(); err != nil {
		return nil, err
	}
	policy, err := m.Config.gatewayPolicy(env)
	if err != nil {
		return nil, err
	}
	estimator, err := m.Config.estimator()
	if err != nil {
		return nil, err
	}

	// ---- model gateway: the real use cases, the identity authority, the real
	// destination policy and estimator, a memory store and a scripted provider ----
	mgmt, err := gwadapters.NewAuthority(in.Authority)
	if err != nil {
		return nil, err
	}
	m.Workloads = gatewaytest.NewMemAuthority()
	m.Creds = gatewaytest.NewMemCredentials()
	m.Creds.Put(gatewaytest.Secret, devProviderKey)
	m.Provider = &gatewaytest.ScriptedProvider{}
	if m.GWStore, err = gatewaytest.NewMemStore(m.Workloads.Check, mgmt); err != nil {
		return nil, err
	}
	to := gateway.DefaultTimeouts()
	to.Recheck, to.FinalizeBackoff = time.Minute, time.Millisecond
	if m.Gateway, err = gateway.NewService(gateway.Config{
		Store: devGWStore{m.GWStore}, Workloads: devGWWorkloads{m.Workloads}, Authority: mgmt, Credentials: devGWCredentials{m.Creds},
		Provider: devGWProvider{m.Provider}, Estimator: estimator, Decoder: openai.NewDecoder(), Destinations: policy, Tenants: in.Tenants, Timeouts: to,
	}); err != nil {
		return nil, err
	}

	// ---- MCP proxy: the real use cases and the identity-backed human authority
	// over the memory store and a scripted remote ----
	m.MCPWorld = mcpmem.New()
	cfg := m.MCPWorld.Config()
	if cfg.Human, err = mcpadapters.NewHumanAuthority(in.Authority, in.Access); err != nil {
		return nil, err
	}
	cfg.Store, cfg.Remote = devMCPStore{m.MCPWorld.Store}, devMCPRemote{m.MCPWorld.Remote}
	if m.MCP, err = mcpproxy.NewService(cfg); err != nil {
		return nil, err
	}

	// ---- profile approvals: what the stored settings and registrations enable ----
	models, err := NewModelApprovals(m.GWStore)
	if err != nil {
		return nil, err
	}
	tools, err := mcpadapters.NewToolApprovals(m.MCPWorld.Store)
	if err != nil {
		return nil, err
	}
	m.Approvals = ComposedApprovals{Models: models, Tools: tools}

	if m.Slice, err = AssembleModelTools(ModelToolInputs{
		Env: env, Log: in.Log, Tuning: in.Tuning, Cursors: in.Cursors, Tenants: in.Tenants, Gateway: m.Gateway, MCP: m.MCP,
		Adapters: []any{devGWStore{m.GWStore}, devGWWorkloads{m.Workloads}, devGWCredentials{m.Creds}, devGWProvider{m.Provider}, devMCPStore{m.MCPWorld.Store}, devMCPRemote{m.MCPWorld.Remote}},
	}); err != nil {
		return nil, err
	}

	// ---- native Git trigger: the real use cases, the identity-backed
	// authorizer, the project's repository binding and the real (local) Git
	// reader; the store, secret, definition and admission seams are the module's
	// doubles (T-038 owns the real ones) ----
	trig, err := NewTriggerAuthorizer(in.Authority)
	if err != nil {
		return nil, err
	}
	m.HookStore, m.HookRuns, m.HookSecrets, m.HookDefs = gitwebhooktest.NewStore(), &gitwebhooktest.Runs{}, &gitwebhooktest.Secrets{}, &gitwebhooktest.Definitions{}
	if m.Hook, err = gitwebhook.New(gitwebhook.Dependencies{
		Store: devHookStore{m.HookStore}, Authorizer: trig, Secrets: devHookSecrets{m.HookSecrets}, Definitions: devHookDefinitions{m.HookDefs},
		Repositories: in.Repos, Git: in.Git, Runs: devHookRuns{m.HookRuns},
	}); err != nil {
		return nil, err
	}
	if m.Triggers, err = AssembleTriggers(TriggerInputs{
		Env: env, Log: in.Log, Service: m.Hook, RateLimitPerMinute: 1000,
		Adapters: []any{devHookStore{m.HookStore}, devHookSecrets{m.HookSecrets}, devHookDefinitions{m.HookDefs}, devHookRuns{m.HookRuns}, in.Repos, in.Git},
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// m2Reply is a deterministic provider reply of schema-valid chat completion
// chunks (api/gateway.yaml ChatCompletionChunk), then valid usage and the
// terminator. gatewaytest.Reply streams fixture chunks that are not of that
// shape; this is what a conforming provider adapter hands the gateway.
func m2Reply(model string, in, out int64, texts ...string) gatewaytest.Step {
	return func(_ context.Context, call gateway.ProviderCall) (gateway.Stream, error) {
		var cs []gateway.Chunk
		for _, t := range texts {
			content, _ := json.Marshal(t)
			cs = append(cs, gateway.Chunk{
				Data:  []byte(fmt.Sprintf(`{"id":"chatcmpl-m2","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, model, content)),
				Model: model,
			})
		}
		cs = append(cs, gateway.Chunk{
			Data:   []byte(fmt.Sprintf(`{"id":"chatcmpl-m2","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, model)),
			Model:  model,
			Finish: "stop",
		}, gateway.Chunk{Usage: &gateway.TokenUsage{Input: in, Output: out}}, gateway.Chunk{Done: true})
		return &m2Stream{chunks: cs, dest: gateway.Destination{Origin: call.Endpoint.Origin(), IP: "203.0.113.7"}}, nil
	}
}

type m2Stream struct {
	chunks []gateway.Chunk
	dest   gateway.Destination
}

func (s *m2Stream) Next(ctx context.Context) (gateway.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return gateway.Chunk{}, err
	}
	if len(s.chunks) == 0 {
		return gateway.Chunk{}, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func (s *m2Stream) Destination() gateway.Destination { return s.dest }
func (s *m2Stream) Close() error                     { return nil }
