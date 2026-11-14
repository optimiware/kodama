package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	authapp "github.com/optimiware/kodama/internal/userland/app/auth"
	"github.com/optimiware/kodama/internal/userland/principal"
)

// runAuthPrincipalIDEntry runs `kodama auth principal-id`.
func runAuthPrincipalIDEntry(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("auth principal-id", flag.ContinueOnError)
	flags.SetOutput(stderr)
	issuer := flags.String("issuer", envOr("KODAMA_AUTH_ISSUER", ""), "OIDC issuer URL")
	subject := flags.String("subject", "", "raw OIDC subject claim")
	github := flags.String("github", "", "GitHub username")
	email := flags.String("email", "", "email address")
	local := flags.String("local", "", "local persona identifier")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}

	setFlags := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	selectors := 0
	for _, name := range []string{"subject", "github", "email", "local"} {
		if setFlags[name] {
			selectors++
		}
	}
	trimmedIssuer := strings.TrimSpace(*issuer)
	if trimmedIssuer == "" || selectors != 1 {
		usage(stderr, "auth principal-id requires --issuer and exactly one of --subject, --github, --email or --local")
		return 2
	}
	if strings.ContainsAny(trimmedIssuer, " \t\r\n") {
		fmt.Fprintln(stderr, "kodama: auth principal-id: --issuer must not contain whitespace")
		return 2
	}

	var resolvedSubject string
	var err error
	switch {
	case setFlags["subject"]:
		resolvedSubject = *subject
	case setFlags["github"]:
		resolvedSubject, err = principal.SubjectForGitHub(*github)
	case setFlags["email"]:
		resolvedSubject, err = principal.SubjectForEmail(*email)
	case setFlags["local"]:
		resolvedSubject, err = principal.SubjectForLocal(*local)
	}
	if err != nil {
		fmt.Fprintf(stderr, "kodama: auth principal-id: %v\n", err)
		return 2
	}

	pid, err := principal.ForOIDC(trimmedIssuer, resolvedSubject)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: auth principal-id: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, pid)
	return 0
}

// runAuthServeEntry runs `kodama auth serve`.
func runAuthServeEntry(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("auth serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	issuer := flags.String("issuer", envOr("KODAMA_AUTH_ISSUER", ""), "external OIDC issuer URL")
	listen := flags.String("listen", envOr("KODAMA_AUTH_LISTEN", authapp.DefaultListenAddress), "HTTP/TLS listen address")
	stateDir := flags.String("state-dir", envOr("KODAMA_AUTH_STATE_DIR", ""), "0600 signing state directory (ephemeral when omitted)")
	tenant := flags.String("tenant", envOr("KODAMA_AUTH_TENANT", authapp.DefaultTenantID), "tenant identity claim value")
	tenantClaim := flags.String("tenant-claim", envOr("KODAMA_AUTH_TENANT_CLAIM", authapp.DefaultTenantClaim), "tenant JWT claim name")
	project := flags.String("project", envOr("KODAMA_AUTH_PROJECT", ""), "optional project identity claim value")
	projectClaim := flags.String("project-claim", envOr("KODAMA_AUTH_PROJECT_CLAIM", ""), "optional project JWT claim name")
	clientID := flags.String("client-id", envOr("KODAMA_AUTH_CLIENT_ID", authapp.DefaultClientID), "primary public OAuth client id")
	audience := flags.String("audience", envOr("KODAMA_AUTH_AUDIENCE", authapp.DefaultAudience), "API audience claim value")

	redirectURIs := stringList(splitCommaOrSpace(os.Getenv("KODAMA_AUTH_REDIRECT_URIS")))
	flags.Var(&redirectURIs, "redirect-uri", "explicitly allowed redirect URI (repeatable)")

	localPersonas := flags.String("local-personas", envOr("KODAMA_AUTH_LOCAL_PERSONAS", ""), "comma-separated local personas (for example admin,engineer,reviewer)")
	accessCodeFile := flags.String("access-code-file", envOr("KODAMA_AUTH_ACCESS_CODE_FILE", ""), "file holding the access code for local sign-in and GitHub setup")

	ghClientID := flags.String("github-client-id", envOr("KODAMA_AUTH_GITHUB_CLIENT_ID", ""), "GitHub OAuth client id")
	ghClientSecretFile := flags.String("github-client-secret-file", envOr("KODAMA_AUTH_GITHUB_CLIENT_SECRET_FILE", ""), "file holding the GitHub OAuth client secret")
	ghWebURL := flags.String("github-web-url", envOr("KODAMA_AUTH_GITHUB_WEB_URL", ""), "GitHub web base URL")
	ghAPIURL := flags.String("github-api-url", envOr("KODAMA_AUTH_GITHUB_API_URL", ""), "GitHub API base URL")
	defaultAllowGHSetup, ghSetupEnvErr := envBoolStrict("KODAMA_AUTH_ALLOW_GITHUB_SETUP", false)
	allowGHSetup := flags.Bool("allow-github-setup", defaultAllowGHSetup, "enable 1-click GitHub App Manifest setup at /setup/github")

	upstreamIssuer := flags.String("upstream-oidc-issuer", envOr("KODAMA_AUTH_UPSTREAM_OIDC_ISSUER", ""), "upstream OIDC issuer URL (for example https://accounts.google.com)")
	upstreamClientID := flags.String("upstream-oidc-client-id", envOr("KODAMA_AUTH_UPSTREAM_OIDC_CLIENT_ID", ""), "upstream OIDC client id")
	upstreamSecretFile := flags.String("upstream-oidc-client-secret-file", envOr("KODAMA_AUTH_UPSTREAM_OIDC_CLIENT_SECRET_FILE", ""), "file holding the upstream OIDC client secret")
	upstreamScopes := flags.String("upstream-oidc-scopes", envOr("KODAMA_AUTH_UPSTREAM_OIDC_SCOPES", ""), "comma- or space-separated upstream OIDC scopes")
	upstreamName := flags.String("upstream-oidc-name", envOr("KODAMA_AUTH_UPSTREAM_OIDC_NAME", ""), "display label for upstream OIDC sign-in")

	tlsCertFile := flags.String("tls-cert-file", envOr("KODAMA_AUTH_TLS_CERT_FILE", ""), "TLS certificate PEM file")
	tlsKeyFile := flags.String("tls-key-file", envOr("KODAMA_AUTH_TLS_KEY_FILE", ""), "TLS private key PEM file")

	tokenTTL := flags.Duration("token-ttl", envDuration("KODAMA_AUTH_TOKEN_TTL", 0), "access/ID token lifetime")
	refreshTTL := flags.Duration("refresh-ttl", envDuration("KODAMA_AUTH_REFRESH_TTL", 0), "refresh token lifetime")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if ghSetupEnvErr != nil {
		usage(stderr, ghSetupEnvErr.Error())
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if strings.TrimSpace(*issuer) == "" {
		usage(stderr, "auth serve requires --issuer")
		return 2
	}

	accessCode, err := readOptionalSecretFile("--access-code-file", *accessCodeFile)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: auth serve: %v\n", err)
		return 2
	}
	ghSecret, err := readOptionalSecretFile("--github-client-secret-file", *ghClientSecretFile)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: auth serve: %v\n", err)
		return 2
	}
	upstreamSecret, err := readOptionalSecretFile("--upstream-oidc-client-secret-file", *upstreamSecretFile)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: auth serve: %v\n", err)
		return 2
	}

	config := authapp.Config{
		Issuer:          *issuer,
		ListenAddress:   *listen,
		StateDir:        *stateDir,
		TenantID:        id.TenantID(*tenant),
		TenantClaim:     *tenantClaim,
		ProjectID:       id.ProjectID(*project),
		ProjectClaim:    *projectClaim,
		ClientID:        *clientID,
		Audience:        *audience,
		RedirectURIs:    append([]string(nil), redirectURIs...),
		LocalPersonas:   splitComma(*localPersonas),
		AccessCode:      accessCode,
		AccessTokenTTL:  *tokenTTL,
		RefreshTokenTTL: *refreshTTL,
		TLSCertFile:     *tlsCertFile,
		TLSKeyFile:      *tlsKeyFile,
		GitHub: authapp.GitHubConfig{
			ClientID:     *ghClientID,
			ClientSecret: ghSecret,
			AllowSetup:   *allowGHSetup,
			WebURL:       *ghWebURL,
			APIURL:       *ghAPIURL,
		},
		UpstreamOIDC: authapp.UpstreamOIDCConfig{
			Issuer:       *upstreamIssuer,
			ClientID:     *upstreamClientID,
			ClientSecret: upstreamSecret,
			Scopes:       splitCommaOrSpace(*upstreamScopes),
			DisplayName:  *upstreamName,
		},
	}

	if err := runAuthApp(ctx, config); err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		if class, ok := fault.ClassOf(err); ok && class == fault.InvalidInput {
			return 2
		}
		return 1
	}
	return 0
}

func readOptionalSecretFile(flagName, rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s could not be read", flagName)
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("%s is empty", flagName)
	}
	return secret, nil
}

func splitComma(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func splitCommaOrSpace(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	return strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
}

func envBoolStrict(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return fallback, nil
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", name)
	}
}
