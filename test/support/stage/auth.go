package stage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ClientID and RedirectURI are the public client the issuer registers for the CLI and the suites.
	ClientID    = "kodama-cli"
	RedirectURI = "http://127.0.0.1:18999/callback"

	issuerNamespace = "kodama-deps"
	issuerSecret    = "kodama-oauthsonas-access"
)

// Personas are the identities the stage issuer serves (deploy/stage/shared/stack/oauthsonas.tf).
var Personas = []string{"admin", "analyst", "engineer", "reviewer", "viewer", "outsider", "foreign"}

// Subject is a persona's token subject.
func Subject(persona string) string { return "oauthsonas|" + persona }

// Auth signs personas in at the stage issuer (the oauthsonas persona fixture, O-27): the access-code gate, then
// the authorization-code flow with S256 PKCE. Tokens stay in memory and are renewed before they expire.
type Auth struct {
	kube   Kube
	issuer string

	mu     sync.Mutex
	code   string
	tokens map[string]cached
}

type cached struct {
	token  string
	expiry time.Time
}

// NewAuth returns the sign-in client of an issuer.
func NewAuth(k Kube, issuer string) *Auth {
	return &Auth{kube: k, issuer: strings.TrimRight(issuer, "/"), tokens: map[string]cached{}}
}

// Token returns an access token of the persona, valid for at least a minute.
func (a *Auth) Token(ctx context.Context, persona string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.tokens[persona]; ok && time.Until(c.expiry) > time.Minute {
		return c.token, nil
	}
	tok, err := a.login(ctx, persona)
	if err != nil {
		return "", err
	}
	exp, err := expiry(tok)
	if err != nil {
		return "", err
	}
	a.tokens[persona] = cached{tok, exp}
	return tok, nil
}

// Fresh signs the persona in again and returns an independently issued token.
func (a *Auth) Fresh(ctx context.Context, persona string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, persona)
	tok, err := a.login(ctx, persona)
	if err != nil {
		return "", err
	}
	exp, err := expiry(tok)
	if err != nil {
		return "", err
	}
	a.tokens[persona] = cached{tok, exp}
	return tok, nil
}

func (a *Auth) accessCode(ctx context.Context) (string, error) {
	if a.code != "" {
		return a.code, nil
	}
	c, err := a.kube.SecretValue(ctx, issuerNamespace, issuerSecret, "access-code")
	if err != nil {
		return "", fmt.Errorf("the issuer's access code: %w", err)
	}
	a.code = strings.TrimSpace(c)
	return a.code, nil
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *Auth) login(ctx context.Context, persona string) (string, error) {
	code, err := a.accessCode(ctx)
	if err != nil {
		return "", err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	var disc struct {
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	if err := getJSON(ctx, hc, a.issuer+"/.well-known/openid-configuration", &disc); err != nil {
		return "", fmt.Errorf("issuer discovery: %w", err)
	}

	verifier, state, nonce := random(32), "st"+random(12), "nc"+random(12)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {ClientID}, "response_type": {"code"}, "scope": {"openid profile email"}, "redirect_uri": {RedirectURI},
		"state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "login_hint": {persona},
	}
	authURL, err := url.Parse(disc.Authorization)
	if err != nil {
		return "", err
	}
	authURL.RawQuery = q.Encode()

	// The gate: a request without its cookie gets the access-code form; submitting the code sets the cookie.
	gate := a.issuer + "/oauth2/gate"
	resp, err := post(ctx, hc, gate, url.Values{"code": {code}, "return_to": {authURL.RequestURI()}})
	if err != nil {
		return "", fmt.Errorf("the issuer's access-code gate: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("the issuer's access-code gate answered HTTP %d", resp.StatusCode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err = hc.Do(req)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil || loc.Query().Get("code") == "" {
		return "", fmt.Errorf("authorize for persona %q answered HTTP %d without an authorization code", persona, resp.StatusCode)
	}
	if loc.Query().Get("state") != state {
		return "", errors.New("authorize returned a different state")
	}

	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {ClientID}, "redirect_uri": {RedirectURI}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier}}
	resp, err = post(ctx, hc, disc.Token, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("the token endpoint answered HTTP %d for persona %q", resp.StatusCode, persona)
	}
	return tok.AccessToken, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func post(ctx context.Context, hc *http.Client, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return hc.Do(req)
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// expiry reads the exp claim of a JWT without verifying it (the server verifies; this only schedules renewal).
func expiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("the access token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, err
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &c); err != nil || c.Exp == 0 {
		return time.Time{}, errors.New("the access token carries no expiry")
	}
	return time.Unix(c.Exp, 0), nil
}
