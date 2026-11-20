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

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/shell/app/drivers/mcphttp"
	"github.com/optimiware/kodema/internal/shell/mcpproxy"
	"github.com/optimiware/kodema/internal/shell/secret"
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

		// Unauthenticated initialize, tools/list, tools/call, resources/*, and prompts/* -> 401 Unauthorized with WWW-Authenticate: Bearer.
		for name, body := range map[string]string{
			"initialize":               `{"jsonrpc":"2.0","id":6,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
			"tools/list":               `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`,
			"tools/call":               `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"principal_whoami","arguments":{"input":{}}}}`,
			"resources/list":           `{"jsonrpc":"2.0","id":9,"method":"resources/list","params":{}}`,
			"resources/templates/list": `{"jsonrpc":"2.0","id":13,"method":"resources/templates/list","params":{}}`,
			"resources/read":           `{"jsonrpc":"2.0","id":10,"method":"resources/read","params":{"uri":"kodema://docs/overview"}}`,
			"prompts/list":             `{"jsonrpc":"2.0","id":11,"method":"prompts/list","params":{}}`,
			"prompts/get":              `{"jsonrpc":"2.0","id":12,"method":"prompts/get","params":{"name":"kodema-docs"}}`,
		} {
			unauthReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
			unauthReq.Header.Set("Content-Type", api.MediaType)
			unauthRec := httptest.NewRecorder()
			handler.ServeHTTP(unauthRec, unauthReq)
			if unauthRec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated MCP %s status = %d; want 401 (body %s)", name, unauthRec.Code, unauthRec.Body.String())
			}
			if got := unauthRec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("%s WWW-Authenticate = %q; want Bearer", name, got)
			}
		}

		// Authenticated initialize reports DefaultServerVersion ("dev", not "1.0.0"),
		// and authenticated tools/list returns all 58 tools.
		initReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)))
		initReq.Header.Set("Content-Type", api.MediaType)
		initReq.Header.Set("Authorization", "Bearer dev-admin-token")
		initRec := httptest.NewRecorder()
		handler.ServeHTTP(initRec, initReq)
		if initRec.Code != http.StatusOK {
			t.Fatalf("authenticated initialize status = %d; want 200 (body %s)", initRec.Code, initRec.Body.String())
		}
		var initResp api.MCPResponse
		if err := json.Unmarshal(initRec.Body.Bytes(), &initResp); err != nil {
			t.Fatalf("decode initialize response: %v", err)
		}
		var initResult api.MCPInitializeResult
		if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
			t.Fatalf("decode initialize result: %v", err)
		}
		if initResult.ServerInfo.Version != DefaultServerVersion {
			t.Fatalf("default serverInfo.version = %q; want %q", initResult.ServerInfo.Version, DefaultServerVersion)
		}
		if initResult.Instructions == "" {
			t.Fatal("authenticated initialize returned empty instructions")
		}

		// Authenticated resources/list, resources/read, prompts/list, prompts/get succeed over /mcp.
		for _, tc := range []struct {
			name string
			body string
		}{
			{"resources/list", `{"jsonrpc":"2.0","id":20,"method":"resources/list"}`},
			{"resources/templates/list", `{"jsonrpc":"2.0","id":24,"method":"resources/templates/list"}`},
			{"resources/read", `{"jsonrpc":"2.0","id":21,"method":"resources/read","params":{"uri":"kodema://docs/workload"}}`},
			{"prompts/list", `{"jsonrpc":"2.0","id":22,"method":"prompts/list"}`},
			{"prompts/get", `{"jsonrpc":"2.0","id":23,"method":"prompts/get","params":{"name":"kodema-workload"}}`},
		} {
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", api.MediaType)
			req.Header.Set("Authorization", "Bearer dev-admin-token")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("authenticated %s status = %d; want 200 (body %s)", tc.name, rec.Code, rec.Body.String())
			}
			var resp api.MCPResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode %s response: %v", tc.name, err)
			}
			if resp.Error != nil {
				t.Fatalf("authenticated %s error: %+v", tc.name, resp.Error)
			}
		}

		listReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{}}`)))
		listReq.Header.Set("Content-Type", api.MediaType)
		listReq.Header.Set("Authorization", "Bearer dev-admin-token")
		listRec := httptest.NewRecorder()
		handler.ServeHTTP(listRec, listReq)
		if listRec.Code != http.StatusOK {
			t.Fatalf("authenticated tools/list status = %d; want 200 (body %s)", listRec.Code, listRec.Body.String())
		}
		var listResp api.MCPResponse
		if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("decode tools/list response: %v", err)
		}
		var listed api.MCPToolsListResult
		if err := json.Unmarshal(listResp.Result, &listed); err != nil {
			t.Fatalf("decode tools/list result: %v", err)
		}
		if len(listed.Tools) != 58 {
			t.Fatalf("authenticated tools/list count = %d; want 58", len(listed.Tools))
		}

		// Authenticated tools/call accepts flat top-level arguments and encodes
		// domain/RBAC refusals as CallToolResult with isError=true.
		flatReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"project_list","arguments":{"tenant_id":"tenant-1"}}}`)))
		flatReq.Header.Set("Content-Type", api.MediaType)
		flatReq.Header.Set("Authorization", "Bearer dev-admin-token")
		flatRec := httptest.NewRecorder()
		handler.ServeHTTP(flatRec, flatReq)
		if flatRec.Code != http.StatusOK {
			t.Fatalf("flat tools/call status = %d; want 200 (body %s)", flatRec.Code, flatRec.Body.String())
		}
		var flatResp api.MCPResponse
		if err := json.Unmarshal(flatRec.Body.Bytes(), &flatResp); err != nil {
			t.Fatalf("decode flat tools/call response: %v", err)
		}
		if flatResp.Error != nil {
			t.Fatalf("flat tools/call error: %+v", flatResp.Error)
		}
		flatDecoded, err := api.DecodeMCPResult(flatResp.Result)
		if err != nil || flatDecoded.Operation != "project.list" {
			t.Fatalf("flat DecodeMCPResult = (%+v, %v); want project.list", flatDecoded, err)
		}

		// Forbidden call (project-scoped token calling tenant-scoped project.create) returns CallToolResult isError=true.
		forbiddenReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"project_create","arguments":{"tenant_id":"tenant-1","project_id":"p2","name":"P2","idempotency_key":"k-forbid-1"}}}`)))
		forbiddenReq.Header.Set("Content-Type", api.MediaType)
		forbiddenReq.Header.Set("Authorization", "Bearer dev-project-token")
		forbiddenRec := httptest.NewRecorder()
		handler.ServeHTTP(forbiddenRec, forbiddenReq)
		if forbiddenRec.Code != http.StatusOK {
			t.Fatalf("forbidden tools/call status = %d; want 200 (body %s)", forbiddenRec.Code, forbiddenRec.Body.String())
		}
		var forbiddenResp api.MCPResponse
		if err := json.Unmarshal(forbiddenRec.Body.Bytes(), &forbiddenResp); err != nil {
			t.Fatalf("decode forbidden response: %v", err)
		}
		if forbiddenResp.Error != nil {
			t.Fatalf("forbidden tools/call returned JSON-RPC error %+v; want CallToolResult isError=true", forbiddenResp.Error)
		}
		toolErr, isToolErr, err := api.DecodeMCPToolError(forbiddenResp.Result)
		if err != nil || !isToolErr || toolErr.Code != api.CodeForbidden {
			t.Fatalf("forbidden DecodeMCPToolError = (%+v, %t, %v); want forbidden tool error", toolErr, isToolErr, err)
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
		disc.APIURL = "https://api.kodema.example:8443"
		environment.config.Discovery = disc
		environment.config.ServerVersion = "0.4.0"
		app := environment.startApp(t)
		handler := app.Handler()

		// Unauthenticated initialize, tools/list, and tools/call advertise resource_metadata in WWW-Authenticate.
		wantHeader := `Bearer resource_metadata="https://api.kodema.example:8443/.well-known/oauth-protected-resource"`
		for name, body := range map[string]string{
			"initialize": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
			"tools/list": `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
			"tools/call": `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"principal_whoami","arguments":{"input":{}}}}`,
		} {
			unauthReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
			unauthReq.Header.Set("Content-Type", api.MediaType)
			unauthRec := httptest.NewRecorder()
			handler.ServeHTTP(unauthRec, unauthReq)
			if unauthRec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated MCP %s status = %d; want 401", name, unauthRec.Code)
			}
			if got := unauthRec.Header().Get("WWW-Authenticate"); got != wantHeader {
				t.Fatalf("%s WWW-Authenticate = %q; want %q", name, got, wantHeader)
			}
		}

		// Authenticated initialize advertises the configured Kodema binary version (0.4.0).
		initReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)))
		initReq.Header.Set("Content-Type", api.MediaType)
		initReq.Header.Set("Authorization", "Bearer dev-admin-token")
		initRec := httptest.NewRecorder()
		handler.ServeHTTP(initRec, initReq)
		if initRec.Code != http.StatusOK {
			t.Fatalf("authenticated initialize status = %d; want 200", initRec.Code)
		}
		var initResp api.MCPResponse
		if err := json.Unmarshal(initRec.Body.Bytes(), &initResp); err != nil {
			t.Fatalf("decode initialize response: %v", err)
		}
		var initResult api.MCPInitializeResult
		if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
			t.Fatalf("decode initialize result: %v", err)
		}
		if initResult.ServerInfo.Version != "0.4.0" {
			t.Fatalf("serverInfo.version = %q; want 0.4.0", initResult.ServerInfo.Version)
		}

		// The configured public API origin is the server's own; the request
		// host, which a DNS-rebinding page controls, is not.
		for origin, want := range map[string]int{"http://example.com": http.StatusForbidden, "https://api.kodema.example:8443": http.StatusOK} {
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
			Resource:               "https://api.kodema.example:8443",
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
