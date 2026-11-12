//go:build integration

package composition

// Composition integration evidence for the optional external MCP tool proxy:
// both public edges reach an explicitly configured, project-scoped
// upstream tool with the exact configured arguments, the request-local
// authenticated caller supplies the identity and the committed usage row is
// attributed to that caller and project.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	toolAlias        = "search-docs"
	toolUpstreamName = "docs.search"
	toolSecretName   = "TOOL_TOKEN"
	toolCallArgument = `{"query":"needle","limit":3}`
)

// toolCallUpstream is the deterministic local MCP upstream of this test.
type toolCallUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []toolCallRecord
}

type toolCallRecord struct {
	Header    http.Header
	Name      string
	Arguments json.RawMessage
}

func newToolCallUpstream(t *testing.T) *toolCallUpstream {
	t.Helper()
	upstream := &toolCallUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		var decoded struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(data, &decoded)
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, toolCallRecord{
			Header:    request.Header.Clone(),
			Name:      decoded.Params.Name,
			Arguments: append(json.RawMessage(nil), decoded.Params.Arguments...),
		})
		upstream.mu.Unlock()
		response.Header().Set("Content-Type", api.MediaType)
		_, _ = fmt.Fprintf(response,
			`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"settled"}],"isError":false}}`,
			string(decoded.ID))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (upstream *toolCallUpstream) received() []toolCallRecord {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]toolCallRecord(nil), upstream.calls...)
}

// TestConfiguredToolThroughRESTAndMCP drives one configured project-scoped tool
// through both public edges.
//
// Mutation reasoning: retaining the disabled binding makes both edges return
// not_found; forwarding the public alias instead of the configured upstream tool
// makes the upstream receive "search-docs" instead of "docs.search"; dropping
// the project or principal from the usage commit loses the audit attribution;
// forwarding the caller's bearer credential instead of the configured secret
// mapping changes the upstream Authorization header.
func TestConfiguredToolThroughRESTAndMCP(t *testing.T) {
	upstream := newToolCallUpstream(t)
	harness := newHarness(t)
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	harness.config.ToolProxy = &app.ToolProxyConfig{
		Projects: []app.ProjectToolProxy{{
			Scope: scope,
			Servers: []app.ToolServer{{
				Name:     "search",
				Endpoint: upstream.server.URL,
				Secrets:  []id.SecretName{toolSecretName},
				Credentials: []app.ToolCredential{{
					Secret: toolSecretName,
					Header: "Authorization",
					Scheme: "Bearer",
				}},
				Tools: []app.ToolDefinition{{
					Alias:    toolAlias,
					Name:     toolUpstreamName,
					Validate: func(json.RawMessage) error { return nil },
				}},
			}},
		}},
		SecretRules: []secret.NameRule{{Scope: scope, Names: []id.SecretName{toolSecretName}}},
	}
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  toolSecretName,
		Value:     secretValue,
	}, nil, "tool-secret-write")

	// REST edge.
	restResult, restError := environment.call(projectToken, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: projectID,
		Tool:      toolAlias,
		Arguments: json.RawMessage(toolCallArgument),
	}, nil, "")
	if restError.Code != "" {
		t.Fatalf("REST tool.invoke code = %q", restError.Code)
	}
	var restTool api.ToolInvokeResult
	environment.decode(restResult, &restTool)
	if restTool.IsError || restTool.Content == "" {
		t.Fatalf("REST tool.invoke result = %+v", restTool)
	}

	// MCP edge.
	mcpResult, mcpError, err := environment.mcpCall(projectToken, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: projectID,
		Tool:      toolAlias,
		Arguments: json.RawMessage(toolCallArgument),
	}, nil, "")
	if err != nil {
		t.Fatalf("MCP tool.invoke: %v", err)
	}
	if mcpError.Code != "" {
		t.Fatalf("MCP tool.invoke code = %q", mcpError.Code)
	}
	var mcpTool api.ToolInvokeResult
	environment.decode(mcpResult, &mcpTool)
	if mcpTool.IsError || mcpTool.Content != restTool.Content {
		t.Fatalf("MCP tool.invoke result = %+v; REST = %+v", mcpTool, restTool)
	}

	calls := upstream.received()
	if len(calls) != 2 {
		t.Fatalf("configured upstream calls = %d; want 2 (one per edge)", len(calls))
	}
	for index, call := range calls {
		if call.Name != toolUpstreamName {
			t.Fatalf("upstream call %d tool = %q; want the configured upstream name %q", index, call.Name, toolUpstreamName)
		}
		if got := call.Header.Get("Authorization"); got != "Bearer "+secretValue {
			t.Fatalf("upstream call %d authorization = %q; want the configured credential mapping", index, got)
		}
		if got := string(call.Arguments); got != toolCallArgument {
			t.Fatalf("upstream call %d arguments = %s; want the exact configured arguments %s", index, got, toolCallArgument)
		}
		if got := call.Header.Get("Authorization"); got == "Bearer "+projectToken {
			t.Fatalf("upstream call %d forwarded the caller bearer credential", index)
		}
	}

	// The public alias is the only accepted public name; the configured
	// upstream name is not a second alias.
	_, aliasError := environment.call(projectToken, "tool.invoke", api.ToolInvokeRequest{
		ProjectID: projectID,
		Tool:      toolUpstreamName,
		Arguments: json.RawMessage(toolCallArgument),
	}, nil, "")
	if aliasError.Code != api.CodeNotFound {
		t.Fatalf("upstream-name invocation code = %q; want %q", aliasError.Code, api.CodeNotFound)
	}
	if len(upstream.received()) != 2 {
		t.Fatalf("upstream calls after the non-alias refusal = %d; want 2", len(upstream.received()))
	}

	// One committed usage row per edge, attributed to the authenticated caller
	// in the configured project scope.
	usage := toolUsageRows(t, harness, scope)
	if len(usage) != 2 {
		t.Fatalf("tool usage rows = %d; want 2", len(usage))
	}
	for _, row := range usage {
		if row.Actor != "admin-1" {
			t.Fatalf("tool usage actor = %q; want the authenticated caller admin-1", row.Actor)
		}
		var detail struct {
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
			t.Fatalf("decode usage detail: %v", err)
		}
		if detail.Server != "search" || detail.Tool != toolUpstreamName || detail.Outcome != "ok" {
			t.Fatalf("usage detail = %+v; want search/%s/ok", detail, toolUpstreamName)
		}
	}

	// Raw content retention is off by default, so a settled tool result never
	// creates a content capture row.
	page, err := harness.store.Query(context.Background(), scope, store.ScanQuery{
		Table: store.ContentRecordsTable,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("scan content_records: %v", err)
	}
	if len(page.Rows) != 0 {
		t.Fatalf("content record rows = %d; raw content retention is off by default", len(page.Rows))
	}
}

// toolUsageRows returns the committed tool.invoke audit rows of one scope.
func toolUsageRows(t *testing.T, harness *harness, scope tenancy.Scope) []toolAudit {
	t.Helper()
	rows := harness.store.Audit(scope)
	usage := make([]toolAudit, 0, len(rows))
	for _, row := range rows {
		if row.Operation == "tool.invoke" {
			usage = append(usage, toolAudit{Actor: row.Actor, Detail: row.Detail})
		}
	}
	return usage
}

type toolAudit struct {
	Actor  id.PrincipalID
	Detail string
}
