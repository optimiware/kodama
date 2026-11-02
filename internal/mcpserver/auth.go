package mcpserver

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/optimiware/kodama/internal/identity"
	identityoidc "github.com/optimiware/kodama/internal/identity/oidc"
)

// Authenticator is the MCP OAuth resource-server half. It verifies the bearer
// locally and records the raw token only so the exchange provider can obtain a
// separate delegated API credential. No token is stored, refreshed or minted
// here.
type Authenticator struct {
	verifier    *identityoidc.Verifier
	resource    string
	metadataURL string
	issuer      string
	scopes      []string
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
	scopes := append([]string(nil), cfg.Scopes...)
	if len(scopes) == 0 {
		scopes = []string{"openid"}
	}
	return &Authenticator{verifier: verifier, resource: cfg.Resource, metadataURL: metadataURL, issuer: cfg.Issuer, scopes: scopes}, nil
}

// Middleware verifies the incoming MCP token, records the verified caller and
// then serves the MCP handler. The raw token is available only to the exchange
// provider; it is never placed in tool arguments or results.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			a.challenge(w, "invalid_request")
			return
		}
		verified, err := a.verifier.VerifyAccessToken(r.Context(), token)
		switch {
		case err == nil:
		case errors.Is(err, identity.ErrUnauthenticated):
			a.challenge(w, "invalid_token")
			return
		default:
			http.Error(w, "authentication dependency unavailable", http.StatusServiceUnavailable)
			return
		}
		caller := Caller{
			SubjectToken: token, Principal: verified.Principal,
			Scopes: append([]string(nil), verified.Scopes...), Expiry: verified.Expiry,
		}
		next.ServeHTTP(w, r.WithContext(withCaller(r.Context(), caller)))
	})
}

func (a *Authenticator) challenge(w http.ResponseWriter, code string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer resource_metadata=%q, error=%q`, a.metadataURL, code,
	))
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (a *Authenticator) MetadataHandler() http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               a.resource,
		AuthorizationServers:   []string{a.issuer},
		ScopesSupported:        a.scopes,
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
