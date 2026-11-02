package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/identity"
	identityoidc "github.com/optimiware/kodama/internal/identity/oidc"
)

const (
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType    = "urn:ietf:params:oauth:token-type:access_token"
	maxExchangeBytes   = 1 << 20
)

// Caller is the verified request identity available to the executor. The raw
// subject token is retained only for the token exchange and is never exposed to
// tool arguments, results or logs.
type Caller struct {
	SubjectToken string
	Principal    identity.Principal
	Scopes       []string
	Expiry       time.Time
}

// UpstreamTokenProvider obtains the separate credential used to call Kodama.
// It must never return the MCP client's token.
type UpstreamTokenProvider interface {
	Token(ctx context.Context, caller Caller) (string, error)
}

// ExchangeProvider exchanges an MCP-bound access token for a delegated Kodama
// API access token using RFC 8693.
type ExchangeProvider struct {
	cfg      Config
	client   *http.Client
	secret   string
	verifier *identityoidc.Verifier
}

var _ UpstreamTokenProvider = (*ExchangeProvider)(nil)

// NewExchangeProvider builds the exchange client and the verifier used on the
// returned API token. The secret is read by the caller from its mounted file.
func NewExchangeProvider(cfg Config, client *http.Client, secret string, verifier *identityoidc.Verifier) (*ExchangeProvider, error) {
	if client == nil || verifier == nil || secret == "" {
		return nil, errors.New("mcpserver: exchange needs an HTTP client, verifier and client secret")
	}
	return &ExchangeProvider{cfg: cfg, client: client, secret: secret, verifier: verifier}, nil
}

// Token implements UpstreamTokenProvider.
func (p *ExchangeProvider) Token(ctx context.Context, caller Caller) (string, error) {
	if caller.SubjectToken == "" || caller.Principal.Subject == "" {
		return "", errors.New("mcpserver: token exchange has no verified subject")
	}
	form := url.Values{}
	form.Set("grant_type", tokenExchangeGrant)
	form.Set("subject_token", caller.SubjectToken)
	form.Set("subject_token_type", accessTokenType)
	form.Set("requested_token_type", accessTokenType)
	form.Set("resource", p.cfg.APIResource)
	form.Set("audience", p.cfg.APIAudience)
	if len(p.cfg.ExchangeScopes) > 0 {
		form.Set("scope", strings.Join(p.cfg.ExchangeScopes, " "))
	}
	auth := p.cfg.ExchangeClientAuth
	if auth == "" {
		auth = "client_secret_basic"
	}
	switch auth {
	case "client_secret_basic":
		// Set below from the form-independent header.
	case "client_secret_post":
		form.Set("client_id", p.cfg.ExchangeClientID)
		form.Set("client_secret", p.secret)
	default:
		return "", errors.New("mcpserver: unsupported exchange client authentication")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.ExchangeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("mcpserver: build token exchange request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if auth == "client_secret_basic" {
		req.SetBasicAuth(p.cfg.ExchangeClientID, p.secret)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcpserver: token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBytes+1))
	if err != nil || len(body) > maxExchangeBytes {
		return "", errors.New("mcpserver: token exchange returned an unreadable or oversized response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mcpserver: token exchange returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken     string `json:"access_token"`
		TokenType       string `json:"token_type"`
		IssuedTokenType string `json:"issued_token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", errors.New("mcpserver: token exchange returned malformed JSON")
	}
	if out.AccessToken == "" || len(out.AccessToken) > 8<<10 {
		return "", errors.New("mcpserver: token exchange returned no usable access token")
	}
	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "Bearer") {
		return "", errors.New("mcpserver: token exchange returned a non-bearer token")
	}
	if out.IssuedTokenType != "" && out.IssuedTokenType != accessTokenType {
		return "", errors.New("mcpserver: token exchange returned an unexpected token type")
	}
	if out.ExpiresIn < 0 {
		return "", errors.New("mcpserver: token exchange returned a negative lifetime")
	}
	verified, err := p.verifier.VerifyAccessToken(ctx, out.AccessToken)
	if err != nil {
		if errors.Is(err, identity.ErrUnauthenticated) {
			return "", errors.New("mcpserver: token exchange returned a token not valid for the Kodama API")
		}
		return "", fmt.Errorf("mcpserver: verify exchanged token: %w", err)
	}
	if verified.Principal.Subject != caller.Principal.Subject {
		return "", errors.New("mcpserver: token exchange changed the represented subject")
	}
	if out.AccessToken == caller.SubjectToken {
		return "", errors.New("mcpserver: token exchange returned the client token")
	}
	return out.AccessToken, nil
}

// ReadSecret reads a mounted secret file. It is separate from ReadCA so a
// missing exchange credential is a startup error, not an empty client secret.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token-exchange client secret: %w", err)
	}
	value := strings.TrimSpace(string(b))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("token-exchange client secret is empty or malformed")
	}
	return value, nil
}
