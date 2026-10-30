package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type callerKey struct{}

// withCaller records the verified request identity for the tool handler. The
// subject token is available only to the exchange provider.
func withCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

func callerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok
}

// Executor calls the Kodama API for one MCP tool. It never follows redirects,
// never retries, and never logs request or response bodies.
type Executor struct {
	BaseURL          string
	Client           *http.Client
	Tokens           UpstreamTokenProvider
	MaxResponseBytes int64
	DefaultTenant    string
	DefaultProject   string
}

// NewExecutor builds the fixed HTTP client. A nil CA uses the system roots.
func NewExecutor(baseURL string, timeout time.Duration, maxBytes int64, ca []byte, tokens UpstreamTokenProvider, defaultTenant, defaultProject string) (*Executor, error) {
	if baseURL == "" || timeout <= 0 || maxBytes < 1024 || tokens == nil {
		return nil, errors.New("mcpserver: executor needs a base URL, token provider, positive timeout and response bound")
	}
	transport, err := httpTransport(ca)
	if err != nil {
		return nil, err
	}
	return &Executor{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client: &http.Client{
			Timeout: timeout, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Tokens: tokens, MaxResponseBytes: maxBytes, DefaultTenant: defaultTenant, DefaultProject: defaultProject,
	}, nil
}

func (e *Executor) Call(ctx context.Context, op Operation, args map[string]any) (*mcpCallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	caller, ok := callerFrom(ctx)
	if !ok || caller.SubjectToken == "" {
		return nil, errors.New("mcpserver: no verified caller")
	}
	token, err := e.Tokens.Token(ctx, caller)
	if err != nil {
		return nil, fmt.Errorf("obtain delegated API credential: %w", err)
	}
	if token == "" || token == caller.SubjectToken {
		return nil, errors.New("mcpserver: no separate delegated API credential")
	}
	if e.DefaultTenant != "" {
		if _, ok := args["tenant_id"]; !ok {
			args["tenant_id"] = e.DefaultTenant
		}
	}
	if e.DefaultProject != "" {
		if _, ok := args["project_id"]; !ok {
			args["project_id"] = e.DefaultProject
		}
	}

	path := op.Path
	for _, p := range op.PathParams {
		value, ok := args[p.Arg]
		if !ok || value == nil {
			return nil, fmt.Errorf("missing required path parameter %q", p.Arg)
		}
		text := fmt.Sprint(value)
		if text == "" {
			return nil, fmt.Errorf("path parameter %q is empty", p.Arg)
		}
		path = strings.ReplaceAll(path, "{"+p.API+"}", url.PathEscape(text))
	}
	if strings.Contains(path, "{") {
		return nil, errors.New("unresolved path parameter")
	}

	query := url.Values{}
	for _, p := range op.QueryParams {
		value, ok := args[p.Arg]
		if !ok || value == nil {
			continue
		}
		if op.ID == "getRunLogs" && p.API == "follow" {
			if b, _ := value.(bool); b {
				return nil, errors.New("follow is not supported over MCP; use tail and a bounded read")
			}
		}
		addQuery(query, p.API, value)
	}
	rawURL := e.BaseURL + path
	if encoded := query.Encode(); encoded != "" {
		rawURL += "?" + encoded
	}

	var body io.Reader
	if op.BodySchema != nil {
		value, ok := args["body"]
		if !ok || value == nil {
			if op.BodyRequired {
				return nil, errors.New("missing required JSON body")
			}
		} else {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("encode request body: %w", err)
			}
			if int64(len(encoded)) > e.MaxResponseBytes {
				return nil, errors.New("request body exceeds the configured bound")
			}
			body = bytes.NewReader(encoded)
		}
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", acceptFor(op))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, p := range op.HeaderParams {
		value, ok := args[p.Arg]
		if !ok || value == nil {
			continue
		}
		req.Header.Set(p.API, fmt.Sprint(value))
	}

	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kodama API request failed: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, e.MaxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read Kodama API response: %w", err)
	}
	if int64(len(data)) > e.MaxResponseBytes {
		return &mcpCallResult{
			IsError: true, Text: "The Kodama API response exceeded the configured MCP response bound. Narrow the request with limit or cursor.",
			Meta: map[string]any{"kodama/status": resp.StatusCode, "kodama/code": "response_too_large"},
		}, nil
	}
	requestID := resp.Header.Get("X-Request-ID")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiErrorResult(resp.StatusCode, data, requestID), nil
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	result := &mcpCallResult{
		Text: string(data),
		Meta: map[string]any{"kodama/status": resp.StatusCode},
	}
	if requestID != "" {
		result.Meta["kodama/requestId"] = requestID
	}
	if isJSON(resp.Header.Get("Content-Type")) {
		var structured any
		if json.Unmarshal(data, &structured) == nil {
			result.Structured = structured
		}
	}
	return result, nil
}

func acceptFor(op Operation) string {
	switch op.ID {
	case "getRunLogs":
		return "application/x-ndjson"
	case "observeProjectEvents", "observeRunEvents":
		// A JSON snapshot is bounded and cursor-carrying; an SSE stream would
		// hold an MCP tool call open indefinitely.
		return "application/json"
	default:
		return "application/json"
	}
}

func addQuery(values url.Values, name string, value any) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if item != nil {
				values.Add(name, fmt.Sprint(item))
			}
		}
	case []string:
		for _, item := range v {
			values.Add(name, item)
		}
	default:
		values.Add(name, fmt.Sprint(value))
	}
}

type apiErrorEnvelope struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"requestId"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

func apiErrorResult(status int, data []byte, headerRequestID string) *mcpCallResult {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(data, &envelope)
	code := envelope.Error.Code
	if code == "" {
		code = "api_error"
	}
	message := envelope.Error.Message
	if message == "" {
		message = fmt.Sprintf("Kodama API returned HTTP %d", status)
	}
	requestID := envelope.Error.RequestID
	if requestID == "" {
		requestID = headerRequestID
	}
	meta := map[string]any{"kodama/status": status, "kodama/code": code}
	if requestID != "" {
		meta["kodama/requestId"] = requestID
	}
	if len(envelope.Error.Details) > 0 {
		meta["kodama/details"] = envelope.Error.Details
	}
	return &mcpCallResult{IsError: true, Text: message, Meta: meta}
}

func isJSON(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json")
}

// mcpCallResult is the transport-neutral result before it is converted to the
// SDK's CallToolResult. Keeping it separate makes executor tests cheap.
type mcpCallResult struct {
	Text       string
	Structured any
	IsError    bool
	Meta       map[string]any
}
