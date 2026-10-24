package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

func secretSetting(getenv func(string) string, env, file string) (string, error) {
	if v := getenv(env); v != "" {
		return v, nil
	}
	dir := getenv("KODAMA_SECRETS_DIR")
	if dir == "" {
		dir = "/secrets"
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return "", fmt.Errorf("%s or the %s secret file: %w", env, file, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// runCore serves the core assembly until ctx ends, then drains it.
func runCore(ctx context.Context, process Config, getenv func(string) string, logger *slog.Logger, stderr io.Writer) int {
	cfg, err := LoadCoreConfig(getenv, process)
	if err != nil {
		fmt.Fprintln(stderr, "core configuration:", err)
		return 2
	}
	srv, err := Start(ctx, cfg, logger)
	if err != nil {
		logger.Error("core assembly failed to start", "error", err)
		return 1
	}
	logger.Info("core assembly listening", "address", srv.Addr(), "workers", srv.Core().Workers())
	code := 0
	select {
	case <-ctx.Done():
	case err := <-srv.Done():
		if err != nil {
			logger.Error("server stopped", "error", err)
			code = 1
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.Process.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		logger.Error("shutdown incomplete", "error", err)
		code = 1
	}
	logger.Info("server stopped")
	return code
}

func runMigrate(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kodama migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: kodama migrate\n\nApplies the core schema in its declared order as the owner identity\n(KODAMA_DATABASE_MIGRATION_URL or database-migration-url) and grants the runtime role\n(KODAMA_DATABASE_URL or database-runtime-url, or just its name in\nKODAMA_DATABASE_RUNTIME_ROLE) its minimal access.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "kodama migrate accepts no arguments")
		return 2
	}
	owner, err := secretSetting(getenv, "KODAMA_DATABASE_MIGRATION_URL", "database-migration-url")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// The runtime role may be named without its credential
	// (KODAMA_DATABASE_RUNTIME_ROLE): the migration Pod of the chart never holds it.
	mc := MigrateConfig{MigrationURL: owner, RuntimeRole: getenv("KODAMA_DATABASE_RUNTIME_ROLE")}
	if mc.RuntimeRole == "" {
		if mc.RuntimeURL, err = secretSetting(getenv, "KODAMA_DATABASE_URL", "database-runtime-url"); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	res, err := Migrate(ctx, mc)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 1
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"applied": res.Applied, "verified": res.Skipped})
	return 0
}

func runBootstrap(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kodama bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenantID := fs.String("tenant-id", "", "tenant identifier (required)")
	tenantKey := fs.String("tenant-key", "", "short unique tenant key (required)")
	display := fs.String("display-name", "", "tenant display name (required)")
	operator := fs.String("operator-id", "", "operator identifier recorded in audit (required)")
	issuer := fs.String("eligible-issuer", getenv("KODAMA_OIDC_ISSUER"), "issuer of the first administrator")
	subject := fs.String("eligible-subject", "", "subject of the first administrator (required)")
	tokenFile := fs.String("token-file", "", "file holding the administrator's access token (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	env, err := ParseEnv(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "bootstrap:", err)
		return 2
	}
	if fs.NArg() != 0 || *tenantID == "" || *tenantKey == "" || *display == "" || *operator == "" || *subject == "" || *tokenFile == "" {
		fmt.Fprintln(stderr, "kodama bootstrap needs --tenant-id, --tenant-key, --display-name, --operator-id, --eligible-subject and --token-file")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "token file:", err)
		return 2
	}
	admin, err := secretSetting(getenv, "KODAMA_DATABASE_MIGRATION_URL", "database-migration-url")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	res, err := Bootstrap(ctx, BootstrapRequest{
		Env: env, AdminDatabaseURL: admin,
		OIDC: OIDCConfig{Issuer: getenv("KODAMA_OIDC_ISSUER"), Audience: getenv("KODAMA_OIDC_AUDIENCE"),
			AllowInsecureHTTP: strings.EqualFold(getenv("KODAMA_OIDC_ALLOW_INSECURE_HTTP"), "true")},
		Config: identity.BootstrapConfig{
			Profile: identity.ProfileStandalone, Eligible: identity.Principal{Issuer: *issuer, Subject: *subject}, OperatorID: *operator,
			TenantID: domain.TenantID(*tenantID), TenantKey: *tenantKey, DisplayName: *display,
		},
		AccessToken: strings.TrimSpace(string(token)),
	})
	if err != nil {
		fmt.Fprintln(stderr, "bootstrap:", err)
		if errors.Is(err, identity.ErrBootstrapConflict) || errors.Is(err, identity.ErrBootstrapNotEligible) {
			return 4
		}
		return 1
	}
	_ = json.NewEncoder(stdout).Encode(map[string]any{"tenantId": res.Tenant, "userId": res.UserID, "created": res.Created})
	return 0
}

func runCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	runner, err := NewCLI(CLIConfig{Streams: root.Streams{In: os.Stdin, Out: stdout, Err: stderr}, Getenv: getenv})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return int(runner.Run(ctx, args))
}
