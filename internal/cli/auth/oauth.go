package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// randomToken returns 32 bytes of randomness as unpadded base64url (43
// characters, the RFC 7636 minimum entropy for a verifier and ample for state).
func randomToken(r io.Reader) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// validateEndpoints requires HTTPS provider endpoints. Plain http is accepted
// only for loopback hosts and only when the composition root injected the
// local-development exception.
func validateEndpoints(a root.AuthConfig, allowInsecureLoopback bool) error {
	for _, e := range []struct{ name, raw string }{
		{"authorization endpoint", a.AuthorizationEndpoint}, {"token endpoint", a.TokenEndpoint},
	} {
		u, err := url.Parse(e.raw)
		if err != nil || u.Host == "" {
			return root.Usagef("the %s is not a valid URL", e.name)
		}
		switch {
		case u.Scheme == "https":
		case u.Scheme == "http" && allowInsecureLoopback && client.IsLoopbackHost(u.Hostname()):
		default:
			return root.Usagef("the %s must use https", e.name)
		}
	}
	return nil
}

// oauthConfig is the public-client authorization-code configuration. The
// client ID travels in the request body: there is no client secret.
func oauthConfig(a root.AuthConfig, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: a.ClientID,
		Endpoint: oauth2.Endpoint{
			AuthURL: a.AuthorizationEndpoint, TokenURL: a.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: redirect,
		Scopes:      a.Scopes,
	}
}

// httpClientFor returns a copy of c that never follows redirects, so the
// authorization code and verifier cannot be forwarded to another destination.
func httpClientFor(c *http.Client) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	if cp.Timeout == 0 {
		cp.Timeout = 30 * time.Second
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

// Exchange outcomes.
var (
	// ErrExchangeRejected: the provider answered the code exchange with an error.
	ErrExchangeRejected = errors.New("the provider rejected the authorization code")
	// ErrExchangeUncertain: no definitive answer arrived. The single-use code
	// may or may not have been consumed; a new login attempt is required.
	ErrExchangeUncertain = errors.New("the code exchange outcome is unknown")
	// ErrBadToken: the provider's answer is not a usable bearer access token.
	ErrBadToken = errors.New("the provider returned no usable access token")
)

// exchange redeems the code once. It never retries. Only the access token is
// used: an ID token in the response is ignored and discarded.
func exchange(ctx context.Context, cfg *oauth2.Config, hc *http.Client, code, verifier string, now time.Time) (Credential, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 {
			return Credential{}, fmt.Errorf("%w: %w", ErrExchangeRejected, err)
		}
		return Credential{}, fmt.Errorf("%w: %w", ErrExchangeUncertain, err)
	}
	if tok.AccessToken == "" || strings.ContainsAny(tok.AccessToken, " \t\r\n\x00") {
		return Credential{}, ErrBadToken
	}
	typ := tok.Type()
	if typ != "Bearer" {
		return Credential{}, fmt.Errorf("%w: unsupported token type", ErrBadToken)
	}
	return Credential{AccessToken: tok.AccessToken, TokenType: typ, Expiry: tok.Expiry, ObtainedAt: now}, nil
}
