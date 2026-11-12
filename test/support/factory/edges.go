package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// REST performs one raw REST envelope call and returns either the validated
// result or the public error body. The suite uses the raw edge where it must
// inspect the exact public status/code.
func (h *Harness) REST(token, operation string, input any, expected *uint64, key string) (api.Result, api.Error) {
	h.T.Helper()
	route, ok := api.LookupRoute(operation)
	if !ok {
		h.T.Fatalf("unknown operation %q", operation)
	}
	path := expandPath(h.T, route, input)
	request := api.Request{
		ExpectedVersion: expected,
		IdempotencyKey:  key,
		Input:           encodeInput(h.T, input),
	}
	status, data := h.restRaw(string(route.Method), path, token, request)
	if status == api.HTTPStatusSuccess {
		var result api.Result
		if err := json.Unmarshal(data, &result); err != nil {
			h.T.Fatalf("%s result: %v; body=%s", operation, err, string(data))
		}
		if err := result.Validate(); err != nil {
			h.T.Fatalf("%s result: %v", operation, err)
		}
		return result, api.Error{}
	}
	var publicError api.Error
	if err := json.Unmarshal(data, &publicError); err != nil {
		h.T.Fatalf("%s error body is not an api.Error: %v; body=%s", operation, err, string(data))
	}
	if want, ok := api.HTTPStatus(publicError.Code); !ok || want != status {
		h.T.Fatalf("%s error status=%d code=%q", operation, status, publicError.Code)
	}
	return api.Result{}, publicError
}

// MustREST performs one REST call and fails the test on a refusal.
func (h *Harness) MustREST(token, operation string, input any, expected *uint64, key string) api.Result {
	h.T.Helper()
	result, publicError := h.REST(token, operation, input, expected, key)
	if publicError.Code != "" {
		h.T.Fatalf("%s over REST refused: %s (%s)", operation, publicError.Code, publicError.Message)
	}
	return result
}

// MCP performs one authorized MCP tools/call.
func (h *Harness) MCP(token, operation string, input any, expected *uint64, key string) (api.Result, api.Error, error) {
	h.T.Helper()
	arguments := api.Request{
		ExpectedVersion: expected,
		IdempotencyKey:  key,
		Input:           encodeInput(h.T, input),
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return api.Result{}, api.Error{}, err
	}
	response, err := h.mcpRaw(api.MCPMethodToolsCall, api.MCPToolsCallParams{
		Name:      operation,
		Arguments: encoded,
	}, token)
	if err != nil {
		return api.Result{}, api.Error{}, err
	}
	if response.Error != nil {
		var publicError api.Error
		if err := json.Unmarshal(response.Error.Data, &publicError); err != nil {
			return api.Result{}, api.Error{}, fmt.Errorf("decode MCP error data: %w; data=%s", err, string(response.Error.Data))
		}
		return api.Result{}, publicError, nil
	}
	var result api.Result
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return api.Result{}, api.Error{}, fmt.Errorf("decode MCP result: %w; result=%s", err, string(response.Result))
	}
	if err := result.Validate(); err != nil {
		return api.Result{}, api.Error{}, fmt.Errorf("invalid MCP result: %w", err)
	}
	return result, api.Error{}, nil
}

// MustMCP performs one MCP tools/call and fails the test on a transport error
// or a refusal.
func (h *Harness) MustMCP(token, operation string, input any, expected *uint64, key string) api.Result {
	h.T.Helper()
	result, publicError, err := h.MCP(token, operation, input, expected, key)
	if err != nil {
		h.T.Fatalf("%s over MCP: %v", operation, err)
	}
	if publicError.Code != "" {
		h.T.Fatalf("%s over MCP refused: %s (%s)", operation, publicError.Code, publicError.Message)
	}
	return result
}

// Decode decodes one result body into target.
func (h *Harness) Decode(result api.Result, target any) {
	h.T.Helper()
	if err := json.Unmarshal(result.Body, target); err != nil {
		h.T.Fatalf("decode %s body: %v; body=%s", result.Operation, err, string(result.Body))
	}
}

// Client returns the public Go client transport bound to the served address.
// The caller may replace RoundTripper or Retry for a lost-response scenario.
func (h *Harness) Client(token string) *client.HTTPTransport {
	return &client.HTTPTransport{
		BaseURL:      h.BaseURL,
		RoundTripper: h.roundTripper(),
		Source: staticCredential{credential: client.Credential{
			Kind:      client.KindSession,
			Token:     token,
			ExpiresAt: time.Now().Add(time.Hour),
		}},
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	}
}

// roundTripper returns the transport the harness helpers use. The default
// harness keeps using the process default; the TLS harness uses its
// CA-trusting fixture transport. It is never installed process-wide.
func (h *Harness) roundTripper() http.RoundTripper {
	if h.clientTransport != nil {
		return h.clientTransport
	}
	return http.DefaultTransport
}

// httpClient returns the client that issues the harness's own requests.
func (h *Harness) httpClient() *http.Client {
	if h.clientTransport != nil {
		return &http.Client{Transport: h.roundTripper()}
	}
	return http.DefaultClient
}

// staticCredential is one fixed bearer credential source.
type staticCredential struct {
	credential client.Credential
}

// Credential implements client.CredentialSource.
func (source staticCredential) Credential(context.Context) (client.Credential, error) {
	return source.credential, nil
}

// restRaw performs one raw REST envelope request and returns the status and the
// exact response bytes.
func (h *Harness) restRaw(method, path, token string, request api.Request) (int, []byte) {
	h.T.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		h.T.Fatalf("marshal request: %v", err)
	}
	httpRequest, err := http.NewRequest(method, h.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		h.T.Fatalf("build request: %v", err)
	}
	httpRequest.Header.Set("Content-Type", api.MediaType)
	if token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := h.httpClient().Do(httpRequest)
	if err != nil {
		h.T.Fatalf("request %s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		h.T.Fatalf("read response: %v", err)
	}
	return response.StatusCode, data
}

// mcpRaw performs one raw MCP JSON-RPC request over HTTP.
func (h *Harness) mcpRaw(method string, params any, token string) (api.MCPResponse, error) {
	body := map[string]any{"jsonrpc": api.MCPJSONRPC, "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return api.MCPResponse{}, err
	}
	request, err := http.NewRequest(http.MethodPost, h.BaseURL+appMCPPath, bytes.NewReader(encoded))
	if err != nil {
		return api.MCPResponse{}, err
	}
	request.Header.Set("Content-Type", api.MediaType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := h.httpClient().Do(request)
	if err != nil {
		return api.MCPResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return api.MCPResponse{}, err
	}
	var decoded api.MCPResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return api.MCPResponse{}, fmt.Errorf("decode MCP response: %w; body=%s", err, string(data))
	}
	return decoded, nil
}

// appMCPPath is the composition's fixed MCP JSON-RPC path.
const appMCPPath = "/mcp"

// Invoke sends one operation through the public Go client transport and
// returns the result or the public error without failing the test. It is safe
// to call from a test goroutine, which the race schedules need for a request
// that is intentionally in flight while another commit runs. A transport-level
// failure that carries no public code is returned as err.
func (h *Harness) Invoke(ctx context.Context, token, operation string, input any, expected *uint64, key string) (api.Result, api.Error, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return api.Result{}, api.Error{}, fmt.Errorf("marshal %s input: %w", operation, err)
	}
	transport := h.Client(token)
	result, err := transport.Invoke(ctx, api.Invocation{
		Operation: operation,
		Request: api.Request{
			ExpectedVersion: expected,
			IdempotencyKey:  key,
			Input:           encoded,
		},
	})
	if err == nil {
		return result, api.Error{}, nil
	}
	if code, ok := fault.CodeOf(err); ok {
		return api.Result{}, api.Error{Code: string(code), Message: err.Error()}, nil
	}
	return api.Result{}, api.Error{}, err
}

// expandPath substitutes the declared route parameters from the canonical DTO
// fields of the request input.
func expandPath(t interface {
	Helper()
	Fatalf(string, ...any)
}, route api.Route, input any) string {
	t.Helper()
	encoded := encodeInput(t, input)
	var fields map[string]any
	if len(encoded) != 0 {
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("route input is not an object: %v", err)
		}
	}
	segments := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	for index, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		value, ok := lookupField(fields, canonicalRouteField(route.Operation, name))
		if !ok || value == "" {
			t.Fatalf("route %s requires parameter %q", route.Operation, name)
		}
		segments[index] = value
	}
	return "/" + strings.Join(segments, "/")
}

// canonicalRouteField maps one route-template parameter to the canonical DTO
// field from the operation codec table.
func canonicalRouteField(operation, name string) string {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		for _, param := range codec.RouteParams {
			if param.Name == name {
				return param.Field
			}
		}
	}
	return name
}

// lookupField reads one dotted JSON field path.
func lookupField(fields map[string]any, path string) (string, bool) {
	var current any = fields
	for segment := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[segment]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}

// encodeInput marshals one operation input, normalizing nil to an empty object.
func encodeInput(t interface {
	Helper()
	Fatalf(string, ...any)
}, input any) json.RawMessage {
	t.Helper()
	if input == nil {
		return json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return encoded
}
