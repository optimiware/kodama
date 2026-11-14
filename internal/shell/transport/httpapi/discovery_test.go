package httpapi_test

// These tests pin the one unauthenticated public discovery route served by the
// REST edge, api.DiscoveryPath. It is a fixed root route outside the 58-operation
// registry: it is served before authentication and before operation routing,
// validates the consumer-owned provider's document on every response, and never
// falls back to a partial, invalid or oversized document.
//
// The route is exercised over a real httptest server so method, header, body and
// size behavior is observed on the wire rather than through an in-process
// recorder. A fake DiscoveryProvider stands in for the Shell composition.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

// discoveryFixture returns one canonical valid discovery document. The api_url
// path carries an ampersand to pin that the route's strict JSON does not HTML
// escape public URL characters.
func discoveryFixture() api.DiscoveryDocument {
	return api.DiscoveryDocument{
		Issuer:                      "https://auth.example.com",
		ClientID:                    "kodama-cli",
		Audience:                    "kodama",
		AuthorizationEndpoint:       "https://auth.example.com/authorize",
		TokenEndpoint:               "https://auth.example.com/token",
		DeviceAuthorizationEndpoint: "https://auth.example.com/device",
		RevocationEndpoint:          "https://auth.example.com/revoke",
		Scopes:                      []string{"openid", "profile", "email", "offline_access"},
		APIURL:                      "https://api.example.com/kodama&cli",
	}
}

// fakeDiscovery is one consumer-owned discovery port whose result every test
// controls explicitly.
type fakeDiscovery struct {
	mu       sync.Mutex
	calls    int
	document api.DiscoveryDocument
	err      error
}

func (fake *fakeDiscovery) Discovery(context.Context) (api.DiscoveryDocument, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if fake.err != nil {
		return api.DiscoveryDocument{}, fake.err
	}
	return fake.document, nil
}

func (fake *fakeDiscovery) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

// newDiscoveryServer builds the real REST edge over a real httptest server. The
// router is a recording fake so every test can prove the discovery route never
// dispatches an operation.
func newDiscoveryServer(t *testing.T, provider httpapi.DiscoveryProvider) (*httptest.Server, *fakeRouter) {
	t.Helper()
	router := &fakeRouter{}
	edge, err := httpapi.NewEdgeWithConfig(httpapi.Config{
		Router:    router,
		Discovery: provider,
	})
	if err != nil {
		t.Fatalf("NewEdgeWithConfig: %v", err)
	}
	server := httptest.NewServer(edge)
	t.Cleanup(server.Close)
	return server, router
}

// discoveryRequest performs one request against the real server and returns the
// status, headers and complete body.
func discoveryRequest(t *testing.T, method, url string, body io.Reader, header http.Header) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s): %v", method, url, err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, raw
}

// decodeDiscoveryError strictly decodes the fixed public error body.
func decodeDiscoveryError(t *testing.T, body []byte) api.Error {
	t.Helper()
	var publicError api.Error
	if err := json.Unmarshal(body, &publicError); err != nil {
		t.Fatalf("decode api.Error from %q: %v", truncateBody(body), err)
	}
	if err := publicError.Validate(); err != nil {
		t.Fatalf("invalid api.Error body %q: %v", truncateBody(body), err)
	}
	return publicError
}

func truncateBody(body []byte) string {
	const limit = 512
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}

// TestDiscoveryServesProviderDocument pins the successful document: it equals
// the provider's document, strictly decodes through api.DecodeDiscoveryDocument,
// and carries the fixed public, cacheable, non-sniffing headers.
func TestDiscoveryServesProviderDocument(t *testing.T) {
	document := discoveryFixture()
	provider := &fakeDiscovery{document: document}
	server, router := newDiscoveryServer(t, provider)

	response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", response.StatusCode, truncateBody(body))
	}
	if got := response.Header.Get("Content-Type"); got != api.MediaType {
		t.Fatalf("Content-Type = %q; want %q", got, api.MediaType)
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q; want %q", got, "public, max-age=300")
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q; want nosniff", got)
	}
	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("discovery route advertises a CORS wildcard: %q", got)
	}
	if strings.Contains(string(body), `\u0026`) {
		t.Fatalf("body HTML-escaped a public URL character: %s", truncateBody(body))
	}
	if !bytes.Contains(body, []byte("kodama&cli")) {
		t.Fatalf("body = %s; want the unescaped api_url", truncateBody(body))
	}

	decoded, err := api.DecodeDiscoveryDocument(body)
	if err != nil {
		t.Fatalf("DecodeDiscoveryDocument: %v", err)
	}
	if !reflect.DeepEqual(decoded, document) {
		t.Fatalf("served document = %#v; want %#v", decoded, document)
	}
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d; want 1", got)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

// TestDiscoveryIsUnauthenticated pins that no credential is required and that a
// presented credential is ignored: both requests succeed and the token is never
// echoed.
func TestDiscoveryIsUnauthenticated(t *testing.T) {
	cases := []struct {
		name          string
		authorization string
	}{
		{"no header", ""},
		{"forged bearer", "Bearer FORGED-SECRET-TOKEN"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &fakeDiscovery{document: discoveryFixture()}
			server, router := newDiscoveryServer(t, provider)

			header := make(http.Header)
			if testCase.authorization != "" {
				header.Set("Authorization", testCase.authorization)
			}
			response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, header)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d; want 200 (body %s)", response.StatusCode, truncateBody(body))
			}
			if strings.Contains(string(body), "FORGED-SECRET-TOKEN") {
				t.Fatalf("response rendered the presented credential: %s", truncateBody(body))
			}
			if got := provider.callCount(); got != 1 {
				t.Fatalf("provider calls = %d; want 1", got)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestDiscoveryNilProviderIsNotFound pins that an unwired discovery route is a
// clean fixed 404 not_found, never a 500 and never a partial document.
func TestDiscoveryNilProviderIsNotFound(t *testing.T) {
	server, router := newDiscoveryServer(t, nil)

	response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (body %s)", response.StatusCode, truncateBody(body))
	}
	publicError := decodeDiscoveryError(t, body)
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("code = %q; want %q", publicError.Code, api.CodeNotFound)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

// TestDiscoveryInvalidDocumentIsRefused pins that an invalid provider document
// is a fixed secret-free 503 on every response: the document is never served,
// partially or otherwise, and no field of it leaks into the error body.
func TestDiscoveryInvalidDocumentIsRefused(t *testing.T) {
	cases := []struct {
		name     string
		document api.DiscoveryDocument
	}{
		{"zero document", api.DiscoveryDocument{}},
		{"foreign authorization endpoint", func() api.DiscoveryDocument {
			document := discoveryFixture()
			document.AuthorizationEndpoint = "https://evil.example.com/authorize"
			return document
		}()},
		{"plain http issuer outside numeric loopback", func() api.DiscoveryDocument {
			document := discoveryFixture()
			document.Issuer = "http://localhost:8080"
			document.AuthorizationEndpoint = "http://localhost:8080/authorize"
			document.TokenEndpoint = "http://localhost:8080/token"
			document.DeviceAuthorizationEndpoint = "http://localhost:8080/device"
			document.RevocationEndpoint = "http://localhost:8080/revoke"
			return document
		}()},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &fakeDiscovery{document: testCase.document}
			server, router := newDiscoveryServer(t, provider)

			for attempt := range 2 {
				response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
				if response.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("attempt %d status = %d; want 503 (body %s)",
						attempt, response.StatusCode, truncateBody(body))
				}
				publicError := decodeDiscoveryError(t, body)
				if publicError.Code != api.CodeInternalFailure {
					t.Fatalf("attempt %d code = %q; want %q", attempt, publicError.Code, api.CodeInternalFailure)
				}
				if len(body) > 1024 {
					t.Fatalf("refusal body = %d bytes; want a fixed short body", len(body))
				}
				if bytes.Contains(body, []byte("evil.example.com")) ||
					bytes.Contains(body, []byte("localhost:8080")) {
					t.Fatalf("refusal leaked a provider document field: %s", truncateBody(body))
				}
			}
			if got := provider.callCount(); got != 2 {
				t.Fatalf("provider calls = %d; want validation on both responses", got)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("router calls = %d; want 0", got)
			}
		})
	}
}

// TestDiscoveryProviderErrorIsRefused pins that a provider fault is a fixed,
// secret-free 503 dependency_unavailable and never the provider's error text.
func TestDiscoveryProviderErrorIsRefused(t *testing.T) {
	provider := &fakeDiscovery{err: errors.New("SECRET-SENTINEL: identity provider unreachable")}
	server, router := newDiscoveryServer(t, provider)

	response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (body %s)", response.StatusCode, truncateBody(body))
	}
	publicError := decodeDiscoveryError(t, body)
	if publicError.Code != api.CodeDependencyUnavailable {
		t.Fatalf("code = %q; want %q", publicError.Code, api.CodeDependencyUnavailable)
	}
	if bytes.Contains(body, []byte("SECRET-SENTINEL")) {
		t.Fatalf("refusal rendered the provider error: %s", truncateBody(body))
	}
	if len(body) > 1024 {
		t.Fatalf("refusal body = %d bytes; want a fixed short body", len(body))
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

// TestDiscoveryRefusesOtherMethods pins that only GET and HEAD are accepted and
// that a refusal advertises the allowed methods without consulting the provider.
func TestDiscoveryRefusesOtherMethods(t *testing.T) {
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			provider := &fakeDiscovery{document: discoveryFixture()}
			server, router := newDiscoveryServer(t, provider)

			response, body := discoveryRequest(t, method, server.URL+api.DiscoveryPath, nil, nil)
			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s status = %d; want 405 (body %s)", method, response.StatusCode, truncateBody(body))
			}
			if got := response.Header.Get("Allow"); got != "GET, HEAD" {
				t.Fatalf("%s Allow = %q; want %q", method, got, "GET, HEAD")
			}
			if got := decodeDiscoveryError(t, body).Code; got != api.CodeInvalidInput {
				t.Fatalf("%s code = %q; want %q", method, got, api.CodeInvalidInput)
			}
			if got := provider.callCount(); got != 0 {
				t.Fatalf("%s provider calls = %d; want 0", method, got)
			}
			if got := router.callCount(); got != 0 {
				t.Fatalf("%s router calls = %d; want 0", method, got)
			}
		})
	}
}

// TestDiscoveryRefusesRequestBody pins that the discovery route accepts no
// request body, even a well-formed JSON one.
func TestDiscoveryRefusesRequestBody(t *testing.T) {
	provider := &fakeDiscovery{document: discoveryFixture()}
	server, router := newDiscoveryServer(t, provider)

	response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath,
		strings.NewReader(`{"issuer":"injected"}`), nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body %s)", response.StatusCode, truncateBody(body))
	}
	publicError := decodeDiscoveryError(t, body)
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("code = %q; want %q", publicError.Code, api.CodeInvalidInput)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d; want 0", got)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}

// TestDiscoveryRefusesQuery pins that no public route carries a query string,
// so the discovery route refuses one rather than serving a variant document.
func TestDiscoveryRefusesQuery(t *testing.T) {
	provider := &fakeDiscovery{document: discoveryFixture()}
	server, _ := newDiscoveryServer(t, provider)

	response, body := discoveryRequest(t, http.MethodGet,
		server.URL+api.DiscoveryPath+"?issuer=evil.example.com", nil, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body %s)", response.StatusCode, truncateBody(body))
	}
	publicError := decodeDiscoveryError(t, body)
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("code = %q; want %q", publicError.Code, api.CodeInvalidInput)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d; want 0", got)
	}
}

// TestDiscoveryHead pins that HEAD returns the GET headers and length with no
// body, so a client can inspect the document without downloading it twice.
func TestDiscoveryHead(t *testing.T) {
	document := discoveryFixture()
	provider := &fakeDiscovery{document: document}
	server, _ := newDiscoveryServer(t, provider)

	getResponse, getBody := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d; want 200", getResponse.StatusCode)
	}

	headResponse, headBody := discoveryRequest(t, http.MethodHead, server.URL+api.DiscoveryPath, nil, nil)
	if headResponse.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d; want 200", headResponse.StatusCode)
	}
	if len(headBody) != 0 {
		t.Fatalf("HEAD body = %q; want empty", truncateBody(headBody))
	}
	if got := headResponse.Header.Get("Content-Type"); got != api.MediaType {
		t.Fatalf("HEAD Content-Type = %q; want %q", got, api.MediaType)
	}
	if got := headResponse.Header.Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("HEAD Cache-Control = %q; want %q", got, "public, max-age=300")
	}
	if headResponse.ContentLength != int64(len(getBody)) {
		t.Fatalf("HEAD Content-Length = %d; want GET body length %d",
			headResponse.ContentLength, len(getBody))
	}
}

// TestDiscoveryRefusesOversizedDocument pins the inclusive response bound: a
// document encoding to exactly api.MaxDiscoveryBytes is served, one byte more is
// a fixed refusal that never streams the oversized body.
func TestDiscoveryRefusesOversizedDocument(t *testing.T) {
	t.Run("exactly the bound", func(t *testing.T) {
		document := discoveryFixture()
		document.APIURL = discoveryURLOfEncodedSize(t, document, api.MaxDiscoveryBytes)
		provider := &fakeDiscovery{document: document}
		server, _ := newDiscoveryServer(t, provider)

		response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; want 200 (body %s)", response.StatusCode, truncateBody(body))
		}
		if len(body) != api.MaxDiscoveryBytes {
			t.Fatalf("body = %d bytes; want exactly %d", len(body), api.MaxDiscoveryBytes)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		document := discoveryFixture()
		document.APIURL = discoveryURLOfEncodedSize(t, document, api.MaxDiscoveryBytes+1)
		provider := &fakeDiscovery{document: document}
		server, router := newDiscoveryServer(t, provider)

		response, body := discoveryRequest(t, http.MethodGet, server.URL+api.DiscoveryPath, nil, nil)
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d; want 503 (body %s)", response.StatusCode, truncateBody(body))
		}
		if got := decodeDiscoveryError(t, body).Code; got != api.CodeInternalFailure {
			t.Fatalf("code = %q; want %q", got, api.CodeInternalFailure)
		}
		if len(body) > 1024 {
			t.Fatalf("refusal body = %d bytes; want a fixed short body", len(body))
		}
		if got := router.callCount(); got != 0 {
			t.Fatalf("router calls = %d; want 0", got)
		}
	})
}

// discoveryURLOfEncodedSize pads the fixture's api_url until the whole document
// encoding is exactly size bytes. The padded api_url stays a valid absolute
// https URL.
func discoveryURLOfEncodedSize(t *testing.T, document api.DiscoveryDocument, size int) string {
	t.Helper()
	base := "https://api.example.com/"
	padding := size - len(base)
	if padding < 0 {
		t.Fatalf("discovery size %d is smaller than the minimal api_url %d", size, len(base))
	}
	for {
		candidate := document
		candidate.APIURL = base + strings.Repeat("a", padding)
		encoded, err := json.Marshal(candidate)
		if err != nil {
			t.Fatalf("marshal candidate: %v", err)
		}
		delta := size - len(encoded)
		if delta == 0 {
			return candidate.APIURL
		}
		padding += delta
	}
}

// TestDiscoveryIsAbsentFromRegistries pins that the discovery route is not one
// of the 58 operations: it is not in the declared route table, does not resolve
// through api.Match and is not advertised as an MCP tool.
func TestDiscoveryIsAbsentFromRegistries(t *testing.T) {
	routes := api.Routes()
	if len(routes) != 58 {
		t.Fatalf("routes = %d; want 58", len(routes))
	}
	for _, route := range routes {
		if route.Path == api.DiscoveryPath {
			t.Fatalf("operation %q claims the discovery path %q", route.Operation, api.DiscoveryPath)
		}
	}
	if _, _, ok := api.Match(api.MethodGet, api.DiscoveryPath); ok {
		t.Fatalf("api.Match resolved the discovery path as an operation")
	}
	// An operation route still resolves normally, so the discovery branch does
	// not shadow the registry.
	if route, _, ok := api.Match(api.MethodGet, "/api/v1/tasks/task-1"); !ok ||
		route.Operation != "task.read" {
		t.Fatalf("api.Match lost the task.read route: %#v (ok=%v)", route, ok)
	}

	catalog := mcp.ToolCatalog()
	if len(catalog) != 58 {
		t.Fatalf("MCP catalog = %d tools; want 58", len(catalog))
	}
	for _, tool := range catalog {
		if tool.Name == api.DiscoveryPath {
			t.Fatalf("MCP catalog advertises the discovery path as a tool")
		}
	}
}

// TestDiscoveryConcurrent pins that concurrent unauthenticated reads are
// race-free and each returns the provider's complete document.
func TestDiscoveryConcurrent(t *testing.T) {
	document := discoveryFixture()
	provider := &fakeDiscovery{document: document}
	server, router := newDiscoveryServer(t, provider)

	const workers = 16
	const perWorker = 4
	var wait sync.WaitGroup
	failures := make(chan string, workers*perWorker)
	for range workers {
		wait.Go(func() {
			for range perWorker {
				response, err := http.Get(server.URL + api.DiscoveryPath)
				if err != nil {
					failures <- err.Error()
					return
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					failures <- err.Error()
					return
				}
				if response.StatusCode != http.StatusOK {
					failures <- "status " + http.StatusText(response.StatusCode)
					return
				}
				decoded, err := api.DecodeDiscoveryDocument(body)
				if err != nil || !reflect.DeepEqual(decoded, document) {
					failures <- "served document differs from the provider document"
					return
				}
			}
		})
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatalf("concurrent discovery read failed: %s", failure)
	}
	if got := provider.callCount(); got != workers*perWorker {
		t.Fatalf("provider calls = %d; want %d", got, workers*perWorker)
	}
	if got := router.callCount(); got != 0 {
		t.Fatalf("router calls = %d; want 0", got)
	}
}
