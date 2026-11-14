package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// TestEdgeRoutesSixFixedEndpoints pins the six product-fixed S08 endpoints to
// their exact operations and path parameters.
func TestEdgeRoutesSixFixedEndpoints(t *testing.T) {
	cases := []struct {
		method    string
		path      string
		operation string
		params    map[string]string
	}{
		{"PUT", "/api/v1/executions/exec-1/actions/action-1/outputs", "execution.action.outputs", map[string]string{"execution": "exec-1", "action": "action-1"}},
		{"POST", "/api/v1/executions/exec-1/actions/action-1/resolve", "execution.request.resolve", map[string]string{"execution": "exec-1", "action": "action-1"}},
		{"POST", "/api/v1/tasks/task-1/ask", "task.ask", map[string]string{"task": "task-1"}},
		{"POST", "/api/v1/tasks/task-1/reply", "task.reply", map[string]string{"task": "task-1"}},
		{"POST", "/api/v1/verify", "actor.verify", map[string]string{}},
		{"GET", "/api/v1/executions/exec-1", "execution.inspect", map[string]string{"execution": "exec-1"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path,
				[]byte(`{"input":{},"idempotency_key":"key-1"}`), "Bearer token-1")
			if response.status != http.StatusOK {
				t.Fatalf("%s %s status = %d; want 200 (body %s)",
					testCase.method, testCase.path, response.status, response.body.String())
			}
			invocation := router.lastInvocation()
			if string(invocation.Operation) != testCase.operation {
				t.Fatalf("operation = %q; want %q", invocation.Operation, testCase.operation)
			}
			if !reflect.DeepEqual(invocation.Params, testCase.params) {
				t.Fatalf("params = %#v; want %#v", invocation.Params, testCase.params)
			}
			if invocation.Credential != "token-1" {
				t.Fatalf("credential = %q; want token-1", invocation.Credential)
			}
		})
	}
}

// TestEdgePreservesGetAndDeleteJSONBodies pins that every REST method carries
// the same api.Request body, including GET and DELETE.
func TestEdgePreservesGetAndDeleteJSONBodies(t *testing.T) {
	expected := api.Request{
		Input:           json.RawMessage(`{"task":"task-1"}`),
		ExpectedVersion: new(uint64(7)),
		IdempotencyKey:  "key-1",
	}
	body, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	cases := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/tasks/task-1"},
		{"DELETE", "/api/v1/projects/project-a/memberships/principal-1"},
	}

	for _, testCase := range cases {
		t.Run(testCase.method, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path, body, "Bearer token-1")
			if response.status != http.StatusOK {
				t.Fatalf("%s status = %d; want 200 (body %s)", testCase.method, response.status, response.body.String())
			}
			if got := router.lastInvocation().Request; !reflect.DeepEqual(got, expected) {
				t.Fatalf("%s request = %#v; want %#v", testCase.method, got, expected)
			}
		})
	}
}

// TestEdgeRefusesUnknownRoute pins unknown paths, wrong methods and trailing
// slashes as 404 not_found before the router is reached.
func TestEdgeRefusesUnknownRoute(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"unknown path", "GET", "/api/v1/nope"},
		{"wrong method", "POST", "/api/v1/tasks/task-1"},
		{"trailing slash", "GET", "/api/v1/tasks/task-1/"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path, []byte(`{"input":{}}`), "Bearer token-1")
			if response.status != http.StatusNotFound {
				t.Fatalf("status = %d; want 404", response.status)
			}
			if got := decodePublicError(t, response); got.Code != api.CodeNotFound {
				t.Fatalf("code = %q; want %q", got.Code, api.CodeNotFound)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestEdgeRefusesQuery pins that a query string is not part of any declared
// path.
func TestEdgeRefusesQuery(t *testing.T) {
	router := &fakeRouter{}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "GET", "/api/v1/tasks/task-1?scope=other", []byte(`{"input":{}}`), "Bearer token-1")
	if response.status != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", response.status)
	}
	if got := decodePublicError(t, response); got.Code != api.CodeInvalidInput {
		t.Fatalf("code = %q; want %q", got.Code, api.CodeInvalidInput)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

// TestEdgeRefusesMalformedEnvelope pins strict api.Request decoding.
func TestEdgeRefusesMalformedEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"not an object", `[]`},
		{"not json", `not json`},
		{"unknown field", `{"input":{},"unknown":1}`},
		{"trailing data", `{"input":{}} {"input":{}}`},
		{"zero expected version", `{"input":{},"expected_version":0}`},
		{"non-object input", `{"input":[]}`},
		{"invalid idempotency key", `{"input":{},"idempotency_key":"bad key"}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask", []byte(testCase.body), "Bearer token-1")
			if response.status != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body %s)", response.status, response.body.String())
			}
			if got := decodePublicError(t, response); got.Code != api.CodeInvalidInput {
				t.Fatalf("code = %q; want %q", got.Code, api.CodeInvalidInput)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestEdgeRequestBound pins the inclusive 1 MiB request bound and the +1
// refusal.
func TestEdgeRequestBound(t *testing.T) {
	t.Run("exactly the bound", func(t *testing.T) {
		router := &fakeRouter{}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask",
			requestBodyOfSize(t, api.MaxRequestBytes), "Bearer token-1")
		if response.status != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %s)", response.status, response.body.String())
		}
		if got := router.callCount(); got != 1 {
			t.Fatalf("router calls = %d; want 1", got)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		router := &fakeRouter{}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask",
			requestBodyOfSize(t, api.MaxRequestBytes+1), "Bearer token-1")
		if response.status != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400", response.status)
		}
		if got := decodePublicError(t, response); got.Code != api.CodeInvalidInput {
			t.Fatalf("code = %q; want %q", got.Code, api.CodeInvalidInput)
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0", got)
		}
	})
}

// TestEdgeResponseBound pins the inclusive 8 MiB encoded result bound and the
// +1 refusal.
func TestEdgeResponseBound(t *testing.T) {
	t.Run("exactly the bound", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return resultOfEncodedSize(t, "task.read", api.MaxResponseBytes), nil
		}}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
		if response.status != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body prefix %q)", response.status, response.body.String()[:min(200, response.body.Len())])
		}
		if response.body.Len() != api.MaxResponseBytes {
			t.Fatalf("response body = %d bytes; want %d", response.body.Len(), api.MaxResponseBytes)
		}
		var result api.Result
		if err := json.Unmarshal(response.body.Bytes(), &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if err := result.Validate(); err != nil {
			t.Fatalf("result is invalid: %v", err)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return resultOfEncodedSize(t, "task.read", api.MaxResponseBytes+1), nil
		}}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
		if response.status != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", response.status)
		}
		if got := decodePublicError(t, response); got.Code != api.CodeInternalFailure {
			t.Fatalf("code = %q; want %q", got.Code, api.CodeInternalFailure)
		}
		if response.body.Len() > 4096 {
			t.Fatalf("oversized result leaked a %d-byte body", response.body.Len())
		}
	})
}

// TestEdgeErrorParityAllPublicCodes pins the declared REST status mapping and
// the stable api.Error body for every public code.
func TestEdgeErrorParityAllPublicCodes(t *testing.T) {
	for _, code := range api.ErrorCodes() {
		t.Run(code, func(t *testing.T) {
			router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
				return api.Result{}, fault.New(fault.Class(code), "public message")
			}}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask",
				[]byte(`{"input":{},"idempotency_key":"key-1"}`), "Bearer token-1")

			wantStatus, ok := api.HTTPStatus(code)
			if !ok {
				t.Fatalf("no REST status for public code %q", code)
			}
			if response.status != wantStatus {
				t.Fatalf("status = %d; want %d", response.status, wantStatus)
			}
			if got := response.header.Get("Content-Type"); got != api.MediaType {
				t.Fatalf("content type = %q; want %q", got, api.MediaType)
			}
			publicError := decodePublicError(t, response)
			if publicError.Code != code {
				t.Fatalf("code = %q; want %q", publicError.Code, code)
			}
			if publicError.Message != "public message" {
				t.Fatalf("message = %q; want the public message", publicError.Message)
			}
		})
	}
}

// TestEdgeSanitizesPrivateCauses pins that neither a fault cause nor an
// unexpected handler error is rendered to the caller.
func TestEdgeSanitizesPrivateCauses(t *testing.T) {
	t.Run("fault cause", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return api.Result{}, fault.Wrap(fault.Unauthenticated, "access token verification failed",
				errors.New("SECRET-SENTINEL"))
		}}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
		if response.status != http.StatusUnauthorized {
			t.Fatalf("status = %d; want 401", response.status)
		}
		if strings.Contains(response.body.String(), "SECRET-SENTINEL") {
			t.Fatalf("response rendered the private cause: %s", response.body.String())
		}
		publicError := decodePublicError(t, response)
		if publicError.Code != api.CodeUnauthenticated || publicError.Message != "access token verification failed" {
			t.Fatalf("public error = %+v; want the public code/message", publicError)
		}
	})

	t.Run("non-fault error", func(t *testing.T) {
		router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
			return api.Result{}, errors.New("SECRET-SENTINEL")
		}}
		edge, err := httpapi.NewEdge(router)
		if err != nil {
			t.Fatalf("NewEdge: %v", err)
		}
		response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
		if response.status != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", response.status)
		}
		if strings.Contains(response.body.String(), "SECRET-SENTINEL") {
			t.Fatalf("response rendered the handler error: %s", response.body.String())
		}
		if got := decodePublicError(t, response); got.Code != api.CodeInternalFailure {
			t.Fatalf("code = %q; want %q", got.Code, api.CodeInternalFailure)
		}
	})
}

// TestEdgePassesCredential pins the exact bearer token handed to the router and
// the empty credential for absent or malformed headers.
func TestEdgePassesCredential(t *testing.T) {
	cases := []struct {
		name          string
		authorization string
		want          string
	}{
		{"bearer", "Bearer token-1", "token-1"},
		{"lowercase scheme", "bearer token-2", "token-2"},
		{"absent", "", ""},
		{"wrong scheme", "Basic abc", ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), testCase.authorization)
			if got := router.lastInvocation().Credential; got != testCase.want {
				t.Fatalf("credential = %q; want %q", got, testCase.want)
			}
		})
	}
}

// TestEdgeResultReplayedPreserved pins that the transport does not strip the
// replay fact from a capability result.
func TestEdgeResultReplayedPreserved(t *testing.T) {
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{
			Operation: "task.read",
			Body:      json.RawMessage(`{"title":"task"}`),
			Replayed:  true,
		}, nil
	}}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d; want 200", response.status)
	}
	var result api.Result
	if err := json.Unmarshal(response.body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Replayed {
		t.Fatal("Replayed was not preserved")
	}
	if result.Operation != "task.read" || string(result.Body) != `{"title":"task"}` {
		t.Fatalf("result = %+v; want the original operation and body", result)
	}
}

// TestEdgeRejectsInvalidRouterResult pins that an invalid result is a sanitized
// internal failure rather than an invalid success response.
func TestEdgeRejectsInvalidRouterResult(t *testing.T) {
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{Operation: "not.registered", Body: json.RawMessage(`{"ok":true}`)}, nil
	}}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "GET", "/api/v1/tasks/task-1", []byte(`{"input":{}}`), "Bearer token-1")
	if response.status != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", response.status)
	}
	if got := decodePublicError(t, response); got.Code != api.CodeInternalFailure {
		t.Fatalf("code = %q; want %q", got.Code, api.CodeInternalFailure)
	}
}

// TestEdgeLogsTheCauseChainWithoutCredentials pins the server-side diagnostic:
// an internal failure logs its cause chain for the operator, while the client
// body stays the generic "internal failure". Request bodies, Authorization
// headers, DSN passwords and private keys never reach the log.
func TestEdgeLogsTheCauseChainWithoutCredentials(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const (
		dsnSecret   = "SENTINEL-DSN-PASSWORD"
		bearerToken = "SENTINEL-AUTHORIZATION-TOKEN"
		bodySecret  = "SENTINEL-REQUEST-BODY"
		authHeader  = "SENTINEL-X-AUTH-TOKEN"
	)
	cause := fmt.Errorf(`dial postgres://kodama_runtime:%s@db:5432/kodama: respect header "Authorization: Bearer %s": header "X-Auth-Token: %s": query failed`,
		dsnSecret, bearerToken, authHeader)
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{}, fault.Wrap(fault.InternalFailure, "internal failure", cause)
	}}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask",
		[]byte(`{"input":{"note":"`+bodySecret+`"},"idempotency_key":"key-1"}`), "Bearer "+bearerToken)

	if response.status != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", response.status)
	}
	publicError := decodePublicError(t, response)
	if publicError.Code != api.CodeInternalFailure || publicError.Message != "internal failure" {
		t.Fatalf("public error = %+v; want the generic internal failure", publicError)
	}
	if !strings.Contains(response.body.String(), publicError.Message) {
		t.Fatalf("response body = %q", response.body.String())
	}
	text := logged.String()
	if text == "" {
		t.Fatal("an internal failure was not logged")
	}
	if !strings.Contains(text, "internal failure") || !strings.Contains(text, "query failed") {
		t.Fatalf("log = %q; want the cause chain for the operator", text)
	}
	for _, secret := range []string{dsnSecret, bearerToken, bodySecret, authHeader} {
		if strings.Contains(text, secret) {
			t.Fatalf("log rendered %q: %s", secret, text)
		}
	}
}

// TestEdgeLogWriterRedactsNamedCredentialForms proves the real edge log writer
// (not only the redactor in isolation) removes a PEM private key, a SQL
// PASSWORD literal, a password assignment and an unlabelled bearer value.
func TestEdgeLogWriterRedactsNamedCredentialForms(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const (
		pemSecret      = "MIIBOgIBAAJBAKj34GkxFhEdgeSentinel"
		sqlSecret      = "edge-sql-sentinel"
		assignedSecret = "edge-assigned-sentinel"
		bearerSecret   = "edge-bearer-sentinel"
	)
	cause := fmt.Errorf("decode failed:\n-----BEGIN RSA PRIVATE KEY-----\n%s\n-----END RSA PRIVATE KEY-----\n"+
		"ALTER ROLE kodama_runtime PASSWORD '%s': password=%s: presented Bearer %s",
		pemSecret, sqlSecret, assignedSecret, bearerSecret)
	router := &fakeRouter{fn: func(context.Context, ops.Invocation) (api.Result, error) {
		return api.Result{}, fault.Wrap(fault.InternalFailure, "internal failure", cause)
	}}
	edge, err := httpapi.NewEdge(router)
	if err != nil {
		t.Fatalf("NewEdge: %v", err)
	}
	response := perform(t, edge, "POST", "/api/v1/tasks/task-1/ask",
		[]byte(`{"input":{"note":"x"},"idempotency_key":"key-1"}`), "Bearer edge-request-credential")
	if response.status != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", response.status)
	}
	text := logged.String()
	for _, secret := range []string{pemSecret, sqlSecret, assignedSecret, bearerSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("edge log rendered %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "decode failed") {
		t.Fatalf("edge log lost the operator diagnostic: %s", text)
	}
}

// TestEdgeAllowsEmptyBodyOnGetAndRefusesNonGet pins that GET routes accept an
// empty or whitespace-only request body as "{}" while POST, PUT, PATCH and
// DELETE continue to refuse empty bodies with 400 invalid_input.
func TestEdgeAllowsEmptyBodyOnGetAndRefusesNonGet(t *testing.T) {
	getCases := []struct {
		name      string
		path      string
		body      []byte
		operation string
		params    map[string]string
	}{
		{"whoami nil body", "/api/v1/whoami", nil, "principal.whoami", map[string]string{}},
		{"whoami empty body", "/api/v1/whoami", []byte(""), "principal.whoami", map[string]string{}},
		{"whoami whitespace body", "/api/v1/whoami", []byte("  \n\t "), "principal.whoami", map[string]string{}},
		{"task read nil body", "/api/v1/tasks/task-1", nil, "task.read", map[string]string{"task": "task-1"}},
		{"pipeline list empty body", "/api/v1/projects/project-a/pipelines", []byte(""), "pipeline.list", map[string]string{"project": "project-a"}},
	}
	for _, testCase := range getCases {
		t.Run(testCase.name, func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, http.MethodGet, testCase.path, testCase.body, "Bearer token-1")
			if response.status != http.StatusOK {
				t.Fatalf("GET %s status = %d; want 200 (body %s)",
					testCase.path, response.status, response.body.String())
			}
			invocation := router.lastInvocation()
			if string(invocation.Operation) != testCase.operation {
				t.Fatalf("operation = %q; want %q", invocation.Operation, testCase.operation)
			}
			if !reflect.DeepEqual(invocation.Params, testCase.params) {
				t.Fatalf("params = %#v; want %#v", invocation.Params, testCase.params)
			}
		})
	}

	nonGetCases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/tasks/task-1/ask"},
		{http.MethodPut, "/api/v1/projects/project-a/memberships/principal-1"},
		{http.MethodPatch, "/api/v1/tasks/task-1"},
		{http.MethodDelete, "/api/v1/projects/project-a/memberships/principal-1"},
	}
	for _, testCase := range nonGetCases {
		t.Run(testCase.method+"_empty", func(t *testing.T) {
			router := &fakeRouter{}
			edge, err := httpapi.NewEdge(router)
			if err != nil {
				t.Fatalf("NewEdge: %v", err)
			}
			response := perform(t, edge, testCase.method, testCase.path, nil, "Bearer token-1")
			if response.status != http.StatusBadRequest {
				t.Fatalf("%s %s status = %d; want 400 (body %s)",
					testCase.method, testCase.path, response.status, response.body.String())
			}
			if got := decodePublicError(t, response); got.Code != api.CodeInvalidInput {
				t.Fatalf("code = %q; want %q", got.Code, api.CodeInvalidInput)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}
