package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/userland/principal"
)

const (
	cookieBrowserSession  = "__kodama_auth_session"
	csrfPurposeAuthorize  = "authorize"
	csrfPurposeDevice     = "device"
	csrfPurposeGitHubInit = "gh_setup"

	upstreamMetadataCacheTTL = 10 * time.Minute
)

type upstreamOIDCMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	fetchedAt             time.Time
}

func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func isValidPKCEVerifier(verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for i := 0; i < len(verifier); i++ {
		ch := verifier[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '-', ch == '.', ch == '_', ch == '~':
		default:
			return false
		}
	}
	return true
}

func isValidPKCEChallenge(challenge string) bool {
	if len(challenge) < 43 || len(challenge) > 128 {
		return false
	}
	for i := 0; i < len(challenge); i++ {
		ch := challenge[i]
		switch {
		case ch >= 'A' && ch <= 'Z':
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

func (server *Server) handleGitHubStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !server.githubConfigured() {
		server.renderErrorPage(w, http.StatusBadRequest, "GitHub Login Disabled", "GitHub login is not configured on this server.")
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "The sign-in form body could not be read.")
		return
	}
	txID := strings.TrimSpace(r.PostFormValue("tx_id"))
	csrf := strings.TrimSpace(r.PostFormValue("csrf_token"))
	browserID, ok := server.verifyCSRFToken(r, csrfPurposeAuthorize, txID, csrf)
	if !ok {
		server.renderErrorPage(w, http.StatusForbidden, "Session Expired", "Your sign-in session expired or failed CSRF validation. Please start sign-in again.")
		return
	}
	now := server.config.Clock()
	if _, ok := server.store.getAuthTx(txID, now); !ok {
		server.renderErrorPage(w, http.StatusBadRequest, "Session Expired", "Your sign-in session expired. Please return to your client and sign in again.")
		return
	}
	server.startGitHubRedirect(w, r, txID, browserID, now)
}

func (server *Server) startGitHubRedirect(w http.ResponseWriter, r *http.Request, txID, browserID string, now time.Time) {
	clientID, _ := server.githubCredentials()
	if clientID == "" {
		server.renderErrorPage(w, http.StatusBadRequest, "GitHub Login Disabled", "GitHub login is not configured.")
		return
	}
	state, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize GitHub sign-in.")
		return
	}
	verifier, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize GitHub sign-in.")
		return
	}
	server.store.putSocialTx(state, socialTx{
		Provider:     "github",
		AuthTxID:     txID,
		BrowserID:    browserID,
		CodeVerifier: verifier,
		CreatedAt:    now,
		ExpiresAt:    now.Add(browserFlowLifetime),
	}, now)

	target, err := url.Parse(server.config.GitHub.WebURL + "/login/oauth/authorize")
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Invalid GitHub authorize URL.")
		return
	}
	q := target.Query()
	q.Set("client_id", clientID)
	q.Set("redirect_uri", server.endpointURL("/oauth/github/callback"))
	q.Set("scope", "read:user user:email")
	q.Set("state", state)
	q.Set("code_challenge", pkceS256Challenge(verifier))
	q.Set("code_challenge_method", "S256")
	target.RawQuery = q.Encode()

	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (server *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	browserID, hasBrowser := server.readBrowserID(r)
	if state == "" || !hasBrowser {
		server.renderErrorPage(w, http.StatusForbidden, "Sign-In Refused", "The GitHub callback state did not match your browser session.")
		return
	}
	now := server.config.Clock()
	social, ok := server.store.consumeSocialTx(state, "github", now)
	if !ok || !constantTimeEqualStrings(browserID, social.BrowserID) {
		server.renderErrorPage(w, http.StatusForbidden, "Sign-In Refused", "The GitHub sign-in request expired, was already used, or did not match your browser session.")
		return
	}
	if r.URL.Query().Get("error") != "" || code == "" {
		server.renderErrorPage(w, http.StatusUnauthorized, "GitHub Sign-In Canceled", "GitHub did not grant authorization.")
		return
	}

	user, err := server.exchangeGitHubUser(r.Context(), code, social.CodeVerifier)
	if err != nil {
		server.renderErrorPage(w, http.StatusBadGateway, "GitHub Sign-In Failed", "Could not verify your GitHub account.")
		return
	}
	server.completeAuthTx(w, r, social.AuthTxID, user, now)
}

func (server *Server) exchangeGitHubUser(ctx context.Context, code, codeVerifier string) (UserIdentity, error) {
	clientID, clientSecret := server.githubCredentials()
	if clientID == "" || clientSecret == "" {
		return UserIdentity{}, errors.New("GitHub credentials not configured")
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {server.endpointURL("/oauth/github/callback")},
		"code_verifier": {codeVerifier},
	}
	tokenBody, status, err := server.outboundRequest(ctx, http.MethodPost,
		server.config.GitHub.WebURL+"/login/oauth/access_token", form, "")
	if err != nil || status < 200 || status >= 300 {
		return UserIdentity{}, errors.New("GitHub token exchange failed")
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(tokenBody, &tokenResp); err != nil || tokenResp.Error != "" || tokenResp.AccessToken == "" {
		return UserIdentity{}, errors.New("GitHub token response invalid")
	}

	userBody, userStatus, err := server.outboundRequest(ctx, http.MethodGet,
		server.config.GitHub.APIURL+"/user", nil, tokenResp.AccessToken)
	if err != nil || userStatus < 200 || userStatus >= 300 {
		return UserIdentity{}, errors.New("GitHub user lookup failed")
	}
	var ghUser struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(userBody, &ghUser); err != nil {
		return UserIdentity{}, errors.New("GitHub user response malformed")
	}
	subject, err := principal.SubjectForGitHub(ghUser.Login)
	if err != nil {
		return UserIdentity{}, err
	}

	email := strings.TrimSpace(ghUser.Email)
	if email == "" {
		if emailsBody, emailsStatus, err := server.outboundRequest(ctx, http.MethodGet,
			server.config.GitHub.APIURL+"/user/emails", nil, tokenResp.AccessToken); err == nil && emailsStatus >= 200 && emailsStatus < 300 {
			var entries []struct {
				Email    string `json:"email"`
				Primary  bool   `json:"primary"`
				Verified bool   `json:"verified"`
			}
			if json.Unmarshal(emailsBody, &entries) == nil {
				for _, entry := range entries {
					if entry.Primary && entry.Verified && strings.TrimSpace(entry.Email) != "" {
						email = strings.TrimSpace(entry.Email)
						break
					}
				}
				if email == "" {
					for _, entry := range entries {
						if entry.Verified && strings.TrimSpace(entry.Email) != "" {
							email = strings.TrimSpace(entry.Email)
							break
						}
					}
				}
			}
		}
	}
	name := strings.TrimSpace(ghUser.Name)
	if name == "" {
		name = strings.TrimSpace(ghUser.Login)
	}
	return UserIdentity{
		Subject: subject,
		Email:   email,
		Name:    name,
		Method:  "github",
	}, nil
}

// handleGitHubSetup serves GET and POST /setup/github for the 1-Click GitHub
// App Manifest registration flow when --allow-github-setup is enabled.
func (server *Server) handleGitHubSetup(w http.ResponseWriter, r *http.Request) {
	if !server.config.GitHub.AllowSetup {
		server.renderErrorPage(w, http.StatusNotFound, "Not Enabled", "1-Click GitHub setup is not enabled on this server.")
		return
	}
	if server.githubConfigured() {
		server.renderErrorPage(w, http.StatusConflict, "Already Configured", "GitHub login is already configured on this server.")
		return
	}
	now := server.config.Clock()

	switch r.Method {
	case http.MethodGet:
		txID := strings.TrimSpace(r.URL.Query().Get("tx"))
		org := strings.TrimSpace(r.URL.Query().Get("org"))
		if org != "" {
			if _, err := principal.SubjectForGitHub(org); err != nil {
				server.renderErrorPage(w, http.StatusBadRequest, "Invalid Organization", "The GitHub organization name is not valid.")
				return
			}
		}
		browserID, err := server.ensureBrowserID(w, r)
		if err != nil {
			server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize setup.")
			return
		}
		if server.config.AccessCode != "" {
			csrf, err := server.issueCSRFToken(csrfPurposeGitHubInit, browserID, "")
			if err != nil {
				server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize setup.")
				return
			}
			renderHTML(w, http.StatusOK, githubSetupTmpl, githubSetupPageData{
				Issuer:            server.config.Issuer,
				TxID:              txID,
				CSRFToken:         csrf,
				Org:               org,
				Locked:            true,
				SetupFormEndpoint: "/setup/github",
			})
			return
		}
		server.renderGitHubManifestForm(w, txID, org, browserID, now)

	case http.MethodPost:
		if err := server.parseBoundedForm(w, r); err != nil {
			server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "The setup form could not be read.")
			return
		}
		csrf := strings.TrimSpace(r.PostFormValue("csrf_token"))
		txID := strings.TrimSpace(r.PostFormValue("tx"))
		org := strings.TrimSpace(r.PostFormValue("org"))
		browserID, ok := server.verifyCSRFToken(r, csrfPurposeGitHubInit, "", csrf)
		if !ok {
			server.renderErrorPage(w, http.StatusForbidden, "Session Expired", "Setup CSRF validation failed. Reload /setup/github and try again.")
			return
		}
		if server.config.AccessCode != "" && !server.verifyAccessCode(r.PostFormValue("access_code")) {
			newCSRF, err := server.issueCSRFToken(csrfPurposeGitHubInit, browserID, "")
			if err != nil {
				server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize setup.")
				return
			}
			renderHTML(w, http.StatusForbidden, githubSetupTmpl, githubSetupPageData{
				Issuer:            server.config.Issuer,
				TxID:              txID,
				CSRFToken:         newCSRF,
				Org:               org,
				Locked:            true,
				ErrorMessage:      "Invalid access code.",
				SetupFormEndpoint: "/setup/github",
			})
			return
		}
		if org != "" {
			if _, err := principal.SubjectForGitHub(org); err != nil {
				server.renderErrorPage(w, http.StatusBadRequest, "Invalid Organization", "The GitHub organization name is not valid.")
				return
			}
		}
		server.renderGitHubManifestForm(w, txID, org, browserID, now)

	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (server *Server) renderGitHubManifestForm(w http.ResponseWriter, txID, org, browserID string, now time.Time) {
	setupState, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize GitHub setup.")
		return
	}
	server.store.putGitHubSetupTx(setupState, txID, browserID, now)

	issuerURL, _ := url.Parse(server.config.Issuer)
	appLabel := "Kodama Auth"
	if issuerURL != nil && issuerURL.Hostname() != "" {
		appLabel = fmt.Sprintf("Kodama Auth (%s)", issuerURL.Hostname())
	}
	if len(appLabel) > 34 {
		appLabel = appLabel[:34]
	}

	manifest := map[string]any{
		"name":                     appLabel,
		"url":                      server.config.Issuer,
		"callback_url":             server.endpointURL("/oauth/github/callback"),
		"redirect_url":             server.endpointURL("/setup/github/callback"),
		"public":                   false,
		"request_oauth_on_install": false,
		"default_permissions": map[string]string{
			"emails": "read",
		},
		"default_events": []string{},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not encode GitHub App manifest.")
		return
	}

	actionPath := "/settings/apps/new"
	if org != "" {
		actionPath = "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	actionURL := server.config.GitHub.WebURL + actionPath + "?state=" + url.QueryEscape(setupState)

	renderHTML(w, http.StatusOK, githubSetupTmpl, githubSetupPageData{
		Issuer:       server.config.Issuer,
		TxID:         txID,
		Org:          org,
		Locked:       false,
		ActionURL:    actionURL,
		ManifestJSON: string(manifestBytes),
	})
}

func (server *Server) handleGitHubSetupCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	if !server.config.GitHub.AllowSetup {
		server.renderErrorPage(w, http.StatusNotFound, "Not Enabled", "1-Click GitHub setup is not enabled.")
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	browserID, hasBrowser := server.readBrowserID(r)
	if state == "" || code == "" || !hasBrowser {
		server.renderErrorPage(w, http.StatusForbidden, "Setup Refused", "The GitHub setup state did not match your browser session.")
		return
	}
	now := server.config.Clock()
	setupTx, ok := server.store.consumeGitHubSetupTx(state, now)
	if !ok || !constantTimeEqualStrings(browserID, setupTx.BrowserID) {
		server.renderErrorPage(w, http.StatusForbidden, "Setup Refused", "The GitHub setup session expired, was already completed, or did not match your browser session.")
		return
	}
	if server.githubConfigured() {
		server.renderErrorPage(w, http.StatusConflict, "Already Configured", "GitHub login is already configured.")
		return
	}

	convURL := server.config.GitHub.APIURL + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	body, status, err := server.outboundRequest(r.Context(), http.MethodPost, convURL, nil, "")
	if err != nil || status < 200 || status >= 300 {
		server.renderErrorPage(w, http.StatusBadGateway, "GitHub Setup Failed", "Could not exchange the GitHub App manifest code.")
		return
	}
	var converted struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &converted); err != nil ||
		strings.TrimSpace(converted.ClientID) == "" || strings.TrimSpace(converted.ClientSecret) == "" {
		server.renderErrorPage(w, http.StatusBadGateway, "GitHub Setup Failed", "GitHub returned an incomplete App conversion response.")
		return
	}
	if err := server.installGitHubCredentials(strings.TrimSpace(converted.ClientID), strings.TrimSpace(converted.ClientSecret)); err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "GitHub Setup Failed", "Could not save GitHub OAuth credentials.")
		return
	}

	continueURL := ""
	continueLabel := ""
	if setupTx.AuthTxID != "" {
		if _, ok := server.store.getAuthTx(setupTx.AuthTxID, now); ok {
			continueURL = "/authorize?tx=" + url.QueryEscape(setupTx.AuthTxID)
			continueLabel = "Return to Sign In"
		}
	}
	renderHTML(w, http.StatusOK, statusTmpl, statusPageData{
		Title:       "GitHub Login Configured",
		Heading:     "GitHub Login Configured",
		Message:     "Your GitHub App credentials are active. Users can now sign in with GitHub.",
		ContinueURL: continueURL,
		ContinueLbl: continueLabel,
	})
}

func (server *Server) handleUpstreamOIDCStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	if !server.upstreamOIDCConfigured() {
		server.renderErrorPage(w, http.StatusBadRequest, "SSO Disabled", "Upstream OIDC login is not configured.")
		return
	}
	if err := server.parseBoundedForm(w, r); err != nil {
		server.renderErrorPage(w, http.StatusBadRequest, "Invalid Request", "The sign-in form body could not be read.")
		return
	}
	txID := strings.TrimSpace(r.PostFormValue("tx_id"))
	csrf := strings.TrimSpace(r.PostFormValue("csrf_token"))
	browserID, ok := server.verifyCSRFToken(r, csrfPurposeAuthorize, txID, csrf)
	if !ok {
		server.renderErrorPage(w, http.StatusForbidden, "Session Expired", "Your sign-in session expired or failed CSRF validation.")
		return
	}
	now := server.config.Clock()
	if _, ok := server.store.getAuthTx(txID, now); !ok {
		server.renderErrorPage(w, http.StatusBadRequest, "Session Expired", "Your sign-in session expired. Please return to your client and sign in again.")
		return
	}
	server.startUpstreamOIDCRedirect(w, r, txID, browserID, now)
}

func (server *Server) startUpstreamOIDCRedirect(w http.ResponseWriter, r *http.Request, txID, browserID string, now time.Time) {
	meta, err := server.fetchUpstreamMetadata(r.Context(), now)
	if err != nil {
		server.renderErrorPage(w, http.StatusBadGateway, "SSO Unavailable", "Could not discover the upstream identity provider configuration.")
		return
	}
	state, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize SSO sign-in.")
		return
	}
	verifier, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize SSO sign-in.")
		return
	}
	nonce, err := randomToken(server.config.Random, 32)
	if err != nil {
		server.renderErrorPage(w, http.StatusInternalServerError, "Internal Error", "Could not initialize SSO sign-in.")
		return
	}
	server.store.putSocialTx(state, socialTx{
		Provider:      "oidc",
		AuthTxID:      txID,
		BrowserID:     browserID,
		CodeVerifier:  verifier,
		UpstreamNonce: nonce,
		CreatedAt:     now,
		ExpiresAt:     now.Add(browserFlowLifetime),
	}, now)

	target, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		server.renderErrorPage(w, http.StatusBadGateway, "SSO Unavailable", "Upstream authorization endpoint is invalid.")
		return
	}
	q := target.Query()
	q.Set("response_type", "code")
	q.Set("client_id", server.config.UpstreamOIDC.ClientID)
	q.Set("redirect_uri", server.endpointURL("/oauth/oidc/callback"))
	q.Set("scope", strings.Join(server.config.UpstreamOIDC.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceS256Challenge(verifier))
	q.Set("code_challenge_method", "S256")
	target.RawQuery = q.Encode()

	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (server *Server) handleUpstreamOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	browserID, hasBrowser := server.readBrowserID(r)
	if state == "" || !hasBrowser {
		server.renderErrorPage(w, http.StatusForbidden, "Sign-In Refused", "The SSO callback state did not match your browser session.")
		return
	}
	now := server.config.Clock()
	social, ok := server.store.consumeSocialTx(state, "oidc", now)
	if !ok || !constantTimeEqualStrings(browserID, social.BrowserID) {
		server.renderErrorPage(w, http.StatusForbidden, "Sign-In Refused", "The SSO sign-in session expired, was already used, or did not match your browser session.")
		return
	}
	if r.URL.Query().Get("error") != "" || code == "" {
		server.renderErrorPage(w, http.StatusUnauthorized, "SSO Sign-In Canceled", "The upstream identity provider refused authorization.")
		return
	}

	user, err := server.exchangeUpstreamOIDCUser(r.Context(), code, social.CodeVerifier, social.UpstreamNonce, now)
	if err != nil {
		server.renderErrorPage(w, http.StatusBadGateway, "SSO Sign-In Failed", "Could not verify your upstream identity token.")
		return
	}
	server.completeAuthTx(w, r, social.AuthTxID, user, now)
}

func (server *Server) exchangeUpstreamOIDCUser(ctx context.Context, code, codeVerifier, expectedNonce string, now time.Time) (UserIdentity, error) {
	meta, err := server.fetchUpstreamMetadata(ctx, now)
	if err != nil {
		return UserIdentity{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {server.endpointURL("/oauth/oidc/callback")},
		"client_id":     {server.config.UpstreamOIDC.ClientID},
		"client_secret": {server.config.UpstreamOIDC.ClientSecret},
		"code_verifier": {codeVerifier},
	}
	tokenBody, status, err := server.outboundRequest(ctx, http.MethodPost, meta.TokenEndpoint, form, "")
	if err != nil || status < 200 || status >= 300 {
		return UserIdentity{}, errors.New("upstream token exchange failed")
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(tokenBody, &tokenResp); err != nil || tokenResp.IDToken == "" {
		return UserIdentity{}, errors.New("upstream token response missing id_token")
	}

	jwksBody, jwksStatus, err := server.outboundRequest(ctx, http.MethodGet, meta.JWKSURI, nil, "")
	if err != nil || jwksStatus < 200 || jwksStatus >= 300 {
		return UserIdentity{}, errors.New("upstream JWKS fetch failed")
	}
	claims, err := verifyUpstreamIDToken(tokenResp.IDToken, jwksBody,
		server.config.UpstreamOIDC.Issuer, server.config.UpstreamOIDC.ClientID, expectedNonce, now)
	if err != nil {
		return UserIdentity{}, err
	}

	email := strings.TrimSpace(claims.Email)
	emailVerified := claims.EmailVerified
	name := strings.TrimSpace(claims.Name)

	if (email == "" || name == "") && meta.UserinfoEndpoint != "" && tokenResp.AccessToken != "" {
		if uiBody, uiStatus, err := server.outboundRequest(ctx, http.MethodGet, meta.UserinfoEndpoint, nil, tokenResp.AccessToken); err == nil && uiStatus >= 200 && uiStatus < 300 {
			var ui map[string]json.RawMessage
			if json.Unmarshal(uiBody, &ui) == nil {
				uiSub, ok := jsonStringClaim(ui, "sub")
				if !ok || uiSub != claims.Subject {
					return UserIdentity{}, errors.New("upstream userinfo subject mismatch")
				}
				if email == "" {
					if e, ok := jsonStringClaim(ui, "email"); ok {
						email = strings.TrimSpace(e)
						emailVerified = jsonBoolClaim(ui, "email_verified")
					}
				}
				if name == "" {
					if n, ok := jsonStringClaim(ui, "name"); ok {
						name = strings.TrimSpace(n)
					}
				}
			}
		}
	}

	var subject string
	if email != "" && emailVerified {
		subject, err = principal.SubjectForEmail(email)
		if err != nil {
			return UserIdentity{}, err
		}
	} else {
		subject = "oidc:" + server.config.UpstreamOIDC.Issuer + ":" + claims.Subject
		// Never publish an unverified email claim into the minted Kodama JWT.
		email = ""
	}
	if name == "" {
		if email != "" {
			name = email
		} else {
			name = claims.Subject
		}
	}
	return UserIdentity{
		Subject: subject,
		Email:   email,
		Name:    name,
		Method:  "oidc",
	}, nil
}

func (server *Server) fetchUpstreamMetadata(ctx context.Context, now time.Time) (upstreamOIDCMetadata, error) {
	server.upstreamMu.Lock()
	if server.upstreamCache != nil && now.Before(server.upstreamCache.fetchedAt.Add(upstreamMetadataCacheTTL)) {
		cached := *server.upstreamCache
		server.upstreamMu.Unlock()
		return cached, nil
	}
	server.upstreamMu.Unlock()

	discoveryURL := strings.TrimRight(server.config.UpstreamOIDC.Issuer, "/") + "/.well-known/openid-configuration"
	body, status, err := server.outboundRequest(ctx, http.MethodGet, discoveryURL, nil, "")
	if err != nil || status < 200 || status >= 300 {
		return upstreamOIDCMetadata{}, errors.New("upstream OIDC discovery failed")
	}
	var meta upstreamOIDCMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return upstreamOIDCMetadata{}, errors.New("upstream OIDC discovery malformed")
	}
	if meta.Issuer != server.config.UpstreamOIDC.Issuer {
		return upstreamOIDCMetadata{}, errors.New("upstream OIDC issuer mismatch")
	}
	if err := validateEndpointURL("upstream authorization_endpoint", meta.AuthorizationEndpoint, true); err != nil {
		return upstreamOIDCMetadata{}, err
	}
	if err := validateEndpointURL("upstream token_endpoint", meta.TokenEndpoint, false); err != nil {
		return upstreamOIDCMetadata{}, err
	}
	if err := validateEndpointURL("upstream jwks_uri", meta.JWKSURI, true); err != nil {
		return upstreamOIDCMetadata{}, err
	}
	if meta.UserinfoEndpoint != "" {
		if err := validateEndpointURL("upstream userinfo_endpoint", meta.UserinfoEndpoint, false); err != nil {
			return upstreamOIDCMetadata{}, err
		}
	}
	meta.fetchedAt = now

	server.upstreamMu.Lock()
	server.upstreamCache = &meta
	server.upstreamMu.Unlock()
	return meta, nil
}

func (server *Server) outboundRequest(ctx context.Context, method, target string, form url.Values, bearerToken string) ([]byte, int, error) {
	var bodyReader io.Reader
	if form != nil {
		bodyReader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kodama-auth")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := server.config.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, defaultOutboundMaxBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(data) > defaultOutboundMaxBytes {
		return nil, resp.StatusCode, errors.New("upstream response exceeded maximum body bound")
	}
	return data, resp.StatusCode, nil
}
