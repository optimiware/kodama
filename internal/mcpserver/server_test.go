package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

type staticOAuth struct{ token string }

func (s staticOAuth) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: s.token}), nil
}
func (staticOAuth) Authorize(context.Context, *http.Request, *http.Response) error { return nil }

type testServer struct {
	mcp         *Server
	http        *httptest.Server
	issuer      *oidctest.Stub
	api         *httptest.Server
	exchange    *httptest.Server
	seen        chan string
	mcpAudience string
	apiAudience string
	apiToken    string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	seen := make(chan string, 8)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "req_mcp")
		switch r.URL.Path {
		case "/api/v1/me":
			_, _ = w.Write([]byte(`{"id":"u_1","issuer":"` + r.Host + `","subject":"user|1"}`))
		default:
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":null}`))
		}
	}))
	t.Cleanup(apiSrv.Close)

	issuer := oidctest.NewStub(t)
	mcpAudience := "https://mcp.kodama.example.test/mcp"
	apiAudience := "https://api.kodama.example.test"
	issuer.Audience = mcpAudience
	apiClaims := issuer.Claims("user|1")
	apiClaims["aud"] = []string{apiAudience}
	apiClaims["act"] = map[string]any{"sub": "kodama-mcp"}
	apiToken := issuer.Token(t, apiClaims)

	exchangeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != tokenExchangeGrant ||
			r.Form.Get("subject_token") == "" ||
			r.Form.Get("resource") != apiAudience ||
			r.Form.Get("audience") != apiAudience {
			http.Error(w, "bad exchange request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      apiToken,
			"token_type":        "Bearer",
			"issued_token_type": accessTokenType,
			"expires_in":        600,
		})
	}))
	t.Cleanup(exchangeSrv.Close)

	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "client-secret")
	if err := os.WriteFile(secretFile, []byte("exchange-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mcpSrv := httptest.NewUnstartedServer(nil)
	resource := "http://" + mcpSrv.Listener.Addr().String() + "/mcp"
	srv, err := New(Config{
		APIURL: apiSrv.URL + "/api/v1", Resource: resource, Audience: mcpAudience, Issuer: issuer.URL, Listen: "127.0.0.1:0",
		APIAudience: apiAudience, APIIssuer: issuer.URL, APIResource: apiAudience,
		ExchangeURL: exchangeSrv.URL, ExchangeClientID: "kodama-mcp", ExchangeClientSecretFile: secretFile,
		AllowInsecureHTTP: true, HTTPTimeout: 5 * time.Second, MaxResponseBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv.Config.Handler = srv.Handler()
	mcpSrv.Start()
	t.Cleanup(mcpSrv.Close)
	return &testServer{
		mcp: srv, http: mcpSrv, issuer: issuer, api: apiSrv, exchange: exchangeSrv, seen: seen,
		mcpAudience: mcpAudience, apiAudience: apiAudience, apiToken: apiToken,
	}
}

func TestServerOAuthMetadataAndChallenge(t *testing.T) {
	ts := newTestServer(t)
	resp, err := ts.http.Client().Get(ts.http.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d", resp.StatusCode)
	}
	var meta struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Resource != ts.mcp.cfg.Resource || len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != ts.issuer.URL {
		t.Fatalf("metadata = %+v", meta)
	}
	if len(meta.BearerMethods) != 1 || meta.BearerMethods[0] != "header" {
		t.Fatalf("bearer methods = %+v", meta.BearerMethods)
	}

	unauth, err := ts.http.Client().Get(ts.http.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauth.StatusCode)
	}
	challenge := unauth.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, "resource_metadata=") || !strings.Contains(challenge, "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("challenge = %q", challenge)
	}
}

func TestServerMCPClientListsAndCallsTools(t *testing.T) {
	ts := newTestServer(t)
	token := ts.issuer.Token(t, ts.issuer.Claims("user|1"))
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.mcp.cfg.Resource, HTTPClient: ts.http.Client(), OAuthHandler: staticOAuth{token: token},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(tools.Tools), len(ts.mcp.Operations()); got != want {
		t.Fatalf("tools = %d, want %d (secret writes are opt-in)", got, want)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "kodama_get_me" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("get_me annotations = %+v", tool.Annotations)
			}
		}
	}
	if !found {
		t.Fatal("kodama_get_me not listed")
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "kodama_get_me", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty content")
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, `"u_1"`) {
		t.Fatalf("content = %#v", res.Content[0])
	}
	select {
	case got := <-ts.seen:
		if got != "Bearer "+ts.apiToken {
			t.Fatalf("forwarded authorization = %q, want the exchanged API token", got)
		}
		if got == "Bearer "+token {
			t.Fatal("the MCP client token was forwarded to the API")
		}
	case <-time.After(time.Second):
		t.Fatal("API was not called")
	}
}

func TestServerExchangeFailureDoesNotCallAPI(t *testing.T) {
	ts := newTestServer(t)
	ts.exchange.Close()
	token := ts.issuer.Token(t, ts.issuer.Claims("user|1"))
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.mcp.cfg.Resource, HTTPClient: ts.http.Client(), OAuthHandler: staticOAuth{token: token},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "kodama_get_me", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("tool call succeeded despite failed exchange: %+v", res)
	}
	select {
	case got := <-ts.seen:
		t.Fatalf("API was called after exchange failure: %q", got)
	default:
	}
}

func TestServerRejectsWrongAudienceAndIDToken(t *testing.T) {
	ts := newTestServer(t)
	for name, claims := range map[string]map[string]any{
		"wrong audience": func() map[string]any {
			c := ts.issuer.Claims("u")
			c["aud"] = []string{"https://other.example"}
			return c
		}(),
		"id token": func() map[string]any {
			c := ts.issuer.Claims("u")
			c["nonce"] = "n"
			return c
		}(),
		"expired": func() map[string]any {
			c := ts.issuer.Claims("u")
			c["exp"] = time.Now().Add(-time.Minute).Unix()
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			token := ts.issuer.Token(t, claims)
			req, _ := http.NewRequest(http.MethodPost, ts.http.URL+"/mcp", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := ts.http.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d body = %s", resp.StatusCode, body)
			}
			body, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(body), token) {
				t.Fatal("response leaked the token")
			}
		})
	}
}

var _ auth.OAuthHandler = staticOAuth{}
