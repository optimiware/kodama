package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/optimiware/kodama/internal/kernel/serve"
)

// runKernel serves the ADR 0013 kernel assembly: projects, tasks, pipelines,
// runs, secrets and artifacts over PostgreSQL, with one container executor.
func runKernel(ctx context.Context, process Config, getenv func(string) string, logger *slog.Logger, stderr io.Writer) int {
	env := getenv("KODAMA_ENV")
	if env == "" {
		env = "development"
	}
	stateDir := getenv("KODAMA_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join("dev", ".state", "kernel")
	}
	logDir := getenv("KODAMA_LOG_DIR")
	if logDir == "" {
		logDir = filepath.Join(stateDir, "logs")
	}
	devUser := getenv("KODAMA_DEV_USER_ID")
	if devUser == "" {
		devUser = "u_dev"
	}
	devName := getenv("KODAMA_DEV_NAME")
	if devName == "" {
		devName = "Development Operator"
	}
	installation := getenv("KODAMA_INSTALLATION_ID")
	if installation == "" {
		installation = "local"
	}
	cfg := serve.Config{
		Addr:           process.HTTPAddr,
		DatabaseURL:    firstNonEmpty(getenv("KODAMA_DATABASE_RUNTIME_URL"), getenv("KODAMA_DATABASE_URL")),
		InstallationID: installation,
		Env:            env,
		Executor:       firstNonEmpty(getenv("KODAMA_EXECUTOR"), "podman"),
		LogDir:         logDir,
		StateDir:       stateDir,
		Keyfile:        getenv("KODAMA_SECRETS_KEYFILE"),
		DevToken:       getenv("KODAMA_DEV_TOKEN"),
		DevUserID:      devUser,
		DevName:        devName,
		OIDCIssuer:     getenv("KODAMA_OIDC_ISSUER"),
		OIDCClientID:   getenv("KODAMA_OIDC_CLIENT_ID"),
	}
	if err := serve.Run(ctx, cfg, logger); err != nil {
		fmt.Fprintln(stderr, "kernel assembly:", err)
		return 1
	}
	return 0
}

// runSeed creates the first tenant and administrator with the owner identity.
func runSeed(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kodama seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant key (required)")
	userID := fs.String("user-id", "", "user id (required)")
	issuer := fs.String("issuer", "kodama:dev", "OIDC issuer")
	subject := fs.String("subject", "", "OIDC subject (default: user id)")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *subject == "" {
		*subject = *userID
	}
	migrationURL := firstNonEmpty(getenv("KODAMA_DATABASE_MIGRATION_URL"), getenv("KODAMA_MIGRATION_DATABASE_URL"))
	id, err := serve.Seed(ctx, migrationURL, *tenant, *userID, *issuer, *subject, *name)
	if err != nil {
		fmt.Fprintln(stderr, "seed:", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s\n", id)
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

var _ = os.Getenv
