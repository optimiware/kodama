package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// DefaultOIDCMaxBodyBytes bounds one OAuth/OIDC response body. A body of
// exactly the bound is accepted; one byte more is refused.
const DefaultOIDCMaxBodyBytes = 1 << 20

// DefaultOIDCTimeout bounds one OAuth/OIDC request.
const DefaultOIDCTimeout = 30 * time.Second

// Bounds on a provider-supplied OAuth error. The provider controls these
// fields, so they are truncated and stripped before they reach a caller.
const (
	maxOAuthErrorCodeBytes        = 64
	maxOAuthErrorDescriptionBytes = 512
)

// errOIDCRedirectRefused is the CheckRedirect sentinel. A redirect is always a
// failure: an authorization endpoint must never relocate a token request.
var errOIDCRedirectRefused = errors.New("OIDC redirect refused")

// HTTPOIDCClient is the real OAuth/OIDC network adapter behind the
// consumer-owned OIDCClient port. It sends every request to exactly the
// endpoint the caller supplied, never follows a redirect, bounds every
// response, decodes strictly and requires https outside a literal numeric
// loopback fixture. It holds no tokens and no long-lived key cache.
type HTTPOIDCClient struct {
	// RoundTripper is the injected HTTP transport. A production caller passes
	// http.DefaultTransport (or a TLS-configured transport); tests pass a fake
	// or the httptest server's transport.
	RoundTripper http.RoundTripper
	// MaxBodyBytes bounds one response body. Zero (or a value above the
	// declared bound) means DefaultOIDCMaxBodyBytes.
	MaxBodyBytes int
	// Timeout bounds one request. Zero means DefaultOIDCTimeout.
	Timeout time.Duration
}

var _ OIDCClient = (*HTTPOIDCClient)(nil)

// Move the request body to the form body. Validate that the request names the
// exact endpoint and carries the RFC-defined members for its grant type.

// Discover reads the one unauthenticated Kodama discovery route. The document
// must be strictly decodable and its api_url must share the configured API
// URL's origin; a document bound to another API origin is refused.
func (client *HTTPOIDCClient) Discover(ctx context.Context, discoveryURL string) (api.DiscoveryDocument, error) {
	parsed, err := validateOIDCEndpoint(discoveryURL)
	if err != nil {
		return api.DiscoveryDocument{}, err
	}
	if !strings.HasSuffix(parsed.Path, api.DiscoveryPath) {
		return api.DiscoveryDocument{}, fault.New(fault.InvalidInput, "the discovery URL is not the fixed Kodama route")
	}
	apiURL := strings.TrimSuffix(discoveryURL, api.DiscoveryPath)
	if strings.TrimRight(apiURL, "/") == "" {
		return api.DiscoveryDocument{}, fault.New(fault.InvalidInput, "the discovery URL has no API origin")
	}
	if _, err := validateOIDCEndpoint(apiURL); err != nil {
		return api.DiscoveryDocument{}, err
	}
	data, err := client.execute(ctx, http.MethodGet, discoveryURL, nil, nil, oidcBodyLimit(client, api.MaxDiscoveryBytes))
	if err != nil {
		return api.DiscoveryDocument{}, err
	}
	document, err := api.DecodeDiscoveryDocument(data)
	if err != nil {
		return api.DiscoveryDocument{}, fault.Wrap(fault.DependencyUnavailable, "the discovery document is invalid", err)
	}
	if !api.SameOrigin(apiURL, document.APIURL) {
		return api.DiscoveryDocument{}, fault.New(fault.DependencyUnavailable,
			"the discovery api_url does not match the configured API origin")
	}
	return document, nil
}

// Token exchanges one authorization code, refresh token or device code at the
// exact endpoint the request names.
func (client *HTTPOIDCClient) Token(ctx context.Context, request TokenRequest) (TokenResponse, error) {
	form, secrets, err := tokenRequestForm(request)
	if err != nil {
		return TokenResponse{}, err
	}
	data, err := client.execute(ctx, http.MethodPost, request.Endpoint, form, secrets, oidcBodyLimit(client, 0))
	if err != nil {
		return TokenResponse{}, err
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    *int64 `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := decodeOIDCJSON(data, &payload, true); err != nil {
		return TokenResponse{}, fault.Wrap(fault.DependencyUnavailable, "the token response is invalid", err)
	}
	if payload.AccessToken == "" {
		return TokenResponse{}, fault.New(fault.DependencyUnavailable, "the token response is missing its access token")
	}
	expires, err := oidcSecondsDuration(payload.ExpiresIn)
	if err != nil {
		return TokenResponse{}, fault.Wrap(fault.DependencyUnavailable, "the token response has an invalid lifetime", err)
	}
	return TokenResponse{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		IDToken:      payload.IDToken,
		TokenType:    payload.TokenType,
		ExpiresIn:    expires,
	}, nil
}

// DeviceAuthorize issues one RFC 8628 device authorization request at the
// exact endpoint the request names.
func (client *HTTPOIDCClient) DeviceAuthorize(ctx context.Context, request DeviceAuthorizationRequest) (DeviceAuthorizationResponse, error) {
	if request.ClientID == "" {
		return DeviceAuthorizationResponse{}, fault.New(fault.InvalidInput, "the device authorization request has no client id")
	}
	for _, scope := range request.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return DeviceAuthorizationResponse{}, fault.New(fault.InvalidInput, "the device authorization request has an invalid scope")
		}
	}
	form := url.Values{"client_id": {request.ClientID}}
	if len(request.Scopes) > 0 {
		form.Set("scope", strings.Join(request.Scopes, " "))
	}
	data, err := client.execute(ctx, http.MethodPost, request.Endpoint, form, nil, oidcBodyLimit(client, 0))
	if err != nil {
		return DeviceAuthorizationResponse{}, err
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               *int64 `json:"expires_in"`
		Interval                *int64 `json:"interval"`
	}
	if err := decodeOIDCJSON(data, &payload, true); err != nil {
		return DeviceAuthorizationResponse{}, fault.Wrap(fault.DependencyUnavailable, "the device authorization response is invalid", err)
	}
	if payload.DeviceCode == "" || payload.UserCode == "" || payload.VerificationURI == "" {
		return DeviceAuthorizationResponse{}, fault.New(fault.DependencyUnavailable, "the device authorization response is incomplete")
	}
	if err := validateBrowserURL(payload.VerificationURI); err != nil {
		return DeviceAuthorizationResponse{}, fault.New(fault.DependencyUnavailable, "the device verification URI is invalid")
	}
	if payload.ExpiresIn == nil || *payload.ExpiresIn <= 0 {
		return DeviceAuthorizationResponse{}, fault.New(fault.DependencyUnavailable, "the device authorization response has no lifetime")
	}
	expires, err := oidcSecondsDuration(payload.ExpiresIn)
	if err != nil {
		return DeviceAuthorizationResponse{}, fault.Wrap(fault.DependencyUnavailable, "the device authorization lifetime is invalid", err)
	}
	interval, err := oidcSecondsDuration(payload.Interval)
	if err != nil {
		return DeviceAuthorizationResponse{}, fault.Wrap(fault.DependencyUnavailable, "the device authorization interval is invalid", err)
	}
	return DeviceAuthorizationResponse{
		DeviceCode:      payload.DeviceCode,
		UserCode:        payload.UserCode,
		VerificationURI: payload.VerificationURI,
		ExpiresIn:       expires,
		Interval:        interval,
	}, nil
}

// Revoke performs one RFC 7009 revocation at the exact endpoint the request
// names. A refresh-token revocation names its hint.
func (client *HTTPOIDCClient) Revoke(ctx context.Context, request RevocationRequest) error {
	if request.Token == "" || request.ClientID == "" {
		return fault.New(fault.InvalidInput, "the revocation request is incomplete")
	}
	if request.TokenTypeHint != "" && request.TokenTypeHint != "refresh_token" && request.TokenTypeHint != "access_token" {
		return fault.New(fault.InvalidInput, "the revocation request has an invalid token type hint")
	}
	form := url.Values{
		"token":     {request.Token},
		"client_id": {request.ClientID},
	}
	if request.TokenTypeHint != "" {
		form.Set("token_type_hint", request.TokenTypeHint)
	}
	_, err := client.execute(ctx, http.MethodPost, request.Endpoint, form, []string{request.Token}, oidcBodyLimit(client, 0))
	return err
}

// JWKS derives the issuer key set only from the issuer's own jwks_uri. The
// OpenID metadata and the key set are fetched from the issuer origin (a
// foreign jwks_uri is refused before any request), both responses are bounded,
// and the key set is returned to the caller without any retained cache, so a
// key never outlives the one login or verification attempt that fetched it.
func (client *HTTPOIDCClient) JWKS(ctx context.Context, issuer string) (json.RawMessage, error) {
	if _, err := validateOIDCEndpoint(issuer); err != nil {
		return nil, err
	}
	metadataURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	metadata, err := client.execute(ctx, http.MethodGet, metadataURL, nil, nil, oidcBodyLimit(client, 0))
	if err != nil {
		return nil, err
	}
	var configuration struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := decodeOIDCJSON(metadata, &configuration, false); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "the issuer metadata is invalid", err)
	}
	if configuration.JWKSURI == "" {
		return nil, fault.New(fault.DependencyUnavailable, "the issuer metadata has no jwks_uri")
	}
	if _, err := validateOIDCEndpoint(configuration.JWKSURI); err != nil {
		return nil, err
	}
	if !api.SameOrigin(issuer, configuration.JWKSURI) {
		return nil, fault.New(fault.DependencyUnavailable, "the jwks_uri does not share the issuer origin")
	}
	keys, err := client.execute(ctx, http.MethodGet, configuration.JWKSURI, nil, nil, oidcBodyLimit(client, 0))
	if err != nil {
		return nil, err
	}
	var keySet struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := decodeOIDCJSON(keys, &keySet, true); err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "the issuer key set is invalid", err)
	}
	if keySet.Keys == nil {
		return nil, fault.New(fault.DependencyUnavailable, "the issuer key set has no keys member")
	}
	for _, key := range keySet.Keys {
		trimmed := bytes.TrimSpace(key)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fault.New(fault.DependencyUnavailable, "the issuer key set contains a non-object key")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, fault.New(fault.DependencyUnavailable, "the issuer key set contains a non-object key")
		}
	}
	return json.RawMessage(append([]byte(nil), keys...)), nil
}

// execute sends one bounded request to exactly endpoint and maps a non-2xx
// response to either an OAuthError (safe, bounded fields) or a dependency
// failure. The request never follows a redirect and never renders a body.
func (client *HTTPOIDCClient) execute(ctx context.Context, method, endpoint string, form url.Values, secrets []string, maxBody int) ([]byte, error) {
	parsed, err := validateOIDCEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client.RoundTripper == nil {
		return nil, fault.New(fault.InternalFailure, "the OIDC client has no HTTP transport")
	}
	httpClient := &http.Client{
		Transport: client.RoundTripper,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errOIDCRedirectRefused
		},
	}
	var body io.Reader
	if form != nil {
		encoded := form.Encode()
		body = strings.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, fault.New(fault.InternalFailure, "the OIDC request could not be built")
	}
	request.Header.Set("Accept", "application/json")
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = DefaultOIDCTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request = request.WithContext(requestCtx)

	response, err := httpClient.Do(request)
	if err != nil {
		switch {
		case errors.Is(err, errOIDCRedirectRefused):
			return nil, fault.New(fault.DependencyUnavailable, "the OIDC endpoint attempted a redirect")
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case requestCtx.Err() != nil:
			return nil, fault.New(fault.DependencyUnavailable, "the OIDC request timed out")
		default:
			return nil, fault.New(fault.DependencyUnavailable, "the OIDC endpoint is unavailable")
		}
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, fault.New(fault.DependencyUnavailable, "the OIDC endpoint attempted a redirect")
	}
	if maxBody <= 0 {
		maxBody = oidcBodyLimit(client, 0)
	}
	data, readErr := readBoundedOIDC(response.Body, maxBody)
	if readErr != nil {
		return nil, fault.New(fault.DependencyUnavailable, "the OIDC response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, oidcErrorFromBody(data, response.StatusCode, secrets)
	}
	return data, nil
}

// tokenRequestForm validates one token exchange and builds its exact form.
func tokenRequestForm(request TokenRequest) (url.Values, []string, error) {
	if request.ClientID == "" {
		return nil, nil, fault.New(fault.InvalidInput, "the token request has no client id")
	}
	form := url.Values{
		"grant_type": {request.GrantType},
		"client_id":  {request.ClientID},
	}
	secrets := []string{request.Code, request.CodeVerifier, request.RefreshToken, request.DeviceCode}
	switch request.GrantType {
	case "authorization_code":
		if request.Code == "" || request.RedirectURI == "" || request.CodeVerifier == "" {
			return nil, nil, fault.New(fault.InvalidInput, "the authorization-code request is incomplete")
		}
		form.Set("code", request.Code)
		form.Set("redirect_uri", request.RedirectURI)
		form.Set("code_verifier", request.CodeVerifier)
	case "refresh_token":
		if request.RefreshToken == "" {
			return nil, nil, fault.New(fault.InvalidInput, "the refresh request has no refresh token")
		}
		form.Set("refresh_token", request.RefreshToken)
	case "urn:ietf:params:oauth:grant-type:device_code":
		if request.DeviceCode == "" {
			return nil, nil, fault.New(fault.InvalidInput, "the device request has no device code")
		}
		form.Set("device_code", request.DeviceCode)
	default:
		return nil, nil, fault.New(fault.InvalidInput, "the token request uses an unsupported grant type")
	}
	return form, secrets, nil
}

// validateOIDCEndpoint accepts only an absolute https URL, or a plain-http URL
// bound to the literal numeric loopback hosts 127.0.0.1 or ::1. User
// information, an opaque URL, a query and a fragment are refused so no secret
// can travel in a URL.
func validateOIDCEndpoint(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, fault.New(fault.InvalidInput, "the OIDC endpoint is empty")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n\\") {
		return nil, fault.New(fault.InvalidInput, "the OIDC endpoint is malformed")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "the OIDC endpoint is malformed")
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil {
		return nil, fault.New(fault.InvalidInput, "the OIDC endpoint must be an absolute URL without user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fault.New(fault.InvalidInput, "the OIDC endpoint must not carry a query or fragment")
	}
	if err := validateOIDCScheme(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// validateBrowserURL checks one user-facing verification URL. A query is
// allowed (verification_uri_complete), but the transport rule still applies.
func validateBrowserURL(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n\\") {
		return fault.New(fault.InvalidInput, "the verification URL is malformed")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return fault.New(fault.InvalidInput, "the verification URL is invalid")
	}
	return validateOIDCScheme(parsed)
}

// validateOIDCScheme applies the transport rule.
func validateOIDCScheme(parsed *url.URL) error {
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host != "127.0.0.1" && host != "::1" {
			return fault.New(fault.InvalidInput, "the OIDC endpoint must use https outside numeric loopback")
		}
		return nil
	default:
		return fault.New(fault.InvalidInput, "the OIDC endpoint scheme is not supported")
	}
}

// decodeOIDCJSON strictly decodes one JSON object and refuses trailing data. A
// strict decode also refuses every unknown member.
func decodeOIDCJSON(data []byte, target any, strict bool) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("the response is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("the response carries trailing data")
	}
	return nil
}

// readBoundedOIDC reads at most max bytes and refuses an oversized body.
func readBoundedOIDC(reader io.Reader, max int) ([]byte, error) {
	if max <= 0 {
		max = DefaultOIDCMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errors.New("the response is too large")
	}
	return data, nil
}

// oidcBodyLimit returns the smaller of the configured bound and one explicit
// caller bound. A configured value above the declared bound is clamped.
func oidcBodyLimit(client *HTTPOIDCClient, limit int) int {
	bound := client.MaxBodyBytes
	if bound <= 0 || bound > DefaultOIDCMaxBodyBytes {
		bound = DefaultOIDCMaxBodyBytes
	}
	if limit > 0 && limit < bound {
		bound = limit
	}
	return bound
}

// oidcSecondsDuration converts one OAuth lifetime in seconds.
func oidcSecondsDuration(seconds *int64) (time.Duration, error) {
	if seconds == nil {
		return 0, nil
	}
	if *seconds < 0 {
		return 0, errors.New("a lifetime is negative")
	}
	if *seconds > int64(maxDuration/time.Second) {
		return 0, errors.New("a lifetime is too large")
	}
	return time.Duration(*seconds) * time.Second, nil
}

// oidcErrorFromBody maps one provider error body to a bounded OAuthError. The
// code and description are sanitized, truncated and stripped of any secret the
// request carried, so a provider cannot replay a token into a diagnostic.
func oidcErrorFromBody(data []byte, status int, secrets []string) error {
	var payload struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &payload); err == nil && strings.TrimSpace(payload.Error) != "" {
		code := sanitizeOAuthCode(payload.Error)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(payload.Error, secret) {
				code = "oauth_error"
				break
			}
		}
		return &OAuthError{
			Code:        code,
			Description: sanitizeOAuthDescription(payload.Description, secrets),
		}
	}
	return fault.New(fault.DependencyUnavailable, fmt.Sprintf("the OIDC endpoint returned status %d", status))
}

// sanitizeOAuthCode keeps only a short identifier-shaped OAuth error code.
func sanitizeOAuthCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "oauth_error"
	}
	var builder strings.Builder
	for _, character := range raw {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-', character == '.':
			if builder.Len() >= maxOAuthErrorCodeBytes {
				return "oauth_error"
			}
			builder.WriteRune(character)
		default:
			return "oauth_error"
		}
	}
	if builder.Len() == 0 {
		return "oauth_error"
	}
	return builder.String()
}

// sanitizeOAuthDescription bounds and redacts one provider description.
func sanitizeOAuthDescription(raw string, secrets []string) string {
	cleaned := strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, raw)
	for _, secret := range secrets {
		if secret != "" {
			cleaned = strings.ReplaceAll(cleaned, secret, "<redacted>")
		}
	}
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return truncateUTF8(cleaned, maxOAuthErrorDescriptionBytes)
}

// truncateUTF8 cuts a string at a rune boundary.
func truncateUTF8(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
