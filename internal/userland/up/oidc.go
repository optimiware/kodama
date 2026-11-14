package up

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
)

const (
	defaultOIDCClientID   = "kodactl"
	defaultAuthCodeTTL    = 5 * time.Minute
	defaultAccessTokenTTL = time.Hour
	defaultRefreshTTL     = 24 * time.Hour
	maxOIDCFormBytes      = 64 << 10
)

// DevIssuerConfig configures the built-in ES256 OIDC issuer mounted under /auth.
type DevIssuerConfig struct {
	// Issuer is the full OIDC issuer URL (for example https://127.0.0.1:8443/auth).
	Issuer string
	// Audience is the API audience minted into access tokens.
	Audience string
	// APIURL is the public Kodama API URL advertised in /.well-known/kodama-configuration.
	APIURL string
	// ClientID is the public OAuth client ID (defaults to "kodactl").
	ClientID string
	// KeyPath optionally persists the ECDSA P-256 signing key across restarts.
	KeyPath string
	// TenantID is the org_id claim minted into access tokens.
	TenantID string
	// ProjectID is the project_id claim minted into project-scoped access tokens.
	ProjectID string
	// IncludeProjectOnAdmin includes project_id on admin tokens as well (used
	// when the server maps a project claim for all human principals).
	IncludeProjectOnAdmin bool
	// AccessTokenTTL overrides the default 1h access token lifetime.
	AccessTokenTTL time.Duration
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// DevIssuer is a built-in single-origin ES256 OIDC identity provider for local
// development and container stacks.
type DevIssuer struct {
	cfg      DevIssuerConfig
	key      *ecdsa.PrivateKey
	kid      string
	jwksJSON []byte

	mu        sync.Mutex
	codes     map[string]authCodeEntry
	devices   map[string]*deviceCodeEntry
	byUser    map[string]string
	refreshes map[string]refreshEntry
}

type authCodeEntry struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	nonce         string
	scope         string
	persona       string
	expiresAt     time.Time
}

type deviceCodeEntry struct {
	clientID  string
	userCode  string
	scope     string
	persona   string
	approved  bool
	expiresAt time.Time
}

type refreshEntry struct {
	clientID  string
	scope     string
	persona   string
	expiresAt time.Time
}

// NewDevIssuer validates cfg, loads or generates the ES256 signing keypair, and
// returns a ready OIDC issuer handler.
func NewDevIssuer(cfg DevIssuerConfig) (*DevIssuer, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	cfg.Audience = strings.TrimRight(strings.TrimSpace(cfg.Audience), "/")
	cfg.APIURL = strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.ClientID == "" {
		cfg.ClientID = defaultOIDCClientID
	}
	if cfg.Issuer == "" {
		return nil, errors.New("OIDC issuer URL is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("OIDC audience is required")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = cfg.Audience
	}
	if strings.TrimSpace(cfg.TenantID) == "" {
		return nil, errors.New("OIDC tenant ID is required")
	}
	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = defaultAccessTokenTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	key, err := loadOrCreateOIDCKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	kid, jwksBytes, err := buildECJWKS(&key.PublicKey)
	if err != nil {
		return nil, err
	}

	issuer := &DevIssuer{
		cfg:       cfg,
		key:       key,
		kid:       kid,
		jwksJSON:  jwksBytes,
		codes:     make(map[string]authCodeEntry),
		devices:   make(map[string]*deviceCodeEntry),
		byUser:    make(map[string]string),
		refreshes: make(map[string]refreshEntry),
	}
	if err := issuer.DiscoveryDocument().Validate(); err != nil {
		return nil, fmt.Errorf("OIDC discovery configuration: %w", err)
	}
	return issuer, nil
}

// Kid returns the active ES256 key ID published in JWKS.
func (issuer *DevIssuer) Kid() string {
	return issuer.kid
}

// JWKSBytes returns a copy of the JSON Web Key Set document.
func (issuer *DevIssuer) JWKSBytes() []byte {
	return append([]byte(nil), issuer.jwksJSON...)
}

// DiscoveryDocument returns the Kodama /.well-known/kodama-configuration document.
func (issuer *DevIssuer) DiscoveryDocument() api.DiscoveryDocument {
	base := issuer.cfg.Issuer
	return api.DiscoveryDocument{
		Issuer:                      base,
		ClientID:                    issuer.cfg.ClientID,
		Audience:                    issuer.cfg.Audience,
		AuthorizationEndpoint:       base + "/authorize",
		TokenEndpoint:               base + "/token",
		DeviceAuthorizationEndpoint: base + "/device",
		RevocationEndpoint:          base + "/revoke",
		Scopes:                      []string{"openid", "profile", "email"},
		APIURL:                      issuer.cfg.APIURL,
	}
}

// MintAccessToken signs an ES256 access token for persona ("admin", "engineer",
// "reviewer" or "operator") with the given lifetime.
func (issuer *DevIssuer) MintAccessToken(persona string, ttl time.Duration) (string, error) {
	return issuer.mintAccessToken(persona, ttl, issuer.cfg.IncludeProjectOnAdmin)
}

// MintTenantAdminToken signs an admin access token without a project claim.
// Tenant operations (memberships, projects) require a tenant-scoped actor;
// project operations require a project-scoped one.
func (issuer *DevIssuer) MintTenantAdminToken(ttl time.Duration) (string, error) {
	return issuer.mintAccessToken("admin", ttl, false)
}

func (issuer *DevIssuer) mintAccessToken(persona string, ttl time.Duration, projectOnAdmin bool) (string, error) {
	persona = normalizePersona(persona)
	if persona == "" {
		return "", errors.New("persona must be admin, engineer, reviewer or operator")
	}
	if ttl <= 0 {
		ttl = issuer.cfg.AccessTokenTTL
	}
	now := issuer.cfg.Now().UTC()
	claims := map[string]any{
		"iss":    issuer.cfg.Issuer,
		"sub":    persona,
		"aud":    issuer.cfg.Audience,
		"org_id": issuer.cfg.TenantID,
		"iat":    now.Unix(),
		"nbf":    now.Unix(),
		"exp":    now.Add(ttl).Unix(),
	}
	if issuer.cfg.ProjectID != "" && (persona != "admin" || projectOnAdmin) {
		claims["project_id"] = issuer.cfg.ProjectID
	}
	return issuer.signJWT(claims)
}

func (issuer *DevIssuer) mintIDToken(persona, clientID, nonce string, ttl time.Duration) (string, error) {
	now := issuer.cfg.Now().UTC()
	claims := map[string]any{
		"iss":   issuer.cfg.Issuer,
		"sub":   persona,
		"aud":   clientID,
		"email": persona + "@kodama.local",
		"name":  "Local " + strings.ToUpper(persona[:1]) + persona[1:],
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return issuer.signJWT(claims)
}

// ServeHTTP serves the OIDC endpoints under /auth/* (or stripped /* when
// mounted behind a prefix-stripping proxy).
func (issuer *DevIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	subpath := r.URL.Path
	if strings.HasPrefix(subpath, "/auth/") {
		subpath = strings.TrimPrefix(subpath, "/auth")
	} else if subpath == "/auth" {
		subpath = "/"
	}

	switch subpath {
	case "/.well-known/openid-configuration":
		issuer.handleOpenIDConfig(w, r)
	case "/jwks.json", "/.well-known/jwks.json":
		issuer.handleJWKS(w, r)
	case "/authorize":
		issuer.handleAuthorize(w, r)
	case "/token":
		issuer.handleToken(w, r)
	case "/device":
		issuer.handleDevice(w, r)
	case "/revoke":
		issuer.handleRevoke(w, r)
	case "/healthz":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	default:
		writeOAuthError(w, http.StatusNotFound, "invalid_request", "unknown auth endpoint")
	}
}

func (issuer *DevIssuer) handleOpenIDConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "GET is required")
		return
	}
	base := issuer.cfg.Issuer
	doc := map[string]any{
		"issuer":                                base,
		"jwks_uri":                              base + "/jwks.json",
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"device_authorization_endpoint":         base + "/device",
		"revocation_endpoint":                   base + "/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code", "client_credentials"},
		"code_challenge_methods_supported":      []string{"S256"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		"subject_types_supported":               []string{"public"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (issuer *DevIssuer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "GET is required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(issuer.jwksJSON)
}

func (issuer *DevIssuer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var values url.Values
	switch r.Method {
	case http.MethodGet:
		values = r.URL.Query()
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxOIDCFormBytes))
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "request body could not be read")
			return
		}
		parsed, err := url.ParseQuery(string(body))
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
			return
		}
		values = parsed
	default:
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "GET or POST is required")
		return
	}

	clientID := strings.TrimSpace(values.Get("client_id"))
	redirectURI := strings.TrimSpace(values.Get("redirect_uri"))
	responseType := strings.TrimSpace(values.Get("response_type"))
	codeChallenge := strings.TrimSpace(values.Get("code_challenge"))
	challengeMethod := strings.TrimSpace(values.Get("code_challenge_method"))
	state := values.Get("state")
	nonce := values.Get("nonce")
	scope := strings.TrimSpace(values.Get("scope"))

	if clientID == "" || clientID != issuer.cfg.ClientID {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "unknown client_id")
		return
	}
	if !validLoopbackCallbackURI(redirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "redirect_uri must be http://127.0.0.1:<port>/callback or http://[::1]:<port>/callback")
		return
	}
	if responseType != "code" {
		redirectAuthError(w, r, redirectURI, state, "unsupported_response_type")
		return
	}
	if codeChallenge == "" || challengeMethod != "S256" {
		redirectAuthError(w, r, redirectURI, state, "invalid_request")
		return
	}

	if r.Method == http.MethodGet && values.Get("prompt") == "select_account" {
		issuer.renderPersonaPicker(w, clientID, redirectURI, codeChallenge, state, nonce, scope)
		return
	}

	personaHint := values.Get("persona")
	if personaHint == "" {
		personaHint = values.Get("login_hint")
	}
	persona := "admin"
	if personaHint != "" {
		persona = normalizePersona(personaHint)
		if persona == "" {
			redirectAuthError(w, r, redirectURI, state, "access_denied")
			return
		}
	}

	code, err := randomTokenHex(24)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "code generation failed")
		return
	}
	now := issuer.cfg.Now().UTC()
	issuer.mu.Lock()
	issuer.pruneLocked(now)
	issuer.codes[code] = authCodeEntry{
		clientID:      clientID,
		redirectURI:   redirectURI,
		codeChallenge: codeChallenge,
		nonce:         nonce,
		scope:         scope,
		persona:       persona,
		expiresAt:     now.Add(defaultAuthCodeTTL),
	}
	issuer.mu.Unlock()

	target, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid redirect_uri")
		return
	}
	q := target.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (issuer *DevIssuer) renderPersonaPicker(w http.ResponseWriter, clientID, redirectURI, challenge, state, nonce, scope string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	action := html.EscapeString(issuer.cfg.Issuer + "/authorize")
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Kodama Local Sign-In</title></head><body>
<h1>Select a Kodama persona</h1>
<form method="post" action="%s">
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="client_id" value="%s">
<input type="hidden" name="redirect_uri" value="%s">
<input type="hidden" name="code_challenge" value="%s">
<input type="hidden" name="code_challenge_method" value="S256">
<input type="hidden" name="state" value="%s">
<input type="hidden" name="nonce" value="%s">
<input type="hidden" name="scope" value="%s">
<button type="submit" name="persona" value="admin">Admin</button>
<button type="submit" name="persona" value="engineer">Engineer</button>
<button type="submit" name="persona" value="reviewer">Reviewer</button>
<button type="submit" name="persona" value="operator">Operator</button>
</form></body></html>`,
		action,
		html.EscapeString(clientID),
		html.EscapeString(redirectURI),
		html.EscapeString(challenge),
		html.EscapeString(state),
		html.EscapeString(nonce),
		html.EscapeString(scope),
	)
}

func (issuer *DevIssuer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST is required")
		return
	}
	values, ok := parseFormBody(w, r)
	if !ok {
		return
	}
	grantType := strings.TrimSpace(values.Get("grant_type"))
	clientID := strings.TrimSpace(values.Get("client_id"))
	if clientID == "" {
		clientID = issuer.cfg.ClientID
	}
	if clientID != issuer.cfg.ClientID {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}

	now := issuer.cfg.Now().UTC()
	switch grantType {
	case "authorization_code":
		code := values.Get("code")
		redirectURI := values.Get("redirect_uri")
		verifier := values.Get("code_verifier")
		if code == "" || redirectURI == "" || verifier == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing code, redirect_uri or code_verifier")
			return
		}
		issuer.mu.Lock()
		issuer.pruneLocked(now)
		entry, found := issuer.codes[code]
		delete(issuer.codes, code)
		issuer.mu.Unlock()
		if !found || !now.Before(entry.expiresAt) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if entry.clientID != clientID || entry.redirectURI != redirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri or client_id mismatch")
			return
		}
		if !verifyPKCES256(verifier, entry.codeChallenge) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		issuer.issueTokenResponse(w, entry.persona, clientID, entry.nonce, entry.scope, true, now)

	case "refresh_token":
		refresh := values.Get("refresh_token")
		if refresh == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing refresh_token")
			return
		}
		issuer.mu.Lock()
		issuer.pruneLocked(now)
		entry, found := issuer.refreshes[refresh]
		delete(issuer.refreshes, refresh)
		issuer.mu.Unlock()
		if !found || !now.Before(entry.expiresAt) || entry.clientID != clientID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		issuer.issueTokenResponse(w, entry.persona, clientID, "", entry.scope, true, now)

	case "urn:ietf:params:oauth:grant-type:device_code":
		deviceCode := values.Get("device_code")
		if deviceCode == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing device_code")
			return
		}
		issuer.mu.Lock()
		issuer.pruneLocked(now)
		entry, found := issuer.devices[deviceCode]
		if !found {
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown device_code")
			return
		}
		if !now.Before(entry.expiresAt) {
			delete(issuer.devices, deviceCode)
			delete(issuer.byUser, entry.userCode)
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "expired_token", "device code expired")
			return
		}
		if entry.clientID != clientID {
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
			return
		}
		if !entry.approved {
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "authorization_pending", "waiting for user approval")
			return
		}
		persona := entry.persona
		scope := entry.scope
		delete(issuer.devices, deviceCode)
		delete(issuer.byUser, entry.userCode)
		issuer.mu.Unlock()
		issuer.issueTokenResponse(w, persona, clientID, "", scope, true, now)

	case "client_credentials":
		persona := normalizePersona(values.Get("persona"))
		if persona == "" {
			persona = "admin"
		}
		issuer.issueTokenResponse(w, persona, clientID, "", values.Get("scope"), false, now)

	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "unsupported grant_type")
	}
}

func (issuer *DevIssuer) issueTokenResponse(w http.ResponseWriter, persona, clientID, nonce, scope string, includeRefresh bool, now time.Time) {
	ttl := issuer.cfg.AccessTokenTTL
	accessToken, err := issuer.MintAccessToken(persona, ttl)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "token signing failed")
		return
	}
	idToken, err := issuer.mintIDToken(persona, clientID, nonce, ttl)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "id_token signing failed")
		return
	}
	resp := map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   int64(ttl / time.Second),
		"id_token":     idToken,
	}
	if scope != "" {
		resp["scope"] = scope
	}
	if includeRefresh {
		refresh, err := randomTokenHex(24)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "refresh token generation failed")
			return
		}
		issuer.mu.Lock()
		issuer.refreshes[refresh] = refreshEntry{
			clientID:  clientID,
			scope:     scope,
			persona:   persona,
			expiresAt: now.Add(defaultRefreshTTL),
		}
		issuer.mu.Unlock()
		resp["refresh_token"] = refresh
	}
	writeJSON(w, http.StatusOK, resp)
}

func (issuer *DevIssuer) handleDevice(w http.ResponseWriter, r *http.Request) {
	now := issuer.cfg.Now().UTC()
	if r.Method == http.MethodGet {
		userCode := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("user_code")))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Kodama Device Login</title></head><body>
<h1>Approve Kodama Device Sign-In</h1>
<form method="post" action="%s">
<input type="text" name="user_code" value="%s" placeholder="XXXX-XXXX" required>
<button type="submit" name="persona" value="admin">Approve as Admin</button>
<button type="submit" name="persona" value="engineer">Approve as Engineer</button>
<button type="submit" name="persona" value="reviewer">Approve as Reviewer</button>
<button type="submit" name="persona" value="operator">Approve as Operator</button>
</form></body></html>`,
			html.EscapeString(issuer.cfg.Issuer+"/device"),
			html.EscapeString(userCode),
		)
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "GET or POST is required")
		return
	}
	values, ok := parseFormBody(w, r)
	if !ok {
		return
	}
	// If user_code is submitted, this POST is user approval from the verification form.
	if userCode := strings.ToUpper(strings.TrimSpace(values.Get("user_code"))); userCode != "" {
		persona := normalizePersona(values.Get("persona"))
		if persona == "" {
			persona = "admin"
		}
		issuer.mu.Lock()
		issuer.pruneLocked(now)
		deviceCode, exists := issuer.byUser[userCode]
		if !exists {
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "unknown or expired user_code")
			return
		}
		entry := issuer.devices[deviceCode]
		if entry == nil || !now.Before(entry.expiresAt) {
			delete(issuer.byUser, userCode)
			delete(issuer.devices, deviceCode)
			issuer.mu.Unlock()
			writeOAuthError(w, http.StatusBadRequest, "expired_token", "device code expired")
			return
		}
		entry.persona = persona
		entry.approved = true
		issuer.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "persona": persona})
		return
	}

	clientID := strings.TrimSpace(values.Get("client_id"))
	if clientID == "" || clientID != issuer.cfg.ClientID {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "unknown client_id")
		return
	}
	deviceCode, err := randomTokenHex(24)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "device code generation failed")
		return
	}
	userRaw, err := randomTokenHex(4)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "user code generation failed")
		return
	}
	userCode := strings.ToUpper(userRaw[:4] + "-" + userRaw[4:8])
	persona := normalizePersona(values.Get("persona"))
	if persona == "" {
		persona = "admin"
	}

	issuer.mu.Lock()
	issuer.pruneLocked(now)
	issuer.devices[deviceCode] = &deviceCodeEntry{
		clientID:  clientID,
		userCode:  userCode,
		scope:     strings.TrimSpace(values.Get("scope")),
		persona:   persona,
		approved:  false,
		expiresAt: now.Add(defaultAuthCodeTTL),
	}
	issuer.byUser[userCode] = deviceCode
	issuer.mu.Unlock()

	verificationURI := issuer.cfg.Issuer + "/device"
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 userCode,
		"verification_uri":          verificationURI,
		"verification_uri_complete": verificationURI + "?user_code=" + url.QueryEscape(userCode),
		"expires_in":                int64(defaultAuthCodeTTL / time.Second),
		"interval":                  int64(1),
	})
}

func (issuer *DevIssuer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST is required")
		return
	}
	values, ok := parseFormBody(w, r)
	if !ok {
		return
	}
	if tok := values.Get("token"); tok != "" {
		issuer.mu.Lock()
		delete(issuer.refreshes, tok)
		issuer.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (issuer *DevIssuer) pruneLocked(now time.Time) {
	for k, v := range issuer.codes {
		if !now.Before(v.expiresAt) {
			delete(issuer.codes, k)
		}
	}
	for k, v := range issuer.devices {
		if !now.Before(v.expiresAt) {
			delete(issuer.byUser, v.userCode)
			delete(issuer.devices, k)
		}
	}
	for k, v := range issuer.refreshes {
		if !now.Before(v.expiresAt) {
			delete(issuer.refreshes, k)
		}
	}
}

func (issuer *DevIssuer) signJWT(claims map[string]any) (string, error) {
	headerBytes, err := json.Marshal(map[string]string{
		"alg": "ES256",
		"kid": issuer.kid,
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." +
		base64.RawURLEncoding.EncodeToString(claimsBytes)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, issuer.key, digest[:])
	if err != nil {
		return "", err
	}
	var sig [64]byte
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig[:]), nil
}

func loadOrCreateOIDCKey(keyPath string) (*ecdsa.PrivateKey, error) {
	path := strings.TrimSpace(keyPath)
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if block, _ := pem.Decode(data); block != nil {
				if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
					if ecKey, ok := parsed.(*ecdsa.PrivateKey); ok && ecKey.Curve == elliptic.P256() {
						_ = os.Chmod(path, 0o600)
						return ecKey, nil
					}
				}
				if ecKey, err := x509.ParseECPrivateKey(block.Bytes); err == nil && ecKey.Curve == elliptic.P256() {
					_ = os.Chmod(path, 0o600)
					return ecKey, nil
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ES256 OIDC key: %w", err)
	}
	if path != "" {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("marshal ES256 OIDC key: %w", err)
		}
		encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := writeFileAtomic(path, encoded, 0o600); err != nil {
			return nil, fmt.Errorf("write ES256 OIDC key: %w", err)
		}
	}
	return key, nil
}

func buildECJWKS(pub *ecdsa.PublicKey) (string, []byte, error) {
	uncompressed, err := pub.Bytes()
	if err != nil || len(uncompressed) != 65 || uncompressed[0] != 4 {
		return "", nil, errors.New("encode P-256 public key failed")
	}
	sum := sha256.Sum256(uncompressed)
	kid := "dev-" + hex.EncodeToString(sum[:8])
	x := base64.RawURLEncoding.EncodeToString(uncompressed[1:33])
	y := base64.RawURLEncoding.EncodeToString(uncompressed[33:65])
	doc := struct {
		Keys []map[string]string `json:"keys"`
	}{
		Keys: []map[string]string{{
			"kty": "EC",
			"crv": "P-256",
			"use": "sig",
			"alg": "ES256",
			"kid": kid,
			"x":   x,
			"y":   y,
		}},
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", nil, err
	}
	return kid, encoded, nil
}

func validLoopbackCallbackURI(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\\#?") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	if parsed.Path != "/callback" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	host, portStr, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	if host != "127.0.0.1" && host != "::1" {
		return false
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port >= 1 && port <= 65535
}

func verifyPKCES256(verifier, expectedChallenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for i := 0; i < len(verifier); i++ {
		ch := verifier[i]
		switch {
		case ch >= 'A' && ch <= 'Z',
			ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9',
			ch == '-', ch == '.', ch == '_', ch == '~':
		default:
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return len(computed) == len(expectedChallenge) &&
		subtle.ConstantTimeCompare([]byte(computed), []byte(expectedChallenge)) == 1
}

func normalizePersona(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "admin", "engineer", "reviewer", "operator":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func parseFormBody(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOIDCFormBytes+1))
	if err != nil || len(body) > maxOIDCFormBytes {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "request body is invalid or too large")
		return nil, false
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return nil, false
	}
	return values, true
}

func redirectAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state, errCode string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, errCode, "authorization refused")
		return
	}
	q := target.Query()
	q.Set("error", errCode)
	if state != "" {
		q.Set("state", state)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": description,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func randomTokenHex(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
