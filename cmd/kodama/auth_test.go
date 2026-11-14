package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	authapp "github.com/optimiware/kodama/internal/userland/app/auth"
	"github.com/optimiware/kodama/internal/userland/cli"
	"github.com/optimiware/kodama/internal/userland/principal"
)

type memorySessionStore struct {
	mu     sync.Mutex
	record *cli.SessionRecord
}

func (s *memorySessionStore) LoadRecord(context.Context) (cli.SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return cli.SessionRecord{}, fs.ErrNotExist
	}
	return *s.record, nil
}

func (s *memorySessionStore) SaveRecord(_ context.Context, rec cli.SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := rec
	s.record = &copied
	return nil
}

func (s *memorySessionStore) DeleteRecord(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return fs.ErrNotExist
	}
	s.record = nil
	return nil
}

type noopSessionLock struct{}

func (noopSessionLock) Acquire(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

type controllableClock struct {
	mu        sync.Mutex
	now       time.Time
	onSleep   func()
	sleepWait time.Duration
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now.IsZero() {
		return time.Now()
	}
	return c.now
}

func (c *controllableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *controllableClock) Sleep(ctx context.Context, _ time.Duration) error {
	if c.onSleep != nil {
		c.onSleep()
	}
	wait := c.sleepWait
	if wait <= 0 {
		wait = 5 * time.Millisecond
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type funcBrowserOpener func(ctx context.Context, authURL string) error

func (fn funcBrowserOpener) Open(ctx context.Context, authURL string) error {
	return fn(ctx, authURL)
}

type authE2EFixture struct {
	authServer *authapp.Server
	authTLS    *httptest.Server
	apiTLS     *httptest.Server
	transport  *http.Transport
	httpClient *http.Client
	verifier   identity.OIDCVerifier
}

func newAuthE2EFixture(t *testing.T, mutate func(*authapp.Config)) *authE2EFixture {
	t.Helper()

	var authSrv *authapp.Server
	authTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSrv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(authTLS.Close)

	var apiOrigin string
	apiTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.DiscoveryPath {
			http.NotFound(w, r)
			return
		}
		doc := api.DiscoveryDocument{
			Issuer:                      authTLS.URL,
			ClientID:                    "kodactl",
			Audience:                    "kodama-api",
			AuthorizationEndpoint:       authTLS.URL + "/authorize",
			TokenEndpoint:               authTLS.URL + "/token",
			DeviceAuthorizationEndpoint: authTLS.URL + "/device",
			RevocationEndpoint:          authTLS.URL + "/revoke",
			Scopes:                      []string{"openid", "profile", "email"},
			APIURL:                      apiOrigin,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(apiTLS.Close)
	apiOrigin = apiTLS.URL

	pool := x509.NewCertPool()
	pool.AddCert(authTLS.Certificate())
	pool.AddCert(apiTLS.Certificate())
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}

	cfg := authapp.Config{
		Issuer:        authTLS.URL,
		TenantID:      "tenant-alpha",
		ProjectID:     "proj-beta",
		ClientID:      "kodactl",
		Audience:      "kodama-api",
		LocalPersonas: []string{"admin", "engineer", "reviewer"},
		AccessCode:    "s3cr3t-code",
		HTTPClient:    httpClient,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	var err error
	authSrv, err = authapp.New(cfg)
	if err != nil {
		t.Fatalf("authapp.New: %v", err)
	}

	verifier, err := oidc.NewVerifier(oidc.Config{
		OIDCConfig: identity.OIDCConfig{
			Issuer:    authTLS.URL,
			Audience:  "kodama-api",
			ClockSkew: time.Minute,
		},
		JWKSURL:            authTLS.URL + "/jwks.json",
		Algorithms:         []string{oidc.AlgorithmRS256, oidc.AlgorithmES256},
		HTTPClient:         httpClient,
		MaxJWKSBytes:       1 << 20,
		MaxTokenBytes:      1 << 20,
		CacheTTL:           5 * time.Minute,
		MinRefreshInterval: time.Second,
		TenantClaim:        "tenant",
		ProjectClaim:       "project",
	})
	if err != nil {
		t.Fatalf("oidc.NewVerifier: %v", err)
	}

	return &authE2EFixture{
		authServer: authSrv,
		authTLS:    authTLS,
		apiTLS:     apiTLS,
		transport:  transport,
		httpClient: httpClient,
		verifier:   verifier,
	}
}

func extractInputValue(t *testing.T, htmlBody, name string) string {
	t.Helper()
	re := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `"\s+value="([^"]*)"`)
	m := re.FindStringSubmatch(htmlBody)
	if len(m) != 2 {
		t.Fatalf("input %q missing from HTML:\n%s", name, htmlBody)
	}
	return m[1]
}

// TestKodactlBrowserPKCELoginRefreshLogoutWithOIDCVerifier drives REAL
// cli.Login (LoginBrowser + HTTPOIDCClient + NewLoopbackListener), cli.Refresh
// and cli.Logout against auth.Server and verifies every issued access_token
// through Kodama's REAL S05 oidc.Verifier.
func TestKodactlBrowserPKCELoginRefreshLogoutWithOIDCVerifier(t *testing.T) {
	fx := newAuthE2EFixture(t, nil)
	store := &memorySessionStore{}
	now := time.Now().UTC().Truncate(time.Second)
	clock := &controllableClock{now: now}

	oidcClient := &cli.HTTPOIDCClient{
		RoundTripper: fx.transport,
	}

	browser := funcBrowserOpener(func(ctx context.Context, authorizationURL string) error {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return err
		}
		noRedirect := &http.Client{
			Transport: fx.transport,
			Jar:       jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizationURL, nil)
		if err != nil {
			return err
		}
		getResp, err := noRedirect.Do(getReq)
		if err != nil {
			return err
		}
		htmlBytes, _ := io.ReadAll(getResp.Body)
		_ = getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /authorize status = %d", getResp.StatusCode)
		}
		txID := extractInputValue(t, string(htmlBytes), "tx_id")
		csrf := extractInputValue(t, string(htmlBytes), "csrf_token")

		form := url.Values{
			"tx_id":       {txID},
			"csrf_token":  {csrf},
			"persona":     {"reviewer"},
			"access_code": {"s3cr3t-code"},
		}
		postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fx.authTLS.URL+"/authorize", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		postResp, err := noRedirect.Do(postReq)
		if err != nil {
			return err
		}
		_ = postResp.Body.Close()
		if postResp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /authorize status = %d; want 303", postResp.StatusCode)
		}

		// Deliver the redirect to kodactl's real bound loopback listener!
		cbURL := postResp.Header.Get("Location")
		cbReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cbURL, nil)
		if err != nil {
			return err
		}
		cbResp, err := (&http.Client{}).Do(cbReq)
		if err != nil {
			return err
		}
		_ = cbResp.Body.Close()
		if cbResp.StatusCode != http.StatusOK {
			t.Fatalf("loopback callback status = %d; want 200", cbResp.StatusCode)
		}
		return nil
	})

	record, err := cli.Login(context.Background(), cli.LoginDeps{
		APIURL:   fx.apiTLS.URL,
		Mode:     cli.LoginBrowser,
		OIDC:     oidcClient,
		Browser:  browser,
		Loopback: cli.NewLoopbackListener(),
		Clock:    clock,
		Lock:     noopSessionLock{},
		Store:    store,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("cli.Login(LoginBrowser): %v", err)
	}
	if record.Token == "" || record.RefreshToken == "" {
		t.Fatal("cli.Login returned empty token or refresh token")
	}

	// Verify the access_token through REAL S05 oidc.Verifier!
	verified, err := fx.verifier.Verify(context.Background(), identity.OIDCRequest{
		AccessToken: record.Token,
		Now:         time.Now(),
	})
	if err != nil {
		t.Fatalf("oidc.Verifier.Verify(access_token): %v", err)
	}

	// Compare PrincipalID against `kodama auth principal-id --issuer ... --local reviewer`.
	var cmdOut, cmdErr bytes.Buffer
	code := run(context.Background(), []string{
		"auth", "principal-id",
		"--issuer", fx.authTLS.URL,
		"--local", "reviewer",
	}, &cmdOut, &cmdErr, defaultDeps())
	if code != 0 {
		t.Fatalf("kodama auth principal-id exit=%d stderr=%s", code, cmdErr.String())
	}
	cliPID := id.PrincipalID(strings.TrimSpace(cmdOut.String()))
	if verified.ID != cliPID {
		t.Fatalf("verified principal = %q; CLI principal-id = %q", verified.ID, cliPID)
	}
	if verified.Kind != identity.KindHuman || verified.Scope != (tenancy.Scope{TenantID: "tenant-alpha", ProjectID: "proj-beta"}) {
		t.Fatalf("unexpected verified principal: %+v", verified)
	}

	// Advance CLI clock near expiry so cli.Refresh rotates the session token!
	clock.Advance(59*time.Minute + 30*time.Second)
	refreshed, err := cli.Refresh(context.Background(), cli.RefreshDeps{
		APIURL:   fx.apiTLS.URL,
		Issuer:   fx.authTLS.URL,
		ClientID: "kodactl",
		Audience: "kodama-api",
		OIDC:     oidcClient,
		Clock:    clock,
		Lock:     noopSessionLock{},
		Store:    store,
	})
	if err != nil {
		t.Fatalf("cli.Refresh: %v", err)
	}
	if refreshed.RefreshToken == record.RefreshToken {
		t.Fatal("cli.Refresh did not receive a rotated refresh token")
	}
	verifiedAfterRefresh, err := fx.verifier.Verify(context.Background(), identity.OIDCRequest{
		AccessToken: refreshed.Token,
		Now:         time.Now(),
	})
	if err != nil {
		t.Fatalf("oidc.Verifier.Verify(refreshed access_token): %v", err)
	}
	if verifiedAfterRefresh.ID != cliPID {
		t.Fatalf("refreshed principal = %q; want %q", verifiedAfterRefresh.ID, cliPID)
	}

	// Logout revokes the rotated refresh token at /revoke and removes the session.
	if err := cli.Logout(context.Background(), cli.LogoutDeps{
		APIURL:   fx.apiTLS.URL,
		Issuer:   fx.authTLS.URL,
		ClientID: "kodactl",
		Audience: "kodama-api",
		OIDC:     oidcClient,
		Lock:     noopSessionLock{},
		Store:    store,
	}); err != nil {
		t.Fatalf("cli.Logout: %v", err)
	}
	if _, err := oidcClient.Token(context.Background(), cli.TokenRequest{
		Endpoint:     fx.authTLS.URL + "/token",
		GrantType:    "refresh_token",
		RefreshToken: refreshed.RefreshToken,
		ClientID:     "kodactl",
	}); err == nil {
		t.Fatal("revoked refresh token was still accepted by /token after cli.Logout")
	}
}

// TestKodactlDeviceLoginEndToEndWithOIDCVerifier drives REAL cli.Login
// (LoginDevice / `kodactl login --device`) against auth.Server and verifies the
// issued access_token with REAL S05 oidc.Verifier.
func TestKodactlDeviceLoginEndToEndWithOIDCVerifier(t *testing.T) {
	fx := newAuthE2EFixture(t, nil)
	store := &memorySessionStore{}

	promptCh := make(chan string, 1)
	var promptOnce sync.Once
	var promptBuf bytes.Buffer
	outWriter := writerFunc(func(p []byte) (int, error) {
		n, err := promptBuf.Write(p)
		if strings.Contains(promptBuf.String(), "\n") {
			promptOnce.Do(func() {
				promptCh <- promptBuf.String()
			})
		}
		return n, err
	})

	// Let the first poll hit "authorization_pending", then approve on the second sleep!
	var sleepCalls int
	var approveMu sync.Mutex
	var approveErr error
	clock := &controllableClock{
		now:       time.Now().UTC(),
		sleepWait: 5 * time.Millisecond,
	}
	clock.onSleep = func() {
		approveMu.Lock()
		defer approveMu.Unlock()
		sleepCalls++
		if sleepCalls != 2 {
			return
		}
		line := <-promptCh
		re := regexp.MustCompile(`open (\S+) and enter the code ([A-Z0-9-]+)`)
		m := re.FindStringSubmatch(line)
		if len(m) != 3 {
			approveErr = errors.New("could not parse device prompt: " + line)
			return
		}
		verifyURI, userCode := m[1], m[2]

		jar, _ := cookiejar.New(nil)
		browser := &http.Client{Transport: fx.transport, Jar: jar}
		getResp, err := browser.Get(verifyURI + "?user_code=" + url.QueryEscape(userCode))
		if err != nil {
			approveErr = err
			return
		}
		pageBytes, _ := io.ReadAll(getResp.Body)
		_ = getResp.Body.Close()
		csrf := extractInputValue(t, string(pageBytes), "csrf_token")

		postResp, err := browser.PostForm(fx.authTLS.URL+"/device/verify", url.Values{
			"csrf_token":  {csrf},
			"user_code":   {strings.ToLower(strings.ReplaceAll(userCode, "-", " "))},
			"method":      {"local"},
			"persona":     {"admin"},
			"access_code": {"s3cr3t-code"},
		})
		if err != nil {
			approveErr = err
			return
		}
		_ = postResp.Body.Close()
		if postResp.StatusCode != http.StatusOK {
			approveErr = errors.New("unexpected /device/verify status")
		}
	}

	record, err := cli.Login(context.Background(), cli.LoginDeps{
		APIURL: fx.apiTLS.URL,
		Mode:   cli.LoginDevice,
		OIDC:   &cli.HTTPOIDCClient{RoundTripper: fx.transport},
		Clock:  clock,
		Lock:   noopSessionLock{},
		Store:  store,
		Out:    outWriter,
	})
	if err != nil {
		t.Fatalf("cli.Login(LoginDevice): %v", err)
	}
	approveMu.Lock()
	errApprove := approveErr
	approveMu.Unlock()
	if errApprove != nil {
		t.Fatalf("browser device approval failed: %v", errApprove)
	}

	verified, err := fx.verifier.Verify(context.Background(), identity.OIDCRequest{
		AccessToken: record.Token,
		Now:         time.Now(),
	})
	if err != nil {
		t.Fatalf("oidc.Verifier.Verify(device access_token): %v", err)
	}
	wantPID, err := principal.ForOIDC(fx.authTLS.URL, "admin")
	if err != nil {
		t.Fatalf("ForOIDC: %v", err)
	}
	if verified.ID != wantPID {
		t.Fatalf("device verified principal = %q; want %q", verified.ID, wantPID)
	}
}

type writerFunc func([]byte) (int, error)

func (fn writerFunc) Write(p []byte) (int, error) { return fn(p) }

// TestSocialLoginBrokersMatchCLIPrincipalIDAndOIDCVerifier verifies both GitHub
// and Upstream OIDC federation through REAL cli.Login + REAL oidc.Verifier +
// `kodama auth principal-id`.
func TestSocialLoginBrokersMatchCLIPrincipalIDAndOIDCVerifier(t *testing.T) {
	// Fake GitHub server.
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_live"})
	})
	ghMux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"login": "Reviewer-Jane",
			"name":  "Jane Reviewer",
			"email": "jane@github.example",
		})
	})
	ghSrv := httptest.NewServer(ghMux)
	defer ghSrv.Close()

	// Fake Google/OIDC server.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	var upURL, lastNonce string
	upMux := http.NewServeMux()
	upMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 upURL,
			"authorization_endpoint": upURL + "/auth",
			"token_endpoint":         upURL + "/token",
			"jwks_uri":               upURL + "/jwks",
		})
	})
	upMux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "up-1",
				"alg": "RS256",
				"use": "sig",
				"n":   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
			}},
		})
	})
	upMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "up-1", "typ": "JWT"})
		claims, _ := json.Marshal(map[string]any{
			"iss":            upURL,
			"sub":            "1099887766",
			"aud":            "google-client-id",
			"exp":            now.Add(time.Hour).Unix(),
			"iat":            now.Unix(),
			"nonce":          lastNonce,
			"email":          "SSO.User@Example.COM",
			"email_verified": true,
			"name":           "SSO User",
		})
		in := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(claims)
		sum := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "up-tok",
			"id_token":     in + "." + base64.RawURLEncoding.EncodeToString(sig),
		})
	})
	upSrv := httptest.NewServer(upMux)
	defer upSrv.Close()
	upURL = upSrv.URL

	fx := newAuthE2EFixture(t, func(cfg *authapp.Config) {
		cfg.GitHub = authapp.GitHubConfig{
			ClientID:     "gh-cid",
			ClientSecret: "gh-csec",
			WebURL:       ghSrv.URL,
			APIURL:       ghSrv.URL,
		}
		cfg.UpstreamOIDC = authapp.UpstreamOIDCConfig{
			Issuer:       upURL,
			ClientID:     "google-client-id",
			ClientSecret: "google-client-secret",
		}
	})

	for _, tc := range []struct {
		name      string
		startPath string
		cbPath    string
		cliArgs   []string
	}{
		{
			name:      "GitHub login",
			startPath: "/oauth/github/start",
			cbPath:    "/oauth/github/callback",
			cliArgs:   []string{"--github", "Reviewer-Jane"},
		},
		{
			name:      "Upstream Google OIDC login",
			startPath: "/oauth/oidc/start",
			cbPath:    "/oauth/oidc/callback",
			cliArgs:   []string{"--email", "SSO.User@Example.COM"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &memorySessionStore{}
			browser := funcBrowserOpener(func(ctx context.Context, authorizationURL string) error {
				jar, _ := cookiejar.New(nil)
				noRedir := &http.Client{
					Transport: fx.transport,
					Jar:       jar,
					CheckRedirect: func(*http.Request, []*http.Request) error {
						return http.ErrUseLastResponse
					},
				}
				getResp, err := noRedir.Get(authorizationURL)
				if err != nil {
					return err
				}
				body, _ := io.ReadAll(getResp.Body)
				_ = getResp.Body.Close()
				txID := extractInputValue(t, string(body), "tx_id")
				csrf := extractInputValue(t, string(body), "csrf_token")

				startResp, err := noRedir.PostForm(fx.authTLS.URL+tc.startPath, url.Values{
					"tx_id":      {txID},
					"csrf_token": {csrf},
				})
				if err != nil {
					return err
				}
				_ = startResp.Body.Close()
				upRedirect, _ := url.Parse(startResp.Header.Get("Location"))
				state := upRedirect.Query().Get("state")
				lastNonce = upRedirect.Query().Get("nonce")

				cbResp, err := noRedir.Get(fx.authTLS.URL + tc.cbPath + "?code=social-code&state=" + url.QueryEscape(state))
				if err != nil {
					return err
				}
				_ = cbResp.Body.Close()

				loopResp, err := (&http.Client{}).Get(cbResp.Header.Get("Location"))
				if err != nil {
					return err
				}
				_ = loopResp.Body.Close()
				return nil
			})

			rec, err := cli.Login(context.Background(), cli.LoginDeps{
				APIURL:   fx.apiTLS.URL,
				Mode:     cli.LoginBrowser,
				OIDC:     &cli.HTTPOIDCClient{RoundTripper: fx.transport},
				Browser:  browser,
				Loopback: cli.NewLoopbackListener(),
				Lock:     noopSessionLock{},
				Store:    store,
			})
			if err != nil {
				t.Fatalf("cli.Login(%s): %v", tc.name, err)
			}
			verified, err := fx.verifier.Verify(context.Background(), identity.OIDCRequest{
				AccessToken: rec.Token,
				Now:         time.Now(),
			})
			if err != nil {
				t.Fatalf("oidc.Verifier.Verify(%s): %v", tc.name, err)
			}

			var stdout, stderr bytes.Buffer
			args := append([]string{"auth", "principal-id", "--issuer", fx.authTLS.URL}, tc.cliArgs...)
			if code := run(context.Background(), args, &stdout, &stderr, defaultDeps()); code != 0 {
				t.Fatalf("auth principal-id exit=%d stderr=%s", code, stderr.String())
			}
			if verified.ID != id.PrincipalID(strings.TrimSpace(stdout.String())) {
				t.Fatalf("verified ID = %q; CLI principal-id = %q", verified.ID, strings.TrimSpace(stdout.String()))
			}
		})
	}
}

func TestAuthPrincipalIDCLIValidation(t *testing.T) {
	const issuer = "https://auth.kodama.example"

	for _, tc := range []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"raw subject", []string{"--issuer", issuer, "--subject", "custom|42"}, "custom|42"},
		{"github login", []string{"--issuer", issuer, "--github", "Octo-Cat"}, "github:octo-cat"},
		{"email", []string{"--issuer", issuer, "--email", "Dev@Example.COM"}, "email:dev@example.com"},
		{"local persona", []string{"--issuer", issuer, "--local", "reviewer"}, "reviewer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), append([]string{"auth", "principal-id"}, tc.args...), &stdout, &stderr, defaultDeps())
			if code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			want, err := principal.ForOIDC(issuer, tc.wantSub)
			if err != nil {
				t.Fatalf("ForOIDC: %v", err)
			}
			if strings.TrimSpace(stdout.String()) != string(want) {
				t.Fatalf("stdout = %q; want %q", stdout.String(), want)
			}
		})
	}

	for _, badArgs := range [][]string{
		{},
		{"--issuer", issuer},
		{"--local", "admin"},
		{"--issuer", issuer, "--github", "octocat", "--local", "admin"},
		{"--issuer", issuer, "--github", "@invalid"},
		{"--issuer", issuer, "--email", "not-an-email"},
		{"--issuer", issuer, "--local", "github:collision"},
		{"--issuer", issuer, "--subject", ""},
		{"--issuer", issuer, "--local", "admin", "extra-arg"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"auth", "principal-id"}, badArgs...), &stdout, &stderr, defaultDeps())
		if code != 2 {
			t.Fatalf("args %v exit=%d; want 2", badArgs, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args %v wrote unexpected stdout %q", badArgs, stdout.String())
		}
	}
}

func TestAuthServeCLIConfigWiringAndSecretFiles(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access-code")
	ghSecretPath := filepath.Join(dir, "gh-secret")
	upSecretPath := filepath.Join(dir, "up-secret")
	for path, val := range map[string]string{
		accessPath:   "local-code-123\n",
		ghSecretPath: "gh-secret-456\r\n",
		upSecretPath: "up-secret-789\n",
	} {
		if err := os.WriteFile(path, []byte(val), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	var captured authapp.Config
	var calls int
	prevRun := runAuthApp
	t.Cleanup(func() { runAuthApp = prevRun })
	runAuthApp = func(_ context.Context, cfg authapp.Config) error {
		calls++
		captured = cfg
		_, err := authapp.New(cfg)
		return err
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"auth", "serve",
		"--issuer", "https://auth.kodama.example",
		"--listen", "127.0.0.1:9099",
		"--state-dir", filepath.Join(dir, "state"),
		"--tenant", "tenant-prod",
		"--tenant-claim", "org_tenant",
		"--project", "proj-main",
		"--project-claim", "org_project",
		"--client-id", "kodactl",
		"--audience", "https://api.kodama.example",
		"--redirect-uri", "https://console.kodama.example/callback",
		"--local-personas", "admin,engineer,reviewer",
		"--access-code-file", accessPath,
		"--github-client-id", "gh-id",
		"--github-client-secret-file", ghSecretPath,
		"--allow-github-setup",
		"--upstream-oidc-issuer", "https://accounts.google.com",
		"--upstream-oidc-client-id", "goog-id",
		"--upstream-oidc-client-secret-file", upSecretPath,
	}, &stdout, &stderr, defaultDeps())
	if code != 0 {
		t.Fatalf("auth serve exit=%d stderr=%s", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("runAuthApp calls = %d; want 1", calls)
	}
	if captured.AccessCode != "local-code-123" ||
		captured.GitHub.ClientSecret != "gh-secret-456" ||
		captured.UpstreamOIDC.ClientSecret != "up-secret-789" {
		t.Fatalf("trimmed secrets mismatch: access=%q gh=%q up=%q",
			captured.AccessCode, captured.GitHub.ClientSecret, captured.UpstreamOIDC.ClientSecret)
	}
	if len(captured.LocalPersonas) != 3 || captured.TenantID != "tenant-prod" || captured.ProjectID != "proj-main" {
		t.Fatalf("unexpected captured config: %+v", captured)
	}

	// Missing login method or partial GitHub flags must return exit code 2.
	for _, badArgs := range [][]string{
		{"--issuer", "https://auth.kodama.example"},
		{"--issuer", "http://auth.kodama.example", "--local-personas", "admin"},
		{"--issuer", "https://auth.kodama.example", "--github-client-id", "only-id"},
		{"--issuer", "https://auth.kodama.example", "--local-personas", "admin", "--access-code-file", filepath.Join(dir, "missing")},
	} {
		var outB, errB bytes.Buffer
		if got := run(context.Background(), append([]string{"auth", "serve"}, badArgs...), &outB, &errB, defaultDeps()); got != 2 {
			t.Fatalf("bad args %v exit=%d; want 2 (stderr=%s)", badArgs, got, errB.String())
		}
	}
}
