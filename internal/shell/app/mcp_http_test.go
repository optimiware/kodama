package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/app/drivers/mcphttp"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestMCPStreamableHTTPCompliance(t *testing.T) {
	t.Run("without discovery metadata", func(t *testing.T) {
		environment := newTestEnvironment(t)
		app := environment.startApp(t)
		handler := app.Handler()

		// GET /mcp and DELETE /mcp -> 405 Method Not Allowed with Allow: POST.
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			req := httptest.NewRequest(method, "/mcp", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s /mcp status = %d; want 405 (body %s)", method, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Fatalf("%s /mcp Allow = %q; want POST", method, got)
			}
		}

		// JSON-RPC notifications -> 202 Accepted with empty body.
		for _, body := range []string{
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		} {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
			req.Header.Set("Content-Type", api.MediaType)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("notification status = %d; want 202 (body %s)", rec.Code, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("notification body = %q; want empty", rec.Body.String())
			}
		}

		// ping -> 200 OK with {}.
		pingReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)))
		pingReq.Header.Set("Content-Type", api.MediaType)
		pingRec := httptest.NewRecorder()
		handler.ServeHTTP(pingRec, pingReq)
		if pingRec.Code != http.StatusOK {
			t.Fatalf("ping status = %d; want 200", pingRec.Code)
		}
		var pingResp api.MCPResponse
		if err := json.Unmarshal(pingRec.Body.Bytes(), &pingResp); err != nil {
			t.Fatalf("decode ping response: %v", err)
		}
		if string(pingResp.Result) != `{}` {
			t.Fatalf("ping result = %s; want {}", string(pingResp.Result))
		}

		// A browser Origin other than the server's own is refused. With no
		// configured public URL the request host is the server's own.
		for origin, want := range map[string]int{"https://evil.example": http.StatusForbidden, "http://example.com": http.StatusOK} {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)))
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("ping with Origin %s status = %d; want %d", origin, rec.Code, want)
			}
		}

		// Unauthenticated tools/call -> 401 Unauthorized with WWW-Authenticate: Bearer.
		unauthReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"principal_whoami","arguments":{"input":{}}}}`)))
		unauthReq.Header.Set("Content-Type", api.MediaType)
		unauthRec := httptest.NewRecorder()
		handler.ServeHTTP(unauthRec, unauthReq)
		if unauthRec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated MCP status = %d; want 401 (body %s)", unauthRec.Code, unauthRec.Body.String())
		}
		if got := unauthRec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Fatalf("WWW-Authenticate = %q; want Bearer", got)
		}

		// /.well-known/oauth-protected-resource is 404 when discovery is not configured.
		metaReq := httptest.NewRequest(http.MethodGet, api.OAuthProtectedResourcePath, nil)
		metaRec := httptest.NewRecorder()
		handler.ServeHTTP(metaRec, metaReq)
		if metaRec.Code != http.StatusNotFound {
			t.Fatalf("unconfigured %s status = %d; want 404", api.OAuthProtectedResourcePath, metaRec.Code)
		}

		// Bodyless REST GET /api/v1/whoami succeeds while bodyless POST refuses.
		whoamiReq := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
		whoamiReq.Header.Set("Authorization", "Bearer dev-admin-token")
		whoamiRec := httptest.NewRecorder()
		handler.ServeHTTP(whoamiRec, whoamiReq)
		if whoamiRec.Code != http.StatusOK {
			t.Fatalf("bodyless GET /api/v1/whoami status = %d; want 200 (body %s)", whoamiRec.Code, whoamiRec.Body.String())
		}

		postReq := httptest.NewRequest(http.MethodPost, "/api/v1/tenants", nil)
		postReq.Header.Set("Authorization", "Bearer dev-admin-token")
		postRec := httptest.NewRecorder()
		handler.ServeHTTP(postRec, postReq)
		if postRec.Code != http.StatusBadRequest {
			t.Fatalf("bodyless POST /api/v1/tenants status = %d; want 400 (body %s)", postRec.Code, postRec.Body.String())
		}
	})

	t.Run("with RFC 9728 OAuth protected resource discovery", func(t *testing.T) {
		environment := newTestEnvironment(t)
		disc := discoveryTestConfig(discoveryTestIssuer)
		disc.APIURL = "https://api.kodama.example:8443"
		environment.config.Discovery = disc
		app := environment.startApp(t)
		handler := app.Handler()

		// Unauthenticated MCP call advertises resource_metadata in WWW-Authenticate.
		unauthReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"principal_whoami","arguments":{"input":{}}}}`)))
		unauthReq.Header.Set("Content-Type", api.MediaType)
		unauthRec := httptest.NewRecorder()
		handler.ServeHTTP(unauthRec, unauthReq)
		if unauthRec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated MCP status = %d; want 401", unauthRec.Code)
		}
		wantHeader := `Bearer resource_metadata="https://api.kodama.example:8443/.well-known/oauth-protected-resource"`
		if got := unauthRec.Header().Get("WWW-Authenticate"); got != wantHeader {
			t.Fatalf("WWW-Authenticate = %q; want %q", got, wantHeader)
		}

		// The configured public API origin is the server's own; the request
		// host, which a DNS-rebinding page controls, is not.
		for origin, want := range map[string]int{"http://example.com": http.StatusForbidden, "https://api.kodama.example:8443": http.StatusOK} {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)))
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("ping with Origin %s status = %d; want %d", origin, rec.Code, want)
			}
		}

		// GET /.well-known/oauth-protected-resource returns RFC 9728 metadata.
		metaReq := httptest.NewRequest(http.MethodGet, api.OAuthProtectedResourcePath, nil)
		metaRec := httptest.NewRecorder()
		handler.ServeHTTP(metaRec, metaReq)
		if metaRec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d; want 200 (body %s)", api.OAuthProtectedResourcePath, metaRec.Code, metaRec.Body.String())
		}
		var metadata api.OAuthProtectedResourceMetadata
		if err := json.Unmarshal(metaRec.Body.Bytes(), &metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		wantMeta := api.OAuthProtectedResourceMetadata{
			Resource:               "https://api.kodama.example:8443",
			AuthorizationServers:   []string{discoveryTestIssuer},
			BearerMethodsSupported: []string{"header"},
		}
		if !reflect.DeepEqual(metadata, wantMeta) {
			t.Fatalf("metadata = %+v; want %+v", metadata, wantMeta)
		}

		// POST on /.well-known/oauth-protected-resource returns 405 with Allow: GET, HEAD.
		badMetaReq := httptest.NewRequest(http.MethodPost, api.OAuthProtectedResourcePath, nil)
		badMetaRec := httptest.NewRecorder()
		handler.ServeHTTP(badMetaRec, badMetaReq)
		if badMetaRec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d; want 405", api.OAuthProtectedResourcePath, badMetaRec.Code)
		}
		if got := badMetaRec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Fatalf("POST %s Allow = %q; want GET, HEAD", api.OAuthProtectedResourcePath, got)
		}
	})

	t.Run("mcphttp driver round-trips against served /mcp CallToolResult", func(t *testing.T) {
		environment := newTestEnvironment(t)
		app := environment.startApp(t)
		server := httptest.NewServer(app.Handler())
		t.Cleanup(server.Close)

		endpoint := server.URL + "/mcp"
		driver, err := mcphttp.New(mcphttp.Config{
			Servers: []mcphttp.Server{{
				Name:     "self",
				Endpoint: endpoint,
				Credentials: []mcphttp.Credential{{
					Secret: "ADMIN_TOKEN",
					Header: "Authorization",
					Scheme: "Bearer",
				}},
			}},
		})
		if err != nil {
			t.Fatalf("mcphttp.New: %v", err)
		}
		bodyReader, err := driver.Call(context.Background(), mcpproxy.UpstreamRequest{
			Server:      "self",
			Endpoint:    endpoint,
			Tool:        "principal_whoami",
			Arguments:   json.RawMessage(`{"input":{}}`),
			Credentials: []secret.Resolved{{Name: "ADMIN_TOKEN", Value: "dev-admin-token"}},
		})
		if err != nil {
			t.Fatalf("mcphttp.Call(principal_whoami): %v", err)
		}
		defer bodyReader.Close()
		mapped, err := io.ReadAll(bodyReader)
		if err != nil {
			t.Fatalf("read mapped response: %v", err)
		}
		decoded, err := api.DecodeMCPResult(mapped)
		if err != nil {
			t.Fatalf("DecodeMCPResult(mapped): %v (raw=%s)", err, string(mapped))
		}
		if decoded.Operation != "principal.whoami" {
			t.Fatalf("decoded operation = %q; want principal.whoami", decoded.Operation)
		}
	})
}
