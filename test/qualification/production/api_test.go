//go:build qualification

package production

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

// TestHarnessAPIEnvelope proves the helper sends the frozen public envelope to
// the frozen route, authenticates with the supplied token, and verifies the
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
		payload, err := json.Marshal(api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
		if err != nil {
			t.Errorf("encode result: %v", err)
		}
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = writer.Write(payload)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{
		APIURL:        server.URL,
		CAPEM:         caPath,
		TLSServerName: "kodama.test",
	})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	result, err := client.Get(
		context.Background(),
		Secret("admin-token-value"),
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
		t.Errorf("path = %q, want the frozen execution.list route", gotPath)
	}
	if gotAuth != "Bearer admin-token-value" {
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
	_, err = client.Get(context.Background(), Secret("admin-token-value"), "execution.list",
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

// TestHarnessAPIMutationRequiresKey proves a mutation that the frozen codec
// marks idempotent is refused before any request when no key is supplied.
func TestHarnessAPIMutationRequiresKey(t *testing.T) {
	caPath, _ := testPKI(t)
	client, err := NewAPI(Config{APIURL: "https://127.0.0.1:1", CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}
	_, err = client.Post(context.Background(), Secret("admin-token-value"), "tenant.create", nil, "")
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
	if _, err := client.Get(context.Background(), Secret("admin-token-value"), "execution.list",
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
	if _, err := client.Get(context.Background(), Secret("admin-token-value"), "execution.list",
		api.ExecutionListRequest{ProjectID: "tenant-a"}, ""); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
	if reached.Load() != 0 {
		t.Fatalf("the handler saw %d request(s) after an untrusted-CA failure", reached.Load())
	}
}

// TestHarnessAPIEnvelopePrecedesAuthentication proves the edge's real order: a
// bodyless request is invalid_input even without a token, while the helper's
// enveloped request without a token (or with a garbage token) reaches
// authentication and is refused as unauthenticated.
func TestHarnessAPIEnvelopePrecedesAuthentication(t *testing.T) {
	caPath, certificate := testPKI(t)
	var enveloped atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var envelope map[string]json.RawMessage
		if len(body) == 0 || json.Unmarshal(body, &envelope) != nil {
			writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
			return
		}
		if _, hasInput := envelope["input"]; !hasInput {
			writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
			return
		}
		if _, hasKey := envelope["idempotency_key"]; !hasKey {
			writePublicTestError(writer, http.StatusBadRequest, api.CodeInvalidInput)
			return
		}
		enveloped.Add(1)
		if request.Header.Get("Authorization") != "Bearer admin-token-value" {
			writePublicTestError(writer, http.StatusUnauthorized, api.CodeUnauthenticated)
			return
		}
		payload, _ := json.Marshal(api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = writer.Write(payload)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	client, err := NewAPI(Config{APIURL: server.URL, CAPEM: caPath, TLSServerName: "kodama.test"})
	if err != nil {
		t.Fatalf("build API helper: %v", err)
	}

	// The same GET with no request body is refused before authentication.
	bodyless, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		server.URL+"/api/v1/projects/tenant-a/executions", nil)
	if err != nil {
		t.Fatalf("build bodyless request: %v", err)
	}
	response, err := client.client.Do(bodyless)
	if err != nil {
		t.Fatalf("bodyless request failed: %v", err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read bodyless response: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("bodyless status = %d, want 400", response.StatusCode)
	}
	var public api.Error
	if err := json.Unmarshal(data, &public); err != nil || public.Code != api.CodeInvalidInput {
		t.Fatalf("bodyless refusal = %s, want invalid_input", data)
	}

	for _, token := range []Secret{"", "garbage-token"} {
		_, err := client.Get(context.Background(), token, "execution.list",
			api.ExecutionListRequest{ProjectID: "tenant-a"}, "")
		var refusal *APIError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusUnauthorized || refusal.Code != api.CodeUnauthenticated {
			t.Fatalf("enveloped request with token %q returned %v, want 401 unauthenticated", token, err)
		}
	}
	if enveloped.Load() != 2 {
		t.Fatalf("the helper sent %d enveloped requests, want 2", enveloped.Load())
	}
}

// writePublicTestError writes one public test error body.
func writePublicTestError(writer http.ResponseWriter, status int, code string) {
	payload, _ := json.Marshal(api.Error{Code: code, Message: "public refusal"})
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(payload)
}

// TestHarnessAPIRefusalIsPublic proves a missing and a garbage token both reach
// the endpoint and both return the public unauthenticated error, without the
// token value appearing in the local error.
func TestHarnessAPIRefusalIsPublic(t *testing.T) {
	caPath, certificate := testPKI(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer admin-token-value" {
			payload, _ := json.Marshal(api.Error{Code: api.CodeUnauthenticated, Message: "missing credential"})
			writer.Header().Set("Content-Type", api.MediaType)
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write(payload)
			return
		}
		payload, _ := json.Marshal(api.Result{
			Operation: "execution.list",
			Body:      json.RawMessage(`{"executions":[]}`),
		})
		writer.Header().Set("Content-Type", api.MediaType)
		_, _ = writer.Write(payload)
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
