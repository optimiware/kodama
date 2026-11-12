//go:build qualification

package production

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
)

// defaultAPITimeout bounds one public request when the caller supplies no
// shorter context deadline.
const defaultAPITimeout = 30 * time.Second

// stageMCPPath is the served MCP JSON-RPC path. The chart pins it to the
// composition default, and the qualification harness treats it as fixed input.
const stageMCPPath = "/mcp"

// API is the qualification-only public REST helper. It speaks the frozen
// envelope over verified TLS and never follows a redirect.
type API struct {
	base           *url.URL
	client         *http.Client
	maxResponse    int
	requestTimeout time.Duration
}

// APIError is one public refusal decoded from the frozen error body. It never
// carries a credential.
type APIError struct {
	Status    int
	Code      string
	RequestID string
}

// Error renders the public refusal without request or credential material.
func (refusal *APIError) Error() string {
	return fmt.Sprintf("public request refused: %s (status %d)", refusal.Code, refusal.Status)
}

// NewAPI builds the stage API helper. The CA file is required and the TLS
// server name is verified explicitly against the service SAN rather than the
// loopback host a port-forward connects to.
func NewAPI(config Config) (*API, error) {
	normalized, err := validateAPIURL(config.APIURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	if config.CAPEM == "" {
		return nil, fmt.Errorf("%s is not configured", InputCAPEM)
	}
	if err := validateDNSName(config.TLSServerName); err != nil {
		return nil, fmt.Errorf("%s: %w", InputTLSServerName, err)
	}
	caPEM, err := os.ReadFile(config.CAPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the CA file", InputCAPEM)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s: the CA file contains no certificate", InputCAPEM)
	}

	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
			ServerName: config.TLSServerName,
		},
		ForceAttemptHTTP2: true,
	}
	return &API{
		base: base,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponse:    api.MaxResponseBytes,
		requestTimeout: defaultAPITimeout,
	}, nil
}

// Get performs one authenticated GET of a frozen public operation.
func (client *API) Get(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.invoke(ctx, api.MethodGet, token, operation, input, idempotencyKey)
}

// Post performs one authenticated POST of a frozen public operation.
func (client *API) Post(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.invoke(ctx, api.MethodPost, token, operation, input, idempotencyKey)
}

// Delete performs one authenticated DELETE of a frozen public operation.
func (client *API) Delete(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.invoke(ctx, api.MethodDelete, token, operation, input, idempotencyKey)
}

// Put performs one authenticated PUT of a frozen public operation.
func (client *API) Put(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.invoke(ctx, api.MethodPut, token, operation, input, idempotencyKey)
}

// PutVersioned performs one authenticated PUT carrying the caller's expected
// resource version in the frozen request envelope.
func (client *API) PutVersioned(ctx context.Context, token Secret, operation string, input any, idempotencyKey string, expectedVersion uint64) (api.Result, error) {
	return client.invokeVersioned(ctx, api.MethodPut, token, operation, input, idempotencyKey, expectedVersion)
}

// PostVersioned performs one authenticated POST carrying the caller's expected
// resource version in the frozen request envelope. The service and membership
// mutations that guard on a committed version use it; a zero version is refused
// before any request because the envelope only accepts a positive version.
func (client *API) PostVersioned(ctx context.Context, token Secret, operation string, input any, idempotencyKey string, expectedVersion uint64) (api.Result, error) {
	return client.invokeVersioned(ctx, api.MethodPost, token, operation, input, idempotencyKey, expectedVersion)
}

// DeleteVersioned performs one authenticated DELETE carrying the caller's
// expected resource version in the frozen request envelope.
func (client *API) DeleteVersioned(ctx context.Context, token Secret, operation string, input any, idempotencyKey string, expectedVersion uint64) (api.Result, error) {
	return client.invokeVersioned(ctx, api.MethodDelete, token, operation, input, idempotencyKey, expectedVersion)
}

// PostAlternate performs one authenticated POST through the operation's
// additive alternate REST binding. It is the only helper that uses the second
// binding, so the canonical route stays the default for every other call.
func (client *API) PostAlternate(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	route, ok := lookupAlternateRoute(operation, api.MethodPost)
	if !ok {
		return api.Result{}, fmt.Errorf("operation %q has no alternate POST binding", operation)
	}
	return client.invokeRoute(ctx, token, route, input, idempotencyKey, nil)
}

// PostMCP sends exactly one JSON-RPC message to the served MCP path. The
// response body is returned unmodified for the caller's MCP decoder: the MCP
// edge answers JSON-RPC refusals with HTTP 200, so only a non-200 status is a
// transport failure here.
func (client *API) PostMCP(ctx context.Context, token Secret, body []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > api.MaxYAMLRequestBytes {
		return nil, fmt.Errorf("the MCP request is empty or exceeds %d bytes", api.MaxYAMLRequestBytes)
	}
	target := *client.base
	target.Path = strings.TrimSuffix(client.base.Path, "/") + stageMCPPath
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, string(api.MethodPost), target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the MCP request")
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
			return nil, requestCtx.Err()
		}
		return nil, fmt.Errorf("the MCP endpoint is unavailable")
	}
	defer response.Body.Close()
	data, err := readBounded(response.Body, client.maxResponse)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, fmt.Errorf("the MCP endpoint redirected; redirects are never followed")
	}
	if response.StatusCode != api.HTTPStatusSuccess {
		return nil, fmt.Errorf("the MCP endpoint returned status %d", response.StatusCode)
	}
	return data, nil
}

// lookupAlternateRoute returns the additive alternate binding of one operation
// and method.
func lookupAlternateRoute(operation string, method api.Method) (api.Route, bool) {
	for _, route := range api.AlternateRoutes() {
		if route.Operation == operation && route.Method == method {
			return route, true
		}
	}
	return api.Route{}, false
}

// invoke sends exactly one canonical request. It never retries: a
// qualification probe must observe the stage's answer, not a client retry
// schedule.
func (client *API) invoke(ctx context.Context, method api.Method, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	route, ok := api.LookupRoute(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	if route.Method != method {
		return api.Result{}, fmt.Errorf("operation %q does not use %s", operation, method)
	}
	return client.invokeRoute(ctx, token, route, input, idempotencyKey, nil)
}

// invokeVersioned performs one canonical request carrying a positive expected
// version.
func (client *API) invokeVersioned(ctx context.Context, method api.Method, token Secret, operation string, input any, idempotencyKey string, expectedVersion uint64) (api.Result, error) {
	if expectedVersion == 0 {
		return api.Result{}, fmt.Errorf("operation %q requires a positive expected version", operation)
	}
	route, ok := api.LookupRoute(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	if route.Method != method {
		return api.Result{}, fmt.Errorf("operation %q does not use %s", operation, method)
	}
	return client.invokeRoute(ctx, token, route, input, idempotencyKey, &expectedVersion)
}

// invokeRoute sends exactly one request for one already-selected binding.
func (client *API) invokeRoute(ctx context.Context, token Secret, route api.Route, input any, idempotencyKey string, expectedVersion *uint64) (api.Result, error) {
	operation := route.Operation
	method := route.Method
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.Result{}, err
	}
	if operationRequiresIdempotencyKey(operation) && idempotencyKey == "" {
		return api.Result{}, fmt.Errorf("operation %q requires an idempotency key", operation)
	}
	raw, err := encodeInput(input)
	if err != nil {
		return api.Result{}, err
	}
	requestBody := api.Request{Input: raw, ExpectedVersion: expectedVersion, IdempotencyKey: idempotencyKey}
	if err := requestBody.Validate(); err != nil {
		return api.Result{}, err
	}
	path, err := expandRoutePath(route, raw)
	if err != nil {
		return api.Result{}, err
	}
	envelope, err := json.Marshal(requestBody)
	if err != nil {
		return api.Result{}, fmt.Errorf("encode the public request envelope")
	}
	if bound := api.MaxEnvelopeBytes(operation); len(envelope) > bound {
		return api.Result{}, fmt.Errorf("the request envelope exceeds %d bytes", bound)
	}

	target := *client.base
	target.Path = strings.TrimSuffix(client.base.Path, "/") + path
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, string(method), target.String(), bytes.NewReader(envelope))
	if err != nil {
		return api.Result{}, fmt.Errorf("build the public request")
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Accept", api.MediaType)
	if token.IsSet() {
		request.Header.Set("Authorization", "Bearer "+token.Reveal())
	}
	request.ContentLength = int64(len(envelope))

	requestCtx := ctx
	if client.requestTimeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, client.requestTimeout)
		defer cancel()
	}
	response, err := client.client.Do(request.WithContext(requestCtx))
	if err != nil {
		if requestCtx.Err() != nil {
			return api.Result{}, requestCtx.Err()
		}
		return api.Result{}, fmt.Errorf("the public endpoint is unavailable")
	}
	defer response.Body.Close()

	data, err := readBounded(response.Body, client.maxResponse)
	if err != nil {
		return api.Result{}, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return api.Result{}, fmt.Errorf("the public endpoint redirected; redirects are never followed")
	}
	if response.StatusCode == api.HTTPStatusSuccess {
		result, err := api.DecodeResult(data)
		if err != nil {
			return api.Result{}, fmt.Errorf("the public response is not a valid result")
		}
		if result.Operation != operation {
			return api.Result{}, fmt.Errorf("the public response names operation %q", result.Operation)
		}
		return result, nil
	}
	public, err := decodePublicError(data)
	if err != nil {
		return api.Result{}, fmt.Errorf("the public response is not a valid error")
	}
	status, ok := api.HTTPStatus(public.Code)
	if !ok || status != response.StatusCode {
		return api.Result{}, fmt.Errorf("the public response status does not match its code")
	}
	return api.Result{}, &APIError{Status: response.StatusCode, Code: public.Code, RequestID: public.RequestID}
}

// encodeInput marshals one operation input without inventing a value.
func encodeInput(input any) (json.RawMessage, error) {
	if input == nil {
		return nil, nil
	}
	if raw, ok := input.(json.RawMessage); ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode the operation input")
	}
	return encoded, nil
}

// decodePublicError decodes and validates one public error body.
func decodePublicError(data []byte) (api.Error, error) {
	var public api.Error
	if err := json.Unmarshal(data, &public); err != nil {
		return api.Error{}, err
	}
	if err := public.Validate(); err != nil {
		return api.Error{}, err
	}
	return public, nil
}

// readBounded reads one bounded response body.
func readBounded(reader io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = api.MaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("the public response body is unreadable")
	}
	if len(data) > limit {
		return nil, fmt.Errorf("the public response body exceeds %d bytes", limit)
	}
	return data, nil
}

// operationRequiresIdempotencyKey reports whether the frozen codec requires a
// caller-supplied key for this operation.
func operationRequiresIdempotencyKey(operation string) bool {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec.RequiresIdempotencyKey
		}
	}
	return false
}

// expandRoutePath substitutes the frozen route parameters from their canonical
// request fields.
func expandRoutePath(route api.Route, input json.RawMessage) (string, error) {
	fields := map[string]string{}
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != route.Operation {
			continue
		}
		for _, parameter := range codec.RouteParams {
			fields[parameter.Name] = parameter.Field
		}
		break
	}
	var parameters map[string]json.RawMessage
	if len(fields) > 0 {
		trimmed := bytes.TrimSpace(input)
		if len(trimmed) > 0 && string(trimmed) != "null" {
			if err := json.Unmarshal(trimmed, &parameters); err != nil {
				return "", fmt.Errorf("route parameters must be a JSON object")
			}
		}
	}
	segments := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	expanded := make([]string, 0, len(segments))
	for _, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			expanded = append(expanded, segment)
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		field, ok := fields[name]
		if !ok {
			return "", fmt.Errorf("route parameter %q has no canonical request field", name)
		}
		raw, ok := jsonField(parameters, field)
		if !ok {
			return "", fmt.Errorf("route parameter %q is missing", name)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("route parameter %q must be a string", name)
		}
		if !safePathParameter(value) {
			return "", fmt.Errorf("route parameter %q is unsafe", name)
		}
		expanded = append(expanded, value)
	}
	return "/" + strings.Join(expanded, "/"), nil
}

// jsonField resolves one dotted JSON field path.
func jsonField(parameters map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	if len(parameters) == 0 {
		return nil, false
	}
	segments := strings.Split(path, ".")
	current := parameters
	for index, segment := range segments {
		raw, ok := current[segment]
		if !ok {
			return nil, false
		}
		if index == len(segments)-1 {
			return raw, true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, false
		}
		current = next
	}
	return nil, false
}

// safePathParameter accepts only a bounded identity-shaped path segment.
func safePathParameter(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			return false
		}
	}
	return true
}
