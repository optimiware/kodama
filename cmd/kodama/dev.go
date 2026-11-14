package main

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	identitystore "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/userland/up"
)

type devReadyInfo struct {
	ListenAddr     string
	PublicURL      string
	StateDir       string
	CACertPath     string
	AdminTokenPath string
	EnvFilePath    string
}

type devDeps struct {
	listen  func(string, string) (net.Listener, error)
	homeDir func() (string, error)
	onReady func(devReadyInfo)
}

func defaultDevDeps() devDeps {
	return devDeps{
		listen:  net.Listen,
		homeDir: os.UserHomeDir,
	}
}

// runDevEntry is the production entrypoint for `kodama dev`.
func runDevEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runDev(ctx, args, stdout, stderr, defaultDevDeps())
}

func runDev(ctx context.Context, args []string, stdout, stderr io.Writer, deps devDeps) int {
	if deps.listen == nil {
		deps.listen = net.Listen
	}
	if deps.homeDir == nil {
		deps.homeDir = os.UserHomeDir
	}

	home, _ := deps.homeDir()
	defaultStateDir := ""
	if strings.TrimSpace(home) != "" {
		defaultStateDir = filepath.Join(home, ".kodama", "dev")
	}

	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", envOr("KODAMA_DEV_LISTEN", "127.0.0.1:8443"), "single-origin HTTPS listen address")
	stateDirFlag := flags.String("state-dir", envOr("KODAMA_DEV_STATE_DIR", defaultStateDir), "local development state directory")
	tenant := flags.String("tenant", envOr("KODAMA_DEV_TENANT", "tenant-local"), "pre-bootstrapped tenant identity")
	project := flags.String("project", envOr("KODAMA_DEV_PROJECT", "project-local"), "pre-bootstrapped project identity")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}

	tenantID, err := id.ParseTenantID(strings.TrimSpace(*tenant))
	if err != nil {
		usage(stderr, fmt.Sprintf("invalid --tenant: %v", err))
		return 2
	}
	projectID, err := id.ParseProjectID(strings.TrimSpace(*project))
	if err != nil {
		usage(stderr, fmt.Sprintf("invalid --project: %v", err))
		return 2
	}

	stateDir, err := resolveDevStateDir(*stateDirFlag, home)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	listenBind, _, err := up.PublicOriginFromListen(*listen)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}

	if err := os.MkdirAll(filepath.Join(stateDir, "data"), 0o700); err != nil {
		fmt.Fprintf(stderr, "kodama: dev state directory: %v\n", err)
		return 1
	}
	_ = os.Chmod(stateDir, 0o700)

	listener, err := deps.listen("tcp", listenBind)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: listen %q: %v\n", listenBind, err)
		return 1
	}
	actualAddr := listener.Addr().String()
	_, publicURL, err := up.PublicOriginFromListen(actualAddr)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	tlsBundle, err := up.EnsureTLS(up.TLSConfig{
		Dir:   filepath.Join(stateDir, "tls"),
		Hosts: []string{actualAddr, *listen},
	})
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: dev TLS setup failed: %v\n", err)
		return 1
	}
	tlsCfg, err := tlsBundle.ServerTLSConfig()
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: dev TLS configuration failed: %v\n", err)
		return 1
	}
	caPool, err := tlsBundle.CertPool()
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: dev CA pool failed: %v\n", err)
		return 1
	}

	issuerURL := publicURL + "/auth"
	devIssuer, err := up.NewDevIssuer(up.DevIssuerConfig{
		Issuer:         issuerURL,
		Audience:       publicURL,
		APIURL:         publicURL,
		ClientID:       "kodactl",
		KeyPath:        filepath.Join(stateDir, "oidc-signing.key"),
		TenantID:       string(tenantID),
		ProjectID:      string(projectID),
		AccessTokenTTL: 24 * time.Hour,
		// Project operations (tasks, pipelines, the inbox) require the actor's
		// scope to equal the project scope, so the admin token names the dev
		// project too.
		IncludeProjectOnAdmin: true,
	})
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: dev OIDC issuer failed: %v\n", err)
		return 1
	}

	adminToken, err := devIssuer.MintAccessToken("admin", 24*time.Hour)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: mint dev admin token failed: %v\n", err)
		return 1
	}

	tenantAdminToken, err := devIssuer.MintTenantAdminToken(24 * time.Hour)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: mint dev tenant admin token failed: %v\n", err)
		return 1
	}

	adminPrincipal, err := shelloidc.PrincipalIDFor(issuerURL, "admin")
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	clock := bridges.NewSystemClock()
	ids := bridges.NewSecureIDs()
	memStore := memory.NewWithClock(clock.Now)

	identityAdapter, err := identitystore.New(identitystore.Config{
		Executor: memStore,
		Scanner:  memStore,
		Clock:    clock,
		IDs:      ids,
	})
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	oidcTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    caPool,
			MinVersion: tls.VersionTLS12,
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	defer oidcTransport.CloseIdleConnections()

	oidcVerifier, err := shelloidc.NewVerifier(shelloidc.Config{
		OIDCConfig: identity.OIDCConfig{
			Issuer:    issuerURL,
			Audience:  publicURL,
			ClockSkew: time.Minute,
		},
		JWKSURL:       issuerURL + "/jwks.json",
		Algorithms:    []string{shelloidc.AlgorithmES256},
		HTTPClient:    &http.Client{Transport: oidcTransport, Timeout: 5 * time.Second},
		MaxJWKSBytes:  1 << 20,
		MaxTokenBytes: 1 << 20,
		CacheTTL:      10 * time.Minute,
		TenantClaim:   "org_id",
		ProjectClaim:  "project_id",
	})
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	staticBootstrapToken, err := randomHexString(24)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	allRoles := []tenancy.Role{
		tenancy.RoleAdmin,
		tenancy.RoleEngineer,
		tenancy.RoleReviewer,
		tenancy.RoleOperator,
	}
	devTokenEntry := shellapp.DevToken{
		Credential: staticBootstrapToken,
		Principal: identity.Principal{
			ID:      adminPrincipal,
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: tenantID},
			Roles:   allRoles,
			Enabled: true,
			Version: 1,
		},
		ExpiresAt: clock.Now().Add(24 * time.Hour),
	}
	staticAuth, err := shellapp.NewDevAuthenticator(clock, []shellapp.DevToken{devTokenEntry})
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	principalResolver := &devOIDCPrincipalResolver{
		verifier:   oidcVerifier,
		static:     staticAuth,
		principals: identityAdapter,
		now:        clock.Now,
	}
	storeValidity, err := readers.NewCredentialValidity(memStore)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	validity := &devHumanCredentialValidity{
		clock: clock,
		store: storeValidity,
	}

	disc := devIssuer.DiscoveryDocument()
	appCfg := shellapp.Config{
		Profile: shellapp.ProfileDevelopment,
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   publicURL,
		},
		InstallationID:       "installation-dev-local",
		DataDir:              filepath.Join(stateDir, "data"),
		DefaultActionTimeout: 5 * time.Minute,
		LoopInterval:         50 * time.Millisecond,
		MCPPath:              shellapp.DefaultMCPPath,
		Discovery: &shellapp.DiscoveryConfig{
			Issuer:                      disc.Issuer,
			ClientID:                    disc.ClientID,
			Audience:                    disc.Audience,
			AuthorizationEndpoint:       disc.AuthorizationEndpoint,
			TokenEndpoint:               disc.TokenEndpoint,
			DeviceAuthorizationEndpoint: disc.DeviceAuthorizationEndpoint,
			RevocationEndpoint:          disc.RevocationEndpoint,
			Scopes:                      append([]string(nil), disc.Scopes...),
			APIURL:                      disc.APIURL,
		},
		Adapters: &shellapp.Adapters{
			Store:              memStore,
			Scanner:            memStore,
			Controller:         memStore.Controller(),
			Outbox:             memStore,
			Credentials:        memStore,
			Clock:              clock,
			IDs:                ids,
			Principal:          principalResolver,
			CredentialValidity: validity,
		},
		Dev: &shellapp.DevelopmentConfig{
			Tokens: []shellapp.DevToken{devTokenEntry},
			Bootstrap: &shellapp.Bootstrap{
				TenantID:       tenantID,
				TenantName:     string(tenantID),
				ProjectID:      projectID,
				ProjectName:    string(projectID),
				AdminPrincipal: adminPrincipal,
				AdminRoles: []string{
					string(tenancy.RoleAdmin),
					string(tenancy.RoleEngineer),
					string(tenancy.RoleReviewer),
					string(tenancy.RoleOperator),
				},
			},
		},
	}

	app, err := shellapp.New(ctx, appCfg)
	if err != nil {
		_ = listener.Close()
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	if err := seedDevProjectPersonas(ctx, memStore, ids, clock.Now(), issuerURL, tenantID, projectID); err != nil {
		_ = listener.Close()
		_ = app.Close(context.Background())
		fmt.Fprintf(stderr, "kodama: seed dev personas: %v\n", err)
		return 1
	}

	adminTokenPath := filepath.Join(stateDir, "admin-token")
	envFilePath := filepath.Join(stateDir, "env.sh")
	if err := up.WriteEnvFiles(up.EnvFileSpec{
		EnvFilePath:    envFilePath,
		AdminTokenPath: adminTokenPath,
		PublicURL:      publicURL,
		CACertPath:     tlsBundle.CACertPath,
		AdminToken:     adminToken,
		TenantID:       string(tenantID),
		ProjectID:      string(projectID),

		TenantAdminTokenPath: filepath.Join(stateDir, "tenant-admin-token"),
		TenantAdminToken:     tenantAdminToken,
	}); err != nil {
		_ = listener.Close()
		_ = app.Close(context.Background())
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}

	appHandler := app.Handler()
	singleOrigin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL != nil && (r.URL.Path == "/auth" || strings.HasPrefix(r.URL.Path, "/auth/")) {
			devIssuer.ServeHTTP(w, r)
			return
		}
		appHandler.ServeHTTP(w, r)
	})

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	appDone := make(chan error, 1)
	go func() {
		appDone <- app.Run(runCtx)
	}()

	httpServer := &http.Server{
		Handler:           singleOrigin,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveDone := make(chan error, 1)
	tlsListener := tls.NewListener(listener, tlsCfg)
	go func() {
		serveDone <- httpServer.Serve(tlsListener)
	}()

	printDevBanner(stdout, publicURL, stateDir, tlsBundle.CACertPath, envFilePath, string(tenantID), string(projectID), home)
	if deps.onReady != nil {
		deps.onReady(devReadyInfo{
			ListenAddr:     actualAddr,
			PublicURL:      publicURL,
			StateDir:       stateDir,
			CACertPath:     tlsBundle.CACertPath,
			AdminTokenPath: adminTokenPath,
			EnvFilePath:    envFilePath,
		})
	}

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
		stopRun()
		_ = app.Close(shutCtx)
		<-serveDone
		<-appDone
		return 0
	case serveErr := <-serveDone:
		stopRun()
		_ = app.Close(context.Background())
		<-appDone
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "kodama: %v\n", serveErr)
			return 1
		}
		return 0
	}
}

func printDevBanner(stdout io.Writer, publicURL, stateDir, caPath, envPath, tenant, project, home string) {
	displayEnv := envPath
	if home != "" && strings.HasPrefix(envPath, home+string(filepath.Separator)) {
		displayEnv = "~" + strings.TrimPrefix(envPath, home)
	}
	fmt.Fprintf(stdout, "Kodama dev server ready (zero containers, single HTTPS origin)\n")
	fmt.Fprintf(stdout, "  URL:          %s\n", publicURL)
	fmt.Fprintf(stdout, "  MCP endpoint: %s%s\n", publicURL, shellapp.DefaultMCPPath)
	fmt.Fprintf(stdout, "  OIDC issuer:  %s/auth\n", publicURL)
	fmt.Fprintf(stdout, "  Local CA:     %s\n", caPath)
	fmt.Fprintf(stdout, "  Scope:        tenant=%s project=%s\n\n", tenant, project)
	fmt.Fprintf(stdout, "Quickstart:\n")
	fmt.Fprintf(stdout, "  source %s\n", displayEnv)
	fmt.Fprintf(stdout, "  kodactl --server %s login\n", publicURL)
	fmt.Fprintf(stdout, "\nMembership administration uses the tenant-scoped credential:\n")
	fmt.Fprintf(stdout, "  kodactl --credential-file \"$KODAMA_TENANT_ADMIN_CREDENTIAL_FILE\" member list\n")
}

func resolveDevStateDir(raw, home string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("dev requires --state-dir (or HOME to be set)")
	}
	if trimmed == "~" && home != "" {
		trimmed = home
	} else if after, ok := strings.CutPrefix(trimmed, "~/"); ok && home != "" {
		trimmed = filepath.Join(home, after)
	}
	if !filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("--state-dir %q must be an absolute path", raw)
	}
	cleaned := filepath.Clean(trimmed)
	if cleaned == "/" {
		return "", errors.New("refusing / as --state-dir")
	}
	if home != "" {
		cleanHome := filepath.Clean(home)
		if cleaned == cleanHome || cleaned == filepath.Dir(cleanHome) {
			return "", fmt.Errorf("refusing %q as --state-dir", raw)
		}
	}
	return cleaned, nil
}

func seedDevProjectPersonas(
	ctx context.Context,
	memStore *memory.Store,
	ids *bridges.IDs,
	now time.Time,
	issuerURL string,
	tenantID id.TenantID,
	projectID id.ProjectID,
) error {
	projectScope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	personas := []struct {
		subject string
		roles   []tenancy.Role
	}{
		{"admin", []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator}},
		{"engineer", []tenancy.Role{tenancy.RoleEngineer}},
		{"reviewer", []tenancy.Role{tenancy.RoleReviewer}},
		{"operator", []tenancy.Role{tenancy.RoleOperator}},
	}
	for _, p := range personas {
		principalID, err := shelloidc.PrincipalIDFor(issuerURL, p.subject)
		if err != nil {
			return err
		}
		page, err := memStore.Query(ctx, projectScope, store.ScanQuery{
			Table:   "memberships",
			Equals:  map[string]any{"principal_id": string(principalID)},
			OrderBy: []store.Order{{Column: "id"}},
			Limit:   1,
		})
		if err != nil {
			return err
		}
		if len(page.Rows) > 0 {
			continue
		}
		encoded, err := json.Marshal(p.roles)
		if err != nil {
			return err
		}
		rowID, err := ids.NewRowID()
		if err != nil {
			return err
		}
		if _, err := memStore.Execute(ctx, projectScope, store.Unit{Writes: []store.Write{{
			Table:  "memberships",
			ID:     rowID,
			Insert: true,
			Values: map[string]any{
				"principal_id": string(principalID),
				"roles":        encoded,
				"version":      uint64(1),
				"created_at":   now,
			},
		}}}); err != nil {
			return err
		}
	}
	return nil
}

func randomHexString(numBytes int) (string, error) {
	slice := make([]byte, numBytes)
	if _, err := io.ReadFull(cryptorand.Reader, slice); err != nil {
		return "", err
	}
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(slice)*2)
	for i, b := range slice {
		out[i*2] = hexChars[b>>4]
		out[i*2+1] = hexChars[b&0x0f]
	}
	return string(out), nil
}

type devOIDCPrincipalResolver struct {
	verifier   identity.OIDCVerifier
	static     authorize.CurrentPrincipal
	principals identity.PrincipalReader
	now        func() time.Time
}

var _ authorize.CurrentPrincipal = (*devOIDCPrincipalResolver)(nil)

func (r *devOIDCPrincipalResolver) Resolve(ctx context.Context, presented string) (authorize.Principal, error) {
	if err := ctx.Err(); err != nil {
		return authorize.Principal{}, fault.Wrap(fault.Unauthenticated, "credential authentication was canceled", err)
	}
	if strings.HasPrefix(presented, token.ExecutionPrefix) ||
		strings.HasPrefix(presented, token.ServicePrefix) ||
		strings.Count(presented, ".") != 2 {
		return r.static.Resolve(ctx, presented)
	}
	now := r.now()
	verified, err := r.verifier.Verify(ctx, identity.OIDCRequest{
		AccessToken: presented,
		Now:         now,
	})
	if err != nil {
		return authorize.Principal{}, devAuthRefusal(err)
	}
	if err := verified.Validate(); err != nil || !verified.Enabled || verified.Kind != identity.KindHuman {
		return authorize.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	current, err := identity.Current(ctx, r.principals, identity.PrincipalRequest{
		Scope:       verified.Scope,
		PrincipalID: verified.ID,
	})
	if err != nil {
		return authorize.Principal{}, devAuthRefusal(err)
	}
	if err := identity.CheckCurrent(verified, current); err != nil || current.Kind != identity.KindHuman {
		return authorize.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	principal := current
	principal.Roles = append([]tenancy.Role(nil), current.Roles...)
	if err := principal.Validate(); err != nil {
		return authorize.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	return principal, nil
}

func devAuthRefusal(err error) error {
	if class, ok := fault.ClassOf(err); ok {
		switch class {
		case fault.DependencyUnavailable, fault.UncertainExternalEffect, fault.InternalFailure:
			return fault.Wrap(fault.DependencyUnavailable, "credential verification is unavailable", err)
		}
	}
	return fault.Wrap(fault.Unauthenticated, "presented credential is invalid", err)
}

type devHumanCredentialValidity struct {
	clock shellapp.Clock
	store event.CredentialValidity
}

var _ event.CredentialValidity = (*devHumanCredentialValidity)(nil)

func (v *devHumanCredentialValidity) ValidUntil(ctx context.Context, principal identity.Principal) (time.Time, error) {
	if principal.Kind == identity.KindHuman {
		return v.clock.Now().Add(time.Hour), nil
	}
	return v.store.ValidUntil(ctx, principal)
}
