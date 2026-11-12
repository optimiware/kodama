//go:build e2e

package harness

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

// API is the e2e-only public REST helper. It speaks the public envelope over
// verified TLS and never follows a redirect.
type API struct {
	base           *url.URL
	client         *http.Client
	maxResponse    int
	requestTimeout time.Duration
}

// APIError is one public refusal decoded from the public error body. It never
// carries a credential; Raw keeps the exact response bytes so a probe can scan
// the whole wire, not only the message, for credential material.
type APIError struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Raw       []byte
}

// Error renders the public refusal without request or credential material.
func (refusal *APIError) Error() string {
	return fmt.Sprintf("public request refused: %s (status %d)", refusal.Code, refusal.Status)
}

// Call is one public operation invocation.
type Call struct {
	Method          api.Method
	Token           Secret
	Operation       string
	Input           any
	IdempotencyKey  string
	ExpectedVersion *uint64
}

// NewAPI builds the e2e API helper. A private installation supplies its CA
// file; without one the system roots are used. The TLS server name is inferred
// from the URL unless an explicit name is configured for a port-forward whose
// loopback host differs from the served name.
func NewAPI(config Config) (*API, error) {
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
	return &API{
		base:           base,
		client:         httpClient,
		maxResponse:    api.MaxResponseBytes,
		requestTimeout: defaultAPITimeout,
	}, nil
}

// newHTTPClient builds the verified-TLS client shared by the REST and MCP
// edges. A private installation supplies its CA file; without one the system
// roots are used. The TLS server name is inferred from the URL unless an
// explicit name is configured for a port-forward whose loopback host differs
// from the served name. Redirects are never followed.
func newHTTPClient(config Config, base *url.URL) (*http.Client, error) {
	transport, err := NewVerifiedTransport(config)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// NewVerifiedTransport builds the verified-TLS RoundTripper shared by the REST
// and MCP helpers and by the in-process CLI edge. A port-forward installation
// needs the same explicit server name on every edge; sharing this builder keeps
// one TLS policy rather than three.
func NewVerifiedTransport(config Config) (http.RoundTripper, error) {
	normalized, err := validateAPIURL(config.APIURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", InputAPIURL, err)
	}
	serverName := base.Hostname()
	if config.TLSServerName != "" {
		if err := validateDNSName(config.TLSServerName); err != nil {
			return nil, fmt.Errorf("%s: %w", InputTLSServerName, err)
		}
		serverName = config.TLSServerName
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
	if config.CAPEM != "" {
		caPEM, err := os.ReadFile(config.CAPEM)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read the CA file", InputCAPEM)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("%s: the CA file contains no certificate", InputCAPEM)
		}
		tlsConfig.RootCAs = pool
	}

	return &http.Transport{
		Proxy:             nil,
		TLSClientConfig:   tlsConfig,
		ForceAttemptHTTP2: true,
	}, nil
}

// Get performs one authenticated GET of a public operation.
func (client *API) Get(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.Invoke(ctx, Call{Method: api.MethodGet, Token: token, Operation: operation, Input: input, IdempotencyKey: idempotencyKey})
}

// Post performs one authenticated POST of a public operation.
func (client *API) Post(ctx context.Context, token Secret, operation string, input any, idempotencyKey string) (api.Result, error) {
	return client.Invoke(ctx, Call{Method: api.MethodPost, Token: token, Operation: operation, Input: input, IdempotencyKey: idempotencyKey})
}

// Invoke sends exactly one request. It never retries: an e2e probe must observe
// the installation's answer, not a client retry schedule.
func (client *API) Invoke(ctx context.Context, call Call) (api.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.Result{}, err
	}
	route, err := selectRoute(call.Method, call.Operation, call.Input)
	if err != nil {
		return api.Result{}, err
	}
	if operationRequiresIdempotencyKey(call.Operation) && call.IdempotencyKey == "" {
		return api.Result{}, fmt.Errorf("operation %q requires an idempotency key", call.Operation)
	}
	raw, err := encodeInput(call.Input)
	if err != nil {
		return api.Result{}, err
	}
	requestBody := api.Request{Input: raw, ExpectedVersion: call.ExpectedVersion, IdempotencyKey: call.IdempotencyKey}
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
	if bound := api.MaxEnvelopeBytes(call.Operation); len(envelope) > bound {
		return api.Result{}, fmt.Errorf("the request envelope exceeds %d bytes", bound)
	}

	target := *client.base
	target.Path = strings.TrimSuffix(client.base.Path, "/") + path
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, string(call.Method), target.String(), bytes.NewReader(envelope))
	if err != nil {
		return api.Result{}, fmt.Errorf("build the public request")
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Accept", api.MediaType)
	if call.Token.IsSet() {
		request.Header.Set("Authorization", "Bearer "+call.Token.Reveal())
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
		if result.Operation != call.Operation {
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
	return api.Result{}, &APIError{Status: response.StatusCode, Code: public.Code, Message: public.Message, RequestID: public.RequestID, Raw: data}
}

// selectRoute resolves the one declared binding a call must use. pipeline.start
// has two bindings; an inline task-free selection uses the alternate
// executions route, while a stored selection keeps the canonical route.
func selectRoute(method api.Method, operation string, input any) (api.Route, error) {
	route, ok := api.LookupRoute(operation)
	if !ok {
		return api.Route{}, fmt.Errorf("unknown public operation %q", operation)
	}
	if route.Method != method {
		return api.Route{}, fmt.Errorf("operation %q does not use %s", operation, method)
	}
	if operation == "pipeline.start" && inlinePipelineStart(input) {
		for _, alternate := range api.AlternateRoutes() {
			if alternate.Operation == operation && alternate.Method == method {
				return alternate, nil
			}
		}
		return api.Route{}, fmt.Errorf("operation %q has no inline binding", operation)
	}
	return route, nil
}

// inlinePipelineStart reports whether the input selects inline YAML without a
// stored pipeline id.
func inlinePipelineStart(input any) bool {
	switch typed := input.(type) {
	case api.PipelineStartRequest:
		return typed.PipelineID == "" && len(typed.YAML) > 0
	case *api.PipelineStartRequest:
		return typed != nil && typed.PipelineID == "" && len(typed.YAML) > 0
	case json.RawMessage:
		var request api.PipelineStartRequest
		if err := json.Unmarshal(typed, &request); err != nil {
			return false
		}
		return request.PipelineID == "" && len(request.YAML) > 0
	default:
		return false
	}
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

// operationRequiresIdempotencyKey reports whether the operation codec requires
// a caller-supplied key for this operation.
func operationRequiresIdempotencyKey(operation string) bool {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec.RequiresIdempotencyKey
		}
	}
	return false
}

// routeParameters returns the route-parameter-name to canonical-request-field
// map of one operation, independent of which binding is used.
func routeParameters(operation string) map[string]string {
	fields := map[string]string{}
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		for _, parameter := range codec.RouteParams {
			fields[parameter.Name] = parameter.Field
		}
		break
	}
	return fields
}

// expandRoutePath substitutes the declared route parameters from their
// canonical request fields.
func expandRoutePath(route api.Route, input json.RawMessage) (string, error) {
	fields := routeParameters(route.Operation)
	var parameters map[string]json.RawMessage
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &parameters); err != nil {
			return "", fmt.Errorf("route parameters must be a JSON object")
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
