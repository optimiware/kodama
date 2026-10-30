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
	"time"

	"github.com/optimiware/kodama/internal/kernel/runner"
	"github.com/optimiware/kodama/internal/shell/serve"
)

// runKernel serves the ADR 0013 kernel assembly: projects, tasks, pipelines,
// runs, secrets and artifacts over PostgreSQL, with one container executor.
func runKernel(ctx context.Context, process Config, getenv func(string) string, logger *slog.Logger, stderr io.Writer) int {
	cfg, err := kernelConfig(process, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "kernel assembly:", err)
		return 2
	}
	if err := serve.Run(ctx, cfg, logger); err != nil {
		fmt.Fprintln(stderr, "kernel assembly:", err)
		return 1
	}
	return 0
}

// kernelConfig reads the kernel assembly's environment. KODAMA_NATS_URL selects
// NATS JetStream for the event outbox (nats://[token@]host:port, the token
// otherwise KODAMA_NATS_TOKEN); unset delivers events in process.
// KODAMA_EVENT_POLL_INTERVAL is the outbox poll period.
func kernelConfig(process Config, getenv func(string) string) (serve.Config, error) {
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
		DatabaseURL:    firstNonEmpty(envValue(getenv, "KODAMA_DATABASE_RUNTIME_URL"), envValue(getenv, "KODAMA_DATABASE_URL")),
		InstallationID: installation,
		Env:            env,
		Executor:       firstNonEmpty(getenv("KODAMA_EXECUTOR"), "podman"),
		LogDir:         logDir,
		StateDir:       stateDir,
		Keyfile:        getenv("KODAMA_SECRETS_KEYFILE"),
		KMSFile:        getenv("KODAMA_KMS_CONFIG_FILE"),
		DevToken:       getenv("KODAMA_DEV_TOKEN"),
		DevUserID:      devUser,
		DevName:        devName,
		OIDCIssuer:     getenv("KODAMA_OIDC_ISSUER"),
		OIDCClientID:   getenv("KODAMA_OIDC_CLIENT_ID"),

		PublicURL:            getenv("KODAMA_PUBLIC_URL"),
		GitWorkerURL:         getenv("KODAMA_GIT_WORKER_URL"),
		GitControlToken:      envValue(getenv, "KODAMA_GIT_CONTROL_TOKEN"),
		GitForwardingKeyFile: getenv("KODAMA_GIT_FORWARDING_KEY_FILE"),
		GitIdentityKeyFile:   getenv("KODAMA_GIT_IDENTITY_KEY_FILE"),
		GitInProcess:         getenv("KODAMA_GIT_HOSTING") == "embedded",
		GitInProcessAck:      getenv("KODAMA_GIT_EMBEDDED_ACK"),

		NATSURL:   getenv("KODAMA_NATS_URL"),
		NATSToken: envValue(getenv, "KODAMA_NATS_TOKEN"),
	}
	if cfg.Executor == "kubernetes" {
		path := getenv("KODAMA_KUBE_CONFIG_FILE")
		if path == "" {
			return serve.Config{}, errors.New("KODAMA_EXECUTOR=kubernetes needs KODAMA_KUBE_CONFIG_FILE")
		}
		f, err := os.Open(path)
		if err != nil {
			return serve.Config{}, fmt.Errorf("KODAMA_KUBE_CONFIG_FILE: %w", err)
		}
		defer f.Close()
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		var k runner.KubeConfig
		if err := dec.Decode(&k); err != nil {
			return serve.Config{}, fmt.Errorf("KODAMA_KUBE_CONFIG_FILE: %w", err)
		}
		k.APIURL = firstNonEmpty(getenv("KODAMA_KUBE_API"), "https://kubernetes.default.svc")
		k.TokenFile = firstNonEmpty(getenv("KODAMA_KUBE_TOKEN_FILE"), "/var/run/secrets/kubernetes.io/serviceaccount/token")
		k.CAFile = firstNonEmpty(getenv("KODAMA_KUBE_CA_FILE"), "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
		cfg.Kube = &k
	}
	if err := serve.ValidateNATSURL(cfg.NATSURL); err != nil {
		return serve.Config{}, fmt.Errorf("KODAMA_NATS_URL: %w", err)
	}
	if cfg.NATSToken != "" && cfg.NATSURL == "" {
		return serve.Config{}, fmt.Errorf("KODAMA_NATS_TOKEN is set without KODAMA_NATS_URL")
	}
	for _, v := range []struct {
		name string
		into *time.Duration
	}{{"KODAMA_GIT_POLL_INTERVAL", &cfg.GitPollInterval}, {"KODAMA_EVENT_POLL_INTERVAL", &cfg.EventInterval}} {
		if raw := getenv(v.name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 || d > time.Hour {
				return serve.Config{}, fmt.Errorf("%s is a duration between 0 and 1h", v.name)
			}
			*v.into = d
		}
	}
	return cfg, nil
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
	var roles roleFlags
	fs.Var(&roles, "role", "tenant-wide role to bind, repeatable (default admin; admin gives no push authority, add engineer to push)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *subject == "" {
		*subject = *userID
	}
	migrationURL := firstNonEmpty(getenv("KODAMA_DATABASE_MIGRATION_URL"), getenv("KODAMA_MIGRATION_DATABASE_URL"))
	id, err := serve.Seed(ctx, migrationURL, *tenant, *userID, *issuer, *subject, *name, roles...)
	if err != nil {
		fmt.Fprintln(stderr, "seed:", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s\n", id)
	return 0
}

// roleFlags collects the repeatable --role flag.
type roleFlags []string

func (r *roleFlags) String() string     { return strings.Join(*r, ",") }
func (r *roleFlags) Set(v string) error { *r = append(*r, v); return nil }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// envValue reads a value or, when only its _FILE form is set, the trimmed
// contents of that file. Secret values are mounted as files in the chart.
func envValue(getenv func(string) string, name string) string {
	if v := getenv(name); v != "" {
		return v
	}
	path := getenv(name + "_FILE")
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

var _ = os.Getenv
