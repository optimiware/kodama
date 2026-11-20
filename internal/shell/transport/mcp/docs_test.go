package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/docs"
	"github.com/optimiware/kodema/internal/shell/transport/mcp"
)

func TestMCPInitializeAdvertisesInstructionsResourcesAndPrompts(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	response := decodeResponse(t, edge.Handle(context.Background(), "", initializeRequest(api.MCPProtocolVersion)))
	if response.Error != nil {
		t.Fatalf("initialize refused: %+v", response.Error)
	}
	var result api.MCPInitializeResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.Instructions != docs.MCPInstructions() {
		t.Fatalf("instructions mismatch; got %d bytes, want %d bytes", len(result.Instructions), len(docs.MCPInstructions()))
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(result.Capabilities, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	for _, capName := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[capName]; !ok {
			t.Errorf("capabilities missing %q: %s", capName, string(result.Capabilities))
		}
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPResourcesListAndRead(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	listResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)))
	if listResp.Error != nil {
		t.Fatalf("resources/list refused: %+v", listResp.Error)
	}
	var listed api.MCPResourcesListResult
	if err := json.Unmarshal(listResp.Result, &listed); err != nil {
		t.Fatalf("decode resources/list result: %v", err)
	}
	topics := docs.Topics()
	if len(listed.Resources) != len(topics) {
		t.Fatalf("resources count = %d; want %d", len(listed.Resources), len(topics))
	}

	for index, resource := range listed.Resources {
		if resource.URI != topics[index].URI || resource.Name != topics[index].Slug || resource.MIMEType != docs.MIMETypeMarkdown {
			t.Fatalf("resource[%d] = %+v; want %+v", index, resource, topics[index])
		}
		readBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      index + 10,
			"method":  "resources/read",
			"params":  map[string]string{"uri": resource.URI},
		})
		readResp := decodeResponse(t, edge.Handle(context.Background(), "", readBody))
		if readResp.Error != nil {
			t.Fatalf("resources/read(%s) refused: %+v", resource.URI, readResp.Error)
		}
		var readResult api.MCPResourcesReadResult
		if err := json.Unmarshal(readResp.Result, &readResult); err != nil {
			t.Fatalf("decode resources/read result: %v", err)
		}
		if len(readResult.Contents) != 1 {
			t.Fatalf("resources/read(%s) contents len = %d; want 1", resource.URI, len(readResult.Contents))
		}
		if readResult.Contents[0].URI != resource.URI || readResult.Contents[0].Text != topics[index].Content {
			t.Fatalf("resources/read(%s) content mismatch", resource.URI)
		}
	}

	// resources/templates/list returns an empty slice cleanly (never unsupported).
	tmplResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":98,"method":"resources/templates/list"}`)))
	if tmplResp.Error != nil {
		t.Fatalf("resources/templates/list refused: %+v", tmplResp.Error)
	}
	var tmplResult api.MCPResourcesTemplatesListResult
	if err := json.Unmarshal(tmplResp.Result, &tmplResult); err != nil {
		t.Fatalf("decode resources/templates/list result: %v", err)
	}
	if tmplResult.ResourceTemplates == nil || len(tmplResult.ResourceTemplates) != 0 {
		t.Fatalf("resourceTemplates = %#v; want empty non-nil slice", tmplResult.ResourceTemplates)
	}

	// Unknown URI returns MCP resource-not-found (-32002) with public code not_found.
	notFound := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":99,"method":"resources/read","params":{"uri":"kodema://docs/missing"}}`)))
	if notFound.Error == nil || notFound.Error.Code != api.MCPCodeResourceNotFound {
		t.Fatalf("unknown resource error = %+v; want %d", notFound.Error, api.MCPCodeResourceNotFound)
	}
	if pub := decodeErrorData(t, notFound); pub.Code != api.CodeNotFound {
		t.Fatalf("unknown resource public code = %q; want %q", pub.Code, api.CodeNotFound)
	}

	// Unknown cursor returns invalid_input (-32602).
	for _, req := range []string{
		`{"jsonrpc":"2.0","id":100,"method":"resources/list","params":{"cursor":"bogus"}}`,
		`{"jsonrpc":"2.0","id":101,"method":"resources/templates/list","params":{"cursor":"bogus"}}`,
	} {
		badCursor := decodeResponse(t, edge.Handle(context.Background(), "", []byte(req)))
		if badCursor.Error == nil || badCursor.Error.Code != -32602 {
			t.Fatalf("bad cursor error for %s = %+v; want -32602", req, badCursor.Error)
		}
	}

	// Tool catalog parity remains untouched (61 operations).
	if got := len(mcp.ToolCatalog()); got != 61 {
		t.Fatalf("ToolCatalog length = %d; want 61", got)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPPromptsListAndGet(t *testing.T) {
	router := &fakeRouter{}
	edge := newEdge(t, router)

	listResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`)))
	if listResp.Error != nil {
		t.Fatalf("prompts/list refused: %+v", listResp.Error)
	}
	var listed api.MCPPromptsListResult
	if err := json.Unmarshal(listResp.Result, &listed); err != nil {
		t.Fatalf("decode prompts/list result: %v", err)
	}
	wantCount := len(docs.Topics()) + 1
	if len(listed.Prompts) != wantCount {
		t.Fatalf("prompts count = %d; want %d", len(listed.Prompts), wantCount)
	}
	if listed.Prompts[0].Name != "kodema-docs" {
		t.Fatalf("first prompt = %q; want kodema-docs", listed.Prompts[0].Name)
	}

	// Default kodema-docs loads overview.
	defaultResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"kodema-docs"}}`)))
	if defaultResp.Error != nil {
		t.Fatalf("prompts/get(kodema-docs) refused: %+v", defaultResp.Error)
	}
	var defaultResult api.MCPPromptsGetResult
	if err := json.Unmarshal(defaultResp.Result, &defaultResult); err != nil {
		t.Fatalf("decode prompts/get result: %v", err)
	}
	overview, _ := docs.Lookup("overview")
	if len(defaultResult.Messages) != 1 || defaultResult.Messages[0].Content.Text != overview.Content {
		t.Fatalf("kodema-docs default message mismatch")
	}

	// kodema-docs with topic=all returns all embedded topics.
	allResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"kodema-docs","arguments":{"topic":"all"}}}`)))
	if allResp.Error != nil {
		t.Fatalf("prompts/get(kodema-docs, all) refused: %+v", allResp.Error)
	}
	var allResult api.MCPPromptsGetResult
	if err := json.Unmarshal(allResp.Result, &allResult); err != nil {
		t.Fatalf("decode all prompt result: %v", err)
	}
	if len(allResult.Messages) != 1 || !strings.Contains(allResult.Messages[0].Content.Text, "# Container Workload Contract") {
		t.Fatalf("kodema-docs all message missing workload docs")
	}

	// Per-topic shortcut prompt kodema-workload works with no arguments.
	topicResp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{"name":"kodema-workload"}}`)))
	if topicResp.Error != nil {
		t.Fatalf("prompts/get(kodema-workload) refused: %+v", topicResp.Error)
	}
	var topicResult api.MCPPromptsGetResult
	if err := json.Unmarshal(topicResp.Result, &topicResult); err != nil {
		t.Fatalf("decode topic prompt result: %v", err)
	}
	workload, _ := docs.Lookup("workload")
	if len(topicResult.Messages) != 1 || topicResult.Messages[0].Content.Text != workload.Content {
		t.Fatalf("kodema-workload message mismatch")
	}

	// Refusal cases: unknown prompt (-32004), unknown topic (-32602), extra args (-32602), unknown cursor (-32602).
	unknownPrompt := decodeResponse(t, edge.Handle(context.Background(), "", []byte(`{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"kodema-nonexistent"}}`)))
	if unknownPrompt.Error == nil || unknownPrompt.Error.Code != -32004 {
		t.Fatalf("unknown prompt error = %+v; want -32004", unknownPrompt.Error)
	}
	for _, bad := range []string{
		`{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":{"name":"kodema-docs","arguments":{"topic":"missing"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"prompts/get","params":{"name":"kodema-docs","arguments":{"extra":"1"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"prompts/get","params":{"name":"kodema-workload","arguments":{"topic":"workload"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"prompts/list","params":{"cursor":"c1"}}`,
	} {
		resp := decodeResponse(t, edge.Handle(context.Background(), "", []byte(bad)))
		if resp.Error == nil || resp.Error.Code != -32602 {
			t.Fatalf("invalid prompt request %s = %+v; want -32602", bad, resp.Error)
		}
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

func TestMCPDocsMethodsRequireAuthenticationWhenPrincipalsConfigured(t *testing.T) {
	principals := &fakePrincipals{}
	router := &fakeRouter{}
	edge := newAuthenticatedEdge(t, router, principals)

	requests := map[string][]byte{
		"resources/list":           []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`),
		"resources/templates/list": []byte(`{"jsonrpc":"2.0","id":5,"method":"resources/templates/list"}`),
		"resources/read":           []byte(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"kodema://docs/overview"}}`),
		"prompts/list":             []byte(`{"jsonrpc":"2.0","id":3,"method":"prompts/list"}`),
		"prompts/get":              []byte(`{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{"name":"kodema-docs"}}`),
	}
	for name, body := range requests {
		unauth := decodeResponse(t, edge.Handle(context.Background(), "", body))
		if unauth.Error == nil || unauth.Error.Code != -32001 {
			t.Fatalf("unauthenticated %s = %+v; want -32001", name, unauth.Error)
		}
		authed := decodeResponse(t, edge.Handle(context.Background(), "valid-token", body))
		if authed.Error != nil {
			t.Fatalf("authenticated %s refused: %+v", name, authed.Error)
		}
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}
