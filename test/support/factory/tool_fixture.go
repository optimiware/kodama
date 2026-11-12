package factory

// Deterministic bounded MCP upstream fixture for the optional external tool
// proxy.
//
// The fixture is a real loopback HTTP server speaking the MCP JSON-RPC
// tools/call subset. It is not an in-process fake transport: the configured
// mcphttp driver performs a real POST, the fixture authenticates the configured
// credential mapping and answers with one bounded JSON-RPC result. Routing,
// authority, credential custody, client bounds and usage persistence stay real;
// only the upstream tool is scripted.
//
// The fixture owns exactly one configured public alias. A second upstream tool
// the fixture could answer is deliberately not configured, so a proxy call for
// it must be refused before any request reaches this server.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

const (
	// ToolAlias is the one configured public alias of the fixture upstream.
	ToolAlias = "search-docs"
	// ToolUpstreamName is the upstream MCP tool name behind ToolAlias. It is
	// never public: the configured alias is the only accepted tool.invoke name.
	ToolUpstreamName = "docs.search"
	// ToolDeniedAlias is a plausible public alias the fixture itself could
	// answer but the composition does not configure. A call for it must be
	// refused before the upstream observes a request.
	ToolDeniedAlias = "erase-history"
	// ToolDeniedUpstreamName is the upstream tool name behind ToolDeniedAlias.
	ToolDeniedUpstreamName = "docs.erase"
	// ToolSecretName is the configured MCP-consumer credential name.
	ToolSecretName id.SecretName = "TOOL_TOKEN"

	toolFixturePath             = "/mcp"
	toolFixtureServerName       = "docs"
	toolFixtureMaxRequestBytes  = 64 << 10
	toolFixtureMaxResponseBytes = 64 << 10
	// ToolAnswer is the bounded text the fixture returns for the allowed tool.
	ToolAnswer         = "bounded fixture answer"
	toolFixtureTimeout = 10 * time.Second
)

// ToolCall is one request the fixture observed. Header is a copy, so a later
// mutation by net/http cannot change the evidence.
type ToolCall struct {
	Method    string
	Path      string
	Header    http.Header
	Body      []byte
	Tool      string
	Arguments json.RawMessage
	// Authorized reports whether the configured bearer mapping was presented.
	Authorized bool
}

// ToolFixture is one real bounded loopback MCP upstream. It is safe for
// concurrent use.
type ToolFixture struct {
	t          testing.TB
	server     *httptest.Server
	endpoint   string
	credential string

	mu        sync.Mutex
	calls     []ToolCall
	refusals  []string
	closeOnce sync.Once
}

// NewToolFixture starts the bounded loopback MCP server and registers cleanup
// before the server starts, so startup cannot leak a listener.
func NewToolFixture(t testing.TB) *ToolFixture {
	t.Helper()
	fixture := &ToolFixture{t: t}
	t.Cleanup(fixture.Close)
	fixture.credential = newHarnessToken(t)
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	fixture.endpoint = fixture.server.URL + toolFixturePath
	return fixture
}

// Close stops the fixture server. It is idempotent.
func (fixture *ToolFixture) Close() {
	if fixture == nil || fixture.server == nil {
		return
	}
	fixture.closeOnce.Do(func() { fixture.server.Close() })
}

// Endpoint returns the configured upstream MCP endpoint.
func (fixture *ToolFixture) Endpoint() string { return fixture.endpoint }

// CredentialName returns the configured MCP-consumer secret name.
func (fixture *ToolFixture) CredentialName() id.SecretName { return ToolSecretName }

// Calls returns a copy of every observed request.
func (fixture *ToolFixture) Calls() []ToolCall {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	calls := make([]ToolCall, len(fixture.calls))
	copy(calls, fixture.calls)
	for index := range calls {
		calls[index].Header = calls[index].Header.Clone()
		calls[index].Body = append([]byte(nil), calls[index].Body...)
		calls[index].Arguments = append(json.RawMessage(nil), calls[index].Arguments...)
	}
	return calls
}

// CallCount returns the number of observed requests.
func (fixture *ToolFixture) CallCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return len(fixture.calls)
}

// Refusals returns the fixture-side refusal diagnostics.
func (fixture *ToolFixture) Refusals() []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]string(nil), fixture.refusals...)
}

// AssertAuthenticated requires every observed request to carry exactly the
// configured bearer mapping. The credential value is never rendered.
func (fixture *ToolFixture) AssertAuthenticated(t testing.TB) {
	t.Helper()
	calls := fixture.Calls()
	if len(calls) == 0 {
		t.Fatal("factory: tool fixture observed no authenticated request")
	}
	for index, call := range calls {
		if !call.Authorized {
			t.Fatalf("factory: tool fixture request %d did not carry the configured credential", index)
		}
		if got := call.Header.Get("Authorization"); got != "Bearer "+fixture.credential {
			t.Fatalf("factory: tool fixture request %d authorization did not match the configured mapping", index)
		}
	}
}

// AssertCredentialAbsent fails when any evidence string carries the generated
// upstream credential. The credential itself is never rendered in the failure.
func (fixture *ToolFixture) AssertCredentialAbsent(t testing.TB, evidence ...string) {
	t.Helper()
	if fixture.credential == "" {
		t.Fatal("factory: tool fixture has no upstream credential")
	}
	for index, value := range evidence {
		if strings.Contains(value, fixture.credential) {
			t.Fatalf("factory: evidence value %d contains the generated upstream credential", index)
		}
	}
}

// toolsConfig renders the real trusted composition configuration for one
// project scope. The MCP secret policy is separate from the workload policy.
func (fixture *ToolFixture) toolsConfig(scope tenancy.Scope) *app.ToolProxyConfig {
	return &app.ToolProxyConfig{
		Projects: []app.ProjectToolProxy{{
			Scope: scope,
			Servers: []app.ToolServer{{
				Name:     toolFixtureServerName,
				Endpoint: fixture.endpoint,
				Secrets:  []id.SecretName{ToolSecretName},
				Credentials: []app.ToolCredential{{
					Secret: ToolSecretName,
					Header: "Authorization",
					Scheme: "Bearer",
				}},
				Tools: []app.ToolDefinition{{
					Alias:       ToolAlias,
					Name:        ToolUpstreamName,
					Description: "bounded fixture search",
					Validate:    toolFixtureArguments,
				}},
			}},
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{ToolSecretName},
		}},
		Timeout: toolFixtureTimeout,
	}
}

// toolFixtureArguments admits a bounded JSON object. The API validates the
// request shape first; this configured validator refuses an unusable value
// before the proxy resolves a credential or dispatches.
func toolFixtureArguments(arguments json.RawMessage) error {
	if len(arguments) == 0 || len(arguments) > toolFixtureMaxRequestBytes || !json.Valid(arguments) {
		return errors.New("tool arguments are not a usable bounded JSON value")
	}
	return nil
}

// writeCredential writes the generated upstream credential through the public
// secret.write operation. The value travels only in the request body.
func (fixture *ToolFixture) writeCredential(h *Harness) {
	h.T.Helper()
	h.MustREST(h.ProjectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: ProjectID,
		SecretID:  string(ToolSecretName),
		Value:     fixture.credential,
	}, nil, "secret-"+string(ToolSecretName))
}

// assertUsable refuses a fixture that was never started.
func (fixture *ToolFixture) assertUsable(t testing.TB) {
	t.Helper()
	if fixture == nil || fixture.server == nil || fixture.endpoint == "" {
		t.Fatal("factory: tool fixture was not started")
	}
	if fixture.credential == "" {
		t.Fatal("factory: tool fixture has no upstream credential")
	}
}

// WithToolFixture enables the optional external MCP tool proxy against one
// deterministic bounded upstream. A factory without this option keeps the
// proxy-disabled composition.
func WithToolFixture(fixture *ToolFixture) Option {
	return func(options *Options) { options.ToolFixture = fixture }
}

// composeToolsConfig returns the configured tool proxy for one harness scope,
// or nil when the fixture is not selected.
func (options Options) composeToolsConfig(scope tenancy.Scope) *app.ToolProxyConfig {
	if options.ToolFixture == nil {
		return nil
	}
	return options.ToolFixture.toolsConfig(scope)
}

// toolFixtureSecretConflicts reports whether a caller-supplied secret map
// already owns the fixture credential name.
func toolFixtureSecretConflicts(secrets map[string]string) bool {
	_, conflict := secrets[string(ToolSecretName)]
	return conflict
}

// handle is the bounded MCP upstream. Every failure is fail-closed and never
// synthesizes a tool result.
func (fixture *ToolFixture) handle(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		fixture.refuse(response, http.StatusMethodNotAllowed, "unexpected method %s", request.Method)
		return
	}
	if request.URL.Path != toolFixturePath {
		fixture.refuse(response, http.StatusNotFound, "unexpected path %q", request.URL.Path)
		return
	}
	if !strings.HasPrefix(request.Header.Get("Content-Type"), api.MediaType) {
		fixture.refuse(response, http.StatusUnsupportedMediaType, "unexpected content type")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, toolFixtureMaxRequestBytes))
	if err != nil {
		fixture.refuse(response, http.StatusRequestEntityTooLarge, "request body exceeds the fixture bound")
		return
	}
	var decoded struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.JSONRPC != api.MCPJSONRPC {
		fixture.refuse(response, http.StatusBadRequest, "request is not MCP JSON-RPC")
		return
	}
	authorized := request.Header.Get("Authorization") == "Bearer "+fixture.credential
	fixture.mu.Lock()
	fixture.calls = append(fixture.calls, ToolCall{
		Method:     decoded.Method,
		Path:       request.URL.Path,
		Header:     request.Header.Clone(),
		Body:       append([]byte(nil), body...),
		Tool:       decoded.Params.Name,
		Arguments:  append(json.RawMessage(nil), decoded.Params.Arguments...),
		Authorized: authorized,
	})
	fixture.mu.Unlock()
	if !authorized {
		fixture.refuse(response, http.StatusUnauthorized, "configured credential mapping is missing")
		return
	}
	if decoded.Method != api.MCPMethodToolsCall || decoded.Params.Name != ToolUpstreamName {
		fixture.refuseEnvelope(response, decoded.ID, -32601, "tool is not exposed by this fixture")
		return
	}
	content := []byte(`[{"type":"text","text":"` + ToolAnswer + `"}]`)
	envelope, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		} `json:"result"`
	}{
		JSONRPC: api.MCPJSONRPC,
		ID:      decoded.ID,
		Result: struct {
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		}{Content: content},
	})
	if err != nil || len(envelope) > toolFixtureMaxResponseBytes {
		fixture.refuse(response, http.StatusInternalServerError, "fixture response exceeds the bound")
		return
	}
	response.Header().Set("Content-Type", api.MediaType)
	_, _ = response.Write(envelope)
}

// refuseEnvelope answers one JSON-RPC error without rendering request bytes.
func (fixture *ToolFixture) refuseEnvelope(response http.ResponseWriter, requestID json.RawMessage, code int, message string) {
	if len(requestID) == 0 {
		requestID = json.RawMessage("null")
	}
	response.Header().Set("Content-Type", api.MediaType)
	_, _ = fmt.Fprintf(response,
		`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`,
		string(requestID), code, message)
}

// refuse records one fixture-side diagnostic and answers a bounded status.
func (fixture *ToolFixture) refuse(response http.ResponseWriter, status int, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	fixture.mu.Lock()
	fixture.refusals = append(fixture.refusals, message)
	fixture.mu.Unlock()
	http.Error(response, message, status)
}

// ToolUsageRow is one committed tool.invoke audit row.
type ToolUsageRow struct {
	ID     string
	Actor  string
	Detail string
}

// ToolUsageRows returns the committed tool.invoke audit rows of the fixture
// project through the runtime role and transaction-local scope.
func (h *Harness) ToolUsageRows(ctx context.Context) ([]ToolUsageRow, error) {
	h.T.Helper()
	rows, err := pgtest.ScopedQuery(ctx, h.runtimeDB, h.Scope, 4,
		`SELECT id, actor, operation, coalesce(detail, '')
		   FROM audit
		  WHERE operation = 'tool.invoke'
		  ORDER BY at, id`)
	if err != nil {
		return nil, fmt.Errorf("scan tool usage rows: %w", err)
	}
	usage := make([]ToolUsageRow, 0, len(rows))
	for _, row := range rows {
		if len(row) != 4 {
			return nil, fmt.Errorf("tool usage row has %d columns; want 4", len(row))
		}
		usage = append(usage, ToolUsageRow{ID: row[0], Actor: row[1], Detail: row[3]})
	}
	return usage, nil
}
