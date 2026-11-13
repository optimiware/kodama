package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/principal"
)

// Server is one constructed OIDC Provider and Social Login Broker.
type Server struct {
	config        Config
	material      *signingMaterial
	store         *authStore
	handler       http.Handler
	secureCookies bool

	ghMu           sync.RWMutex
	ghClientID     string
	ghClientSecret string

	upstreamMu    sync.Mutex
	upstreamCache *upstreamOIDCMetadata
}

// New validates config, loads or creates signing state, and constructs a ready
// OIDC & Social Login Broker without opening any network listener.
func New(config Config) (*Server, error) {
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	material, err := loadOrCreateMaterial(normalized.StateDir, normalized.Random)
	if err != nil {
		return nil, err
	}

	ghID := normalized.GitHub.ClientID
	ghSecret := normalized.GitHub.ClientSecret
	if ghID == "" && normalized.StateDir != "" {
		if persisted, ok, err := loadPersistedGitHubCredentials(normalized.StateDir); err != nil {
			return nil, err
		} else if ok {
			ghID = persisted.ClientID
			ghSecret = persisted.ClientSecret
		}
	}

	hasLocal := len(normalized.LocalPersonas) > 0
	hasGitHub := ghID != "" && ghSecret != ""
	hasGitHubSetup := normalized.GitHub.AllowSetup
	hasUpstream := normalized.UpstreamOIDC.Issuer != ""
	if !hasLocal && !hasGitHub && !hasGitHubSetup && !hasUpstream {
		return nil, fault.New(fault.InvalidInput,
			"auth server requires at least one sign-in method (--local-personas, GitHub OAuth, --allow-github-setup or upstream OIDC)")
	}

	srv := &Server{
		config:         normalized,
		material:       material,
		store:          newAuthStore(),
		secureCookies:  strings.HasPrefix(strings.ToLower(normalized.Issuer), "https://"),
		ghClientID:     ghID,
		ghClientSecret: ghSecret,
	}
	srv.handler = srv.buildHandler()
	return srv, nil
}

// Handler returns the HTTP handler serving all OIDC, OAuth 2.1 and social
// broker endpoints.
func (server *Server) Handler() http.Handler {
	return server.handler
}

// Issuer returns the configured external OIDC issuer URL.
func (server *Server) Issuer() string {
	return server.config.Issuer
}

// KeyID returns the deterministic JWK kid of the active ES256 signing key.
func (server *Server) KeyID() string {
	return server.material.kid
}

// Serve serves HTTP requests on listener until ctx is canceled or the server
// encounters a fatal error, performing a bounded graceful shutdown on cancellation.
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if listener == nil {
		return fault.New(fault.InvalidInput, "auth server requires a non-nil listener")
	}
	httpServer := &http.Server{
		Handler:           server.handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGracePeriod)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return <-errCh
	}
}

// Run constructs a Server from config, binds ListenAddress (wrapping TLS when
// TLSCertFile and TLSKeyFile are provided), and serves until ctx is canceled.
func Run(ctx context.Context, config Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	server, err := New(config)
	if err != nil {
		return err
	}

	var tlsConfig *tls.Config
	if server.config.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(server.config.TLSCertFile, server.config.TLSKeyFile)
		if err != nil {
			return fault.Wrap(fault.InvalidInput, "load TLS certificate and key", err)
		}
		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
	}

	listener := server.config.Listener
	if listener == nil {
		opened, err := net.Listen("tcp", server.config.ListenAddress)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "listen on auth address", err)
		}
		listener = opened
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	return server.Serve(ctx, listener)
}

func (server *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/.well-known/openid-configuration", server.handleDiscovery)
	mux.HandleFunc("/.well-known/oauth-authorization-server", server.handleDiscovery)
	mux.HandleFunc("/jwks.json", server.handleJWKS)
	mux.HandleFunc("/authorize", server.handleAuthorize)
	mux.HandleFunc("/token", server.handleToken)
	mux.HandleFunc("/device", server.handleDevice)
	mux.HandleFunc("/device/verify", server.handleDeviceVerify)
	mux.HandleFunc("/revoke", server.handleRevoke)
	mux.HandleFunc("/register", server.handleRegister)
	mux.HandleFunc("/whoami", server.handleWhoAmI)
	mux.HandleFunc("/userinfo", server.handleUserInfo)
	mux.HandleFunc("/oauth/github/start", server.handleGitHubStart)
	mux.HandleFunc("/oauth/github/callback", server.handleGitHubCallback)
	mux.HandleFunc("/setup/github", server.handleGitHubSetup)
	mux.HandleFunc("/setup/github/callback", server.handleGitHubSetupCallback)
	mux.HandleFunc("/oauth/oidc/start", server.handleUpstreamOIDCStart)
	mux.HandleFunc("/oauth/oidc/callback", server.handleUpstreamOIDCCallback)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action *")
		mux.ServeHTTP(w, r)
	})
}

func (server *Server) endpointURL(path string) string {
	return strings.TrimRight(server.config.Issuer, "/") + path
}

func (server *Server) githubConfigured() bool {
	id, secret := server.githubCredentials()
	return id != "" && secret != ""
}

func (server *Server) githubCredentials() (string, string) {
	server.ghMu.RLock()
	defer server.ghMu.RUnlock()
	return server.ghClientID, server.ghClientSecret
}

func (server *Server) installGitHubCredentials(clientID, clientSecret string) error {
	server.ghMu.Lock()
	defer server.ghMu.Unlock()
	if server.ghClientID != "" && server.ghClientSecret != "" {
		return fault.New(fault.VersionConflict, "GitHub OAuth credentials are already configured")
	}
	if err := savePersistedGitHubCredentials(server.config.StateDir, persistedGitHubCredentials{
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}); err != nil {
		return err
	}
	server.ghClientID = clientID
	server.ghClientSecret = clientSecret
	return nil
}

func (server *Server) upstreamOIDCConfigured() bool {
	return server.config.UpstreamOIDC.Issuer != "" &&
		server.config.UpstreamOIDC.ClientID != "" &&
		server.config.UpstreamOIDC.ClientSecret != ""
}

func (server *Server) verifyAccessCode(submitted string) bool {
	if server.config.AccessCode == "" {
		return true
	}
	return constantTimeEqualStrings(submitted, server.config.AccessCode)
}

func (server *Server) isBuiltInClient(clientID string) bool {
	if clientID == server.config.ClientID {
		return true
	}
	return slices.Contains(server.config.AdditionalClientIDs, clientID)
}

func (server *Server) isKnownClient(clientID string) bool {
	if server.isBuiltInClient(clientID) {
		return true
	}
	_, ok := server.store.lookupRegisteredClient(clientID)
	return ok
}

func (server *Server) isAllowedRedirect(clientID, redirectURI string) bool {
	if err := validateRedirectURI(redirectURI); err != nil {
		return false
	}
	if server.isBuiltInClient(clientID) {
		if isDynamicLoopbackCallback(redirectURI) {
			return true
		}
		return slices.Contains(server.config.RedirectURIs, redirectURI)
	}
	reg, ok := server.store.lookupRegisteredClient(clientID)
	if !ok {
		return false
	}
	if !slices.Contains(reg.GrantTypes, "authorization_code") {
		return false
	}
	for _, allowed := range reg.RedirectURIs {
		if matchesRegisteredRedirectURI(allowed, redirectURI) {
			return true
		}
	}
	return false
}

func (server *Server) personaOptions(selectedHint string) []personaOption {
	options := make([]personaOption, 0, len(server.config.LocalPersonas))
	matched := false
	for _, name := range server.config.LocalPersonas {
		pid, _ := principal.ForOIDC(server.config.Issuer, name)
		sel := selectedHint != "" && name == selectedHint
		if sel {
			matched = true
		}
		options = append(options, personaOption{
			Name:        name,
			PrincipalID: string(pid),
			Selected:    sel,
		})
	}
	if !matched && len(options) > 0 {
		options[0].Selected = true
	}
	return options
}
