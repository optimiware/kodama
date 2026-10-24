package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/layer"
)

// Resource prefix of this task's containers and labels.
const Prefix = "t010"

// The development client of the fixture profile (docs/dev/configuration.md): the
// public kodama-cli client with S256 PKCE and its loopback redirect URI.
const (
	ClientID    = "kodama-cli"
	RedirectURI = "http://127.0.0.1:18999/callback"
	// APIAudience is the factory API audience of the fixture profile.
	APIAudience = "https://kodama.example.test/api"
)

// RequireEnv is the existing switch that turns an unavailable dependency into a
// failure instead of a skip (see test/support/postgres).
const RequireEnv = "KODAMA_REQUIRE_POSTGRES"

// Personas are the fixture's people. Their `roles` claims are test inputs only:
// no persona's claims establish membership.
var Personas = []Persona{
	{ID: "admin", Roles: []string{"admin"}},
	{ID: "analyst", Roles: []string{"analyst"}},
	{ID: "engineer", Roles: []string{"engineer"}},
	{ID: "reviewer", Roles: []string{"reviewer"}},
	{ID: "viewer", Roles: []string{"viewer"}},
	{ID: "outsider", Roles: []string{"admin", "reviewer"}}, // claims galore, bindings none
	{ID: "foreign", Roles: []string{"engineer"}},
}

// Persona is one oauthsonas persona.
type Persona struct {
	ID    string
	Roles []string
}

// Subject is the persona's token subject.
func (p Persona) Subject() string { return "oauthsonas|" + p.ID }

// Fixture is one running, isolated oauthsonas instance: its own issuer URL, signing
// keys and persona set. Tests never touch another instance or the shared
// development issuer's configuration.
type Fixture struct {
	Issuer      string
	Audience    string
	ClientID    string
	RedirectURI string

	t      testing.TB
	client *http.Client
}

// Tokens is one authorization-code exchange.
type Tokens struct {
	Access  string
	ID      string
	Persona string
	// Nonce is the nonce the ID token must carry.
	Nonce string
}

// Start launches an isolated oauthsonas container with the pinned image and
// waits for discovery. It skips the test when no container provider is
// available, or fails when KODAMA_REQUIRE_POSTGRES=1.
func Start(t testing.TB) *Fixture {
	t.Helper()
	layer.SkipService(t, "OIDC issuer")
	provider, err := containerProvider()
	if err != nil {
		unavailable(t, err.Error())
	}
	image, err := pinnedImage()
	if err != nil {
		t.Fatalf("oauthsonas image: %v", err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	issuer := fmt.Sprintf("http://127.0.0.1:%d", port)

	dir := t.TempDir()
	cfg := map[string]any{
		"issuer": issuer, "api_audience": APIAudience, "token_ttl": "15m", "authorization_code_ttl": "5m", "refresh_token_ttl": "8h",
		"clients": []map[string]any{{"id": ClientID, "name": "Kodama development CLI", "public": true,
			"redirect_uris": []string{RedirectURI}, "post_logout_redirect_uris": []string{"http://127.0.0.1:18999/"}, "allowed_origins": []string{"http://127.0.0.1:18999"}}},
	}
	var personas []map[string]any
	for _, p := range Personas {
		personas = append(personas, map[string]any{"id": p.ID, "subject": p.Subject(), "email": p.ID + "@example.test", "name": "Fixture " + p.ID, "roles": p.Roles})
	}
	cfg["personas"] = personas
	data, _ := json.Marshal(cfg)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oauthsonas.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("%s-oauthsonas-%s", Prefix, randHex(t, 6))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, provider, "run", "-d", "--name", name, "--label", "kodama.task="+Prefix,
		"-p", fmt.Sprintf("127.0.0.1:%d:8181", port), "-e", "OAUTHSONAS_ALLOW_NON_LOOPBACK=true",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "-v", dir+":/secrets:ro",
		image, "--config", "/secrets/oauthsonas.json", "--listen", "0.0.0.0:8181").CombinedOutput()
	t.Cleanup(func() {
		rm := exec.Command(provider, "rm", "-f", name)
		_ = rm.Run()
	})
	if err != nil {
		unavailable(t, fmt.Sprintf("cannot start the oauthsonas container with %s: %v: %s", provider, err, strings.TrimSpace(string(out))))
	}

	f := &Fixture{Issuer: issuer, Audience: APIAudience, ClientID: ClientID, RedirectURI: RedirectURI, t: t,
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := f.client.Get(issuer + "/.well-known/openid-configuration")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return f
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command(provider, "logs", name).CombinedOutput()
			t.Fatalf("oauthsonas did not become ready: %v\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func unavailable(t testing.TB, why string) {
	t.Helper()
	if os.Getenv(RequireEnv) == "1" {
		t.Fatalf("real OIDC issuer required (%s=1): %s", RequireEnv, why)
	}
	t.Skipf("real OIDC issuer unavailable: %s (set %s=1 to require it)", why, RequireEnv)
}

// containerProvider follows the development scripts' rule: DEV_PROVIDER, else
// podman by default on every OS, with explicit Docker selection for CI.
func containerProvider() (string, error) {
	p := os.Getenv("DEV_PROVIDER")
	if p == "" {
		p = "podman"
	}
	path, err := exec.LookPath(p)
	if err != nil {
		return "", fmt.Errorf("container provider %q not found", p)
	}
	return path, nil
}

var imageLine = regexp.MustCompile(`image:\s*(ghcr\.io/optimiware/oauthsonas:\S+)`)

// pinnedImage reads the digest-pinned image from the development Compose file, so
// the pin has one owner.
func pinnedImage() (string, error) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "dev", "compose.yaml"))
	if err != nil {
		return "", err
	}
	m := imageLine.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("no oauthsonas image in dev/compose.yaml")
	}
	return string(m[1]), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// Discovery is the subset of the discovery document the flow uses.
type Discovery struct {
	Issuer        string `json:"issuer"`
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	JWKS          string `json:"jwks_uri"`
}

// Discover fetches the discovery document.
func (f *Fixture) Discover() Discovery {
	f.t.Helper()
	resp, err := f.client.Get(f.Issuer + "/.well-known/openid-configuration")
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var d Discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		f.t.Fatal(err)
	}
	return d
}

// Login runs the authorization-code flow with S256 PKCE for the persona, using a
// unique state, nonce and verifier per call, and returns independently issued
// tokens.
func (f *Fixture) Login(persona string) Tokens {
	f.t.Helper()
	d := f.Discover()
	state, nonce := "st"+randHex(f.t, 16), "nc"+randHex(f.t, 16)
	verifier := randHex(f.t, 32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {f.ClientID}, "response_type": {"code"}, "scope": {"openid profile email"}, "redirect_uri": {f.RedirectURI},
		"state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "login_hint": {persona},
	}
	resp, err := f.client.Get(d.Authorization + "?" + q.Encode())
	if err != nil {
		f.t.Fatalf("authorize: %v", err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		f.t.Fatalf("authorize for %s returned HTTP %d without a redirect", persona, resp.StatusCode)
	}
	if loc.Query().Get("state") != state || loc.Query().Get("code") == "" {
		f.t.Fatalf("authorize for %s: %s (%s)", persona, loc.Query().Get("error"), loc.Query().Get("error_description"))
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {f.ClientID}, "redirect_uri": {f.RedirectURI}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}}
	tresp, err := f.client.PostForm(d.Token, form)
	if err != nil {
		f.t.Fatalf("token: %v", err)
	}
	defer tresp.Body.Close()
	body, _ := io.ReadAll(tresp.Body)
	if tresp.StatusCode != http.StatusOK {
		f.t.Fatalf("token exchange for %s: HTTP %d", persona, tresp.StatusCode)
	}
	var tok struct {
		Access string `json:"access_token"`
		ID     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Access == "" || tok.ID == "" {
		f.t.Fatalf("token response lacks tokens: %v", err)
	}
	return Tokens{Access: tok.Access, ID: tok.ID, Persona: persona, Nonce: nonce}
}

// Subject returns a persona's subject.
func Subject(persona string) string { return "oauthsonas|" + persona }
