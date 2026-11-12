package app

// Tests for the optional external MCP tool proxy composition.
//
// The named behavioral tests are:
//
//   - TestToolsDisabledIsInert
//   - TestToolCompositionRefusesAmbiguousConfiguration
//   - TestToolCompositionPreservesUncertainOutcome
//
// TestConfiguredToolThroughRESTAndMCP lives in the composition integration
// suite (test/integration/composition/tools_test.go) and
// TestToolCompositionUsesCurrentAuthorityAndConsumerPolicy lives in
// tools_authority_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	toolTestTenant   = id.TenantID("tenant-1")
	toolTestProject  = id.ProjectID("project-1")
	toolTestOther    = id.ProjectID("project-2")
	toolTestSecret   = "tool-fixture-secret-3f9a1c"
	toolTestUpstream = "docs.search"
)

func toolTestScope(project id.ProjectID) tenancy.Scope {
	return tenancy.Scope{TenantID: toolTestTenant, ProjectID: project}
}

// permissiveToolValidator is the explicit configured argument validator used by
// the fixtures. A configured tool without one must be refused at construction.
func permissiveToolValidator(json.RawMessage) error { return nil }

// toolUpstream is a deterministic local MCP upstream. It records every received
// tools/call and can answer with a settled result or a failing status.
type toolUpstream struct {
	server *httptest.Server
	status int
	body   []byte

	mu    sync.Mutex
	calls []toolUpstreamCall
}

type toolUpstreamCall struct {
	Header    http.Header
	Method    string
	Name      string
	Arguments json.RawMessage
}

func newToolUpstream(t *testing.T, status int, body string) *toolUpstream {
	t.Helper()
	upstream := &toolUpstream{status: status, body: []byte(body)}
	upstream.server = httptest.NewServer(http.HandlerFunc(upstream.handle))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (upstream *toolUpstream) url() string { return upstream.server.URL }

func (upstream *toolUpstream) handle(response http.ResponseWriter, request *http.Request) {
	data, _ := io.ReadAll(request.Body)
	var decoded struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(data, &decoded)

	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, toolUpstreamCall{
		Header:    request.Header.Clone(),
		Method:    decoded.Method,
		Name:      decoded.Params.Name,
		Arguments: append(json.RawMessage(nil), decoded.Params.Arguments...),
	})
	upstream.mu.Unlock()

	if upstream.status != 0 {
		response.WriteHeader(upstream.status)
		_, _ = response.Write(upstream.body)
		return
	}
	response.Header().Set("Content-Type", api.MediaType)
	_, _ = fmt.Fprintf(response,
		`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"settled"}],"isError":false}}`,
		string(decoded.ID))
}

func (upstream *toolUpstream) received() []toolUpstreamCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]toolUpstreamCall(nil), upstream.calls...)
}

// toolProxyFixture is one valid enabled configuration over local upstreams.
func toolProxyFixture(scopes ...tenancy.Scope) *ToolProxyConfig {
	projects := make([]ProjectToolProxy, 0, len(scopes))
	for _, scope := range scopes {
		projects = append(projects, ProjectToolProxy{
			Scope: scope,
			Servers: []ToolServer{{
				Name:     "search",
				Endpoint: "https://tools.example.com/mcp",
				Tools: []ToolDefinition{{
					Alias:    "search-docs",
					Name:     toolTestUpstream,
					Validate: permissiveToolValidator,
				}},
			}},
		})
	}
	return &ToolProxyConfig{Projects: projects}
}

// toolServerFixture is one upstream server declaration with an explicit alias.
func toolServerFixture(name string, endpoint string, alias string, upstreamTool string) ToolServer {
	return ToolServer{
		Name:     name,
		Endpoint: endpoint,
		Tools: []ToolDefinition{{
			Alias:    alias,
			Name:     upstreamTool,
			Validate: permissiveToolValidator,
		}},
	}
}

// startToolApp constructs and starts one composition, registering cleanup.
func startToolApp(t *testing.T, config Config) *App {
	t.Helper()
	composed, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := composed.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := composed.Close(closeCtx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return composed
}

// toolAppConfig builds a fully injected test-profile composition and rebuilds
// the static authenticator after a test appends its own dev tokens.
func toolAppConfig(t *testing.T, update func(*Config)) Config {
	t.Helper()
	config := fullTestProfileConfig(t)
	if update != nil {
		update(&config)
	}
	authenticator, err := NewDevAuthenticator(config.Adapters.Clock, config.Dev.Tokens)
	if err != nil {
		t.Fatalf("NewDevAuthenticator: %v", err)
	}
	config.Adapters.Principal = authenticator
	return config
}

// engineerToken is the project-scoped human holding the engineer role snapshot.
const engineerToken = "dev-project-token"

// callTool performs one REST tool.invoke and returns the raw recorder, result
// and public error.
func callTool(t *testing.T, handler http.Handler, token, project, alias string, arguments string) (*httptest.ResponseRecorder, api.Result, api.Error) {
	t.Helper()
	return restCall(t, handler, http.MethodPost,
		"/api/v1/projects/"+project+"/tools/"+alias+"/invoke",
		token,
		api.ToolInvokeRequest{ProjectID: project, Tool: alias, Arguments: json.RawMessage(arguments)},
		nil, "")
}

// mcpCallTool performs one MCP tools/call of tool.invoke and returns the raw
// result envelope and the public error.
func mcpCallTool(t *testing.T, handler http.Handler, token, project, alias string, arguments string) (api.Result, api.Error) {
	t.Helper()
	input, err := json.Marshal(api.ToolInvokeRequest{
		ProjectID: project,
		Tool:      alias,
		Arguments: json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("marshal tool.invoke input: %v", err)
	}
	envelope, err := json.Marshal(api.Request{Input: input})
	if err != nil {
		t.Fatalf("marshal request envelope: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": api.MCPJSONRPC,
		"id":      1,
		"method":  api.MCPMethodToolsCall,
		"params":  api.MCPToolsCallParams{Name: "tool.invoke", Arguments: envelope},
	})
	if err != nil {
		t.Fatalf("marshal MCP request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, DefaultMCPPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("MCP tool.invoke status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.MCPResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Error != nil {
		var publicError api.Error
		if err := json.Unmarshal(response.Error.Data, &publicError); err != nil {
			t.Fatalf("decode MCP error data: %v; data=%s", err, string(response.Error.Data))
		}
		return api.Result{}, publicError
	}
	var result api.Result
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode MCP result: %v; result=%s", err, string(response.Result))
	}
	return result, api.Error{}
}

// TestToolsDisabledIsInert pins the default composition: without explicit tool
// configuration, the optional surface keeps the not_found refusal, the
// composition constructs, and the disabled port is returned without any
// dependency or transport construction at all.
//
// Mutation reasoning: unconditionally building the concrete mcphttp transport
// with an empty server set fails construction, so app.New would fail; an
// accidental enablement would return a settled tool result instead of
// not_found; constructing dependencies before the enabled check would panic on
// the empty dependency bundle below.
func TestToolsDisabledIsInert(t *testing.T) {
	tools, err := newConfiguredTools(toolProxyDependencies{})
	if err != nil {
		t.Fatalf("disabled tool construction: %v", err)
	}
	if _, ok := tools.(disabledTools); !ok {
		t.Fatalf("disabled tool port = %T; want disabledTools with no upstream dependency", tools)
	}

	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	recorder, _, publicError := callTool(t, app.Handler(), engineerToken, string(toolTestProject), "search", `{}`)
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the disabled MCP tool proxy succeeded")
	}
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("disabled tool refusal = %q; want %q", publicError.Code, api.CodeNotFound)
	}
}

// TestToolCompositionRequiresExactPublicAlias pins that the public alias is
// resolved by exact project-namespace lookup only. A case variant, a prefix of
// a configured alias or an alias with surrounding whitespace must refuse with
// not_found and must never reach an upstream or commit a usage row. The exact
// alias still resolves and dispatches its configured upstream tool.
//
// Mutation reasoning: a case-insensitive fallback (strings.EqualFold) resolves
// "Docs-Read"/"DOCS-READ"; a prefix fallback (strings.HasPrefix) resolves
// "docs"/"other"; a trimming fallback resolves " docs-read ". Every survivor
// reaches the upstream and commits a usage row, so each assertion below fails.
func TestToolCompositionRequiresExactPublicAlias(t *testing.T) {
	upstream := newToolUpstream(t, 0, "")
	config := &ToolProxyConfig{Projects: []ProjectToolProxy{{
		Scope: toolTestScope(toolTestProject),
		Servers: []ToolServer{
			toolServerFixture("one", upstream.url(), "docs-read", "docs.read"),
			toolServerFixture("two", upstream.url(), "other-write", "docs.write"),
		},
	}}}
	clock := newFakeClock()
	principal := toolPrincipal("admin-1", tenancy.Scope{TenantID: toolTestTenant}, tenancy.RoleEngineer)
	usage := memory.NewWithClock(clock.Now)
	tools, err := newConfiguredTools(toolProxyDependencies{
		Config:         config,
		InstallationID: "installation-1",
		Controller:     &toolTestController{},
		Credentials:    mustToolBinding(t, principal, clock),
		Authorizer:     authorize.NewMatrix(),
		Authority:      allowAllToolAuthority{},
		Store:          usage,
		IDs:            &fixedToolIDs{},
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("newConfiguredTools: %v", err)
	}

	for _, alias := range []string{
		"Docs-Read", "DOCS-READ", // case variants
		"docs", "other", // prefixes of the configured aliases
		" docs-read", "docs-read ", "docs-read\n", // surrounding whitespace
	} {
		before := len(upstream.received())
		_, callErr := tools.Call(context.Background(), principal, toolTestProject, alias, json.RawMessage(`{}`))
		if callErr == nil {
			t.Fatalf("alias %q resolved; the public alias must match exactly", alias)
		}
		if class, ok := fault.ClassOf(callErr); !ok || class != fault.NotFound {
			t.Fatalf("alias %q refusal class = %v (ok=%v); want not_found", alias, class, ok)
		}
		if after := len(upstream.received()); after != before {
			t.Fatalf("alias %q reached the upstream %d times; want 0", alias, after-before)
		}
		if audits := toolAudits(t, usage, toolTestScope(toolTestProject)); len(audits) != 0 {
			t.Fatalf("alias %q committed %d usage rows; want 0", alias, len(audits))
		}
	}

	// Control: the exact public alias still resolves and dispatches its exact
	// configured upstream tool.
	if _, err := tools.Call(context.Background(), principal, toolTestProject, "docs-read", json.RawMessage(`{"query":"needle"}`)); err != nil {
		t.Fatalf("exact configured alias: %v", err)
	}
	calls := upstream.received()
	if len(calls) != 1 {
		t.Fatalf("exact alias upstream calls = %d; want 1", len(calls))
	}
	if calls[0].Name != "docs.read" {
		t.Fatalf("exact alias upstream tool = %q; want the configured upstream name docs.read", calls[0].Name)
	}
	audits := toolAudits(t, usage, toolTestScope(toolTestProject))
	if len(audits) != 1 {
		t.Fatalf("exact alias usage rows = %d; want 1", len(audits))
	}
	// Raw content retention is off by default: the settled call is
	// metadata-only, so no content capture row is committed and the usage row
	// does not claim retained bytes. A Retention{RawContent:true} mutant
	// captures the redacted request/response and sets ContentRetained, failing
	// both assertions.
	if detail := decodeToolUsageDetail(t, audits[0].Detail); detail.ContentRetained {
		t.Fatal("exact alias usage row says raw content was retained; default retention must keep it off")
	}
	if records := toolContentRecords(t, usage, toolTestScope(toolTestProject)); len(records) != 0 {
		t.Fatalf("exact alias content record rows = %d; raw content retention is off by default", len(records))
	}
}

// TestToolCompositionRoutesEachAliasToItsOwnServer pins multi-server project
// composition: with two servers in one project, each public alias reaches only
// its own configured endpoint with its own configured upstream tool name and
// only its own credential header. The other server sees nothing.
//
// Mutation reasoning: routing every alias through the first configured server
// sends "beta-docs" to the alpha endpoint or refuses it, so the beta upstream
// never receives its call; taking the transport credential mapping from the
// first server refuses the beta credential mapping, so the beta call fails
// before dispatch. Both survivors fail the per-server call, header and usage
// assertions below.
func TestToolCompositionRoutesEachAliasToItsOwnServer(t *testing.T) {
	alpha := newToolUpstream(t, 0, "")
	beta := newToolUpstream(t, 0, "")
	const (
		alphaSecret = "alpha-upstream-secret-7c1a"
		betaSecret  = "beta-upstream-secret-4e8b"
	)
	config := toolAppConfig(t, func(config *Config) {
		alphaServer := toolServerFixture("alpha", alpha.url(), "alpha-docs", "alpha.search")
		alphaServer.Secrets = []id.SecretName{"ALPHA_TOKEN"}
		alphaServer.Credentials = []ToolCredential{{Secret: "ALPHA_TOKEN", Header: "Authorization", Scheme: "Bearer"}}
		betaServer := toolServerFixture("beta", beta.url(), "beta-docs", "beta.search")
		betaServer.Secrets = []id.SecretName{"BETA_TOKEN"}
		betaServer.Credentials = []ToolCredential{{Secret: "BETA_TOKEN", Header: "X-Beta-Key"}}
		config.ToolProxy = &ToolProxyConfig{
			Projects: []ProjectToolProxy{{
				Scope:   toolTestScope(toolTestProject),
				Servers: []ToolServer{alphaServer, betaServer},
			}},
			SecretRules: []secret.NameRule{{
				Scope: toolTestScope(toolTestProject),
				Names: []id.SecretName{"ALPHA_TOKEN", "BETA_TOKEN"},
			}},
		}
	})
	app := startToolApp(t, config)
	grantProjectEngineer(t, app.Handler(), "dev-admin-token")
	writeToolSecret(t, app, "ALPHA_TOKEN", alphaSecret)
	writeToolSecret(t, app, "BETA_TOKEN", betaSecret)

	// REST edge reaches the first server's alias.
	recorder, _, publicError := callTool(t, app.Handler(), engineerToken, string(toolTestProject), "alpha-docs", `{"query":"alpha"}`)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("REST alpha tool.invoke status=%d code=%q body=%s", recorder.Code, publicError.Code, recorder.Body.String())
	}
	// MCP edge reaches the second server's alias.
	mcpResult, mcpError := mcpCallTool(t, app.Handler(), engineerToken, string(toolTestProject), "beta-docs", `{"query":"beta"}`)
	if mcpError.Code != "" {
		t.Fatalf("MCP beta tool.invoke code=%q", mcpError.Code)
	}
	var betaResult api.ToolInvokeResult
	if err := json.Unmarshal(mcpResult.Body, &betaResult); err != nil {
		t.Fatalf("decode MCP beta tool result: %v; body=%s", err, string(mcpResult.Body))
	}
	if betaResult.IsError || betaResult.Content == "" {
		t.Fatalf("MCP beta tool result = %+v", betaResult)
	}

	alphaCalls := alpha.received()
	if len(alphaCalls) != 1 {
		t.Fatalf("alpha upstream calls = %d; want exactly 1", len(alphaCalls))
	}
	if alphaCalls[0].Name != "alpha.search" {
		t.Fatalf("alpha upstream tool = %q; want the configured alpha tool alpha.search", alphaCalls[0].Name)
	}
	if got := alphaCalls[0].Header.Get("Authorization"); got != "Bearer "+alphaSecret {
		t.Fatalf("alpha upstream authorization = %q; want only the alpha credential mapping", got)
	}
	if got := alphaCalls[0].Header.Get("X-Beta-Key"); got != "" {
		t.Fatalf("alpha upstream received the beta credential header %q", got)
	}

	betaCalls := beta.received()
	if len(betaCalls) != 1 {
		t.Fatalf("beta upstream calls = %d; want exactly 1", len(betaCalls))
	}
	if betaCalls[0].Name != "beta.search" {
		t.Fatalf("beta upstream tool = %q; want the configured beta tool beta.search", betaCalls[0].Name)
	}
	if got := betaCalls[0].Header.Get("X-Beta-Key"); got != betaSecret {
		t.Fatalf("beta upstream credential header = %q; want only the beta credential mapping", got)
	}
	if got := betaCalls[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("beta upstream received the alpha credential header %q", got)
	}

	// One usage row per edge, each attributed to the authenticated caller and
	// to its own configured server/tool.
	audits := toolAudits(t, config.Adapters.Store, toolTestScope(toolTestProject))
	if len(audits) != 2 {
		t.Fatalf("tool usage rows = %d; want 2", len(audits))
	}
	byServer := make(map[string]toolUsageDetail, len(audits))
	for _, audit := range audits {
		if audit.Actor != "admin-1" {
			t.Fatalf("tool usage actor = %q; want the authenticated caller admin-1", audit.Actor)
		}
		detail := decodeToolUsageDetail(t, audit.Detail)
		byServer[detail.Server] = detail
	}
	if detail, ok := byServer["alpha"]; !ok || detail.Tool != "alpha.search" || detail.Outcome != "ok" {
		t.Fatalf("alpha usage detail = %+v (present=%v); want alpha/alpha.search/ok", detail, ok)
	}
	if detail, ok := byServer["beta"]; !ok || detail.Tool != "beta.search" || detail.Outcome != "ok" {
		t.Fatalf("beta usage detail = %+v (present=%v); want beta/beta.search/ok", detail, ok)
	}

	// Raw content retention is off by default, so both settled success calls
	// commit no content capture row and never mark their usage row retained.
	// The mutant Retention{RawContent:true} persists the redacted request and
	// response bytes of each success and sets ContentRetained, failing both
	// assertions below.
	if records := toolContentRecords(t, config.Adapters.Scanner, toolTestScope(toolTestProject)); len(records) != 0 {
		t.Fatalf("content record rows on success = %d; raw content retention is off by default", len(records))
	}
	for _, audit := range audits {
		if detail := decodeToolUsageDetail(t, audit.Detail); detail.ContentRetained {
			t.Fatalf("usage row %s/%s says raw content was retained; default retention must keep it off", detail.Server, detail.Tool)
		}
	}
}

// TestToolCompositionRefusesAmbiguousConfiguration pins the construction-time
// refusal of incomplete or ambiguous enabled configuration. Every case is a
// separate trusted-configuration defect; none may be silently resolved by
// last-entry-wins, dropping a credential mapping or defaulting a validator.
func TestToolCompositionRefusesAmbiguousConfiguration(t *testing.T) {
	valid := func() *ToolProxyConfig {
		return toolProxyFixture(toolTestScope(toolTestProject))
	}
	tests := []struct {
		name   string
		mutate func(*ToolProxyConfig)
		needle string
	}{
		{
			name: "duplicate public alias in one project",
			mutate: func(config *ToolProxyConfig) {
				config.Projects[0].Servers[0].Tools = append(config.Projects[0].Servers[0].Tools, ToolDefinition{
					Alias:    "search-docs",
					Name:     "docs.other",
					Validate: permissiveToolValidator,
				})
			},
			needle: "alias",
		},
		{
			name: "missing argument validator",
			mutate: func(config *ToolProxyConfig) {
				config.Projects[0].Servers[0].Tools[0].Validate = nil
			},
			needle: "validator",
		},
		{
			name: "declared secret without a credential mapping",
			mutate: func(config *ToolProxyConfig) {
				server := &config.Projects[0].Servers[0]
				server.Secrets = []id.SecretName{"TOOL_TOKEN"}
			},
			needle: "credential",
		},
		{
			name: "credential mapping drops one declared secret",
			mutate: func(config *ToolProxyConfig) {
				server := &config.Projects[0].Servers[0]
				server.Secrets = []id.SecretName{"TOOL_TOKEN", "TOOL_SECOND"}
				server.Credentials = []ToolCredential{{Secret: "TOOL_TOKEN", Header: "Authorization", Scheme: "Bearer"}}
			},
			needle: "credential",
		},
		{
			name: "credential mapping names an undeclared secret",
			mutate: func(config *ToolProxyConfig) {
				server := &config.Projects[0].Servers[0]
				server.Secrets = []id.SecretName{"TOOL_TOKEN"}
				server.Credentials = []ToolCredential{
					{Secret: "TOOL_TOKEN", Header: "Authorization", Scheme: "Bearer"},
					{Secret: "TOOL_EXTRA", Header: "X-Extra"},
				}
			},
			needle: "credential",
		},
		{
			name: "duplicate project scope",
			mutate: func(config *ToolProxyConfig) {
				config.Projects = append(config.Projects, ProjectToolProxy{
					Scope:   toolTestScope(toolTestProject),
					Servers: toolProxyFixture(toolTestScope(toolTestProject)).Projects[0].Servers,
				})
			},
			needle: "project",
		},
		{
			name: "enabled configuration without projects",
			mutate: func(config *ToolProxyConfig) {
				config.Projects = nil
			},
			needle: "project",
		},
		{
			name: "tenant-scoped project selection",
			mutate: func(config *ToolProxyConfig) {
				config.Projects[0].Scope = tenancy.Scope{TenantID: toolTestTenant}
			},
			needle: "project",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := newTestEnvironment(t).config.resolved()
			tools := valid()
			test.mutate(tools)
			config.ToolProxy = tools
			err := config.validate()
			if err == nil {
				t.Fatalf("%s was accepted; want a construction refusal", test.name)
			}
			if test.needle != "" && !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("%s refusal = %v; want %q named", test.name, err, test.needle)
			}

			// The same defect must fail the enabled constructor itself, not
			// only the pre-adapter configuration validation.
			if _, err := newConfiguredTools(toolProxyDependencies{Config: tools}); err == nil {
				t.Fatalf("%s was accepted by newConfiguredTools", test.name)
			}
		})
	}
}

// TestToolCompositionPreservesUncertainOutcome pins the ambiguous upstream
// outcome: exactly one attempt, the uncertain public class (never a
// replay-safe pre-effect class), one metadata-only usage row and no credential
// or raw-content disclosure. Raw content retention stays off by default.
func TestToolCompositionPreservesUncertainOutcome(t *testing.T) {
	upstream := newToolUpstream(t, http.StatusInternalServerError, `{"marker":"raw-upstream-body-5c2d"}`)

	var logs bytes.Buffer
	config := toolAppConfig(t, func(config *Config) {
		config.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		server := toolServerFixture("search", upstream.url(), "search-docs", toolTestUpstream)
		server.Secrets = []id.SecretName{"TOOL_TOKEN"}
		server.Credentials = []ToolCredential{{Secret: "TOOL_TOKEN", Header: "Authorization", Scheme: "Bearer"}}
		config.ToolProxy = &ToolProxyConfig{
			Projects: []ProjectToolProxy{{
				Scope:   toolTestScope(toolTestProject),
				Servers: []ToolServer{server},
			}},
			SecretRules: []secret.NameRule{{
				Scope: toolTestScope(toolTestProject),
				Names: []id.SecretName{"TOOL_TOKEN"},
			}},
		}
	})
	app := startToolApp(t, config)
	grantProjectEngineer(t, app.Handler(), "dev-admin-token")
	writeToolSecret(t, app, "TOOL_TOKEN", toolTestSecret)

	recorder, _, publicError := callTool(t, app.Handler(), engineerToken, string(toolTestProject), "search-docs", `{"query":"needle"}`)
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatalf("the ambiguous upstream outcome succeeded: %s", recorder.Body.String())
	}
	if publicError.Code != api.CodeUncertainExternalEffect {
		t.Fatalf("ambiguous outcome class = %q; want %q (a replay-safe class would allow a retry)",
			publicError.Code, api.CodeUncertainExternalEffect)
	}
	if calls := upstream.received(); len(calls) != 1 {
		t.Fatalf("upstream attempts = %d; want exactly 1", len(calls))
	} else if got := calls[0].Header.Get("Authorization"); got != "Bearer "+toolTestSecret {
		t.Fatalf("upstream authorization = %q; want the configured credential mapping", got)
	}

	body := recorder.Body.String()
	if strings.Contains(body, toolTestSecret) {
		t.Fatalf("the refusal rendered the upstream credential: %s", body)
	}
	if strings.Contains(body, "raw-upstream-body-5c2d") {
		t.Fatalf("the refusal rendered the raw upstream body: %s", body)
	}
	if strings.Contains(logs.String(), toolTestSecret) {
		t.Fatalf("the composition log rendered the upstream credential: %s", logs.String())
	}

	audits := toolAudits(t, config.Adapters.Store, toolTestScope(toolTestProject))
	if len(audits) != 1 {
		t.Fatalf("tool usage rows = %d; want exactly 1", len(audits))
	}
	detail := decodeToolUsageDetail(t, audits[0].Detail)
	if detail.Outcome != "uncertain_external_effect" {
		t.Fatalf("usage outcome = %q; want uncertain_external_effect", detail.Outcome)
	}
	if detail.Server != "search" || detail.Tool != toolTestUpstream {
		t.Fatalf("usage attribution = %s/%s; want search/%s", detail.Server, detail.Tool, toolTestUpstream)
	}
	if detail.ContentRetained {
		t.Fatal("usage row says raw content was retained; default retention must keep it off")
	}
	if strings.Contains(audits[0].Detail, toolTestSecret) {
		t.Fatalf("usage detail rendered the upstream credential: %s", audits[0].Detail)
	}
	if records := toolContentRecords(t, config.Adapters.Scanner, toolTestScope(toolTestProject)); len(records) != 0 {
		t.Fatalf("content record rows = %d; raw content retention is off by default", len(records))
	}
}

// writeToolSecret writes one encrypted secret through the public REST edge.
func writeToolSecret(t *testing.T, app *App, name, value string) {
	t.Helper()
	recorder, _, publicError := restCall(t, app.Handler(), http.MethodPut,
		"/api/v1/projects/"+string(toolTestProject)+"/secrets/"+name, "dev-project-token",
		api.SecretWriteRequest{ProjectID: string(toolTestProject), SecretID: name, Value: value},
		nil, "tool-secret-"+name)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("secret.write status=%d code=%q", recorder.Code, publicError.Code)
	}
}

// toolUsageDetail mirrors the bounded mcpproxy usage detail.
type toolUsageDetail struct {
	Server          string `json:"server"`
	Tool            string `json:"tool"`
	Outcome         string `json:"outcome"`
	RequestBytes    int    `json:"request_bytes"`
	ResponseBytes   int    `json:"response_bytes"`
	DurationMS      int64  `json:"duration_ms"`
	ContentRetained bool   `json:"content_retained"`
}

func decodeToolUsageDetail(t *testing.T, detail string) toolUsageDetail {
	t.Helper()
	var decoded toolUsageDetail
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("decode usage detail %q: %v", detail, err)
	}
	return decoded
}

// toolAudits returns the committed tool.invoke audit rows of one scope. The
// composition's in-process store is the memory adapter, which exposes its
// committed audit rows for inspection.
func toolAudits(t *testing.T, executor store.Executor, scope tenancy.Scope) []event.Audit {
	t.Helper()
	memoryStore, ok := executor.(*memory.Store)
	if !ok {
		t.Fatalf("tool audit inspection requires the memory store; got %T", executor)
	}
	filtered := make([]event.Audit, 0)
	for _, row := range memoryStore.Audit(scope) {
		if row.Operation == "tool.invoke" {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

// toolContentRecords returns the committed raw-content rows of one scope.
func toolContentRecords(t *testing.T, scanner store.Scanner, scope tenancy.Scope) []store.Row {
	t.Helper()
	page, err := scanner.Query(context.Background(), scope, store.ScanQuery{
		Table: store.ContentRecordsTable,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("scan content_records: %v", err)
	}
	return page.Rows
}

// toolPrincipal returns one valid human principal fixture.
func toolPrincipal(idValue string, scope tenancy.Scope, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID(idValue),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: 1,
	}
}
