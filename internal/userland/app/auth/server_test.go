package auth_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/app/auth"
	"github.com/optimiware/kodama/internal/userland/principal"
)

func newPKCEPair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func extractHiddenInput(t *testing.T, htmlBody, fieldName string) string {
	t.Helper()
	pattern := regexp.MustCompile(`name="` + regexp.QuoteMeta(fieldName) + `"\s+value="([^"]*)"`)
	m := pattern.FindStringSubmatch(htmlBody)
	if len(m) != 2 {
		t.Fatalf("hidden input %q not found in HTML:\n%s", fieldName, htmlBody)
	}
	return m[1]
}

func noRedirectBrowserClient(t *testing.T, baseTransport http.RoundTripper) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Transport: baseTransport,
		Jar:       jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestStateDirPersistenceAndDeterministicKID(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "auth-state")

	srv1, err := auth.New(auth.Config{
		Issuer:        "https://auth.example.com",
		StateDir:      stateDir,
		LocalPersonas: []string{"admin"},
	})
	if err != nil {
		t.Fatalf("first auth.New: %v", err)
	}

	for _, name := range []string{"signing-key.pem", "cookie-secret.key"} {
		info, err := os.Stat(filepath.Join(stateDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s mode = %04o; want 0600", name, perm)
		}
	}

	srv2, err := auth.New(auth.Config{
		Issuer:        "https://auth.example.com",
		StateDir:      stateDir,
		LocalPersonas: []string{"admin"},
	})
	if err != nil {
		t.Fatalf("second auth.New: %v", err)
	}
	if srv1.KeyID() == "" || srv1.KeyID() != srv2.KeyID() {
		t.Fatalf("kid across restart = (%q, %q); want identical deterministic kid", srv1.KeyID(), srv2.KeyID())
	}

	// Loosening permissions on the signing key must be refused at startup.
	if err := os.Chmod(filepath.Join(stateDir, "signing-key.pem"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := auth.New(auth.Config{
		Issuer:        "https://auth.example.com",
		StateDir:      stateDir,
		LocalPersonas: []string{"admin"},
	}); err == nil {
		t.Fatal("world-readable signing-key.pem was accepted; want refusal")
	}
}

func TestLocalPKCEFlowAccessCodeRefreshRotationAndRevocation(t *testing.T) {
	var srv *auth.Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	var err error
	srv, err = auth.New(auth.Config{
		Issuer:        ts.URL,
		TenantID:      "tenant-1",
		ProjectID:     "project-1",
		LocalPersonas: []string{"admin", "engineer", "reviewer"},
		AccessCode:    "top-secret-code",
		RedirectURIs:  []string{"https://app.example.com/oauth/cb"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// Healthz & Discovery & JWKS
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %v, %v", resp, err)
	}
	_ = resp.Body.Close()

	resp, err = ts.Client().Get(ts.URL + "/jwks.json")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jwks.json = %v, %v", resp, err)
	}
	jwksBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(jwksBytes, &jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("JWKS = %s, err = %v", jwksBytes, err)
	}
	k := jwks.Keys[0]
	if k["kty"] != "EC" || k["crv"] != "P-256" || k["alg"] != "ES256" || k["use"] != "sig" || k["kid"] != srv.KeyID() || k["x"] == "" || k["y"] == "" {
		t.Fatalf("unexpected JWK fields: %+v", k)
	}

	// PKCE plain or missing must be refused; unregistered redirect_uri must be refused.
	verifier, challenge := newPKCEPair(t)
	for _, badQuery := range []string{
		"client_id=kodactl&redirect_uri=https://evil.example.com/cb&response_type=code&code_challenge=" + challenge + "&code_challenge_method=S256",
		"client_id=kodactl&redirect_uri=http://127.0.0.1:45123/callback&response_type=code",
		"client_id=kodactl&redirect_uri=http://127.0.0.1:45123/callback&response_type=code&code_challenge=" + verifier + "&code_challenge_method=plain",
		"client_id=kodactl&redirect_uri=http://127.0.0.1:45123/callback&response_type=token&code_challenge=" + challenge + "&code_challenge_method=S256",
	} {
		r, err := ts.Client().Get(ts.URL + "/authorize?" + badQuery)
		if err != nil {
			t.Fatalf("GET /authorize bad query: %v", err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad query %q returned %d; want 400", badQuery, r.StatusCode)
		}
	}

	// RFC 8252 §7.3 IPv6 dynamic loopback port must also work.
	redirectURI := "http://[::1]:54321/callback"
	browser := noRedirectBrowserClient(t, ts.Client().Transport)
	authURL := ts.URL + "/authorize?" + url.Values{
		"client_id":             {"kodactl"},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"state-xyz"},
		"nonce":                 {"nonce-123"},
		"login_hint":            {"reviewer"},
	}.Encode()

	getResp, err := browser.Get(authURL)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize = %v, %v", getResp, err)
	}
	pageBytes, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	page := string(pageBytes)

	wantPID, err := principal.ForOIDC(ts.URL, "reviewer")
	if err != nil {
		t.Fatalf("ForOIDC: %v", err)
	}
	if !strings.Contains(page, string(wantPID)) {
		t.Fatalf("login page did not display reviewer PrincipalID %q", wantPID)
	}

	txID := extractHiddenInput(t, page, "tx_id")
	csrf := extractHiddenInput(t, page, "csrf_token")

	// Missing/wrong CSRF token or cross-browser login-CSRF must be rejected with 403.
	otherBrowser := noRedirectBrowserClient(t, ts.Client().Transport)
	crossBrowserResp, err := otherBrowser.PostForm(ts.URL+"/authorize", url.Values{
		"tx_id":       {txID},
		"csrf_token":  {csrf},
		"persona":     {"reviewer"},
		"access_code": {"top-secret-code"},
	})
	if err != nil {
		t.Fatalf("POST /authorize cross-browser: %v", err)
	}
	_ = crossBrowserResp.Body.Close()
	if crossBrowserResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-browser login-CSRF status = %d; want 403", crossBrowserResp.StatusCode)
	}

	// Opening a second tab in the SAME browser must not invalidate Tab 1's CSRF token!
	tab2Resp, err := browser.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {"kodactl"},
		"redirect_uri":          {"http://127.0.0.1:59999/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"tab2-state"},
	}.Encode())
	if err != nil || tab2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize tab2 = %v, %v", tab2Resp, err)
	}
	_ = tab2Resp.Body.Close()

	// Missing/wrong CSRF token must be rejected with 403.
	badCSRFResp, err := browser.PostForm(ts.URL+"/authorize", url.Values{
		"tx_id":       {txID},
		"csrf_token":  {"tampered"},
		"persona":     {"reviewer"},
		"access_code": {"top-secret-code"},
	})
	if err != nil {
		t.Fatalf("POST /authorize bad CSRF: %v", err)
	}
	_ = badCSRFResp.Body.Close()
	if badCSRFResp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF status = %d; want 403", badCSRFResp.StatusCode)
	}

	// Wrong access code must be rejected with 403 and re-issue a fresh CSRF token.
	wrongCodeResp, err := browser.PostForm(ts.URL+"/authorize", url.Values{
		"tx_id":       {txID},
		"csrf_token":  {csrf},
		"persona":     {"reviewer"},
		"access_code": {"wrong-code"},
	})
	if err != nil {
		t.Fatalf("POST /authorize wrong access code: %v", err)
	}
	retryPageBytes, _ := io.ReadAll(wrongCodeResp.Body)
	_ = wrongCodeResp.Body.Close()
	if wrongCodeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong access code status = %d; want 403", wrongCodeResp.StatusCode)
	}
	csrf = extractHiddenInput(t, string(retryPageBytes), "csrf_token")

	// Valid access code issues redirect with code & state.
	okResp, err := browser.PostForm(ts.URL+"/authorize", url.Values{
		"tx_id":       {txID},
		"csrf_token":  {csrf},
		"persona":     {"reviewer"},
		"access_code": {"top-secret-code"},
	})
	if err != nil {
		t.Fatalf("POST /authorize valid: %v", err)
	}
	_ = okResp.Body.Close()
	if okResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /authorize status = %d; want 303", okResp.StatusCode)
	}
	cbURL, err := url.Parse(okResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	if cbURL.Query().Get("state") != "state-xyz" {
		t.Fatalf("callback state = %q; want state-xyz", cbURL.Query().Get("state"))
	}
	code := cbURL.Query().Get("code")
	if code == "" {
		t.Fatal("callback code is empty")
	}

	// Exchange code with valid PKCE verifier.
	tokResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {"kodactl"},
		"code_verifier": {verifier},
	})
	if err != nil || tokResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /token = %v, %v", tokResp, err)
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(tokResp.Body).Decode(&tokens); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	_ = tokResp.Body.Close()
	if tokens.TokenType != "Bearer" || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.IDToken == "" || tokens.ExpiresIn <= 0 {
		t.Fatalf("incomplete token response: %+v", tokens)
	}

	// Replaying the single-use authorization code must fail.
	replayCodeResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {"kodactl"},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatalf("replay code: %v", err)
	}
	_ = replayCodeResp.Body.Close()
	if replayCodeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed code status = %d; want 400", replayCodeResp.StatusCode)
	}

	// /whoami returns the exact derived PrincipalID.
	whoReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/whoami", nil)
	whoReq.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	whoResp, err := ts.Client().Do(whoReq)
	if err != nil || whoResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /whoami = %v, %v", whoResp, err)
	}
	var who map[string]string
	_ = json.NewDecoder(whoResp.Body).Decode(&who)
	_ = whoResp.Body.Close()
	if id.PrincipalID(who["principal_id"]) != wantPID || who["tenant"] != "tenant-1" || who["project"] != "project-1" {
		t.Fatalf("/whoami = %+v; want principal_id=%s", who, wantPID)
	}

	// Refresh token rotation + replay detection revoking the token family!
	rotResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tokens.RefreshToken},
		"client_id":     {"kodactl"},
	})
	if err != nil || rotResp.StatusCode != http.StatusOK {
		t.Fatalf("refresh = %v, %v", rotResp, err)
	}
	var rotated struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(rotResp.Body).Decode(&rotated)
	_ = rotResp.Body.Close()
	if rotated.RefreshToken == "" || rotated.RefreshToken == tokens.RefreshToken {
		t.Fatal("refresh token did not rotate")
	}

	// Replaying the old refresh token must fail AND revoke the rotated family!
	replayRefreshResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tokens.RefreshToken},
		"client_id":     {"kodactl"},
	})
	if err != nil {
		t.Fatalf("replay refresh: %v", err)
	}
	_ = replayRefreshResp.Body.Close()
	if replayRefreshResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed old refresh status = %d; want 400", replayRefreshResp.StatusCode)
	}

	afterCompromiseResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rotated.RefreshToken},
		"client_id":     {"kodactl"},
	})
	if err != nil {
		t.Fatalf("family after replay: %v", err)
	}
	_ = afterCompromiseResp.Body.Close()
	if afterCompromiseResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rotated token in compromised family status = %d; want 400", afterCompromiseResp.StatusCode)
	}

	// Revoking an access token invalidates /whoami.
	revResp, err := ts.Client().PostForm(ts.URL+"/revoke", url.Values{
		"token":           {tokens.AccessToken},
		"client_id":       {"kodactl"},
		"token_type_hint": {"access_token"},
	})
	if err != nil || revResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /revoke = %v, %v", revResp, err)
	}
	_ = revResp.Body.Close()

	whoAfterRev, err := ts.Client().Do(whoReq)
	if err != nil {
		t.Fatalf("GET /whoami after revoke: %v", err)
	}
	_ = whoAfterRev.Body.Close()
	if whoAfterRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/whoami after access_token revoke = %d; want 401", whoAfterRev.StatusCode)
	}
}

func TestDynamicClientRegistrationMCPFlow(t *testing.T) {
	var srv *auth.Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	var err error
	srv, err = auth.New(auth.Config{
		Issuer:        ts.URL,
		LocalPersonas: []string{"engineer"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	// Non-loopback plain HTTP redirect_uri must be rejected at /register.
	badRegBody, _ := json.Marshal(map[string]any{
		"client_name":   "Evil Client",
		"redirect_uris": []string{"http://evil.example.com/callback"},
	})
	badReg, err := ts.Client().Post(ts.URL+"/register", "application/json", bytes.NewReader(badRegBody))
	if err != nil {
		t.Fatalf("POST /register bad: %v", err)
	}
	_ = badReg.Body.Close()
	if badReg.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad /register status = %d; want 400", badReg.StatusCode)
	}

	// Valid MCP public PKCE client registration.
	regBody, _ := json.Marshal(map[string]any{
		"client_name":                "Claude Code MCP",
		"redirect_uris":              []string{"http://127.0.0.1:38491/oauth/callback"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	regResp, err := ts.Client().Post(ts.URL+"/register", "application/json", bytes.NewReader(regBody))
	if err != nil || regResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /register = %v, %v", regResp, err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&reg)
	_ = regResp.Body.Close()
	if !strings.HasPrefix(reg.ClientID, "mcp-") {
		t.Fatalf("client_id = %q; want mcp-*", reg.ClientID)
	}

	// Authorize + exchange using the dynamically registered client_id.
	verifier, challenge := newPKCEPair(t)
	browser := noRedirectBrowserClient(t, ts.Client().Transport)
	getResp, err := browser.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {reg.ClientID},
		"redirect_uri":          {"http://127.0.0.1:38491/oauth/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"mcp-state"},
	}.Encode())
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize MCP = %v, %v", getResp, err)
	}
	htmlBytes, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	txID := extractHiddenInput(t, string(htmlBytes), "tx_id")
	csrf := extractHiddenInput(t, string(htmlBytes), "csrf_token")

	postResp, err := browser.PostForm(ts.URL+"/authorize", url.Values{
		"tx_id":      {txID},
		"csrf_token": {csrf},
		"persona":    {"engineer"},
	})
	if err != nil || postResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /authorize MCP = %v, %v", postResp, err)
	}
	_ = postResp.Body.Close()
	loc, _ := url.Parse(postResp.Header.Get("Location"))
	code := loc.Query().Get("code")

	// Exchange using HTTP Basic public client auth (empty secret), as some MCP clients send.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:38491/oauth/callback"},
		"code_verifier": {verifier},
	}
	tokReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.SetBasicAuth(reg.ClientID, "")
	tokResp, err := ts.Client().Do(tokReq)
	if err != nil || tokResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /token MCP Basic = %v, %v", tokResp, err)
	}
	_ = tokResp.Body.Close()
}

func TestOneClickGitHubSetupAndSocialLogin(t *testing.T) {
	// Fake GitHub web + API server.
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/app-manifests/temp-manifest-code/conversions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":     "gh-app-client-1",
			"client_secret": "gh-app-secret-1",
		})
	})
	ghMux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "gh-app-client-1" || r.FormValue("client_secret") != "gh-app-secret-1" ||
			r.FormValue("code") != "gh-oauth-code" || r.FormValue("code_verifier") == "" {
			http.Error(w, "bad exchange", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_testtoken"})
	})
	ghMux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_testtoken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login": "Octo-Cat",
			"name":  "The Octocat",
		})
	})
	ghMux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"email": "octocat@github.example", "primary": true, "verified": true},
		})
	})
	ghServer := httptest.NewServer(ghMux)
	defer ghServer.Close()

	stateDir := filepath.Join(t.TempDir(), "state")
	var srv *auth.Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	var err error
	srv, err = auth.New(auth.Config{
		Issuer:   ts.URL,
		StateDir: stateDir,
		GitHub: auth.GitHubConfig{
			AllowSetup: true,
			WebURL:     ghServer.URL,
			APIURL:     ghServer.URL,
		},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	browser := noRedirectBrowserClient(t, ts.Client().Transport)

	// 1. Start /authorize -> page shows "Configure GitHub Login (1-Click)".
	verifier, challenge := newPKCEPair(t)
	authResp, err := browser.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {"kodactl"},
		"redirect_uri":          {"http://127.0.0.1:41111/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"gh-flow-state"},
	}.Encode())
	if err != nil || authResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize = %v, %v", authResp, err)
	}
	loginHTMLBytes, _ := io.ReadAll(authResp.Body)
	_ = authResp.Body.Close()
	if !strings.Contains(string(loginHTMLBytes), "/setup/github?tx=") {
		t.Fatalf("expected 1-click GitHub setup link in HTML:\n%s", loginHTMLBytes)
	}
	reSetupLink := regexp.MustCompile(`/setup/github\?tx=([^"]+)`)
	txMatch := reSetupLink.FindStringSubmatch(string(loginHTMLBytes))
	if len(txMatch) != 2 {
		t.Fatal("could not extract tx from setup link")
	}
	txID := txMatch[1]

	// 2. Visit GET /setup/github?tx=<txID> -> extract state from action URL, complete callback.
	setupResp, err := browser.Get(ts.URL + "/setup/github?tx=" + txID)
	if err != nil || setupResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /setup/github = %v, %v", setupResp, err)
	}
	setupHTMLBytes, _ := io.ReadAll(setupResp.Body)
	_ = setupResp.Body.Close()
	reAction := regexp.MustCompile(`action="[^"]*\?state=([^"]+)"`)
	stateMatch := reAction.FindStringSubmatch(string(setupHTMLBytes))
	if len(stateMatch) != 2 {
		t.Fatalf("could not extract setup state from HTML:\n%s", setupHTMLBytes)
	}
	setupState, _ := url.QueryUnescape(stateMatch[1])

	cbSetupResp, err := browser.Get(ts.URL + "/setup/github/callback?code=temp-manifest-code&state=" + url.QueryEscape(setupState))
	if err != nil || cbSetupResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /setup/github/callback = %v, %v", cbSetupResp, err)
	}
	_ = cbSetupResp.Body.Close()

	// Verify github-oauth.json was persisted 0600!
	ghFileInfo, err := os.Stat(filepath.Join(stateDir, "github-oauth.json"))
	if err != nil || ghFileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("github-oauth.json stat = %v, err = %v", ghFileInfo, err)
	}

	// Second setup attempt must now be refused with 409 Conflict.
	secondSetup, err := browser.Get(ts.URL + "/setup/github")
	if err != nil {
		t.Fatalf("second GET /setup/github: %v", err)
	}
	_ = secondSetup.Body.Close()
	if secondSetup.StatusCode != http.StatusConflict {
		t.Fatalf("second setup status = %d; want 409", secondSetup.StatusCode)
	}

	// 3. Resume /authorize?tx=<txID> and click "Continue with GitHub".
	resumeResp, err := browser.Get(ts.URL + "/authorize?tx=" + url.QueryEscape(txID))
	if err != nil || resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize?tx = %v, %v", resumeResp, err)
	}
	resumeHTMLBytes, _ := io.ReadAll(resumeResp.Body)
	_ = resumeResp.Body.Close()
	csrf := extractHiddenInput(t, string(resumeHTMLBytes), "csrf_token")

	ghStartResp, err := browser.PostForm(ts.URL+"/oauth/github/start", url.Values{
		"tx_id":      {txID},
		"csrf_token": {csrf},
	})
	if err != nil || ghStartResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /oauth/github/start = %v, %v", ghStartResp, err)
	}
	_ = ghStartResp.Body.Close()
	ghAuthURL, _ := url.Parse(ghStartResp.Header.Get("Location"))
	socialState := ghAuthURL.Query().Get("state")
	if ghAuthURL.Query().Get("code_challenge_method") != "S256" || socialState == "" {
		t.Fatalf("unexpected GitHub authorize redirect: %s", ghAuthURL)
	}

	ghCBResp, err := browser.Get(ts.URL + "/oauth/github/callback?code=gh-oauth-code&state=" + url.QueryEscape(socialState))
	if err != nil || ghCBResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /oauth/github/callback = %v, %v", ghCBResp, err)
	}
	_ = ghCBResp.Body.Close()
	clientCB, _ := url.Parse(ghCBResp.Header.Get("Location"))
	authCode := clientCB.Query().Get("code")

	tokResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {"http://127.0.0.1:41111/callback"},
		"client_id":     {"kodactl"},
		"code_verifier": {verifier},
	})
	if err != nil || tokResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /token = %v, %v", tokResp, err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tok)
	_ = tokResp.Body.Close()

	whoReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/whoami", nil)
	whoReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	whoResp, err := ts.Client().Do(whoReq)
	if err != nil || whoResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /whoami = %v, %v", whoResp, err)
	}
	var who map[string]string
	_ = json.NewDecoder(whoResp.Body).Decode(&who)
	_ = whoResp.Body.Close()

	ghSub, _ := principal.SubjectForGitHub("Octo-Cat")
	wantPID, _ := principal.ForOIDC(ts.URL, ghSub)
	if who["subject"] != "github:octo-cat" || id.PrincipalID(who["principal_id"]) != wantPID || who["email"] != "octocat@github.example" {
		t.Fatalf("/whoami = %+v; want github:octo-cat / %s", who, wantPID)
	}
}

func TestUpstreamOIDCFederationRS256(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	var upstreamURL string
	var capturedNonce string
	upstreamMux := http.NewServeMux()
	upstreamMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 upstreamURL,
			"authorization_endpoint": upstreamURL + "/auth",
			"token_endpoint":         upstreamURL + "/token",
			"userinfo_endpoint":      upstreamURL + "/userinfo",
			"jwks_uri":               upstreamURL + "/jwks",
		})
	})
	upstreamMux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "google-rsa-1",
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			}},
		})
	})
	upstreamMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("client_id") != "up-client" || r.FormValue("client_secret") != "up-secret" || r.FormValue("code_verifier") == "" {
			http.Error(w, "bad upstream token req", http.StatusBadRequest)
			return
		}
		now := time.Now()
		hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "google-rsa-1", "typ": "JWT"})
		claims, _ := json.Marshal(map[string]any{
			"iss":            upstreamURL,
			"sub":            "google-sub-999",
			"aud":            "up-client",
			"exp":            now.Add(time.Hour).Unix(),
			"iat":            now.Unix(),
			"nonce":          capturedNonce,
			"email":          "Alice.Eng@Company.Example",
			"email_verified": true,
		})
		input := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
		sum := sha256.Sum256([]byte(input))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		idTok := input + "." + base64.RawURLEncoding.EncodeToString(sig)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "up-access-tok",
			"id_token":     idTok,
			"token_type":   "Bearer",
		})
	})
	upstreamMux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":  "google-sub-999",
			"name": "Alice Engineer",
		})
	})
	upServer := httptest.NewServer(upstreamMux)
	defer upServer.Close()
	upstreamURL = upServer.URL

	var srv *auth.Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	srv, err = auth.New(auth.Config{
		Issuer: ts.URL,
		UpstreamOIDC: auth.UpstreamOIDCConfig{
			Issuer:       upstreamURL,
			ClientID:     "up-client",
			ClientSecret: "up-secret",
			DisplayName:  "Google Workspace",
		},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	browser := noRedirectBrowserClient(t, ts.Client().Transport)
	verifier, challenge := newPKCEPair(t)
	getResp, err := browser.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {"kodactl"},
		"redirect_uri":          {"http://127.0.0.1:42222/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"sso-state"},
	}.Encode())
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /authorize = %v, %v", getResp, err)
	}
	htmlBytes, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	txID := extractHiddenInput(t, string(htmlBytes), "tx_id")
	csrf := extractHiddenInput(t, string(htmlBytes), "csrf_token")

	startResp, err := browser.PostForm(ts.URL+"/oauth/oidc/start", url.Values{
		"tx_id":      {txID},
		"csrf_token": {csrf},
	})
	if err != nil || startResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /oauth/oidc/start = %v, %v", startResp, err)
	}
	_ = startResp.Body.Close()
	upAuthLoc, _ := url.Parse(startResp.Header.Get("Location"))
	capturedNonce = upAuthLoc.Query().Get("nonce")
	socialState := upAuthLoc.Query().Get("state")

	cbResp, err := browser.Get(ts.URL + "/oauth/oidc/callback?code=up-code&state=" + url.QueryEscape(socialState))
	if err != nil || cbResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /oauth/oidc/callback = %v, %v", cbResp, err)
	}
	_ = cbResp.Body.Close()
	appLoc, _ := url.Parse(cbResp.Header.Get("Location"))

	tokResp, err := ts.Client().PostForm(ts.URL+"/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {appLoc.Query().Get("code")},
		"redirect_uri":  {"http://127.0.0.1:42222/callback"},
		"client_id":     {"kodactl"},
		"code_verifier": {verifier},
	})
	if err != nil || tokResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /token = %v, %v", tokResp, err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tok)
	_ = tokResp.Body.Close()

	whoReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/whoami", nil)
	whoReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	whoResp, err := ts.Client().Do(whoReq)
	if err != nil || whoResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /whoami = %v, %v", whoResp, err)
	}
	var who map[string]string
	_ = json.NewDecoder(whoResp.Body).Decode(&who)
	_ = whoResp.Body.Close()

	emailSub, _ := principal.SubjectForEmail("Alice.Eng@Company.Example")
	wantPID, _ := principal.ForOIDC(ts.URL, emailSub)
	if who["subject"] != "email:alice.eng@company.example" || id.PrincipalID(who["principal_id"]) != wantPID || who["name"] != "Alice Engineer" {
		t.Fatalf("/whoami = %+v; want email:alice.eng@company.example / %s", who, wantPID)
	}
}

func TestRunServesTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	issuer := "https://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- auth.Run(ctx, auth.Config{
			Issuer:        issuer,
			TLSCertFile:   certPath,
			TLSKeyFile:    keyPath,
			LocalPersonas: []string{"admin"},
			Listener:      ln,
		})
	}()

	pool := x509.NewCertPool()
	cert, _ := x509.ParseCertificate(der)
	pool.AddCert(cert)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}

	resp, err := client.Get(issuer + "/.well-known/openid-configuration")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS discovery = %v, %v", resp, err)
	}
	_ = resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("auth.Run returned error on shutdown: %v", err)
	}
}
