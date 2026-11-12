package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// HTTPTransport is the concrete public REST transport. It implements Transport
// over an injected http.RoundTripper and reads Source on every attempt. Zero
// Now/Sleep fields fall back to time.Now/time.Sleep; a zero MaxResponseBytes
// falls back to api.MaxResponseBytes.
type HTTPTransport struct {
	BaseURL          string
	RoundTripper     http.RoundTripper
	Source           CredentialSource
	Retry            RetryPolicy
	Now              func() time.Time
	Sleep            func(time.Duration)
	MaxResponseBytes int
}

var _ Transport = (*HTTPTransport)(nil)

// Invoke sends one operation over its declared REST binding. The request envelope
// is built once, so every retry preserves the exact bytes, idempotency key and
// expected version while the credential source is read again.
func (transport *HTTPTransport) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.Result{}, err
	}

	route, body, target, err := transport.prepare(invocation)
	if err != nil {
		return api.Result{}, err
	}

	now := time.Now
	if transport.Now != nil {
		now = transport.Now
	}
	sleep := transport.Sleep

	mutation := route.Method != api.MethodGet
	hasKey := invocation.Request.IdempotencyKey != ""

	var lastErr error
	for attempt := 1; attempt <= transport.Retry.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return api.Result{}, err
		}
		header, err := Bearer(ctx, transport.Source, now())
		if err != nil {
			return api.Result{}, err
		}
		request, err := http.NewRequestWithContext(ctx, string(route.Method), target.String(), bytes.NewReader(body))
		if err != nil {
			return api.Result{}, fault.New(fault.InternalFailure, "cannot build the public request")
		}
		request.Header.Set("Content-Type", api.MediaType)
		request.Header.Set("Accept", api.MediaType)
		request.Header.Set("Authorization", header)
		request.ContentLength = int64(len(body))

		response, err := transport.RoundTripper.RoundTrip(request)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return api.Result{}, contextErr
			}
			lastErr = fault.New(fault.DependencyUnavailable, "the public endpoint is unavailable")
			if !RetryAllowed(fault.CodeDependencyUnavailable, mutation, hasKey) || attempt == transport.Retry.MaxAttempts {
				return api.Result{}, lastErr
			}
			if err := wait(ctx, sleep, transport.Retry, attempt); err != nil {
				return api.Result{}, err
			}
			continue
		}

		result, failure, retryable := transport.decode(route.Operation, response, mutation, hasKey)
		if failure == nil {
			return result, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return api.Result{}, contextErr
		}
		lastErr = failure
		if !retryable || attempt == transport.Retry.MaxAttempts {
			return api.Result{}, failure
		}
		if err := wait(ctx, sleep, transport.Retry, attempt); err != nil {
			return api.Result{}, err
		}
	}
	return api.Result{}, lastErr
}

// prepare validates the invocation and transport configuration, resolves the
// declared route and returns the concrete target, the one request body and the
// route. Every refusal here is invalid_input before any request is sent.
func (transport *HTTPTransport) prepare(invocation api.Invocation) (api.Route, []byte, *url.URL, error) {
	if err := invocation.Validate(); err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, publicMessage(err))
	}
	route, ok := api.LookupRoute(invocation.Operation)
	if !ok {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, "unknown operation")
	}
	route, err := selectBinding(route, invocation.Request)
	if err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, err.Error())
	}
	if transport.RoundTripper == nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, "the HTTP transport has no RoundTripper")
	}
	if transport.Source == nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, "the HTTP transport has no credential source")
	}
	if err := transport.Retry.Validate(); err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, err.Error())
	}
	base, err := parseBaseURL(transport.BaseURL)
	if err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, err.Error())
	}
	path, err := expandRoute(route, invocation.Request.Input)
	if err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, err.Error())
	}
	body, err := json.Marshal(invocation.Request)
	if err != nil {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, "the request envelope is not encodable")
	}
	if bound := api.MaxEnvelopeBytes(invocation.Operation); len(body) > bound {
		return api.Route{}, nil, nil, fault.New(fault.InvalidInput, fmt.Sprintf(
			"the request envelope is %d bytes; at most %d are allowed", len(body), bound))
	}
	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + path
	target.RawPath = ""
	return route, body, &target, nil
}

// selectBinding chooses the concrete REST binding of one invocation. Every
// operation keeps its canonical route except pipeline.start: a stored pipeline
// id keeps the canonical start route, while an inline YAML document uses the
// additive task-free executions binding. Exactly one selection is required and
// an inline selection never carries an expected pipeline version; every
// violation is invalid_input before any request.
func selectBinding(canonical api.Route, request api.Request) (api.Route, error) {
	if canonical.Operation != "pipeline.start" {
		return canonical, nil
	}
	var selection api.PipelineStartRequest
	trimmed := bytes.TrimSpace(request.Input)
	if len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &selection); err != nil {
			return api.Route{}, fmt.Errorf("pipeline.start input is not a valid selection: %v", err)
		}
	}
	stored := selection.PipelineID != ""
	inline := len(selection.YAML) > 0
	switch {
	case stored && inline:
		return api.Route{}, fmt.Errorf("pipeline.start accepts exactly one of pipeline_id or yaml")
	case !stored && !inline:
		return api.Route{}, fmt.Errorf("pipeline.start requires exactly one of pipeline_id or yaml")
	case inline:
		if request.ExpectedVersion != nil {
			return api.Route{}, fmt.Errorf("an inline pipeline.start does not accept an expected_version")
		}
		for _, alternate := range api.AlternateRoutes() {
			if alternate.Operation == "pipeline.start" {
				return alternate, nil
			}
		}
		return api.Route{}, fmt.Errorf("pipeline.start has no alternate binding")
	}
	return canonical, nil
}

// decode reads one bounded response and returns either a result or a public
// fault plus whether a retry is allowed. A redirect, malformed body or
// inconsistent status is internal_failure and never retried.
func (transport *HTTPTransport) decode(operation string, response *http.Response, mutation, hasKey bool) (api.Result, error, bool) {
	if response == nil {
		return api.Result{}, fault.New(fault.InternalFailure, "the HTTP transport returned no response"), false
	}
	if response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return api.Result{}, fault.New(fault.InternalFailure, "redirect responses are never followed"), false
	}
	max := transport.MaxResponseBytes
	if max <= 0 {
		max = api.MaxResponseBytes
	}
	data, err := readBounded(response.Body, max)
	if err != nil {
		return api.Result{}, fault.New(fault.InternalFailure, "the response body is unreadable or oversized"), false
	}
	if response.StatusCode == api.HTTPStatusSuccess {
		result, err := api.DecodeResult(data)
		if err != nil {
			return api.Result{}, fault.New(fault.InternalFailure, "the response is not a valid public result"), false
		}
		if result.Operation != operation {
			return api.Result{}, fault.New(fault.InternalFailure, "the response names a different operation"), false
		}
		return result, nil, false
	}
	public, err := decodeError(data)
	if err != nil {
		return api.Result{}, fault.New(fault.InternalFailure, "the response is not a valid public error"), false
	}
	status, ok := api.HTTPStatus(public.Code)
	if !ok || status != response.StatusCode {
		return api.Result{}, fault.New(fault.InternalFailure, "the response status does not match its public code"), false
	}
	class, ok := fault.ClassFor(fault.Code(public.Code))
	if !ok {
		return api.Result{}, fault.New(fault.InternalFailure, "the response code is not a public code"), false
	}
	failure := fault.New(class, public.Message)
	return api.Result{}, failure, RetryAllowed(fault.Code(public.Code), mutation, hasKey)
}

// parseBaseURL validates one absolute http(s) base URL without userinfo, query
// or fragment.
func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("base URL is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("base URL is invalid: %v", err)
	}
	if parsed.Opaque != "" {
		return nil, fmt.Errorf("base URL must not be opaque")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		// S05 requires HTTPS Bearer tokens. Cleartext is only acceptable for a
		// development loopback endpoint, never for a remote host.
		if !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("base URL must use https outside loopback")
		}
	default:
		return nil, fmt.Errorf("base URL scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("base URL must be absolute")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("base URL must not carry user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("base URL must not carry a query or fragment")
	}
	return parsed, nil
}

// isLoopbackHost reports whether host is a development loopback host that may
// carry a bearer token over cleartext. The parsed URL hostname is compared
// exactly, so lookalikes such as "localhost.evil.com" or "127.0.0.1.evil.com"
// are refused.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// expandRoute substitutes the route parameters from their canonical
// request fields, published by api.OperationCodec.RouteParams. The REST edge
// strictly decodes the same typed field and fills/compares it from the route,
// so the client reads the canonical field (for example project -> project_id)
// and leaves it in the request body. A missing, non-string or unsafe value
// refuses before any request.
func expandRoute(route api.Route, input json.RawMessage) (string, error) {
	segments := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	fields, err := routeParamFields(route.Operation)
	if err != nil {
		return "", err
	}
	var parameters map[string]json.RawMessage
	if len(fields) > 0 {
		trimmed := strings.TrimSpace(string(input))
		if trimmed != "" && trimmed != "null" {
			if err := json.Unmarshal([]byte(trimmed), &parameters); err != nil {
				return "", fmt.Errorf("route parameters must be a JSON object")
			}
		}
	}
	expanded := make([]string, 0, len(segments))
	for _, segment := range segments {
		name, ok := routeParameter(segment)
		if !ok {
			expanded = append(expanded, segment)
			continue
		}
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
		if !validPathParameter(value) {
			return "", fmt.Errorf("route parameter %q is unsafe", name)
		}
		expanded = append(expanded, value)
	}
	return "/" + strings.Join(expanded, "/"), nil
}

// routeParamFields returns the route-template name to canonical request field
// table of one operation.
func routeParamFields(operation string) (map[string]string, error) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		fields := make(map[string]string, len(codec.RouteParams))
		for _, param := range codec.RouteParams {
			fields[param.Name] = param.Field
		}
		return fields, nil
	}
	return nil, fmt.Errorf("operation %q has no public codec", operation)
}

// jsonField resolves one dotted JSON field path inside a decoded input object.
// A missing segment, a non-object intermediate value or an empty object
// reports absence.
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

// routeParameter reports whether a route template segment is one {name}
// parameter and returns its name.
func routeParameter(segment string) (string, bool) {
	if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
	if name == "" {
		return "", false
	}
	return name, true
}

// validPathParameter refuses separators, whitespace, control characters and the
// dot segments that could escape the declared route.
func validPathParameter(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		switch character {
		case '/', '\\', '?', '#':
			return false
		}
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// readBounded reads at most max bytes and refuses an oversized body.
func readBounded(reader io.Reader, max int) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("response body is missing")
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf("response body exceeds %d bytes", max)
	}
	return data, nil
}

// decodeError decodes exactly one public error object with no trailing data.
func decodeError(data []byte) (api.Error, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var public api.Error
	if err := decoder.Decode(&public); err != nil {
		return api.Error{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return api.Error{}, fmt.Errorf("trailing data after the public error")
	}
	if err := public.Validate(); err != nil {
		return api.Error{}, err
	}
	return public, nil
}

// wait sleeps the bounded exponential backoff before the next attempt.
func wait(ctx context.Context, sleep func(time.Duration), policy RetryPolicy, failedAttempt int) error {
	delay := backoff(policy, failedAttempt)
	if sleep != nil {
		sleep(delay)
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backoff returns BaseDelay*2^(failedAttempt-1) capped at MaxDelay.
func backoff(policy RetryPolicy, failedAttempt int) time.Duration {
	delay := policy.BaseDelay
	for index := 1; index < failedAttempt; index++ {
		if delay > policy.MaxDelay/2 {
			return policy.MaxDelay
		}
		delay *= 2
	}
	if delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}

// publicMessage returns the public message of an api.Error or the error text.
func publicMessage(err error) string {
	if public, ok := errors.AsType[*api.Error](err); ok {
		return public.Message
	}
	return err.Error()
}

// FileCredentialSource reads one opaque mounted execution token on every
// Credential call. The file contains only the token; surrounding whitespace is
// trimmed. The credential kind is KindExecution and its expiry is unknown
// because the file never carries one.
type FileCredentialSource struct {
	Path     string
	ReadFile func(string) ([]byte, error) // nil means os.ReadFile
}

var _ CredentialSource = FileCredentialSource{}

// Credential reads the mounted token again for every authentication. An empty
// path, read failure, empty or oversized file and a canceled context all refuse
// with the stable unauthenticated class and carry no credential material.
func (source FileCredentialSource) Credential(ctx context.Context) (Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, fault.Wrap(fault.Unauthenticated, "mounted credential read canceled", err)
	}
	if source.Path == "" {
		return Credential{}, fault.New(fault.Unauthenticated, "mounted credential path is empty")
	}
	read := source.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(source.Path)
	if err != nil {
		return Credential{}, fault.New(fault.Unauthenticated, "mounted credential is unreadable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return Credential{}, fault.New(fault.Unauthenticated, "mounted credential is empty")
	}
	if len(token) > api.MaxCredentialBytes {
		return Credential{}, fault.New(fault.Unauthenticated, "mounted credential exceeds the public bound")
	}
	return Credential{Kind: KindExecution, Token: token}, nil
}
