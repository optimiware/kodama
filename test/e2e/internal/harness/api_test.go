//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestHarnessAPIEnvelope proves the helper sends the public envelope to
// the declared route, authenticates with the supplied token, and verifies the
// installation CA against the configured TLS server name.
func TestHarnessAPIEnvelope(t *testing.T) {
	caPath, certificate := testPKI(t)

	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotBody   map[string]json.RawMessage
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		gotMethod = request.Method
		gotPath = request.URL.Path
		gotAuth = request.Header.Get("Authorization")
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not a JSON object: %v", err)
		}
		writeTestResult(t, writer, api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	result, err := client.Get(
		context.Background(),
		Secret("project-token-value"),
		"execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"},
		"probe-1",
	)
	if err != nil {
		t.Fatalf("authenticated execution.list failed: %v", err)
	}
	if result.Operation != "execution.list" {
		t.Fatalf("result operation = %q, want execution.list", result.Operation)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/projects/tenant-a/executions" {
		t.Errorf("path = %q, want the declared execution.list route", gotPath)
	}
	if gotAuth != "Bearer project-token-value" {
		t.Errorf("authorization header = %q, want the supplied bearer token", gotAuth)
	}
	if string(gotBody["idempotency_key"]) != `"probe-1"` {
		t.Errorf("idempotency_key = %s, want probe-1", gotBody["idempotency_key"])
	}
	var input map[string]string
	if err := json.Unmarshal(gotBody["input"], &input); err != nil {
		t.Fatalf("envelope input is not an object: %v", err)
	}
	if input["project_id"] != "tenant-a" {
		t.Errorf("envelope project_id = %q, want tenant-a", input["project_id"])
	}
}

// TestHarnessAPIInlinePipelineStartUsesAlternateRoute proves an inline
// task-free start selects the alternate executions binding instead of the
// canonical stored-pipeline binding.
func TestHarnessAPIInlinePipelineStartUsesAlternateRoute(t *testing.T) {
	caPath, certificate := testPKI(t)
	var (
		gotPath  string
		gotInput map[string]json.RawMessage
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("request body is not an object: %v", err)
		}
		if err := json.Unmarshal(envelope["input"], &gotInput); err != nil {
			t.Errorf("envelope input is not an object: %v", err)
		}
		writeTestResult(t, writer, api.Result{
			Operation: "pipeline.start",
			Body:      json.RawMessage(`{"execution_id":"exec-1","version":1}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	document := []byte("actions:\n  - name: build\n    image: docker.io/library/node:26.13.0-alpine\n    commands: [true]\n")
	if _, err := client.Post(context.Background(), Secret("project-token-value"), "pipeline.start",
		api.PipelineStartRequest{ProjectID: "tenant-a", YAML: document}, "inline-1"); err != nil {
		t.Fatalf("inline pipeline.start failed: %v", err)
	}
	if gotPath != "/api/v1/projects/tenant-a/executions" {
		t.Fatalf("inline start path = %q; want the alternate executions binding", gotPath)
	}
	decoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	if string(gotInput["yaml"]) != string(decoded) {
		t.Fatalf("inline start yaml = %s; want the exact posted bytes", gotInput["yaml"])
	}
	if string(gotInput["project_id"]) != `"tenant-a"` {
		t.Fatalf("inline start project_id = %s; want tenant-a", gotInput["project_id"])
	}
}

// TestHarnessAPIStoredPipelineStartUsesCanonicalRoute proves a stored start
// keeps the canonical pipeline binding and carries the expected pipeline
// version in the envelope.
func TestHarnessAPIStoredPipelineStartUsesCanonicalRoute(t *testing.T) {
	caPath, certificate := testPKI(t)
	var (
		gotPath     string
		gotVersion  json.RawMessage
		gotPipeline string
	)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("request body is not an object: %v", err)
		}
		gotVersion = envelope["expected_version"]
		var input map[string]json.RawMessage
		if err := json.Unmarshal(envelope["input"], &input); err != nil {
			t.Errorf("envelope input is not an object: %v", err)
		}
		gotPipeline = string(input["pipeline_id"])
		writeTestResult(t, writer, api.Result{
			Operation: "pipeline.start",
			Body:      json.RawMessage(`{"execution_id":"exec-2","version":1}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	expected := uint64(7)
	if _, err := client.Invoke(context.Background(), Call{
		Method:          api.MethodPost,
		Token:           Secret("project-token-value"),
		Operation:       "pipeline.start",
		Input:           api.PipelineStartRequest{ProjectID: "tenant-a", PipelineID: "pipe-1"},
		IdempotencyKey:  "stored-1",
		ExpectedVersion: &expected,
	}); err != nil {
		t.Fatalf("stored pipeline.start failed: %v", err)
	}
	if gotPath != "/api/v1/projects/tenant-a/pipelines/pipe-1/start" {
		t.Fatalf("stored start path = %q; want the canonical pipeline binding", gotPath)
	}
	if string(gotVersion) != "7" {
		t.Fatalf("stored start expected_version = %s; want 7", gotVersion)
	}
	if gotPipeline != `"pipe-1"` {
		t.Fatalf("stored start pipeline_id = %s; want pipe-1", gotPipeline)
	}
}

// TestHarnessAPIRejectsRedirects proves a redirect is refused locally and the
// redirect target is never followed.
func TestHarnessAPIRejectsRedirects(t *testing.T) {
	caPath, certificate := testPKI(t)
	var followed atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/elsewhere" {
			followed.Add(1)
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "/elsewhere", http.StatusFound)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	_, err = client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, "")
	if err == nil {
		t.Fatal("a redirect response was accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "redirect") {
		t.Fatalf("redirect error %q does not name redirects", err)
	}
	if followed.Load() != 0 {
		t.Fatal("the redirect target was followed")
	}
}

// TestHarnessAPIMutationRequiresKey proves a mutation that the operation codec
// marks idempotent is refused before any request when no key is supplied.
func TestHarnessAPIMutationRequiresKey(t *testing.T) {
	caPath, _ := testPKI(t)
	client, err := NewAPI(Config{APIURL: "https://127.0.0.1:1", CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	_, err = client.Post(context.Background(), Secret("project-token-value"), "tenant.create", nil, "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "idempotency") {
		t.Fatalf("mutation without an idempotency key returned %v", err)
	}
}

// TestHarnessAPIRejectsWrongServerName proves the configured TLS server name is
// verified against the certificate SAN before any request reaches the handler.
func TestHarnessAPIRejectsWrongServerName(t *testing.T) {
	caPath, certificate := testPKI(t)
	var reached atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "wrong.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("a certificate for another server name was accepted")
	}
	if reached.Load() != 0 {
		t.Fatalf("the handler saw %d request(s) after a TLS server-name mismatch", reached.Load())
	}
}

// TestHarnessAPIRejectsUntrustedCA proves a server certificate from another CA
// is refused before any request reaches the handler.
func TestHarnessAPIRejectsUntrustedCA(t *testing.T) {
	_, certificate := testPKI(t)
	otherCA, _ := testPKI(t)
	var reached atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: otherCA, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
	if reached.Load() != 0 {
		t.Fatalf("the handler saw %d request(s) after an untrusted-CA failure", reached.Load())
	}
}

// TestHarnessAPIRefusalIsPublic proves a missing and a garbage token both reach
// the endpoint and both return the public unauthenticated error, without the
// token value appearing in the local error.
func TestHarnessAPIRefusalIsPublic(t *testing.T) {
	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer project-token-value" {
			payload, _ := json.Marshal(api.Error{Code: api.CodeUnauthenticated, Message: "missing credential"})
			writer.Header().Set("Content-Type", api.MediaType)
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write(payload)
			return
		}
		writeTestResult(t, writer, api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	for _, token := range []Secret{"", "garbage-token"} {
		_, err := client.Get(context.Background(), token, "execution.list",
			api.ExecutionListRequest{ProjectID: "tenant-a"}, "")
		var public *APIError
		if !errors.As(err, &public) {
			t.Fatalf("token %q: error %v is not a public API error", token, err)
		}
		if public.Status != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, public.Status)
		}
		if public.Code != api.CodeUnauthenticated {
			t.Errorf("token %q: code = %q, want %q", token, public.Code, api.CodeUnauthenticated)
		}
		if token != "" && strings.Contains(err.Error(), token.Reveal()) {
			t.Fatalf("local error leaked the refused token: %v", err)
		}
	}
}

// TestHarnessAPIResponseOperationMismatch proves a result naming another
// operation is refused rather than accepted as the caller's answer.
func TestHarnessAPIResponseOperationMismatch(t *testing.T) {
	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeTestResult(t, writer, api.Result{
			Operation: "execution.inspect",
			Body:      json.RawMessage(`{"execution_id":"exec-1","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("a result naming another operation was accepted")
	}
}

// TestHarnessAPIReadBounded proves the response-body bound refuses an oversized
// body instead of reading it, and accepts an exact-limit body.
func TestHarnessAPIReadBounded(t *testing.T) {
	if data, err := readBounded(strings.NewReader("12345"), 5); err != nil || string(data) != "12345" {
		t.Fatalf("exact-limit body = %q, %v; want accepted", data, err)
	}
	if _, err := readBounded(strings.NewReader("123456"), 5); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body error = %v; want a bounded-response refusal", err)
	}
	if data, err := readBounded(strings.NewReader("123"), 0); err != nil || string(data) != "123" {
		t.Fatalf("default-bound body = %q, %v; want accepted", data, err)
	}

	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeTestResult(t, writer, api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[],"next_cursor":"` + strings.Repeat("c", 128) + `"}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	client.maxResponse = 32
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v; want a bounded-response refusal", err)
	}
}

// TestHarnessAPIInfersServerName proves an installation reached by its own
// verified name needs no Q01_TLS_SERVER_NAME: the TLS name is the URL host.
func TestHarnessAPIInfersServerName(t *testing.T) {
	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeTestResult(t, writer, api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	// A direct installation URL carries the name the certificate serves; an
	// explicit port-forward may still override it.
	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath})
	if err != nil {
		t.Fatalf("build API helper without an explicit TLS name: %v", err)
	}
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err != nil {
		t.Fatalf("inferred TLS server name was refused: %v", err)
	}
}

// TestHarnessAPIUsesSystemRootsWhenNoCA proves an installation with no declared
// CA is verified against the system roots rather than a fabricated pool.
func TestHarnessAPIUsesSystemRootsWhenNoCA(t *testing.T) {
	_, certificate := testPKI(t)
	var reached atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached.Add(1)
		writeTestResult(t, writer, api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL})
	if err != nil {
		t.Fatalf("build system-roots API helper: %v", err)
	}
	if _, err := client.Get(context.Background(), Secret("project-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("a private CA was trusted without being configured")
	}
	if reached.Load() != 0 {
		t.Fatalf("the handler saw %d request(s) after a system-roots TLS failure", reached.Load())
	}
}

// writeTestResult writes one public success envelope.
func writeTestResult(t *testing.T, writer http.ResponseWriter, result api.Result) {
	t.Helper()
	payload, err := json.Marshal(result)
	if err != nil {
		t.Errorf("encode result: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	_, _ = writer.Write(payload)
}
