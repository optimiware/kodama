//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
)

// DefaultMCPPath is the composition HTTP path of the MCP JSON-RPC edge. The
// installation descriptor does not carry an override, so the documented default
// is used; a non-default deployment path would make this probe fail loudly
// rather than silently speak to the wrong route.
const DefaultMCPPath = "/mcp"

// MCP is the e2e-only MCP edge helper. It speaks the supported JSON-RPC subset
// (initialize, tools/list, tools/call) over verified TLS and never follows a
// redirect. A tools/call carries the same api.Request envelope as REST, so a
// parity probe can compare the two edges operation for operation.
type MCP struct {
	base           *url.URL
	client         *http.Client
	maxResponse    int
	requestTimeout time.Duration
}

// MCPRefusal is one public refusal decoded from an MCP error response. The
// JSON-RPC number and the public error body are both retained so a probe can
// assert the declared mapping and compare the typed error with REST.
type MCPRefusal struct {
	JSONRPCCode int
	Public      api.Error
}

// Error renders the public refusal without request or credential material.
func (refusal *MCPRefusal) Error() string {
	return fmt.Sprintf("MCP request refused: %s (jsonrpc %d)", refusal.Public.Code, refusal.JSONRPCCode)
}

// MCPCall is one tools/call invocation.
type MCPCall struct {
	ID              string
	Token           Secret
	Operation       string
	Input           any
	IdempotencyKey  string
	ExpectedVersion *uint64
}

// NewMCP builds the e2e MCP edge helper from the same installation inputs as
// the REST helper.
func NewMCP(config Config) (*MCP, error) {
	normalized, err := validateAPIURL(config.APIURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	httpClient, err := newHTTPClient(config, base)
	if err != nil {
		return nil, err
	}
	return &MCP{
		base:           base,
		client:         httpClient,
		maxResponse:    3 * api.MaxResponseBytes,
		requestTimeout: defaultAPITimeout,
	}, nil
}

// Call performs one tools/call with the operation request envelope. It returns
// the decoded result, the exact JSON-RPC response bytes (for credential
// redaction scans), and either a *MCPRefusal or a transport/decode failure.
func (client *MCP) Call(ctx context.Context, call MCPCall) (api.Result, []byte, error) {
	rawInput, err := encodeInput(call.Input)
	if err != nil {
		return api.Result{}, nil, err
	}
	request := api.Request{
		Input:           rawInput,
		ExpectedVersion: call.ExpectedVersion,
		IdempotencyKey:  call.IdempotencyKey,
	}
	if err := request.Validate(); err != nil {
		return api.Result{}, nil, err
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		return api.Result{}, nil, fmt.Errorf("encode the MCP operation envelope")
	}
	params, err := json.Marshal(api.MCPToolsCallParams{Name: call.Operation, Arguments: arguments})
	if err != nil {
		return api.Result{}, nil, fmt.Errorf("encode the MCP tools/call parameters")
	}
	id := call.ID
	if id == "" {
		id = "parity"
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return api.Result{}, nil, fmt.Errorf("encode the MCP request id")
	}
	body, err := json.Marshal(api.MCPRequest{
		JSONRPC: api.MCPJSONRPC,
		ID:      idJSON,
		Method:  api.MCPMethodToolsCall,
		Params:  params,
	})
	if err != nil {
		return api.Result{}, nil, fmt.Errorf("encode the MCP request")
	}
	response, raw, err := client.roundTrip(ctx, call.Token, body)
	if err != nil {
		return api.Result{}, raw, err
	}
	if response.Error != nil {
		public, err := api.DecodeMCPErrorData(response.Error.Data)
		if err != nil {
			return api.Result{}, raw, fmt.Errorf("the MCP error data is not a public error: %w", err)
		}
		want, ok := api.MCPErrorCode(public.Code)
		if !ok || want != response.Error.Code {
			return api.Result{}, raw, fmt.Errorf("the MCP error number does not match its public code")
		}
		if response.Error.Message != public.Message {
			return api.Result{}, raw, fmt.Errorf("the MCP error message does not match its public error")
		}
		return api.Result{}, raw, &MCPRefusal{JSONRPCCode: response.Error.Code, Public: public}
	}
	result, err := api.DecodeMCPResult(response.Result)
	if err != nil {
		return api.Result{}, raw, fmt.Errorf("the MCP result is not a public result: %w", err)
	}
	if result.Operation != call.Operation {
		return api.Result{}, raw, fmt.Errorf("the MCP result names operation %q", result.Operation)
	}
	return result, raw, nil
}

// Tools performs the pinned initialize handshake and returns the advertised
// tools/list catalog together with the exact JSON-RPC response bytes of both
// exchanges, so a probe can scan the protocol wire for credential material. It
// proves the edge speaks the documented protocol and that every parity
// operation is advertised under its declared name.
func (client *MCP) Tools(ctx context.Context) ([]api.MCPTool, [][]byte, error) {
	initializeParams, err := json.Marshal(api.MCPInitializeParams{
		ProtocolVersion: api.MCPProtocolVersion,
		ClientInfo:      api.MCPImplementation{Name: "kodama-e2e", Version: "1.0.0"},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("encode the MCP initialize parameters")
	}
	response, initializeRaw, err := client.request(ctx, "", api.MCPMethodInitialize, "initialize", initializeParams)
	if err != nil {
		return nil, [][]byte{initializeRaw}, err
	}
	if response.Error != nil {
		return nil, [][]byte{initializeRaw}, fmt.Errorf("MCP initialize was refused: %s", response.Error.Message)
	}
	var initialized api.MCPInitializeResult
	if err := json.Unmarshal(response.Result, &initialized); err != nil {
		return nil, [][]byte{initializeRaw}, fmt.Errorf("decode the MCP initialize result: %w", err)
	}
	if err := initialized.Validate(); err != nil {
		return nil, [][]byte{initializeRaw}, fmt.Errorf("invalid MCP initialize result: %w", err)
	}
	response, listRaw, err := client.request(ctx, "", api.MCPMethodToolsList, "tools-list", json.RawMessage(`{}`))
	wire := [][]byte{initializeRaw, listRaw}
	if err != nil {
		return nil, wire, err
	}
	if response.Error != nil {
		return nil, wire, fmt.Errorf("MCP tools/list was refused: %s", response.Error.Message)
	}
	var listed api.MCPToolsListResult
	if err := json.Unmarshal(response.Result, &listed); err != nil {
		return nil, wire, fmt.Errorf("decode the MCP tools/list result: %w", err)
	}
	seen := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		if tool.Name == "" {
			return nil, wire, fmt.Errorf("tools/list advertised an empty tool name")
		}
		if seen[tool.Name] {
			return nil, wire, fmt.Errorf("tools/list advertised %q twice", tool.Name)
		}
		seen[tool.Name] = true
		if len(tool.InputSchema) == 0 {
			return nil, wire, fmt.Errorf("tools/list advertised %q without an input schema", tool.Name)
		}
	}
	return listed.Tools, wire, nil
}

// request sends one non-tools/call message and returns the decoded response.
func (client *MCP) request(ctx context.Context, token Secret, method, id string, params json.RawMessage) (api.MCPResponse, []byte, error) {
	idJSON, err := json.Marshal(id)
	if err != nil {
		return api.MCPResponse{}, nil, fmt.Errorf("encode the MCP request id")
	}
	body, err := json.Marshal(api.MCPRequest{
		JSONRPC: api.MCPJSONRPC,
		ID:      idJSON,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return api.MCPResponse{}, nil, fmt.Errorf("encode the MCP request")
	}
	return client.roundTrip(ctx, token, body)
}

// roundTrip posts one JSON-RPC request and decodes the single response.
func (client *MCP) roundTrip(ctx context.Context, token Secret, body []byte) (api.MCPResponse, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.MCPResponse{}, nil, err
	}
	target := *client.base
	target.Path = strings.TrimSuffix(client.base.Path, "/") + DefaultMCPPath
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return api.MCPResponse{}, nil, fmt.Errorf("build the MCP request")
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Accept", api.MediaType)
	if token.IsSet() {
		request.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	request.ContentLength = int64(len(body))

	requestCtx := ctx
	if client.requestTimeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, client.requestTimeout)
		defer cancel()
	}
	response, err := client.client.Do(request.WithContext(requestCtx))
	if err != nil {
		if requestCtx.Err() != nil {
			return api.MCPResponse{}, nil, requestCtx.Err()
		}
		return api.MCPResponse{}, nil, fmt.Errorf("the MCP endpoint is unavailable")
	}
	defer response.Body.Close()
	data, err := readBounded(response.Body, client.maxResponse)
	if err != nil {
		return api.MCPResponse{}, nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return api.MCPResponse{}, data, fmt.Errorf("the MCP endpoint redirected; redirects are never followed")
	}
	if response.StatusCode != api.HTTPStatusSuccess && response.StatusCode != http.StatusUnauthorized {
		return api.MCPResponse{}, data, fmt.Errorf("the MCP endpoint returned status %d", response.StatusCode)
	}
	var decoded api.MCPResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return api.MCPResponse{}, data, fmt.Errorf("the MCP response is not valid JSON-RPC")
	}
	if err := decoded.Validate(); err != nil {
		return api.MCPResponse{}, data, fmt.Errorf("the MCP response is invalid: %w", err)
	}
	return decoded, data, nil
}
