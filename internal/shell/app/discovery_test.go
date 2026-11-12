package app

// These tests pin the Shell composition of the public discovery document. The
// REST edge owns the route and its validation; the composition owns the one
// configured document, reuses the OIDC verifier issuer/audience as the single
// source of truth, and leaves the route a clean 404 when login is not
// published. A served document never carries a client secret or key material.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

const (
	discoveryTestIssuer = "https://auth.example.com"
	discoveryWarning    = "public login discovery is not configured"
)

// discoveryTestConfig returns one valid development discovery configuration.
func discoveryTestConfig(issuer string) *DiscoveryConfig {
	return &DiscoveryConfig{
		Issuer:                      issuer,
		ClientID:                    "kodama-cli",
		Audience:                    "kodama",
		AuthorizationEndpoint:       issuer + "/authorize",
		TokenEndpoint:               issuer + "/token",
		DeviceAuthorizationEndpoint: issuer + "/device",
		RevocationEndpoint:          issuer + "/revoke",
		Scopes:                      []string{"openid", "profile", "email", "offline_access"},
		APIURL:                      "https://api.example.com/kodama&cli",
	}
}

// discoveryTestDocument is the exact document a composition serves for the
// matching configuration.
func discoveryTestDocument(issuer, audience string) api.DiscoveryDocument {
	return api.DiscoveryDocument{
		Issuer:                      issuer,
		ClientID:                    "kodama-cli",
		Audience:                    audience,
		AuthorizationEndpoint:       issuer + "/authorize",
		TokenEndpoint:               issuer + "/token",
		DeviceAuthorizationEndpoint: issuer + "/device",
		RevocationEndpoint:          issuer + "/revoke",
		Scopes:                      []string{"openid", "profile", "email", "offline_access"},
		APIURL:                      "https://api.example.com/kodama&cli",
	}
}

// getDiscovery performs one in-process discovery request against the composed
// public handler.
func getDiscovery(t *testing.T, handler http.Handler, header http.Header) (int, http.Header, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, api.DiscoveryPath, nil)
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Header().Clone(), recorder.Body.Bytes()
}

// decodeDiscovery strictly decodes one served document.
func decodeDiscovery(t *testing.T, body []byte) api.DiscoveryDocument {
	t.Helper()
	document, err := api.DecodeDiscoveryDocument(body)
	if err != nil {
		t.Fatalf("DecodeDiscoveryDocument: %v; body=%s", err, string(body))
	}
	return document
}

// serveComposedApp serves the already constructed app on one injected loopback
// listener and registers its teardown.
func serveComposedApp(t *testing.T, app *App) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- app.Serve(ctx, listener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	})
	return "http://" + listener.Addr().String()
}

// TestDevelopmentCompositionServesDiscoveryOnTheInjectedListener proves the
// real composed public handler serves the exact configured document without a
// credential, and that a presented credential is ignored rather than required.
func TestDevelopmentCompositionServesDiscoveryOnTheInjectedListener(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.Discovery = discoveryTestConfig(discoveryTestIssuer)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := serveComposedApp(t, app)

	request, err := http.NewRequest(http.MethodGet, base+api.DiscoveryPath, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer an-unrelated-development-credential")

	var (
		status int
		body   []byte
	)
	eventually(t, 5*time.Second, func() bool {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			return false
		}
		status, body = response.StatusCode, raw
		return status == http.StatusOK
	})
	if status != http.StatusOK {
		t.Fatalf("discovery status = %d; want 200", status)
	}
	document := decodeDiscovery(t, body)
	if want := discoveryTestDocument(discoveryTestIssuer, "kodama"); !reflect.DeepEqual(document, want) {
		t.Fatalf("served discovery = %+v; want %+v", document, want)
	}
}

// TestTestCompositionServesDiscovery proves the fully injected test profile
// serves the same exact document.
func TestTestCompositionServesDiscovery(t *testing.T) {
	config := fullTestProfileConfig(t)
	config.Discovery = discoveryTestConfig(discoveryTestIssuer)
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New ProfileTest: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	status, header, body := getDiscovery(t, app.Handler(), nil)
	if status != http.StatusOK {
		t.Fatalf("discovery status = %d; want 200; body=%s", status, string(body))
	}
	if header.Get("Content-Type") != api.MediaType {
		t.Fatalf("Content-Type = %q; want %q", header.Get("Content-Type"), api.MediaType)
	}
	document := decodeDiscovery(t, body)
	if want := discoveryTestDocument(discoveryTestIssuer, "kodama"); !reflect.DeepEqual(document, want) {
		t.Fatalf("served discovery = %+v; want %+v", document, want)
	}
}

// TestCompositionWithoutDiscoveryKeepsRouteNotFound documents the clean 404:
// kodactl login then reports that the login configuration is unavailable instead
// of inventing OAuth endpoints.
func TestCompositionWithoutDiscoveryKeepsRouteNotFound(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)

	status, _, body := getDiscovery(t, app.Handler(), http.Header{
		"Authorization": []string{"Bearer any-credential"},
	})
	if status != http.StatusNotFound {
		t.Fatalf("discovery status = %d; want 404; body=%s", status, string(body))
	}
	var publicError api.Error
	if err := json.Unmarshal(body, &publicError); err != nil {
		t.Fatalf("decode refusal: %v; body=%s", err, string(body))
	}
	if publicError.Code != api.CodeNotFound {
		t.Fatalf("refusal code = %q; want %q", publicError.Code, api.CodeNotFound)
	}
	for _, forbidden := range []string{"client_secret", "jwks", "private", "key"} {
		if strings.Contains(strings.ToLower(string(body)), forbidden) {
			t.Fatalf("404 body carries forbidden material %q: %s", forbidden, string(body))
		}
	}
}

// TestProductionCompositionServesDiscoveryAndReusesOIDC proves the production
// profile serves a document whose issuer and audience are the OIDC verifier's,
// even when the discovery configuration leaves them unset.
func TestProductionCompositionServesDiscoveryAndReusesOIDC(t *testing.T) {
	config, logs := productionDiscoveryConfig(t)
	discovery := discoveryTestConfig("https://issuer.example")
	// The issuer and audience deliberately come only from the OIDC verifier.
	discovery.Issuer = ""
	discovery.Audience = ""
	discovery.APIURL = "https://kodama.example"
	config.Production.Discovery = discovery

	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	status, _, body := getDiscovery(t, app.Handler(), nil)
	if status != http.StatusOK {
		t.Fatalf("discovery status = %d; want 200; body=%s", status, string(body))
	}
	document := decodeDiscovery(t, body)
	want := discoveryTestDocument("https://issuer.example", "kodama")
	want.ClientID = "kodama-cli"
	want.APIURL = "https://kodama.example"
	if !reflect.DeepEqual(document, want) {
		t.Fatalf("served discovery = %+v; want %+v", document, want)
	}
	if strings.Contains(logs.String(), discoveryWarning) {
		t.Fatalf("configured discovery still logged the unavailable-login warning: %s", logs.String())
	}
}

// TestProductionWithoutDiscoveryWarnsOnceAndStillStarts pins the safer-for-users
// choice: service-only production deployments start successfully, log exactly
// one warning class, and keep the route a clean 404.
func TestProductionWithoutDiscoveryWarnsOnceAndStillStarts(t *testing.T) {
	config, logs := productionDiscoveryConfig(t)
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition without discovery: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if count := strings.Count(logs.String(), discoveryWarning); count != 1 {
		t.Fatalf("login warning count = %d; want exactly 1; logs=%s", count, logs.String())
	}
	status, _, body := getDiscovery(t, app.Handler(), nil)
	if status != http.StatusNotFound {
		t.Fatalf("discovery status = %d; want 404; body=%s", status, string(body))
	}
}

// TestDiscoveryConfigurationIsRefusedAtConstruction refuses a bad or drifting
// document before any adapter or goroutine exists.
func TestDiscoveryConfigurationIsRefusedAtConstruction(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T) Config
		want   string
	}{
		{
			name: "production issuer drift",
			mutate: func(t *testing.T) Config {
				config, _ := productionDiscoveryConfig(t)
				discovery := discoveryTestConfig("https://issuer.example")
				discovery.Issuer = "https://other.example"
				config.Production.Discovery = discovery
				return config
			},
			want: "discovery issuer differs",
		},
		{
			name: "production audience drift",
			mutate: func(t *testing.T) Config {
				config, _ := productionDiscoveryConfig(t)
				discovery := discoveryTestConfig("https://issuer.example")
				discovery.Audience = "another-audience"
				config.Production.Discovery = discovery
				return config
			},
			want: "discovery audience differs",
		},
		{
			name: "two disagreeing copies",
			mutate: func(t *testing.T) Config {
				config, _ := productionDiscoveryConfig(t)
				config.Discovery = discoveryTestConfig("https://issuer.example")
				other := discoveryTestConfig("https://issuer.example")
				other.ClientID = "another-client"
				config.Production.Discovery = other
				return config
			},
			want: "declared differently",
		},
		{
			name: "empty public client id",
			mutate: func(t *testing.T) Config {
				config := newTestEnvironment(t).config
				config.Discovery = discoveryTestConfig(discoveryTestIssuer)
				config.Discovery.ClientID = ""
				return config
			},
			want: "client_id is empty",
		},
		{
			name: "plain http endpoint outside loopback",
			mutate: func(t *testing.T) Config {
				config := newTestEnvironment(t).config
				config.Discovery = discoveryTestConfig(discoveryTestIssuer)
				config.Discovery.TokenEndpoint = "http://other.example/token"
				return config
			},
			want: "must use https outside numeric loopback",
		},
		{
			name: "foreign authorization endpoint origin",
			mutate: func(t *testing.T) Config {
				config := newTestEnvironment(t).config
				config.Discovery = discoveryTestConfig(discoveryTestIssuer)
				config.Discovery.AuthorizationEndpoint = "https://other.example/authorize"
				return config
			},
			want: "does not share the issuer origin",
		},
		{
			name: "empty scopes",
			mutate: func(t *testing.T) Config {
				config := newTestEnvironment(t).config
				config.Discovery = discoveryTestConfig(discoveryTestIssuer)
				config.Discovery.Scopes = nil
				return config
			},
			want: "scopes are empty",
		},
		{
			name: "plain http api url outside loopback",
			mutate: func(t *testing.T) Config {
				config := newTestEnvironment(t).config
				config.Discovery = discoveryTestConfig(discoveryTestIssuer)
				config.Discovery.APIURL = "http://api.example.com"
				return config
			},
			want: "api_url",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			app, err := New(context.Background(), testCase.mutate(t))
			if err == nil {
				t.Fatal("New accepted an invalid discovery configuration")
			}
			if app != nil {
				t.Fatal("New returned an app for an invalid discovery configuration")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
			eventually(t, time.Second, func() bool {
				return runtime.NumGoroutine() <= baseline+1
			})
		})
	}
}

// TestServedDiscoveryCarriesNoSecretOrKeyMaterial scans the served bytes for
// the members a public client configuration must never publish.
func TestServedDiscoveryCarriesNoSecretOrKeyMaterial(t *testing.T) {
	development := newTestEnvironment(t)
	development.config.Discovery = discoveryTestConfig(discoveryTestIssuer)
	developmentApp := development.startApp(t)

	production, _ := productionDiscoveryConfig(t)
	discovery := discoveryTestConfig("https://issuer.example")
	discovery.Issuer = ""
	discovery.Audience = ""
	discovery.APIURL = "https://kodama.example"
	production.Production.Discovery = discovery
	productionApp, err := New(context.Background(), production)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := productionApp.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	for name, handler := range map[string]http.Handler{
		"development": developmentApp.Handler(),
		"production":  productionApp.Handler(),
	} {
		status, _, body := getDiscovery(t, handler, nil)
		if status != http.StatusOK {
			t.Fatalf("%s discovery status = %d; want 200", name, status)
		}
		lower := strings.ToLower(string(body))
		for _, forbidden := range []string{"client_secret", "jwks", "private", "key"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s discovery carries forbidden member %q: %s", name, forbidden, string(body))
			}
		}
		// The strict decoder refuses every unknown member, so a served document
		// can never smuggle a secret member past the published shape.
		decodeDiscovery(t, body)
	}
}

// TestStaticDiscoveryReturnsIndependentCopies pins that no caller can mutate
// the shared configuration, including under concurrent requests.
func TestStaticDiscoveryReturnsIndependentCopies(t *testing.T) {
	want := discoveryTestDocument(discoveryTestIssuer, "kodama")
	provider, err := newStaticDiscovery(want)
	if err != nil {
		t.Fatalf("newStaticDiscovery: %v", err)
	}

	first, err := provider.Discovery(context.Background())
	if err != nil {
		t.Fatalf("Discovery: %v", err)
	}
	first.Issuer = "https://mutated.example"
	first.Scopes[0] = "mutated"

	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			document, err := provider.Discovery(context.Background())
			if err != nil {
				t.Errorf("concurrent Discovery: %v", err)
				return
			}
			document.Scopes[0] = "mutated"
			document.APIURL = "https://mutated.example"
		})
	}
	wait.Wait()

	second, err := provider.Discovery(context.Background())
	if err != nil {
		t.Fatalf("Discovery: %v", err)
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("shared document mutated: %+v; want %+v", second, want)
	}
}

// TestDiscoveryWiringLeavesOperationsAndMCPUnchanged pins that the composition
// still advertises exactly the 51 declared operations and the same MCP catalog.
func TestDiscoveryWiringLeavesOperationsAndMCPUnchanged(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.config.Discovery = discoveryTestConfig(discoveryTestIssuer)
	app := environment.startApp(t)

	if routes := api.Routes(); len(routes) != 51 {
		t.Fatalf("api.Routes() has %d entries; want the 51 declared operations", len(routes))
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("tools/list status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(response.Result.Tools) != 51 {
		t.Fatalf("tools/list returned %d tools; want 51", len(response.Result.Tools))
	}
	for index, route := range api.Routes() {
		if response.Result.Tools[index].Name != route.Operation {
			t.Fatalf("tool %d = %q; want %q", index, response.Result.Tools[index].Name, route.Operation)
		}
	}
}

// productionDiscoveryConfig builds the construction-only production input with
// the fake NATS dial and a captured logger.
func productionDiscoveryConfig(t *testing.T) (Config, *bytes.Buffer) {
	t.Helper()
	config := validProductionConfig(t)
	config.Production.dialNATS = (&fakeNATSServer{}).dial
	logs := &bytes.Buffer{}
	config.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return config, logs
}
