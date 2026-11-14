package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/userland/principal"
)

type tokenSuccessResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

func (server *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	claims := []string{"iss", "sub", "aud", "exp", "iat", "jti", "nonce", "email", "name", server.config.TenantClaim}
	if server.config.ProjectID != "" {
		claims = append(claims, server.config.ProjectClaim)
	}
	doc := map[string]any{
		"issuer":                                     server.config.Issuer,
		"authorization_endpoint":                     server.endpointURL("/authorize"),
		"token_endpoint":                             server.endpointURL("/token"),
		"jwks_uri":                                   server.endpointURL("/jwks.json"),
		"userinfo_endpoint":                          server.endpointURL("/userinfo"),
		"device_authorization_endpoint":              server.endpointURL("/device"),
		"revocation_endpoint":                        server.endpointURL("/revoke"),
		"registration_endpoint":                      server.endpointURL("/register"),
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
		"subject_types_supported":                    []string{"public"},
		"id_token_signing_alg_values_supported":      []string{"ES256"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":           []string{"S256"},
		"scopes_supported":                           []string{"openid", "profile", "email", "offline_access"},
		"claims_supported":                           claims,
	}
	writeJSON(w, http.StatusOK, doc)
}

func (server *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(server.material.jwksJSON)
	}
}

func (server *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		server.handleAuthorizeGet(w, r)
	case http.MethodPost:
		server.handleAuthorizePost(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (server *Server) handleAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	now := server.config.Clock()
	q := r.URL.Query()

	// Resume an existing pending transaction after 1-click GitHub setup.
	if resumeTx := strings.TrimSpace(q.Get("tx")); resumeTx != "" && q.Get("client_id") == "" {
		tx, ok := server.store.getAuthTx(resumeTx, now)
		if !ok || tx.Kind != txAuthorize {
			server.renderErrorPage(w, http.StatusBadRequest, "Session Expired", "Your sign-in session has expired. Please start sign-in again.")
			return
		}
		server.renderLoginForTx(w, r, tx, http.StatusOK, "")
		return
	}

	clientID := strings.TrimSpace(q.Get("client_id"))
	redirectURI := strings.TrimSpace(q.Get("redirect_uri"))
	if clientID == "" || redirectURI == "" || !server.isAllowedRedirect(clientID, redirectURI) {
		server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "Unrecognized client_id or disallowed redirect_uri.")
		return
	}

	responseType := strings.TrimSpace(q.Get("response_type"))
	if responseType != "code" {
		server.renderErrorPage(w, http.StatusBadRequest, "Unsupported Response Type", "Only response_type=code is supported.")
		return
	}

	challengeMethod := strings.TrimSpace(q.Get("code_challenge_method"))
	challenge := strings.TrimSpace(q.Get("code_challenge"))
	if challengeMethod != "S256" || !isValidPKCEChallenge(challenge) {
		server.renderErrorPage(w, http.StatusBadRequest, "PKCE Required", "PKCE with code_challenge_method=S256 and a valid code_challenge is required.")
		return
	}

	scope := normalizeScopeString(q.Get("scope"))
	txID, err := randomToken(server.config.Random, 24)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not allocate authorization session.")
		return
	}
	tx := authTx{
		ID:            txID,
		Kind:          txAuthorize,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		State:         q.Get("state"),
		Nonce:         q.Get("nonce"),
		Scope:         scope,
		CodeChallenge: challenge,
		LoginHint:     strings.TrimSpace(q.Get("login_hint")),
		CreatedAt:     now,
		ExpiresAt:     now.Add(browserFlowLifetime),
	}
	server.store.putAuthTx(tx, now)
	server.renderLoginForTx(w, r, tx, http.StatusOK, "")
}

func (server *Server) renderLoginForTx(w http.ResponseWriter, r *http.Request, tx authTx, status int, errMsg string) {
	browserID, err := server.ensureBrowserID(w, r)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize browser session.")
		return
	}
	csrf, err := server.issueCSRFToken(csrfPurposeAuthorize, browserID, tx.ID)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not issue CSRF token.")
		return
	}

	ghEnabled := server.githubConfigured()
	renderHTML(w, status, loginTmpl, loginPageData{
		Issuer:              server.config.Issuer,
		ClientID:            tx.ClientID,
		TenantID:            string(server.config.TenantID),
		TxID:                tx.ID,
		CSRFToken:           csrf,
		ErrorMessage:        errMsg,
		GitHubEnabled:       ghEnabled,
		GitHubSetupEnabled:  !ghEnabled && server.config.GitHub.AllowSetup,
		UpstreamOIDCEnabled: server.upstreamOIDCConfigured(),
		UpstreamOIDCName:    server.config.UpstreamOIDC.DisplayName,
		LocalEnabled:        len(server.config.LocalPersonas) > 0,
		Personas:            server.personaOptions(tx.LoginHint),
		RequireAccessCode:   server.config.AccessCode != "",
	})
}

func (server *Server) handleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if len(server.config.LocalPersonas) == 0 {
		server.renderErrorPage(w, http.StatusBadRequest, "Local Sign-In Disabled", "Local persona sign-in is not enabled on this server.")
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "The sign-in form body could not be read.")
		return
	}
	txID := strings.TrimSpace(r.PostFormValue("tx_id"))
	csrf := strings.TrimSpace(r.PostFormValue("csrf_token"))
	if _, ok := server.verifyCSRFToken(r, csrfPurposeAuthorize, txID, csrf); !ok {
		server.renderErrorPage(w, http.StatusForbidden, "Session Expired", "Your sign-in session expired or failed CSRF validation. Please try signing in again.")
		return
	}
	now := server.config.Clock()
	tx, ok := server.store.getAuthTx(txID, now)
	if !ok || tx.Kind != txAuthorize {
		server.renderErrorPage(w, http.StatusBadRequest, "Session Expired", "Your sign-in session has expired. Please return to your client and sign in again.")
		return
	}

	persona := strings.TrimSpace(r.PostFormValue("persona"))
	if !slices.Contains(server.config.LocalPersonas, persona) {
		server.renderLoginForTx(w, r, tx, http.StatusBadRequest, "Please choose one of the configured local personas.")
		return
	}
	if server.config.AccessCode != "" && !server.verifyAccessCode(r.PostFormValue("access_code")) {
		tx.LoginHint = persona
		server.renderLoginForTx(w, r, tx, http.StatusForbidden, "Invalid access code.")
		return
	}

	user := UserIdentity{
		Subject: persona,
		Email:   persona + "@local",
		Name:    persona,
		Method:  "local",
	}
	server.completeAuthTx(w, r, txID, user, now)
}

func (server *Server) completeAuthTx(w http.ResponseWriter, r *http.Request, txID string, user UserIdentity, now time.Time) {
	tx, ok := server.store.consumeAuthTx(txID, now)
	if !ok {
		server.renderErrorPage(w, http.StatusBadRequest, "Session Expired", "Your sign-in session expired or was already completed.")
		return
	}
	switch tx.Kind {
	case txAuthorize:
		code, err := randomToken(server.config.Random, 32)
		if err != nil {
			server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not generate authorization code.")
			return
		}
		server.store.putAuthCode(code, authCodeEntry{
			ClientID:      tx.ClientID,
			RedirectURI:   tx.RedirectURI,
			CodeChallenge: tx.CodeChallenge,
			Nonce:         tx.Nonce,
			Scope:         tx.Scope,
			User:          user,
			CreatedAt:     now,
			ExpiresAt:     now.Add(server.config.AuthCodeTTL),
		}, now)

		redirectTarget, err := url.Parse(tx.RedirectURI)
		if err != nil {
			server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Stored redirect URI is invalid.")
			return
		}
		q := redirectTarget.Query()
		q.Set("code", code)
		if tx.State != "" {
			q.Set("state", tx.State)
		}
		redirectTarget.RawQuery = q.Encode()
		http.Redirect(w, r, redirectTarget.String(), http.StatusSeeOther)

	case txDevice:
		if !server.store.resolveDeviceSession(tx.DeviceHash, deviceApproved, user, now) {
			server.renderErrorPage(w, http.StatusBadRequest, "Device Code Expired", "The device authorization request expired or was already resolved.")
			return
		}
		pid, _ := principal.ForOIDC(server.config.Issuer, user.Subject)
		renderHTML(w, http.StatusOK, statusTmpl, statusPageData{
			Title:       "Device Authorized",
			Heading:     "Device Authorized",
			Message:     "Your CLI session is now signed in. You may close this window and return to your terminal.",
			PrincipalID: string(pid),
			Subject:     user.Subject,
		})

	default:
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Unknown sign-in transaction state.")
	}
}

func (server *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "request body is malformed or too large")
		return
	}
	clientID, ok := extractPublicClientID(r)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client authentication")
		return
	}
	if !server.isKnownClient(clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}

	now := server.config.Clock()
	grantType := strings.TrimSpace(r.PostFormValue("grant_type"))
	if !server.clientMayUseGrant(clientID, grantType) {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "client is not registered for this grant_type")
		return
	}
	switch grantType {
	case "authorization_code":
		code := strings.TrimSpace(r.PostFormValue("code"))
		redirectURI := strings.TrimSpace(r.PostFormValue("redirect_uri"))
		verifier := strings.TrimSpace(r.PostFormValue("code_verifier"))
		if code == "" || redirectURI == "" || verifier == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code, redirect_uri and code_verifier are required")
			return
		}
		if !isValidPKCEVerifier(verifier) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier is invalid")
			return
		}
		entry, err := server.store.consumeAuthCode(code, now)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if !constantTimeEqualStrings(entry.ClientID, clientID) || entry.RedirectURI != redirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id or redirect_uri does not match authorization code")
			return
		}
		if !constantTimeEqualStrings(pkceS256Challenge(verifier), entry.CodeChallenge) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		server.issueTokenSet(w, entry.User, clientID, entry.Scope, entry.Nonce, "", now)

	case "refresh_token":
		rawRefresh := strings.TrimSpace(r.PostFormValue("refresh_token"))
		if rawRefresh == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
			return
		}
		nextRefresh, err := randomToken(server.config.Random, 32)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not generate refresh token")
			return
		}
		nextRefresh = refreshFamily(rawRefresh) + "." + nextRefresh
		rotated, err := server.store.rotateRefreshToken(rawRefresh, nextRefresh, clientID, server.config.RefreshTokenTTL, now)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired or already rotated")
			return
		}
		server.issueTokenSet(w, rotated.User, clientID, rotated.Scope, "", nextRefresh, now)

	case "urn:ietf:params:oauth:grant-type:device_code":
		deviceCode := strings.TrimSpace(r.PostFormValue("device_code"))
		if deviceCode == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "device_code is required")
			return
		}
		entry, err := server.store.pollDeviceSession(deviceCode, clientID, now)
		if err != nil {
			switch {
			case errors.Is(err, errAuthPending):
				writeOAuthError(w, http.StatusBadRequest, "authorization_pending", "device authorization is still pending")
			case errors.Is(err, errExpiredToken):
				writeOAuthError(w, http.StatusBadRequest, "expired_token", "device authorization has expired")
			case errors.Is(err, errAccessDenied):
				writeOAuthError(w, http.StatusBadRequest, "access_denied", "device authorization was denied")
			default:
				writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "device_code is invalid")
			}
			return
		}
		server.issueTokenSet(w, entry.User, clientID, entry.Scope, "", "", now)

	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type is not supported")
	}
}

func (server *Server) issueTokenSet(w http.ResponseWriter, user UserIdentity, clientID, scope, nonce, preIssuedRefresh string, now time.Time) {
	accessToken, _, err := server.mintAccessToken(user, clientID, scope, nonce, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not sign access token")
		return
	}
	idToken, err := server.mintIDToken(user, clientID, nonce, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not sign identity token")
		return
	}
	refreshToken := preIssuedRefresh
	if refreshToken == "" {
		rawRefresh, err := randomToken(server.config.Random, 32)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not generate refresh token")
			return
		}
		familyID, err := randomToken(server.config.Random, 16)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not generate token family")
			return
		}
		refreshToken = familyID + "." + rawRefresh
		server.store.issueRefreshToken(refreshToken, clientID, scope, user, server.config.RefreshTokenTTL, now)
	}
	expiresSec := int64(server.config.AccessTokenTTL / time.Second)
	if expiresSec <= 0 {
		expiresSec = 1
	}
	writeJSON(w, http.StatusOK, tokenSuccessResponse{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresSec,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		Scope:        scope,
	})
}

func (server *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		server.handleDeviceAuthorizePost(w, r)
	case http.MethodGet:
		server.handleDevicePageGet(w, r)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (server *Server) handleDeviceAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if err := server.parseBoundedForm(w, r); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "request body is malformed or too large")
		return
	}
	clientID, ok := extractPublicClientID(r)
	if !ok || !server.isKnownClient(clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown or invalid client_id")
		return
	}
	if !server.clientMayUseGrant(clientID, "urn:ietf:params:oauth:grant-type:device_code") {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "client is not registered for the device grant")
		return
	}
	scope := normalizeScopeString(r.PostFormValue("scope"))
	now := server.config.Clock()
	deviceCode, userCode, err := server.store.createDeviceSession(server.config.Random, clientID, scope, server.config.DeviceCodeTTL, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not create device session")
		return
	}
	verificationURI := server.endpointURL("/device")
	expiresSec := int64(server.config.DeviceCodeTTL / time.Second)
	if expiresSec <= 0 {
		expiresSec = 60
	}
	intervalSec := int64(server.config.DevicePollInterval / time.Second)
	if intervalSec <= 0 {
		intervalSec = 1
	}
	writeJSON(w, http.StatusOK, deviceAuthResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURI + "?user_code=" + url.QueryEscape(userCode),
		ExpiresIn:               expiresSec,
		Interval:                intervalSec,
	})
}

func (server *Server) handleDevicePageGet(w http.ResponseWriter, r *http.Request) {
	userCode := strings.TrimSpace(r.URL.Query().Get("user_code"))
	server.renderDevicePage(w, r, http.StatusOK, userCode, "", "")
}

func (server *Server) renderDevicePage(w http.ResponseWriter, r *http.Request, status int, userCode, selectedPersona, errMsg string) {
	browserID, err := server.ensureBrowserID(w, r)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not generate browser session.")
		return
	}
	csrf, err := server.issueCSRFToken(csrfPurposeDevice, browserID, "")
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not generate CSRF token.")
		return
	}
	ghEnabled := server.githubConfigured()
	renderHTML(w, status, deviceTmpl, devicePageData{
		Issuer:              server.config.Issuer,
		TenantID:            string(server.config.TenantID),
		CSRFToken:           csrf,
		UserCode:            userCode,
		ErrorMessage:        errMsg,
		GitHubEnabled:       ghEnabled,
		GitHubSetupEnabled:  !ghEnabled && server.config.GitHub.AllowSetup,
		UpstreamOIDCEnabled: server.upstreamOIDCConfigured(),
		UpstreamOIDCName:    server.config.UpstreamOIDC.DisplayName,
		LocalEnabled:        len(server.config.LocalPersonas) > 0,
		Personas:            server.personaOptions(selectedPersona),
		RequireAccessCode:   server.config.AccessCode != "",
	})
}

func (server *Server) handleDeviceVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "The device verification form could not be read.")
		return
	}
	csrf := strings.TrimSpace(r.PostFormValue("csrf_token"))
	browserID, ok := server.verifyCSRFToken(r, csrfPurposeDevice, "", csrf)
	if !ok {
		server.renderErrorPage(w, http.StatusForbidden, "Session Expired", "Device verification CSRF check failed. Reload /device and try again.")
		return
	}
	now := server.config.Clock()
	userCode := strings.TrimSpace(r.PostFormValue("user_code"))
	devEntry, ok := server.store.lookupPendingDeviceByUserCode(userCode, now)
	if !ok {
		server.renderDevicePage(w, r, http.StatusBadRequest, userCode, "", "Invalid or expired device code.")
		return
	}

	if strings.TrimSpace(r.PostFormValue("action")) == "deny" {
		server.store.resolveDeviceSession(devEntry.DeviceHash, deviceDenied, UserIdentity{}, now)
		renderHTML(w, http.StatusOK, statusTmpl, statusPageData{
			Title:   "Device Request Denied",
			Heading: "Device Request Denied",
			Message: "The device authorization request was denied.",
		})
		return
	}

	method := strings.TrimSpace(r.PostFormValue("method"))
	if method == "" {
		method = "local"
	}

	switch method {
	case "github":
		if !server.githubConfigured() {
			server.renderDevicePage(w, r, http.StatusBadRequest, userCode, "", "GitHub login is not configured.")
			return
		}
		txID, err := randomToken(server.config.Random, 24)
		if err != nil {
			server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not allocate device transaction.")
			return
		}
		server.store.putAuthTx(authTx{
			ID:         txID,
			Kind:       txDevice,
			ClientID:   devEntry.ClientID,
			Scope:      devEntry.Scope,
			DeviceHash: devEntry.DeviceHash,
			UserCode:   devEntry.DisplayUserCode,
			CreatedAt:  now,
			ExpiresAt:  now.Add(browserFlowLifetime),
		}, now)
		server.startGitHubRedirect(w, r, txID, browserID, now)

	case "oidc":
		if !server.upstreamOIDCConfigured() {
			server.renderDevicePage(w, r, http.StatusBadRequest, userCode, "", "SSO login is not configured.")
			return
		}
		txID, err := randomToken(server.config.Random, 24)
		if err != nil {
			server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not allocate device transaction.")
			return
		}
		server.store.putAuthTx(authTx{
			ID:         txID,
			Kind:       txDevice,
			ClientID:   devEntry.ClientID,
			Scope:      devEntry.Scope,
			DeviceHash: devEntry.DeviceHash,
			UserCode:   devEntry.DisplayUserCode,
			CreatedAt:  now,
			ExpiresAt:  now.Add(browserFlowLifetime),
		}, now)
		server.startUpstreamOIDCRedirect(w, r, txID, browserID, now)

	case "local":
		if len(server.config.LocalPersonas) == 0 {
			server.renderDevicePage(w, r, http.StatusBadRequest, userCode, "", "Local persona sign-in is not enabled.")
			return
		}
		persona := strings.TrimSpace(r.PostFormValue("persona"))
		if !slices.Contains(server.config.LocalPersonas, persona) {
			server.renderDevicePage(w, r, http.StatusBadRequest, userCode, persona, "Please choose one of the configured local personas.")
			return
		}
		if server.config.AccessCode != "" && !server.verifyAccessCode(r.PostFormValue("access_code")) {
			server.renderDevicePage(w, r, http.StatusForbidden, userCode, persona, "Invalid access code.")
			return
		}
		user := UserIdentity{
			Subject: persona,
			Email:   persona + "@local",
			Name:    persona,
			Method:  "local",
		}
		if !server.store.resolveDeviceSession(devEntry.DeviceHash, deviceApproved, user, now) {
			server.renderDevicePage(w, r, http.StatusBadRequest, userCode, persona, "The device code expired or was already resolved.")
			return
		}
		pid, _ := principal.ForOIDC(server.config.Issuer, user.Subject)
		renderHTML(w, http.StatusOK, statusTmpl, statusPageData{
			Title:       "Device Authorized",
			Heading:     "Device Authorized",
			Message:     "Your CLI session is now signed in. You may close this window and return to your terminal.",
			PrincipalID: string(pid),
			Subject:     user.Subject,
		})

	default:
		server.renderDevicePage(w, r, http.StatusBadRequest, userCode, "", "Unsupported sign-in method.")
	}
}

func (server *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "request body is malformed or too large")
		return
	}
	clientID, ok := extractPublicClientID(r)
	if !ok || !server.isKnownClient(clientID) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown or invalid client_id")
		return
	}
	rawToken := strings.TrimSpace(r.PostFormValue("token"))
	if rawToken == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	hint := strings.TrimSpace(r.PostFormValue("token_type_hint"))
	if hint != "" && hint != "refresh_token" && hint != "access_token" {
		// RFC 7009 §2.1: unsupported hints are ignored if the server can still check revocation.
	}
	now := server.config.Clock()
	server.store.revokeRefreshToken(rawToken, clientID)
	if claims, err := server.verifyOwnAccessToken(rawToken, now); err == nil && claims.JTI != "" && claims.Client == clientID {
		// Keep the revocation until the token stops verifying, clock skew included.
		server.store.revokeJTI(claims.JTI, claims.Expiry.Add(jwtClockSkew), now)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (server *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	body, err := server.readBoundedBody(w, r)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration body is unreadable or too large")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration body must be a JSON object")
		return
	}
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		Scope                   string   `json:"scope"`
	}
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "registration JSON is malformed")
		return
	}
	authMethod := strings.TrimSpace(req.TokenEndpointAuthMethod)
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public PKCE clients (token_endpoint_auth_method=none) are supported")
		return
	}

	grantTypes := make([]string, 0, len(req.GrantTypes))
	for _, gt := range req.GrantTypes {
		gt = strings.TrimSpace(gt)
		if gt == "" {
			continue
		}
		switch gt {
		case "authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code":
			if !slices.Contains(grantTypes, gt) {
				grantTypes = append(grantTypes, gt)
			}
		default:
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type requested")
			return
		}
	}
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}

	responseTypes := make([]string, 0, len(req.ResponseTypes))
	for _, rt := range req.ResponseTypes {
		rt = strings.TrimSpace(rt)
		if rt == "" {
			continue
		}
		if rt != "code" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "only response_type=code is supported")
			return
		}
		if !slices.Contains(responseTypes, rt) {
			responseTypes = append(responseTypes, rt)
		}
	}
	if len(responseTypes) == 0 && slices.Contains(grantTypes, "authorization_code") {
		responseTypes = []string{"code"}
	}

	redirects := make([]string, 0, len(req.RedirectURIs))
	for _, rawURI := range req.RedirectURIs {
		cleanURI := strings.TrimSpace(rawURI)
		if err := validateRedirectURI(cleanURI); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be https or numeric loopback http without fragment")
			return
		}
		if !slices.Contains(redirects, cleanURI) {
			redirects = append(redirects, cleanURI)
		}
	}
	if slices.Contains(grantTypes, "authorization_code") && len(redirects) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required for authorization_code clients")
		return
	}

	rawRandom, err := randomToken(server.config.Random, 16)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not allocate client_id")
		return
	}
	clientID := "mcp-" + rawRandom
	now := server.config.Clock()
	clientName := strings.TrimSpace(req.ClientName)
	if len(clientName) > 128 {
		clientName = strings.ToValidUTF8(clientName[:128], "")
	}

	server.store.registerClient(registeredClient{
		ClientID:      clientID,
		ClientName:    clientName,
		RedirectURIs:  redirects,
		GrantTypes:    grantTypes,
		ResponseTypes: responseTypes,
		IssuedAt:      now.Unix(),
	})

	resp := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        now.Unix(),
		"redirect_uris":              redirects,
		"grant_types":                grantTypes,
		"response_types":             responseTypes,
		"token_endpoint_auth_method": "none",
	}
	if clientName != "" {
		resp["client_name"] = clientName
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (server *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	claims, ok := server.authenticateBearer(w, r)
	if !ok {
		return
	}
	pid, err := principal.ForOIDC(server.config.Issuer, claims.Subject)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "invalid token subject")
		return
	}
	out := map[string]any{
		"principal_id": string(pid),
		"issuer":       server.config.Issuer,
		"subject":      claims.Subject,
		"tenant":       claims.Tenant,
	}
	if claims.Project != "" {
		out["project"] = claims.Project
	}
	if claims.Email != "" {
		out["email"] = claims.Email
	}
	if claims.Name != "" {
		out["name"] = claims.Name
	}
	writeJSON(w, http.StatusOK, out)
}

func (server *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	claims, ok := server.authenticateBearer(w, r)
	if !ok {
		return
	}
	out := map[string]any{
		"sub":                     claims.Subject,
		server.config.TenantClaim: claims.Tenant,
	}
	if claims.Project != "" && server.config.ProjectClaim != "" {
		out[server.config.ProjectClaim] = claims.Project
	}
	if claims.Email != "" {
		out["email"] = claims.Email
	}
	if claims.Name != "" {
		out["name"] = claims.Name
	}
	writeJSON(w, http.StatusOK, out)
}

func (server *Server) authenticateBearer(w http.ResponseWriter, r *http.Request) (verifiedAccessClaims, bool) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(authHeader) <= len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "Bearer access token is required")
		return verifiedAccessClaims{}, false
	}
	rawToken := strings.TrimSpace(authHeader[len(prefix):])
	claims, err := server.verifyOwnAccessToken(rawToken, server.config.Clock())
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "access token could not be verified")
		return verifiedAccessClaims{}, false
	}
	return claims, true
}

func extractPublicClientID(r *http.Request) (string, bool) {
	formID := strings.TrimSpace(r.PostFormValue("client_id"))
	basicID, basicSecret, hasBasic := r.BasicAuth()
	if hasBasic {
		if basicSecret != "" {
			return "", false
		}
		basicID = strings.TrimSpace(basicID)
		if basicID == "" {
			return "", false
		}
		if formID != "" && formID != basicID {
			return "", false
		}
		return basicID, true
	}
	if formID == "" {
		return "", false
	}
	if strings.TrimSpace(r.PostFormValue("client_secret")) != "" {
		return "", false
	}
	return formID, true
}

func normalizeScopeString(raw string) string {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return "openid profile email"
	}
	return strings.Join(fields, " ")
}

func (server *Server) parseBoundedForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, server.config.MaxBodyBytes)
	return r.ParseForm()
}

func (server *Server) readBoundedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, server.config.MaxBodyBytes)
	return io.ReadAll(limited)
}

func (server *Server) signCookieValue(payload string) string {
	mac := hmac.New(sha256.New, server.material.cookieSecret)
	_, _ = mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return url.QueryEscape(payload) + "." + sig
}

func (server *Server) decodeSignedCookieValue(rawCookieVal string) (string, bool) {
	encodedPayload, hexSig, ok := strings.Cut(rawCookieVal, ".")
	if !ok || encodedPayload == "" || hexSig == "" {
		return "", false
	}
	gotSig, err := hex.DecodeString(hexSig)
	if err != nil {
		return "", false
	}
	payload, err := url.QueryUnescape(encodedPayload)
	if err != nil || payload == "" {
		return "", false
	}
	mac := hmac.New(sha256.New, server.material.cookieSecret)
	_, _ = mac.Write([]byte(payload))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return "", false
	}
	return payload, true
}

func (server *Server) readBrowserID(r *http.Request) (string, bool) {
	for _, c := range r.Cookies() {
		if c.Name != cookieBrowserSession || c.Value == "" {
			continue
		}
		if payload, ok := server.decodeSignedCookieValue(c.Value); ok {
			return payload, true
		}
	}
	return "", false
}

func (server *Server) ensureBrowserID(w http.ResponseWriter, r *http.Request) (string, error) {
	if existing, ok := server.readBrowserID(r); ok {
		return existing, nil
	}
	fresh, err := randomToken(server.config.Random, 32)
	if err != nil {
		return "", err
	}
	server.setSignedCookie(w, cookieBrowserSession, fresh, int(browserFlowLifetime.Seconds()))
	return fresh, nil
}

func (server *Server) computeCSRFMAC(purpose, browserID, contextID, nonce string) []byte {
	mac := hmac.New(sha256.New, server.material.cookieSecret)
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(browserID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(contextID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(nonce))
	return mac.Sum(nil)
}

func (server *Server) issueCSRFToken(purpose, browserID, contextID string) (string, error) {
	nonce, err := randomToken(server.config.Random, 16)
	if err != nil {
		return "", err
	}
	sig := hex.EncodeToString(server.computeCSRFMAC(purpose, browserID, contextID, nonce))
	return nonce + "." + sig, nil
}

func (server *Server) verifyCSRFToken(r *http.Request, purpose, contextID, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	nonce, hexSig, ok := strings.Cut(token, ".")
	if !ok || nonce == "" || hexSig == "" {
		return "", false
	}
	gotSig, err := hex.DecodeString(hexSig)
	if err != nil {
		return "", false
	}
	for _, c := range r.Cookies() {
		if c.Name != cookieBrowserSession || c.Value == "" {
			continue
		}
		browserID, validCookie := server.decodeSignedCookieValue(c.Value)
		if !validCookie {
			continue
		}
		expected := server.computeCSRFMAC(purpose, browserID, contextID, nonce)
		if hmac.Equal(gotSig, expected) {
			return browserID, true
		}
	}
	return "", false
}

func (server *Server) setSignedCookie(w http.ResponseWriter, name, payload string, maxAgeSeconds int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    server.signCookieValue(payload),
		Path:     "/",
		MaxAge:   maxAgeSeconds,
		HttpOnly: true,
		Secure:   server.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (server *Server) renderErrorPage(w http.ResponseWriter, status int, heading, message string) {
	renderHTML(w, status, statusTmpl, statusPageData{
		Title:   heading,
		Heading: heading,
		Message: message,
		IsError: true,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, oauthErrorResponse{
		Error:            code,
		ErrorDescription: description,
	})
}

func writeMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "HTTP method not allowed")
}
