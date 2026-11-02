package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/identity"
	identityoidc "github.com/optimiware/kodama/internal/identity/oidc"
	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

func exchangeFixture(t *testing.T, mutate func(map[string]any)) (*ExchangeProvider, *oidctest.Stub, string) {
	t.Helper()
	issuer := oidctest.NewStub(t)
	issuer.Audience = "https://mcp.example.test/mcp"
	apiAudience := "https://api.example.test"
	claims := issuer.Claims("user|1")
	claims["aud"] = []string{apiAudience}
	claims["act"] = map[string]any{"sub": "kodama-mcp"}
	if mutate != nil {
		mutate(claims)
	}
	apiToken := issuer.Token(t, claims)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != tokenExchangeGrant ||
			r.Form.Get("subject_token") != "mcp-token" ||
			r.Form.Get("resource") != apiAudience ||
			r.Form.Get("audience") != apiAudience ||
			r.Form.Get("scope") != "kodama.api" {
			http.Error(w, "bad exchange request", http.StatusBadRequest)
			return
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != "kodama-mcp" || pass != "exchange-secret" {
			http.Error(w, "bad client authentication", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": apiToken, "token_type": "Bearer",
			"issued_token_type": accessTokenType, "expires_in": 600,
		})
	}))
	t.Cleanup(srv.Close)

	client, err := httpClient(5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identityoidc.NewVerifier(identityoidc.Config{
		Issuer: issuer.URL, Audience: apiAudience, AllowInsecureHTTP: true, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ExchangeURL: srv.URL, ExchangeClientID: "kodama-mcp", ExchangeClientSecretFile: "/unused",
		ExchangeScopes: []string{"kodama.api"}, APIResource: apiAudience, APIAudience: apiAudience,
	}
	p, err := NewExchangeProvider(cfg, client, "exchange-secret", verifier)
	if err != nil {
		t.Fatal(err)
	}
	return p, issuer, apiToken
}

func TestExchangeProviderReturnsSeparateAPIToken(t *testing.T) {
	p, _, apiToken := exchangeFixture(t, nil)
	got, err := p.Token(context.Background(), Caller{
		SubjectToken: "mcp-token",
		Principal:    identity.Principal{Issuer: "https://issuer.example", Subject: "user|1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != apiToken || got == "mcp-token" {
		t.Fatalf("exchanged token = %q", got)
	}
}

func TestExchangeProviderRejectsWrongAPIAudience(t *testing.T) {
	p, _, _ := exchangeFixture(t, func(c map[string]any) { c["aud"] = []string{"https://other.example"} })
	_, err := p.Token(context.Background(), Caller{
		SubjectToken: "mcp-token",
		Principal:    identity.Principal{Issuer: "https://issuer.example", Subject: "user|1"},
	})
	if err == nil || !strings.Contains(err.Error(), "not valid for the Kodama API") {
		t.Fatalf("error = %v", err)
	}
}

func TestExchangeProviderRejectsSubjectChange(t *testing.T) {
	p, _, _ := exchangeFixture(t, func(c map[string]any) { c["sub"] = "user|2" })
	_, err := p.Token(context.Background(), Caller{
		SubjectToken: "mcp-token",
		Principal:    identity.Principal{Issuer: "https://issuer.example", Subject: "user|1"},
	})
	if err == nil || !strings.Contains(err.Error(), "represented subject") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadSecretRejectsMissingAndMalformed(t *testing.T) {
	if _, err := ReadSecret(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing secret accepted")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(path); err == nil {
		t.Fatal("malformed secret accepted")
	}
}
