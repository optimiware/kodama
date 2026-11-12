package cli

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// defaultLoginTimeout bounds a browser callback wait when the caller omits one.
const defaultLoginTimeout = 5 * time.Minute

// cleanupTimeout bounds a detached persistence or deletion. Cleanup must still
// complete after the caller's context dies, but it must not hang forever.
const cleanupTimeout = 15 * time.Second

// cleanupContext detaches a security-relevant persistence or deletion from the
// caller's cancellation and bounds it.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// persistSessionRecord writes one record after a token was obtained, even if
// the caller's context died meanwhile.
func persistSessionRecord(ctx context.Context, store SessionRecordStore, record SessionRecord) error {
	writeCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return store.SaveRecord(writeCtx, record)
}

// deleteSessionRecord removes one stale record even if the caller's context
// died; it is the ambiguous-rotation and logout cleanup path.
func deleteSessionRecord(ctx context.Context, store SessionRecordStore) error {
	deleteCtx, cancel := cleanupContext(ctx)
	defer cancel()
	return store.DeleteRecord(deleteCtx)
}

// Login runs exactly one login flow over the injected ports. It discovers the
// API-bound OAuth configuration, selects the browser authorization-code + PKCE
// flow or the device authorization flow, verifies the result and persists one
// version 2 session. No token, code, verifier or nonce reaches an error or an
// output writer.
func Login(ctx context.Context, deps LoginDeps) (SessionRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SessionRecord{}, err
	}
	apiURL, err := normalizeAPIURL(deps.APIURL)
	if err != nil {
		return SessionRecord{}, err
	}
	if deps.OIDC == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no OIDC client is configured")
	}
	if deps.Store == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no session store is configured")
	}
	if deps.Lock == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no session lock is configured")
	}
	clock := deps.Clock
	if clock == nil {
		clock = systemClock{}
	}
	random := deps.Random
	if random == nil {
		random = cryptorand.Reader
	}
	if deps.Out == nil {
		deps.Out = io.Discard
	}
	if deps.Err == nil {
		deps.Err = io.Discard
	}

	document, err := deps.OIDC.Discover(ctx, apiURL+api.DiscoveryPath)
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the login configuration is unavailable", err)
	}
	if err := document.Validate(); err != nil {
		return SessionRecord{}, fault.New(fault.DependencyUnavailable, "the login configuration is invalid")
	}
	if document.APIURL != apiURL {
		return SessionRecord{}, fault.New(fault.DependencyUnavailable, "the login configuration is bound to another API URL")
	}

	switch deps.Mode {
	case LoginBrowser:
		return loginBrowser(ctx, deps, document, apiURL, clock, random)
	case LoginDevice:
		return loginDevice(ctx, deps, document, apiURL, clock)
	case LoginAuto:
		return SessionRecord{}, fault.New(fault.InvalidInput, "a concrete login mode is required")
	default:
		return SessionRecord{}, fault.New(fault.InvalidInput, "unknown login mode")
	}
}

// loginBrowser runs the authorization-code + PKCE (S256) flow.
func loginBrowser(ctx context.Context, deps LoginDeps, document api.DiscoveryDocument, apiURL string, clock Clock, random Random) (SessionRecord, error) {
	if deps.Browser == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no browser opener is configured")
	}
	if deps.Loopback == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "no loopback listener is configured")
	}
	verifier, err := NewPKCEVerifier(random)
	if err != nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "the login secrets could not be generated")
	}
	state, err := NewState(random)
	if err != nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "the login secrets could not be generated")
	}
	nonce, err := NewNonce(random)
	if err != nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "the login secrets could not be generated")
	}
	address, wait, closeListener, err := deps.Loopback.Listen(ctx)
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the loopback callback could not be bound", err)
	}
	if wait == nil || closeListener == nil {
		return SessionRecord{}, fault.New(fault.InternalFailure, "the loopback listener is incomplete")
	}
	defer func() {
		_ = closeListener()
	}()
	redirectURI, err := loopbackRedirectURI(address)
	if err != nil {
		return SessionRecord{}, err
	}
	authorizationURL, err := authorizationURL(document, redirectURI, PKCEChallenge(verifier), state, nonce)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := deps.Browser.Open(ctx, authorizationURL); err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the system browser could not be opened", err)
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	params, err := wait(waitCtx)
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.Unauthenticated, "the authorization response did not arrive", err)
	}
	if !equalSecret(params.State, state) {
		return SessionRecord{}, fault.New(fault.Unauthenticated, "the authorization response state did not match")
	}
	if params.Error != "" {
		return SessionRecord{}, fault.New(fault.Unauthenticated, "the authorization server refused the request")
	}
	if params.Code == "" {
		return SessionRecord{}, fault.New(fault.Unauthenticated, "the authorization response is missing its code")
	}
	response, err := deps.OIDC.Token(ctx, TokenRequest{
		Endpoint:     document.TokenEndpoint,
		GrantType:    "authorization_code",
		Code:         params.Code,
		RedirectURI:  redirectURI,
		ClientID:     document.ClientID,
		CodeVerifier: verifier,
	})
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the authorization code could not be exchanged", err)
	}
	keys, err := deps.OIDC.JWKS(ctx, document.Issuer)
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the issuer keys are unavailable", err)
	}
	// The ID token proves the authentication to THIS OAuth client, so its
	// audience must contain the client_id (OIDC Core 3.1.3.7), not the API
	// audience the access token binds. The persisted session record still
	// binds document.Audience (the API audience).
	expectations := IDTokenExpectations{
		Issuer:   document.Issuer,
		Audience: document.ClientID,
		Nonce:    nonce,
		Now:      clock.Now(),
	}
	if err := VerifyIDToken(response.IDToken, keys, expectations); err != nil {
		return SessionRecord{}, reauthenticationRequired("the identity token was refused")
	}
	record, err := recordFromToken(document, apiURL, response, clock.Now())
	if err != nil {
		return SessionRecord{}, err
	}
	if err := persistSessionRecord(ctx, deps.Store, record); err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the session could not be persisted", err)
	}
	return record, nil
}

// loginDevice runs the RFC 8628 device authorization flow with a bounded poll.
func loginDevice(ctx context.Context, deps LoginDeps, document api.DiscoveryDocument, apiURL string, clock Clock) (SessionRecord, error) {
	response, err := deps.OIDC.DeviceAuthorize(ctx, DeviceAuthorizationRequest{
		Endpoint: document.DeviceAuthorizationEndpoint,
		ClientID: document.ClientID,
		Scopes:   append([]string(nil), document.Scopes...),
	})
	if err != nil {
		return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the device authorization is unavailable", err)
	}
	if response.DeviceCode == "" || response.VerificationURI == "" {
		return SessionRecord{}, fault.New(fault.DependencyUnavailable, "the device authorization response is incomplete")
	}
	fmt.Fprintf(deps.Out, "To sign in, open %s and enter the code %s\n", response.VerificationURI, response.UserCode)

	lifetime := response.ExpiresIn
	if lifetime <= 0 || lifetime > maxDeviceLoginLifetime {
		lifetime = maxDeviceLoginLifetime
	}
	deadline := clock.Now().Add(lifetime)
	interval := response.Interval
	if interval <= 0 {
		interval = defaultDevicePollInterval
	}
	for {
		if err := ctx.Err(); err != nil {
			return SessionRecord{}, err
		}
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return SessionRecord{}, reauthenticationRequired("the device authorization expired")
		}
		delay := min(interval, remaining)
		if err := clock.Sleep(ctx, delay); err != nil {
			return SessionRecord{}, err
		}
		if !clock.Now().Before(deadline) {
			return SessionRecord{}, reauthenticationRequired("the device authorization expired")
		}
		token, tokenErr := deps.OIDC.Token(ctx, TokenRequest{
			Endpoint:   document.TokenEndpoint,
			GrantType:  "urn:ietf:params:oauth:grant-type:device_code",
			DeviceCode: response.DeviceCode,
			ClientID:   document.ClientID,
		})
		if tokenErr != nil {
			var oauthErr *OAuthError
			if !errors.As(tokenErr, &oauthErr) {
				return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the device token endpoint failed", tokenErr)
			}
			next, terminal, scheduleErr := NextPollInterval(interval, oauthErr.Code)
			if terminal || scheduleErr != nil {
				return SessionRecord{}, reauthenticationRequired("the device authorization was refused")
			}
			interval = next
			continue
		}
		record, err := recordFromToken(document, apiURL, token, clock.Now())
		if err != nil {
			return SessionRecord{}, err
		}
		if err := persistSessionRecord(ctx, deps.Store, record); err != nil {
			return SessionRecord{}, fault.Wrap(fault.DependencyUnavailable, "the session could not be persisted", err)
		}
		return record, nil
	}
}

// recordFromToken builds the version 2 session bound to the discovery document.
func recordFromToken(document api.DiscoveryDocument, apiURL string, response TokenResponse, now time.Time) (SessionRecord, error) {
	if response.AccessToken == "" {
		return SessionRecord{}, fault.New(fault.DependencyUnavailable, "the token response is missing its access token")
	}
	if response.ExpiresIn <= 0 {
		return SessionRecord{}, fault.New(fault.DependencyUnavailable, "the token response is missing its lifetime")
	}
	return SessionRecord{
		Version:            SessionFileVersion,
		Kind:               "session",
		Token:              response.AccessToken,
		ExpiresAt:          now.Add(response.ExpiresIn),
		RefreshToken:       response.RefreshToken,
		Issuer:             document.Issuer,
		ClientID:           document.ClientID,
		Audience:           document.Audience,
		APIURL:             apiURL,
		TokenEndpoint:      document.TokenEndpoint,
		RevocationEndpoint: document.RevocationEndpoint,
	}, nil
}

// authorizationURL builds the exact authorization request from the discovered
// endpoint and query members.
func authorizationURL(document api.DiscoveryDocument, redirectURI, challenge, state, nonce string) (string, error) {
	parsed, err := url.Parse(document.AuthorizationEndpoint)
	if err != nil || parsed.Host == "" {
		return "", fault.New(fault.DependencyUnavailable, "the authorization endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", document.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("scope", strings.Join(document.Scopes, " "))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// loopbackRedirectURI validates the bound callback address and builds the
// redirect URI. Only the literal numeric loopback host is accepted.
func loopbackRedirectURI(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fault.New(fault.InternalFailure, "the loopback callback address is invalid")
	}
	if host != "127.0.0.1" {
		return "", fault.New(fault.InternalFailure, "the loopback callback is not bound to the numeric loopback host")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return "", fault.New(fault.InternalFailure, "the loopback callback port is invalid")
	}
	return "http://" + net.JoinHostPort(host, port) + OAuthCallbackPath, nil
}

// normalizeAPIURL validates the configured API target and removes one trailing
// slash so the discovery path joins exactly once.
func normalizeAPIURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", fault.New(fault.InvalidInput, "the API URL is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil {
		return "", fault.New(fault.InvalidInput, "the API URL is invalid")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fault.New(fault.InvalidInput, "the API URL must not carry a query or fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
	case "http":
		if parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
			return "", fault.New(fault.InvalidInput, "the API URL must use https outside numeric loopback")
		}
	default:
		return "", fault.New(fault.InvalidInput, "the API URL scheme is not supported")
	}
	return trimmed, nil
}

// equalSecret compares two secrets in constant time.
func equalSecret(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// reauthenticationRequired returns a stable refusal carrying the sentinel.
func reauthenticationRequired(message string) error {
	return fault.Wrap(fault.Unauthenticated, message, ErrReauthenticationRequired)
}

// systemClock is the production clock and cancellable sleep.
type systemClock struct{}

// Now returns the wall clock.
func (systemClock) Now() time.Time { return time.Now() }

// Sleep waits for duration or the context, whichever comes first.
func (systemClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
