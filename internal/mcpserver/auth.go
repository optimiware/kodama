package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/optimiware/kodama/internal/identity"
	identityoidc "github.com/optimiware/kodama/internal/identity/oidc"
)

// Authenticator is the MCP OAuth resource-server half. It verifies the bearer
// locally and records the raw token only so the executor can forward the same
// credential to Kodama. No token is stored, refreshed or minted here.
type Authenticator struct {
	verifier    *identityoidc.Verifier
	resource    string
	metadataURL string
	issuer      string
}

func NewAuthenticator(cfg Config, ca []byte) (*Authenticator, error) {
	client, err := httpClient(cfg.HTTPTimeout, ca)
	if err != nil {
		return nil, err
	}
	verifier, err := identityoidc.NewVerifier(identityoidc.Config{
		Issuer: cfg.Issuer, Audience: cfg.Audience, AllowInsecureHTTP: cfg.AllowInsecureHTTP,
		HTTPClient: client,
	})
	if err != nil {
		return nil, fmt.Errorf("mcpserver: OIDC verifier: %w", err)
	}
	metadataURL, err := protectedResourceMetadataURL(cfg.Resource)
	if err != nil {
		return nil, err
	}
	return &Authenticator{verifier: verifier, resource: cfg.Resource, metadataURL: metadataURL, issuer: cfg.Issuer}, nil
}

func (a *Authenticator) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	verified, err := a.verifier.VerifyAccessToken(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, identity.ErrUnauthenticated):
		return nil, fmt.Errorf("%w: the token is not valid for this MCP server", auth.ErrInvalidToken)
	case errors.Is(err, identity.ErrUnavailable):
		return nil, err
	default:
		return nil, err
	}
	return &auth.TokenInfo{
		Scopes: verified.Scopes, Expiration: verified.Expiry, UserID: verified.Principal.Subject,
	}, nil
}

// Middleware records the raw bearer, verifies it, and then serves the MCP
// handler. The raw token is available only to this package's executor.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	authHandler := auth.RequireBearerToken(a.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: a.metadataURL,
		ClockSkew:           30 * time.Second,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := bearerToken(r); ok {
			r = r.WithContext(withRawToken(r.Context(), token))
		}
		authHandler(next).ServeHTTP(w, r)
	})
}

func (a *Authenticator) MetadataHandler() http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               a.resource,
		AuthorizationServers:   []string{a.issuer},
		ScopesSupported:        []string{"openid"},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Kodama MCP",
	})
}

// MetadataPaths are the discovery locations this server serves. The
// path-specific form is what the MCP SDK derives from the resource URL; the
// root form is kept for clients that probe it directly.
func (a *Authenticator) MetadataPaths() []string {
	u, _ := url.Parse(a.metadataURL)
	root := "/.well-known/oauth-protected-resource"
	if u.Path == root {
		return []string{root}
	}
	return []string{u.Path, root}
}

func protectedResourceMetadataURL(resource string) (string, error) {
	u, err := url.Parse(resource)
	if err != nil || u.Host == "" {
		return "", errors.New("mcpserver: resource must be an absolute URL")
	}
	path := strings.TrimSuffix(u.Path, "/")
	u.Path = "/.well-known/oauth-protected-resource" + path
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func bearerToken(r *http.Request) (string, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return "", false
	}
	return fields[1], true
}
