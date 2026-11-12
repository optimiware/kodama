package app

// Current-authority and per-consumer policy tests for the optional external MCP
// tool proxy composition.
//
// TestToolCompositionUsesCurrentAuthorityAndConsumerPolicy is the named
// behavioral test. It combines the real composed HTTP edge with direct
// construction of the same proxy dependencies so a bypass that the public
// router would also refuse is still caught at the proxy boundary.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestToolCompositionUsesCurrentAuthorityAndConsumerPolicy covers the four
// boundaries the composed proxy must re-read at call time.
func TestToolCompositionUsesCurrentAuthorityAndConsumerPolicy(t *testing.T) {
	t.Run("another project is refused before dispatch", func(t *testing.T) {
		upstreamOne := newToolUpstream(t, 0, "")
		upstreamTwo := newToolUpstream(t, 0, "")
		extra := DevToken{
			Credential: "dev-other-token",
			Principal: identity.Principal{
				ID:      "admin-2",
				Kind:    identity.KindHuman,
				Scope:   toolTestScope(toolTestOther),
				Roles:   []tenancy.Role{tenancy.RoleEngineer},
				Enabled: true,
				Version: 1,
			},
			ExpiresAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		}
		config := toolAppConfig(t, func(config *Config) {
			config.Dev.Tokens = append(config.Dev.Tokens, extra)
			config.ToolProxy = &ToolProxyConfig{Projects: []ProjectToolProxy{
				{
					Scope: toolTestScope(toolTestProject),
					Servers: []ToolServer{
						toolServerFixture("one", upstreamOne.url(), "search-docs", toolTestUpstream),
					},
				},
				{
					Scope: toolTestScope(toolTestOther),
					Servers: []ToolServer{
						toolServerFixture("two", upstreamTwo.url(), "other-docs", "other.search"),
					},
				},
			}}
		})
		app := startToolApp(t, config)

		// A project-scoped principal naming another project is uniformly
		// not_found at the shared scope resolver, before any handler.
		recorder, _, publicError := callTool(t, app.Handler(), engineerToken, string(toolTestOther), "other-docs", `{}`)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("a project-scoped principal reached another project's configured tool")
		}
		if publicError.Code != api.CodeNotFound {
			t.Fatalf("foreign project refusal = %q; want %q", publicError.Code, api.CodeNotFound)
		}
		if calls := upstreamTwo.received(); len(calls) != 0 {
			t.Fatalf("foreign project upstream calls = %d; want 0", len(calls))
		}
	})

	t.Run("revoked membership is refused at the call boundary", func(t *testing.T) {
		upstream := newToolUpstream(t, 0, "")
		revoked := DevToken{
			Credential: "dev-revoked-token",
			Principal: identity.Principal{
				ID:      "outsider-1",
				Kind:    identity.KindHuman,
				Scope:   toolTestScope(toolTestProject),
				Roles:   []tenancy.Role{tenancy.RoleEngineer},
				Enabled: true,
				Version: 1,
			},
			ExpiresAt: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
		}
		config := toolAppConfig(t, func(config *Config) {
			config.Dev.Tokens = append(config.Dev.Tokens, revoked)
			config.ToolProxy = &ToolProxyConfig{Projects: []ProjectToolProxy{{
				Scope:   toolTestScope(toolTestProject),
				Servers: []ToolServer{toolServerFixture("search", upstream.url(), "search-docs", toolTestUpstream)},
			}}}
		})
		app := startToolApp(t, config)

		// The credential's role snapshot would pass the shared edge matrix, but
		// there is no committed membership. The proxy must re-read the current
		// membership fact and refuse before the upstream effect.
		recorder, _, publicError := callTool(t, app.Handler(), "dev-revoked-token", string(toolTestProject), "search-docs", `{}`)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("a principal without a current membership invoked the configured tool")
		}
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("revoked membership refusal = %q; want %q", publicError.Code, api.CodeForbidden)
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("revoked membership upstream calls = %d; want 0", len(calls))
		}
	})

	t.Run("workload secret rules do not authorize the MCP consumer", func(t *testing.T) {
		upstream := newToolUpstream(t, 0, "")
		config := toolAppConfig(t, func(config *Config) {
			// The workload allow-list names the credential. The MCP allow-list
			// is deliberately empty: reusing the workload policy would resolve
			// the name; the MCP policy must refuse it.
			config.SecretRules = []secret.NameRule{{
				Scope: toolTestScope(toolTestProject),
				Names: []id.SecretName{"TOOL_TOKEN"},
			}}
			server := toolServerFixture("search", upstream.url(), "search-docs", toolTestUpstream)
			server.Secrets = []id.SecretName{"TOOL_TOKEN"}
			server.Credentials = []ToolCredential{{Secret: "TOOL_TOKEN", Header: "Authorization", Scheme: "Bearer"}}
			config.ToolProxy = &ToolProxyConfig{Projects: []ProjectToolProxy{{
				Scope:   toolTestScope(toolTestProject),
				Servers: []ToolServer{server},
			}}}
		})
		app := startToolApp(t, config)
		grantProjectEngineer(t, app.Handler(), "dev-admin-token")

		recorder, _, publicError := callTool(t, app.Handler(), engineerToken, string(toolTestProject), "search-docs", `{}`)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the workload secret rule authorized the MCP consumer")
		}
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("consumer policy refusal = %q; want %q (a workload-policy reuse would resolve the name)",
				publicError.Code, api.CodeForbidden)
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("unauthorized secret upstream calls = %d; want 0", len(calls))
		}
	})

	t.Run("project selection is never widened", func(t *testing.T) {
		upstreamOne := newToolUpstream(t, 0, "")
		upstreamTwo := newToolUpstream(t, 0, "")
		config := &ToolProxyConfig{Projects: []ProjectToolProxy{
			{
				Scope:   toolTestScope(toolTestProject),
				Servers: []ToolServer{toolServerFixture("one", upstreamOne.url(), "search-docs", toolTestUpstream)},
			},
			{
				Scope:   toolTestScope(toolTestOther),
				Servers: []ToolServer{toolServerFixture("two", upstreamTwo.url(), "other-docs", "other.search")},
			},
		}}
		clock := newFakeClock()
		tenantWide := toolPrincipal("admin-1", tenancy.Scope{TenantID: toolTestTenant}, tenancy.RoleEngineer)
		tools, err := newConfiguredTools(toolProxyDependencies{
			Config:         config,
			InstallationID: "installation-1",
			Controller:     &toolTestController{execution: executionFactRow("unused", "admin-1", "running")},
			Credentials:    mustToolBinding(t, tenantWide, clock),
			Authorizer:     authorize.NewMatrix(),
			Authority:      allowAllToolAuthority{},
			Store:          &recordingToolExecutor{},
			IDs:            &fixedToolIDs{},
			Clock:          clock,
		})
		if err != nil {
			t.Fatalf("newConfiguredTools: %v", err)
		}

		// A tenant-wide principal selecting project-2 with project-1's alias
		// must not fall back to its own (empty) project or to project-1's
		// proxy.
		if _, err := tools.Call(context.Background(), tenantWide, toolTestOther, "search-docs", json.RawMessage(`{}`)); err == nil {
			t.Fatal("another project's public alias reached an upstream")
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("foreign project alias class = %v; want not_found", class)
		}
		if calls := upstreamOne.received(); len(calls) != 0 {
			t.Fatalf("widened project selection reached project-1's upstream %d times", len(calls))
		}
		if calls := upstreamTwo.received(); len(calls) != 0 {
			t.Fatalf("foreign alias reached project-2's upstream %d times", len(calls))
		}

		// The projects' own aliases still resolve, so the refusal above is a
		// real selection rather than a disabled port.
		if _, err := tools.Call(context.Background(), tenantWide, toolTestProject, "search-docs", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("project-1's configured alias: %v", err)
		}
		if _, err := tools.Call(context.Background(), tenantWide, toolTestOther, "other-docs", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("project-2's configured alias: %v", err)
		}
		if calls := upstreamTwo.received(); len(calls) != 1 {
			t.Fatalf("project-2 upstream calls = %d; want 1", len(calls))
		}
		if calls := upstreamOne.received(); len(calls) != 1 {
			t.Fatalf("project-1 upstream calls = %d; want 1 after its own alias call", len(calls))
		}
	})

	t.Run("an unconfigured project and a foreign tenant are never selected", func(t *testing.T) {
		upstream := newToolUpstream(t, 0, "")
		config := &ToolProxyConfig{Projects: []ProjectToolProxy{{
			Scope:   toolTestScope(toolTestProject),
			Servers: []ToolServer{toolServerFixture("search", upstream.url(), "search-docs", toolTestUpstream)},
		}}}
		clock := newFakeClock()
		tenantWide := toolPrincipal("admin-1", tenancy.Scope{TenantID: toolTestTenant}, tenancy.RoleEngineer)
		foreignTenant := toolPrincipal("admin-2", tenancy.Scope{TenantID: "tenant-2", ProjectID: toolTestProject}, tenancy.RoleEngineer)
		usage := memory.NewWithClock(clock.Now)
		tools, err := newConfiguredTools(toolProxyDependencies{
			Config:         config,
			InstallationID: "installation-1",
			Controller:     &toolTestController{},
			Credentials:    mustToolBinding(t, tenantWide, clock),
			Authorizer:     authorize.NewMatrix(),
			Authority:      allowAllToolAuthority{},
			Store:          usage,
			IDs:            &fixedToolIDs{},
			Clock:          clock,
		})
		if err != nil {
			t.Fatalf("newConfiguredTools: %v", err)
		}

		// A tenant-wide principal naming an unconfigured project must not fall
		// back to the single configured project: the selection key is absent, so
		// the call is not_found before any dispatch or usage commit.
		_, callErr := tools.Call(context.Background(), tenantWide, toolTestOther, "search-docs", json.RawMessage(`{}`))
		if callErr == nil {
			t.Fatal("an unconfigured project fell back to the configured project")
		}
		if class, ok := fault.ClassOf(callErr); !ok || class != fault.NotFound {
			t.Fatalf("unconfigured project refusal class = %v (ok=%v); want not_found", class, ok)
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("unconfigured project upstream calls = %d; want 0", len(calls))
		}
		if audits := toolAudits(t, usage, toolTestScope(toolTestProject)); len(audits) != 0 {
			t.Fatalf("unconfigured project committed %d usage rows to the configured project; want 0", len(audits))
		}

		// The project id alone is not the selection key: a principal from another
		// tenant naming the same configured project id must be refused at
		// selection, before the proxy or any upstream is reached.
		_, callErr = tools.Call(context.Background(), foreignTenant, toolTestProject, "search-docs", json.RawMessage(`{}`))
		if callErr == nil {
			t.Fatal("a foreign tenant reached the configured project")
		}
		if class, ok := fault.ClassOf(callErr); !ok || class != fault.NotFound {
			t.Fatalf("foreign tenant refusal class = %v (ok=%v); want not_found", class, ok)
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("foreign tenant upstream calls = %d; want 0", len(calls))
		}
		if audits := toolAudits(t, usage, toolTestScope(toolTestProject)); len(audits) != 0 {
			t.Fatalf("foreign tenant committed %d usage rows to the configured project; want 0", len(audits))
		}

		// Control: the configured project still resolves for its own tenant, so
		// the two refusals above are a real selection rather than a dead port.
		if _, err := tools.Call(context.Background(), tenantWide, toolTestProject, "search-docs", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("configured project's own alias: %v", err)
		}
		if calls := upstream.received(); len(calls) != 1 {
			t.Fatalf("configured project upstream calls = %d; want 1 after its own alias call", len(calls))
		}
		if audits := toolAudits(t, usage, toolTestScope(toolTestProject)); len(audits) != 1 {
			t.Fatalf("configured project usage rows = %d; want 1", len(audits))
		}
	})

	t.Run("execution current facts gate the proxy itself", func(t *testing.T) {
		upstream := newToolUpstream(t, 0, "")
		config := toolProxyFixture(toolTestScope(toolTestProject))
		config.Projects[0].Servers[0].Endpoint = upstream.url()
		clock := newFakeClock()

		// The direct construction uses the same dependency shape as wireApp:
		// a real platform execution reader whose permission source grants only
		// ExecConfiguredTools, the shared authority reader seam and the
		// request-local binding authenticator.
		principal := identity.Principal{
			ID:          "exec-1",
			Kind:        identity.KindExecution,
			Scope:       toolTestScope(toolTestProject),
			Enabled:     true,
			Version:     1,
			ExecutionID: "execution-tools-1",
		}

		cases := []struct {
			name        string
			state       string
			actionKind  string
			actionState string
			deadline    time.Time
			allowed     bool
		}{
			{
				name: "running container action", state: "running",
				actionKind: "container", actionState: "running", deadline: clock.Now().Add(time.Hour),
				allowed: true,
			},
			{
				name: "waiting request action", state: "running",
				actionKind: "request", actionState: "running", deadline: clock.Now().Add(time.Hour),
				allowed: false,
			},
			{
				name: "terminal execution", state: "failed",
				allowed: false,
			},
		}

		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				before := len(upstream.received())
				controller := &toolTestController{
					execution: executionFactRow("execution-tools-1", string(principal.ID), test.state),
				}
				if test.actionKind != "" {
					controller.actions = []store.Row{
						actionFactRow("execution-tools-1", "action-tools-1", test.actionKind, test.actionState, test.deadline),
					}
				}
				tools, err := newConfiguredTools(toolProxyDependencies{
					Config:         config,
					InstallationID: "installation-1",
					Controller:     controller,
					Credentials:    mustToolBinding(t, principal, clock),
					Authorizer:     authorize.NewMatrix(),
					Authority:      allowAllToolAuthority{},
					Store:          &recordingToolExecutor{},
					IDs:            &fixedToolIDs{},
					Clock:          clock,
				})
				if err != nil {
					t.Fatalf("newConfiguredTools: %v", err)
				}
				result, callErr := tools.Call(context.Background(), principal, toolTestProject, "search-docs", json.RawMessage(`{"query":"needle"}`))
				after := len(upstream.received())
				if test.allowed {
					if callErr != nil {
						t.Fatalf("running execution tool call: %v", callErr)
					}
					if after-before != 1 {
						t.Fatalf("running execution upstream calls = %d; want 1", after-before)
					}
					if len(result.Content) == 0 {
						t.Fatal("running execution tool call returned no content")
					}
					return
				}
				if callErr == nil {
					t.Fatal("a non-running execution reached the configured tool")
				}
				class, ok := fault.ClassOf(callErr)
				if !ok || class != fault.Forbidden {
					t.Fatalf("execution refusal class = %v (ok=%v); want forbidden", class, ok)
				}
				if after-before != 0 {
					t.Fatalf("refused execution upstream calls = %d; want 0", after-before)
				}
			})
		}
	})
}

// mustToolBinding builds the request-local binding authenticator around a
// static base resolver. withPrincipal always binds the presented principal, so
// the base is never consulted by these tests.
func mustToolBinding(t *testing.T, principal identity.Principal, clock Clock) *bindingAuthenticator {
	t.Helper()
	authenticator, err := newBindingAuthenticator(staticToolPrincipals{principal: principal}, clock)
	if err != nil {
		t.Fatalf("newBindingAuthenticator: %v", err)
	}
	return authenticator
}

// staticToolPrincipals is the base credential resolver of the direct fixtures.
type staticToolPrincipals struct {
	principal identity.Principal
}

func (resolver staticToolPrincipals) Resolve(context.Context, string) (identity.Principal, error) {
	return resolver.principal, nil
}

// allowAllToolAuthority answers every requested current fact as allowed. The
// proxy's execution permission gate is the boundary under test here; the
// composed current-membership authority is covered by the full-app subtests.
type allowAllToolAuthority struct{}

func (allowAllToolAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     true,
		})
	}
	return facts, nil
}

// recordingToolExecutor records the proxy's usage commit. It is the same store
// seam the composition injects; no raw content row is expected by default.
type recordingToolExecutor struct {
	mu    sync.Mutex
	units []store.Unit
}

func (executor *recordingToolExecutor) Execute(_ context.Context, _ tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	executor.mu.Lock()
	executor.units = append(executor.units, unit)
	executor.mu.Unlock()
	return store.UnitResult{}, nil
}

func (executor *recordingToolExecutor) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, fault.New(fault.NotFound, "the fixture holds no business rows")
}

// fixedToolIDs allocates deterministic usage identities.
type fixedToolIDs struct {
	mu   sync.Mutex
	next int
}

func (ids *fixedToolIDs) NewEventID() (id.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return id.EventID(fmt.Sprintf("audit-tool-%d", ids.next)), nil
}

// toolTestController is a minimal controller seam serving one execution row and
// its actions to the real platform execution reader.
type toolTestController struct {
	execution store.Row
	actions   []store.Row
}

func (controller *toolTestController) Read(_ context.Context, table, tableID string) (store.Row, error) {
	if table == "executions" {
		return controller.execution, nil
	}
	return store.Row{}, fault.New(fault.NotFound, "the fixture holds no row")
}

func (controller *toolTestController) Scan(_ context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if scan.Table == "actions" {
		return store.QueryPage{Rows: append([]store.Row(nil), controller.actions...)}, nil
	}
	return store.QueryPage{}, nil
}

func (controller *toolTestController) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, nil
}

// executionFactRow builds the columns the platform execution reader consumes.
func executionFactRow(executionID, principalID, state string) store.Row {
	return store.Row{Table: "executions", Values: map[string]any{
		"id":           executionID,
		"tenant_id":    string(toolTestTenant),
		"project_id":   string(toolTestProject),
		"principal_id": principalID,
		"state":        state,
		"version":      int64(1),
	}}
}

// actionFactRow builds the columns the platform execution reader consumes.
func actionFactRow(executionID, actionID, kind, state string, deadline time.Time) store.Row {
	return store.Row{Table: "actions", Values: map[string]any{
		"id":           actionID,
		"execution_id": executionID,
		"kind":         kind,
		"state":        state,
		"deadline":     deadline,
	}}
}

// ensure the direct fixtures keep satisfying the ports.
var (
	_ authorize.CurrentPrincipal = staticToolPrincipals{}
	_ authorize.AuthorityReader  = allowAllToolAuthority{}
	_ store.Executor             = (*recordingToolExecutor)(nil)
	_ store.Controller           = (*toolTestController)(nil)
	_ mcpproxy.IDSource          = (*fixedToolIDs)(nil)
)
