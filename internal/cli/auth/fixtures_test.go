package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/root"
)

// provider is a local OAuth authorization server fixture. It validates the
// S256 verifier binding on the token endpoint and records every exchange.
type provider struct {
	srv *httptest.Server

	mu         sync.Mutex
	challenge  string
	issuedCode string
	exchanges  atomic.Int32
	lastForm   url.Values

	// behaviour switches
	status       int  // non-zero: answer the token request with this status
	drop         bool // close the connection without answering
	hang         bool // accept the token request and never answer
	omitToken    bool
	tokenType    string
	accessToken  string
	idToken      string
	expiresInSec int
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{accessToken: "access-token-t017", idToken: "id-token-t017", tokenType: "Bearer", expiresInSec: 3600}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	p.exchanges.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.lastForm = r.PostForm
	challenge, code := p.challenge, p.issuedCode
	p.mu.Unlock()
	if p.hang {
		<-r.Context().Done()
		return
	}
	if p.drop {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		_ = c.Close()
		return
	}
	if p.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != code ||
		got != challenge || r.PostForm.Get("client_id") != "kodama-cli" || r.PostForm.Get("client_secret") != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	body := map[string]any{"token_type": p.tokenType, "expires_in": p.expiresInSec, "id_token": p.idToken}
	if !p.omitToken {
		body["access_token"] = p.accessToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (p *provider) record(authURL string) url.Values {
	u, _ := url.Parse(authURL)
	q := u.Query()
	p.mu.Lock()
	p.challenge = q.Get("code_challenge")
	p.issuedCode = "code-t017"
	p.mu.Unlock()
	return q
}

// freePort returns a loopback port that is free now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func portIsFree(port int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// memStore is an in-memory CredentialStore recording operations.
type memStore struct {
	mu      sync.Mutex
	creds   map[string]Credential
	saves   int
	saveErr error
}

func newMemStore() *memStore { return &memStore{creds: map[string]Credential{}} }

func (m *memStore) Load(_ context.Context, key string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[key]
	if !ok {
		return Credential{}, storeErr(ErrCredentialNotFound, nil, "absent")
	}
	return c, nil
}

func (m *memStore) Save(_ context.Context, key string, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saves++
	m.creds[key] = c
	return nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.creds, key)
	return nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.creds)
}

// loginEnv assembles a Service, store and effective context against p.
type loginEnv struct {
	svc   *Service
	store *memStore
	ec    root.EffectiveContext
	port  int
	prov  *provider
}

func newLoginEnv(t *testing.T, p *provider, browser BrowserOpener, mods ...func(*ServiceConfig)) *loginEnv {
	t.Helper()
	port := freePort(t)
	store := newMemStore()
	cfg := ServiceConfig{Keyring: store, Browser: browser, AllowInsecureLoopbackEndpoints: true, LoginTimeout: 10 * time.Second}
	for _, m := range mods {
		m(&cfg)
	}
	ec := root.EffectiveContext{
		Context: root.Context{
			Factory: "https://factory.t017.example.invalid",
			Auth: root.AuthConfig{
				Issuer: p.srv.URL, ClientID: "kodama-cli", AuthorizationEndpoint: p.srv.URL + "/authorize",
				TokenEndpoint: p.srv.URL + "/token", Scopes: []string{"openid", "kodama"},
				RedirectURI: "http://127.0.0.1:" + strconv.Itoa(port) + "/callback",
			},
		},
		CredentialStore: root.StoreKeyring,
	}
	return &loginEnv{svc: NewService(cfg), store: store, ec: ec, port: port, prov: p}
}

// callback performs a GET against the redirect URI from the authorization URL.
func callback(t *testing.T, redirect string, query url.Values) int {
	t.Helper()
	res, err := http.Get(redirect + "?" + query.Encode())
	if err != nil {
		return -1
	}
	defer res.Body.Close()
	return res.StatusCode
}

// approving is a BrowserOpener that completes the flow like a user who agrees.
func approving(p *provider, then func(redirect, state string)) BrowserOpener {
	return BrowserFunc(func(_ context.Context, authURL string) error {
		q := p.record(authURL)
		go then(q.Get("redirect_uri"), q.Get("state"))
		return nil
	})
}
