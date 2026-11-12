package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// recordedRequest is one request observed by the fake RoundTripper.
type recordedRequest struct {
	method string
	url    *url.URL
	header http.Header
	body   []byte
}

// recordingRoundTripper records every request and delegates the response to
// respond by zero-based attempt index.
type recordingRoundTripper struct {
	mu       sync.Mutex
	requests []recordedRequest
	respond  func(req *http.Request, attempt int) (*http.Response, error)
}

func (roundTripper *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	roundTripper.mu.Lock()
	attempt := len(roundTripper.requests)
	roundTripper.requests = append(roundTripper.requests, recordedRequest{
		method: req.Method,
		url:    cloneURL(req.URL),
		header: req.Header.Clone(),
		body:   body,
	})
	respond := roundTripper.respond
	roundTripper.mu.Unlock()
	if respond == nil {
		return nil, errors.New("unexpected request")
	}
	return respond(req, attempt)
}

func (roundTripper *recordingRoundTripper) recorded() []recordedRequest {
	roundTripper.mu.Lock()
	defer roundTripper.mu.Unlock()
	return append([]recordedRequest(nil), roundTripper.requests...)
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// jsonResponse builds one JSON HTTP response.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// resultResponse builds one successful api.Result response.
func resultResponse(operation, body string) *http.Response {
	payload, err := json.Marshal(api.Result{Operation: operation, Body: json.RawMessage(body)})
	if err != nil {
		panic(err)
	}
	return jsonResponse(api.HTTPStatusSuccess, string(payload))
}

// errorResponse builds one public api.Error response.
func errorResponse(status int, code, message string) *http.Response {
	payload, err := json.Marshal(api.Error{Code: code, Message: message})
	if err != nil {
		panic(err)
	}
	return jsonResponse(status, string(payload))
}

// validCredential is a current execution credential.
func validCredential() client.Credential {
	return client.Credential{
		Kind:      client.KindExecution,
		Token:     "kodex.exec-1.signature",
		ExpiresAt: fixedNow().Add(time.Hour),
	}
}

func testPolicy() client.RetryPolicy {
	return client.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

// newTransport builds one HTTPTransport with deterministic clock and sleep.
func newTransport(roundTripper http.RoundTripper, source client.CredentialSource) *client.HTTPTransport {
	return &client.HTTPTransport{
		BaseURL:      "https://kodama.example.com",
		RoundTripper: roundTripper,
		Source:       source,
		Retry:        testPolicy(),
		Now:          fixedNow,
		Sleep:        func(time.Duration) {},
	}
}

// readInvocation is one GET operation.
func readInvocation() api.Invocation {
	return api.Invocation{
		Operation: "execution.inspect",
		Request: api.Request{
			Input:          json.RawMessage(`{"execution_id":"exec-1"}`),
			IdempotencyKey: "read-key-1",
		},
	}
}

// mutationInvocation is one PUT operation with a caller key and version.
func mutationInvocation() api.Invocation {
	return api.Invocation{
		Operation: "pipeline.apply",
		Request: api.Request{
			Input:           json.RawMessage(`{"project_id":"shop","name":"build","yaml":"bmFtZTogYnVpbGQK"}`),
			ExpectedVersion: new(uint64(7)),
			IdempotencyKey:  "caller-key-1",
		},
	}
}

// codecForOperation returns the codec row for one operation.
func codecForOperation(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

// setRouteField writes one canonical route field, following a dotted JSON path.
func setRouteField(fields map[string]any, path string, value any) {
	segments := strings.Split(path, ".")
	current := fields
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[segment] = next
		}
		current = next
	}
	current[segments[len(segments)-1]] = value
}

// deleteRouteField removes one canonical route field, following a dotted JSON
// path. It is only used to build a missing-field case.
func deleteRouteField(fields map[string]any, path string) {
	segments := strings.Split(path, ".")
	current := fields
	for _, segment := range segments[:len(segments)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, segments[len(segments)-1])
}

// routeTemplateParameter reports whether a route template segment is one
// {name} parameter.
func routeTemplateParameter(segment string) (string, bool) {
	if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
	if name == "" {
		return "", false
	}
	return name, true
}

// canonicalRouteFields builds one input object carrying every route parameter
// under the canonical request field published by the RouteParams table.
func canonicalRouteFields(codec api.OperationCodec) map[string]any {
	fields := make(map[string]any, len(codec.RouteParams))
	for _, param := range codec.RouteParams {
		setRouteField(fields, param.Field, "p-"+param.Name)
	}
	return fields
}

// routeInput derives the concrete path and canonical input object for one
// declared route. A route parameter is carried by its canonical request field
// (for example project -> project_id), never by the route-template name.
func routeInput(route api.Route) (json.RawMessage, string) {
	codec, ok := codecForOperation(route.Operation)
	if !ok {
		panic("no codec for " + route.Operation)
	}
	values := make(map[string]string, len(codec.RouteParams))
	for _, param := range codec.RouteParams {
		values[param.Name] = "p-" + param.Name
	}
	data, err := json.Marshal(canonicalRouteFields(codec))
	if err != nil {
		panic(err)
	}
	segments := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	expanded := make([]string, 0, len(segments))
	for _, segment := range segments {
		name, parameter := routeTemplateParameter(segment)
		if !parameter {
			expanded = append(expanded, segment)
			continue
		}
		value, ok := values[name]
		if !ok {
			panic("route parameter " + name + " has no canonical field")
		}
		expanded = append(expanded, value)
	}
	return data, "/" + strings.Join(expanded, "/")
}

// TestHTTPTransportBindsAllRoutes pins all 51 route bindings: method,
// concrete path from the input parameters, one JSON api.Request body including
// GET/DELETE, expected version, idempotency key and the bearer header.
func TestHTTPTransportBindsAllRoutes(t *testing.T) {
	routes := api.Routes()
	if len(routes) != 51 {
		t.Fatalf("api.Routes() length = %d; want 51", len(routes))
	}
	for _, route := range routes {
		t.Run(route.Operation, func(t *testing.T) {
			input, wantPath := routeInput(route)
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse(route.Operation, `{}`), nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			invocation := api.Invocation{
				Operation: route.Operation,
				Request: api.Request{
					Input:           input,
					ExpectedVersion: new(uint64(7)),
					IdempotencyKey:  "route-key-1",
				},
			}
			result, err := transport.Invoke(context.Background(), invocation)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.Operation != route.Operation {
				t.Fatalf("result operation = %q; want %q", result.Operation, route.Operation)
			}
			recorded := roundTripper.recorded()
			if len(recorded) != 1 {
				t.Fatalf("requests = %d; want 1", len(recorded))
			}
			request := recorded[0]
			if request.method != string(route.Method) {
				t.Errorf("method = %q; want %q", request.method, route.Method)
			}
			if request.url.Path != wantPath {
				t.Errorf("path = %q; want %q", request.url.Path, wantPath)
			}
			if request.url.RawQuery != "" {
				t.Errorf("query = %q; want none", request.url.RawQuery)
			}
			if got := request.header.Get("Authorization"); got != "Bearer "+validCredential().Token {
				t.Errorf("Authorization = %q", got)
			}
			if got := request.header.Get("Content-Type"); got != api.MediaType {
				t.Errorf("Content-Type = %q; want %q", got, api.MediaType)
			}
			if len(request.body) == 0 {
				t.Fatal("request body is empty; every method carries the api.Request JSON body")
			}
			var sent api.Request
			if err := json.Unmarshal(request.body, &sent); err != nil {
				t.Fatalf("request body is not api.Request JSON: %v", err)
			}
			if string(sent.Input) != string(input) {
				t.Errorf("input = %s; want %s", sent.Input, input)
			}
			if sent.ExpectedVersion == nil || *sent.ExpectedVersion != 7 {
				t.Errorf("expected_version = %v; want 7", sent.ExpectedVersion)
			}
			if sent.IdempotencyKey != "route-key-1" {
				t.Errorf("idempotency_key = %q", sent.IdempotencyKey)
			}
		})
	}
}

// TestHTTPTransportExpandsCanonicalRouteFields pins, for every parameterized
// operation in the registry, that the client reads each route parameter
// from the canonical request field published by api.OperationCodec.RouteParams
// and leaves that field in the request body for the REST edge to fill/compare.
func TestHTTPTransportExpandsCanonicalRouteFields(t *testing.T) {
	parameterized := 0
	for _, codec := range api.OperationCodecs() {
		if len(codec.RouteParams) == 0 {
			continue
		}
		parameterized++
		route, ok := api.LookupRoute(codec.Operation)
		if !ok {
			t.Fatalf("operation %q has no route", codec.Operation)
		}
		t.Run(codec.Operation, func(t *testing.T) {
			input, wantPath := routeInput(route)
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse(codec.Operation, `{}`), nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			result, err := transport.Invoke(context.Background(), api.Invocation{
				Operation: codec.Operation,
				Request:   api.Request{Input: input},
			})
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.Operation != codec.Operation {
				t.Fatalf("result operation = %q; want %q", result.Operation, codec.Operation)
			}
			recorded := roundTripper.recorded()
			if len(recorded) != 1 {
				t.Fatalf("requests = %d; want 1", len(recorded))
			}
			if recorded[0].url.Path != wantPath {
				t.Fatalf("path = %q; want %q", recorded[0].url.Path, wantPath)
			}
			var sent api.Request
			if err := json.Unmarshal(recorded[0].body, &sent); err != nil {
				t.Fatalf("request body is not api.Request JSON: %v", err)
			}
			if string(sent.Input) != string(input) {
				t.Fatalf("body input = %s; want the same canonical input %s", sent.Input, input)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(sent.Input, &fields); err != nil {
				t.Fatalf("input is not an object: %v", err)
			}
			for _, param := range codec.RouteParams {
				raw, ok := fields[param.Field]
				if !ok {
					t.Fatalf("body input %s dropped canonical field %q", sent.Input, param.Field)
				}
				var value string
				if err := json.Unmarshal(raw, &value); err != nil || value == "" {
					t.Fatalf("body field %q = %s; want the expanded string", param.Field, raw)
				}
			}
		})
	}
	if parameterized == 0 {
		t.Fatal("the registry has no parameterized operation")
	}
}

// TestHTTPTransportRefusesInvalidCanonicalRouteFields pins that a missing,
// non-string or unsafe canonical route field refuses as invalid_input before
// any request is sent.
func TestHTTPTransportRefusesInvalidCanonicalRouteFields(t *testing.T) {
	for _, codec := range api.OperationCodecs() {
		if len(codec.RouteParams) == 0 {
			continue
		}
		for _, param := range codec.RouteParams {
			t.Run(codec.Operation+"/"+param.Name, func(t *testing.T) {
				cases := []struct {
					name  string
					input func() json.RawMessage
				}{
					{"missing", func() json.RawMessage {
						fields := canonicalRouteFields(codec)
						deleteRouteField(fields, param.Field)
						return mustMarshalInput(t, fields)
					}},
					{"non-string", func() json.RawMessage {
						fields := canonicalRouteFields(codec)
						setRouteField(fields, param.Field, 7)
						return mustMarshalInput(t, fields)
					}},
					{"unsafe", func() json.RawMessage {
						fields := canonicalRouteFields(codec)
						setRouteField(fields, param.Field, "../escape")
						return mustMarshalInput(t, fields)
					}},
				}
				for _, testCase := range cases {
					t.Run(testCase.name, func(t *testing.T) {
						roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
							return resultResponse(codec.Operation, `{}`), nil
						}}
						transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
						_, err := transport.Invoke(context.Background(), api.Invocation{
							Operation: codec.Operation,
							Request:   api.Request{Input: testCase.input()},
						})
						if err == nil {
							t.Fatalf("Invoke accepted a %s route field %q", testCase.name, param.Field)
						}
						if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
							t.Fatalf("class = %v; want invalid_input", class)
						}
						if got := len(roundTripper.recorded()); got != 0 {
							t.Fatalf("requests = %d; want 0", got)
						}
					})
				}
			})
		}
	}
}

// TestHTTPTransportRefusesRouteNameInsteadOfCanonicalField pins that the
// top-level route-template name is not a canonical request field, so it
// refuses before any request.
func TestHTTPTransportRefusesRouteNameInsteadOfCanonicalField(t *testing.T) {
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return resultResponse("membership.list", `{}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	_, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "membership.list",
		Request:   api.Request{Input: json.RawMessage(`{"project":"project-1"}`)},
	})
	if err == nil {
		t.Fatal("Invoke accepted the route-template name instead of project_id")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("class = %v; want invalid_input", class)
	}
	if got := len(roundTripper.recorded()); got != 0 {
		t.Fatalf("requests = %d; want 0", got)
	}
}

// TestHTTPTransportMatchesRESTEdgeRouteRule confirms the pair end-to-end: the
// client's canonical input is accepted by an in-test handler that applies the
// same api.DecodeRESTRequest fill/conflict rule as the REST edge.
func TestHTTPTransportMatchesRESTEdgeRouteRule(t *testing.T) {
	var (
		mu        sync.Mutex
		seenPath  string
		seenInput json.RawMessage
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		route, params, ok := api.Match(api.Method(request.Method), request.URL.Path)
		if !ok {
			writePublicError(writer, fault.New(fault.NotFound, "no operation is bound to this method and path"))
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writePublicError(writer, fault.New(fault.InvalidInput, "request body is unreadable"))
			return
		}
		decoded, err := api.DecodeRequest(body)
		if err != nil {
			writePublicError(writer, err)
			return
		}
		if _, err := api.DecodeRESTRequest(route.Operation, decoded.Input, params); err != nil {
			writePublicError(writer, err)
			return
		}
		mu.Lock()
		seenPath = request.URL.Path
		seenInput = append(json.RawMessage(nil), decoded.Input...)
		mu.Unlock()
		payload, err := json.Marshal(api.Result{Operation: route.Operation, Body: json.RawMessage(`{"memberships":[]}`)})
		if err != nil {
			writePublicError(writer, fault.New(fault.InternalFailure, "internal failure"))
			return
		}
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(api.HTTPStatusSuccess)
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	transport := &client.HTTPTransport{
		BaseURL:      server.URL,
		RoundTripper: server.Client().Transport,
		Source:       &fakeSource{credentials: []client.Credential{validCredential()}},
		Retry:        testPolicy(),
		Now:          fixedNow,
		Sleep:        func(time.Duration) {},
	}
	result, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "membership.list",
		Request:   api.Request{Input: json.RawMessage(`{"project_id":"project-1"}`)},
	})
	if err != nil {
		t.Fatalf("canonical REST call failed: %v", err)
	}
	if result.Operation != "membership.list" {
		t.Fatalf("result operation = %q; want membership.list", result.Operation)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenPath != "/api/v1/projects/project-1/memberships" {
		t.Fatalf("edge saw path %q", seenPath)
	}
	if string(seenInput) != `{"project_id":"project-1"}` {
		t.Fatalf("edge saw input %s; want the canonical field retained", seenInput)
	}
}

// mustMarshalInput marshals one test input object.
func mustMarshalInput(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return data
}

// writePublicError writes one api.Error response using the public status map.
func writePublicError(writer http.ResponseWriter, err error) {
	code, message := api.CodeInternalFailure, "internal failure"
	var publicError *api.Error
	if errors.As(err, &publicError) && publicError != nil {
		code, message = publicError.Code, publicError.Message
	} else if faultCode, ok := fault.CodeOf(err); ok {
		code, message = string(faultCode), err.Error()
	}
	status, ok := api.HTTPStatus(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	payload, marshalErr := json.Marshal(api.Error{Code: code, Message: message})
	if marshalErr != nil {
		payload = []byte(`{"code":"internal_failure","message":"internal failure"}`)
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// TestHTTPTransportRefusesUnsafePathParameters pins the path-parameter
// boundary: traversal, separators, whitespace and control characters refuse
// before any request is sent.
func TestHTTPTransportRefusesUnsafePathParameters(t *testing.T) {
	cases := []string{"", ".", "..", "../secret", "a/b", `a\b`, "a b", "a\tb", "a?b", "a#b"}
	for _, value := range cases {
		roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
			return resultResponse("pipeline.read", `{}`), nil
		}}
		transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
		input, err := json.Marshal(map[string]string{"project_id": value, "pipeline_id": "build"})
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		_, err = transport.Invoke(context.Background(), api.Invocation{
			Operation: "pipeline.read",
			Request:   api.Request{Input: input},
		})
		if err == nil {
			t.Errorf("Invoke accepted path parameter %q", value)
			continue
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("path parameter %q: class = %v; want invalid_input", value, class)
		}
		if got := len(roundTripper.recorded()); got != 0 {
			t.Errorf("path parameter %q: requests = %d; want 0", value, got)
		}
	}

	// A missing parameter is equally invalid.
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return resultResponse("pipeline.read", `{}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	if _, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "pipeline.read",
		Request:   api.Request{Input: json.RawMessage(`{"project_id":"shop"}`)},
	}); err == nil {
		t.Fatal("Invoke accepted a missing route parameter")
	}
	if got := len(roundTripper.recorded()); got != 0 {
		t.Fatalf("requests = %d; want 0", got)
	}
}

// TestHTTPTransportReadRetriesDependencyFailure pins a read retry with a
// bounded backoff and a preserved request.
func TestHTTPTransportReadRetriesDependencyFailure(t *testing.T) {
	var sleeps []time.Duration
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, attempt int) (*http.Response, error) {
		if attempt == 0 {
			return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "try again"), nil
		}
		return resultResponse("execution.inspect", `{"status":"running"}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	transport.Sleep = func(delay time.Duration) { sleeps = append(sleeps, delay) }
	result, err := transport.Invoke(context.Background(), readInvocation())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Operation != "execution.inspect" {
		t.Fatalf("result operation = %q", result.Operation)
	}
	recorded := roundTripper.recorded()
	if len(recorded) != 2 {
		t.Fatalf("requests = %d; want 2", len(recorded))
	}
	if string(recorded[0].body) != string(recorded[1].body) {
		t.Fatalf("retry body changed:\n%s\n%s", recorded[0].body, recorded[1].body)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Millisecond {
		t.Fatalf("sleeps = %v; want [1ms]", sleeps)
	}
}

// TestHTTPTransportMutationRetriesWithSameKey pins that a lost response after a
// mutation is retried with exactly the same key, version and bytes; the client
// cannot distinguish crash-before-effect from lost-response-after-effect.
func TestHTTPTransportMutationRetriesWithSameKey(t *testing.T) {
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, attempt int) (*http.Response, error) {
		if attempt == 0 {
			return nil, errors.New("connection reset by peer")
		}
		return resultResponse("pipeline.apply", `{"version":8}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	invocation := mutationInvocation()
	result, err := transport.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Operation != "pipeline.apply" {
		t.Fatalf("result operation = %q", result.Operation)
	}
	recorded := roundTripper.recorded()
	if len(recorded) != 2 {
		t.Fatalf("requests = %d; want 2", len(recorded))
	}
	if string(recorded[0].body) != string(recorded[1].body) {
		t.Fatalf("retry body changed:\n%s\n%s", recorded[0].body, recorded[1].body)
	}
	var first, second api.Request
	if err := json.Unmarshal(recorded[0].body, &first); err != nil {
		t.Fatalf("first body: %v", err)
	}
	if err := json.Unmarshal(recorded[1].body, &second); err != nil {
		t.Fatalf("second body: %v", err)
	}
	if first.IdempotencyKey != "caller-key-1" || second.IdempotencyKey != "caller-key-1" {
		t.Fatalf("idempotency keys = %q, %q", first.IdempotencyKey, second.IdempotencyKey)
	}
	if second.ExpectedVersion == nil || *second.ExpectedVersion != 7 {
		t.Fatalf("retry expected version = %v", second.ExpectedVersion)
	}
	if string(first.Input) != string(second.Input) {
		t.Fatalf("retry input changed: %s vs %s", first.Input, second.Input)
	}
}

// TestHTTPTransportMutationWithoutKeyDoesNotRetry pins that an ambiguous
// mutation without a caller key is never replayed.
func TestHTTPTransportMutationWithoutKeyDoesNotRetry(t *testing.T) {
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "unavailable"), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	invocation := mutationInvocation()
	invocation.Request.IdempotencyKey = ""
	_, err := transport.Invoke(context.Background(), invocation)
	if err == nil {
		t.Fatal("Invoke accepted an ambiguous mutation without a key")
	}
	if code, ok := fault.CodeOf(err); !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("code = %v; want dependency_unavailable", code)
	}
	if got := len(roundTripper.recorded()); got != 1 {
		t.Fatalf("requests = %d; want 1", got)
	}
}

// TestHTTPTransportNeverRetriesTerminalMutationCodes pins that changed content,
// stale versions and uncertain external effects are never retried.
func TestHTTPTransportNeverRetriesTerminalMutationCodes(t *testing.T) {
	cases := []struct {
		code   string
		status int
		want   fault.Code
	}{
		{api.CodeChangedIdempotencyContent, http.StatusUnprocessableEntity, fault.CodeChangedIdempotencyContent},
		{api.CodeVersionConflict, http.StatusConflict, fault.CodeVersionConflict},
		{api.CodeUncertainExternalEffect, http.StatusBadGateway, fault.CodeUncertainExternalEffect},
	}
	for _, testCase := range cases {
		t.Run(testCase.code, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return errorResponse(testCase.status, testCase.code, "refused"), nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			_, err := transport.Invoke(context.Background(), mutationInvocation())
			if err == nil {
				t.Fatalf("Invoke accepted %s", testCase.code)
			}
			if code, ok := fault.CodeOf(err); !ok || code != testCase.want {
				t.Fatalf("code = %v; want %v", code, testCase.want)
			}
			if got := len(roundTripper.recorded()); got != 1 {
				t.Fatalf("requests = %d; want 1", got)
			}
		})
	}
}

// TestHTTPTransportExhaustsRetries pins the attempt bound and the exponential
// bounded backoff.
func TestHTTPTransportExhaustsRetries(t *testing.T) {
	var sleeps []time.Duration
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "unavailable"), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	transport.Sleep = func(delay time.Duration) { sleeps = append(sleeps, delay) }
	_, err := transport.Invoke(context.Background(), readInvocation())
	if err == nil {
		t.Fatal("Invoke succeeded after every attempt failed")
	}
	if code, ok := fault.CodeOf(err); !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("code = %v; want dependency_unavailable", code)
	}
	if got := len(roundTripper.recorded()); got != 3 {
		t.Fatalf("requests = %d; want MaxAttempts 3", got)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Millisecond || sleeps[1] != 2*time.Millisecond {
		t.Fatalf("sleeps = %v; want [1ms 2ms]", sleeps)
	}
}

// TestHTTPTransportReadsCredentialEveryAttempt pins current-token sourcing on
// every attempt, including a retry.
func TestHTTPTransportReadsCredentialEveryAttempt(t *testing.T) {
	source := &fakeSource{credentials: []client.Credential{
		{Kind: client.KindExecution, Token: "kodex.exec-1.first", ExpiresAt: fixedNow().Add(time.Hour)},
		{Kind: client.KindExecution, Token: "kodex.exec-1.second", ExpiresAt: fixedNow().Add(time.Hour)},
	}}
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, attempt int) (*http.Response, error) {
		if attempt == 0 {
			return errorResponse(http.StatusServiceUnavailable, api.CodeDependencyUnavailable, "retry"), nil
		}
		return resultResponse("execution.inspect", `{}`), nil
	}}
	transport := newTransport(roundTripper, source)
	if _, err := transport.Invoke(context.Background(), readInvocation()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if source.callCount() != 2 {
		t.Fatalf("credential reads = %d; want 2", source.callCount())
	}
	recorded := roundTripper.recorded()
	if len(recorded) != 2 {
		t.Fatalf("requests = %d; want 2", len(recorded))
	}
	if got := recorded[0].header.Get("Authorization"); got != "Bearer kodex.exec-1.first" {
		t.Errorf("first Authorization = %q", got)
	}
	if got := recorded[1].header.Get("Authorization"); got != "Bearer kodex.exec-1.second" {
		t.Errorf("retry Authorization = %q", got)
	}
}

// TestHTTPTransportRefusesUnusableCredential pins that no request is sent with
// a missing, failed, expired or wrongly-unknown-expiry credential.
func TestHTTPTransportRefusesUnusableCredential(t *testing.T) {
	cases := []struct {
		name   string
		source client.CredentialSource
	}{
		{"source error", &fakeSource{err: fault.New(fault.Unauthenticated, "session ended")}},
		{"expired execution", &fakeSource{credentials: []client.Credential{{
			Kind: client.KindExecution, Token: "old", ExpiresAt: fixedNow().Add(-time.Second),
		}}}},
		{"unknown-expiry session", &fakeSource{credentials: []client.Credential{{
			Kind: client.KindSession, Token: "human",
		}}}},
		{"unknown-expiry service", &fakeSource{credentials: []client.Credential{{
			Kind: client.KindService, Token: "service",
		}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse("execution.inspect", `{}`), nil
			}}
			transport := newTransport(roundTripper, testCase.source)
			_, err := transport.Invoke(context.Background(), readInvocation())
			if err == nil {
				t.Fatal("Invoke accepted an unusable credential")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
				t.Fatalf("class = %v; want unauthenticated", class)
			}
			if got := len(roundTripper.recorded()); got != 0 {
				t.Fatalf("requests = %d; want 0", got)
			}
		})
	}
}

// TestHTTPTransportAllowsMountedExecutionAndServiceCredentials pins that an
// opaque mounted execution credential and a service credential authenticate.
func TestHTTPTransportAllowsMountedExecutionAndServiceCredentials(t *testing.T) {
	cases := []client.Credential{
		{Kind: client.KindExecution, Token: "kodex.exec-1.opaque"},
		{Kind: client.KindService, Token: "kodex.service-1.signature", ExpiresAt: fixedNow().Add(time.Hour)},
	}
	for _, credential := range cases {
		roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
			return resultResponse("execution.inspect", `{}`), nil
		}}
		transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{credential}})
		if _, err := transport.Invoke(context.Background(), readInvocation()); err != nil {
			t.Fatalf("%s credential refused: %v", credential.Kind, err)
		}
		recorded := roundTripper.recorded()
		if len(recorded) != 1 {
			t.Fatalf("%s: requests = %d; want 1", credential.Kind, len(recorded))
		}
		if got := recorded[0].header.Get("Authorization"); got != "Bearer "+credential.Token {
			t.Errorf("%s Authorization = %q", credential.Kind, got)
		}
	}
}

// TestHTTPTransportCancellation pins that cancellation/deadline is returned as
// the context error, sends no further request and is never retried.
func TestHTTPTransportCancellation(t *testing.T) {
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return resultResponse("execution.inspect", `{}`), nil
	}}
	source := &fakeSource{credentials: []client.Credential{validCredential()}}
	transport := newTransport(roundTripper, source)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transport.Invoke(canceled, readInvocation()); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v; want context.Canceled", err)
	}
	if got := len(roundTripper.recorded()); got != 0 {
		t.Fatalf("pre-canceled requests = %d; want 0", got)
	}

	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := transport.Invoke(expired, readInvocation()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline error = %v; want context.DeadlineExceeded", err)
	}
	if got := len(roundTripper.recorded()); got != 0 {
		t.Fatalf("expired deadline requests = %d; want 0", got)
	}

	inFlight, cancelInFlight := context.WithCancel(context.Background())
	roundTripper.respond = func(req *http.Request, _ int) (*http.Response, error) {
		cancelInFlight()
		return nil, req.Context().Err()
	}
	_, err := transport.Invoke(inFlight, readInvocation())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight error = %v; want context.Canceled", err)
	}
	if got := len(roundTripper.recorded()); got != 1 {
		t.Fatalf("in-flight requests = %d; want 1", got)
	}
}

// TestHTTPTransportRefusesMalformedResponses pins malformed, oversized,
// mismatched and wrong-operation responses as internal_failure without a
// mutation retry.
func TestHTTPTransportRefusesMalformedResponses(t *testing.T) {
	cases := []struct {
		name             string
		response         *http.Response
		maxResponseBytes int
	}{
		{"malformed json", jsonResponse(http.StatusOK, "not json"), 0},
		{"empty body", jsonResponse(http.StatusOK, ""), 0},
		{"error body on success", jsonResponse(http.StatusOK, `{"code":"not_found","message":"missing"}`), 0},
		{"oversized", jsonResponse(http.StatusOK, `{"operation":"pipeline.apply","body":{"blob":"`+strings.Repeat("x", 64)+`"}}`), 32},
		{"status mismatch", errorResponse(http.StatusServiceUnavailable, api.CodeNotFound, "missing"), 0},
		{"unknown code", errorResponse(http.StatusBadRequest, "teapot", "short and stout"), 0},
		{"wrong operation", resultResponse("task.read", `{}`), 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return testCase.response, nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			transport.MaxResponseBytes = testCase.maxResponseBytes
			_, err := transport.Invoke(context.Background(), mutationInvocation())
			if err == nil {
				t.Fatal("Invoke accepted a malformed response")
			}
			if code, ok := fault.CodeOf(err); !ok || code != fault.CodeInternalFailure {
				t.Fatalf("code = %v; want internal_failure", code)
			}
			if got := len(roundTripper.recorded()); got != 1 {
				t.Fatalf("requests = %d; want 1", got)
			}
		})
	}
}

// TestHTTPTransportRefusesRedirects pins that a 3xx is never followed.
func TestHTTPTransportRefusesRedirects(t *testing.T) {
	redirect := jsonResponse(http.StatusFound, "")
	redirect.Header.Set("Location", "https://evil.example.com/api/v1/tasks/task-1")
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, attempt int) (*http.Response, error) {
		if attempt == 0 {
			return redirect, nil
		}
		return resultResponse("task.read", `{}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	_, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "task.read",
		Request:   api.Request{Input: json.RawMessage(`{"task_id":"task-1"}`)},
	})
	if err == nil {
		t.Fatal("Invoke followed a redirect")
	}
	if code, ok := fault.CodeOf(err); !ok || code != fault.CodeInternalFailure {
		t.Fatalf("code = %v; want internal_failure", code)
	}
	recorded := roundTripper.recorded()
	if len(recorded) != 1 {
		t.Fatalf("requests = %d; want 1 (no redirect follow)", len(recorded))
	}
	if recorded[0].url.Host != "kodama.example.com" {
		t.Fatalf("request host = %q; want kodama.example.com", recorded[0].url.Host)
	}
}

// TestHTTPTransportRefusesInvalidInvocations pins client-side refusals before
// any request: unknown operations, bad keys, oversized bodies and invalid
// transport configuration.
func TestHTTPTransportRefusesInvalidInvocations(t *testing.T) {
	oversizedInput := json.RawMessage(`{"blob":"` + strings.Repeat("a", api.MaxRequestBytes) + `"}`)
	cases := []struct {
		name       string
		transport  *client.HTTPTransport
		invocation api.Invocation
	}{
		{
			name:       "unknown operation",
			transport:  newTransport(&recordingRoundTripper{}, &fakeSource{credentials: []client.Credential{validCredential()}}),
			invocation: api.Invocation{Operation: "bogus.operation"},
		},
		{
			name:      "invalid idempotency key",
			transport: newTransport(&recordingRoundTripper{}, &fakeSource{credentials: []client.Credential{validCredential()}}),
			invocation: api.Invocation{
				Operation: "task.read",
				Request:   api.Request{Input: json.RawMessage(`{"task_id":"task-1"}`), IdempotencyKey: "bad key"},
			},
		},
		{
			name:      "oversized request",
			transport: newTransport(&recordingRoundTripper{}, &fakeSource{credentials: []client.Credential{validCredential()}}),
			invocation: api.Invocation{
				Operation: "task.read",
				Request:   api.Request{Input: oversizedInput},
			},
		},
		{
			name: "invalid base URL",
			transport: &client.HTTPTransport{
				BaseURL:      "kodama.example.com",
				RoundTripper: &recordingRoundTripper{},
				Source:       &fakeSource{credentials: []client.Credential{validCredential()}},
				Retry:        testPolicy(),
				Now:          fixedNow,
				Sleep:        func(time.Duration) {},
			},
			invocation: readInvocation(),
		},
		{
			name: "invalid retry policy",
			transport: &client.HTTPTransport{
				BaseURL:      "https://kodama.example.com",
				RoundTripper: &recordingRoundTripper{},
				Source:       &fakeSource{credentials: []client.Credential{validCredential()}},
				Retry:        client.RetryPolicy{},
				Now:          fixedNow,
				Sleep:        func(time.Duration) {},
			},
			invocation: readInvocation(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.transport.Invoke(context.Background(), testCase.invocation)
			if err == nil {
				t.Fatal("Invoke accepted an invalid invocation")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v; want invalid_input", class)
			}
		})
	}
}

// TestHTTPTransportRefusesCleartextBearerOutsideLoopback pins the S05 HTTPS
// Bearer rule at the transport boundary: a base URL that would send a token in
// cleartext to a non-loopback host refuses as invalid_input before any
// credential read or request, while loopback development URLs stay usable.
func TestHTTPTransportRefusesCleartextBearerOutsideLoopback(t *testing.T) {
	refused := []string{
		"http://kodama.example.com",
		"http://kodama.example.com:8080",
		"http://localhost.evil.com",
		"http://127.0.0.1.evil.com",
		"http://user@127.0.0.1:8080",
	}
	for _, baseURL := range refused {
		t.Run("refuse "+baseURL, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{}
			source := &fakeSource{credentials: []client.Credential{validCredential()}}
			transport := newTransport(roundTripper, source)
			transport.BaseURL = baseURL
			_, err := transport.Invoke(context.Background(), readInvocation())
			if err == nil {
				t.Fatalf("Invoke accepted cleartext base URL %q", baseURL)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v; want invalid_input", class)
			}
			if calls := len(roundTripper.recorded()); calls != 0 {
				t.Fatalf("RoundTrip calls = %d; want 0", calls)
			}
			if calls := source.callCount(); calls != 0 {
				t.Fatalf("credential reads = %d; want 0", calls)
			}
		})
	}

	accepted := []struct{ baseURL, scheme, host string }{
		{"http://127.0.0.1:8080", "http", "127.0.0.1:8080"},
		{"http://[::1]:8080", "http", "[::1]:8080"},
		{"http://localhost:8080", "http", "localhost:8080"},
		{"https://kodama.example.com", "https", "kodama.example.com"},
	}
	for _, testCase := range accepted {
		t.Run("accept "+testCase.baseURL, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse("execution.inspect", `{}`), nil
			}}
			source := &fakeSource{credentials: []client.Credential{validCredential()}}
			transport := newTransport(roundTripper, source)
			transport.BaseURL = testCase.baseURL
			if _, err := transport.Invoke(context.Background(), readInvocation()); err != nil {
				t.Fatalf("Invoke(%q): %v", testCase.baseURL, err)
			}
			recorded := roundTripper.recorded()
			if len(recorded) != 1 {
				t.Fatalf("requests = %d; want 1", len(recorded))
			}
			if recorded[0].url.Scheme != testCase.scheme || recorded[0].url.Host != testCase.host {
				t.Fatalf("target = %s://%s; want %s://%s",
					recorded[0].url.Scheme, recorded[0].url.Host, testCase.scheme, testCase.host)
			}
			if calls := source.callCount(); calls != 1 {
				t.Fatalf("credential reads = %d; want 1", calls)
			}
		})
	}
}
