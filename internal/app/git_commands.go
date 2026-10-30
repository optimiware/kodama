package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gitjwt "github.com/optimiware/kodama/internal/gitauth/jwt"
	"github.com/optimiware/kodama/internal/gitservice"
	"github.com/optimiware/kodama/internal/kernel/serve"
)

// gitCommandUsage documents the two Git commands registered in the binary.
const gitCommandUsage = `Usage: kodama git serve [flags]
       kodama git-hook <pre-receive|reference-transaction|post-receive> [args...]

git serve runs one tenant's native Git worker: smart HTTP behind the control
plane's forwarding assertions, the server-owned hooks and the receive journal.
git-hook is what the worker's hook shims run; it is not meant to be called by
hand.

Flags override environment settings:
  --tenant                  tenant ID this worker serves (KODAMA_GIT_TENANT_ID)
  --data-root               absolute data root of the tenant's volume (KODAMA_GIT_DATA_ROOT)
  --listen                  listen address, default :8443 (KODAMA_GIT_LISTEN)
  --control-url             the control plane's base URL (KODAMA_GIT_CONTROL_URL)
  --identity-file           file holding the worker identity token (KODAMA_GIT_WORKER_IDENTITY_FILE)
  --forwarding-keys-file    PEM file of the forwarding public key(s) (KODAMA_GIT_FORWARDING_PUBLIC_KEYS_FILE)
  --control-token-file      file holding the control plane's RPC token (KODAMA_GIT_CONTROL_TOKEN_FILE)
  --tls-cert, --tls-key     serve TLS (KODAMA_GIT_TLS_CERT_FILE, KODAMA_GIT_TLS_KEY_FILE)
  --poll-interval           assignment poll interval, default 15s (KODAMA_GIT_POLL_INTERVAL)
  --hook-binary             executable the hook shims run (default: this binary)`

// runGit dispatches `kodama git <subcommand>`.
func runGit(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		out := stderr
		code := 2
		if len(args) > 0 {
			out, code = stdout, 0
		}
		fmt.Fprintln(out, gitCommandUsage)
		return code
	}
	if args[0] != "serve" {
		fmt.Fprintf(stderr, "kodama git: unknown subcommand %q\n%s\n", args[0], gitCommandUsage)
		return 2
	}
	return runGitServe(ctx, args[1:], getenv, stdout, stderr)
}

// parseGitServe reads the worker's configuration from flags and the environment.
// The worker holds no database or broker credential: only the tenant's volume,
// the forwarding public keys, its own identity token and the control plane's
// address (GR-03).
func parseGitServe(args []string, getenv func(string) string, output io.Writer) (gitservice.Config, error) {
	setting := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	flags := flag.NewFlagSet("kodama git serve", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() { fmt.Fprintln(output, gitCommandUsage) }
	tenant := flags.String("tenant", setting("KODAMA_GIT_TENANT_ID", ""), "")
	dataRoot := flags.String("data-root", setting("KODAMA_GIT_DATA_ROOT", ""), "")
	listen := flags.String("listen", setting("KODAMA_GIT_LISTEN", ":8443"), "")
	controlURL := flags.String("control-url", setting("KODAMA_GIT_CONTROL_URL", ""), "")
	identityFile := flags.String("identity-file", setting("KODAMA_GIT_WORKER_IDENTITY_FILE", ""), "")
	keysFile := flags.String("forwarding-keys-file", setting("KODAMA_GIT_FORWARDING_PUBLIC_KEYS_FILE", ""), "")
	tokenFile := flags.String("control-token-file", setting("KODAMA_GIT_CONTROL_TOKEN_FILE", ""), "")
	tlsCert := flags.String("tls-cert", setting("KODAMA_GIT_TLS_CERT_FILE", ""), "")
	tlsKey := flags.String("tls-key", setting("KODAMA_GIT_TLS_KEY_FILE", ""), "")
	poll := flags.String("poll-interval", setting("KODAMA_GIT_POLL_INTERVAL", "15s"), "")
	hook := flags.String("hook-binary", setting("KODAMA_GIT_HOOK_BINARY", ""), "")
	if err := flags.Parse(args); err != nil {
		return gitservice.Config{}, err
	}
	if flags.NArg() != 0 {
		return gitservice.Config{}, errors.New("kodama git serve accepts flags only, not positional arguments")
	}
	for _, required := range []struct{ name, value string }{{"--tenant", *tenant}, {"--data-root", *dataRoot}, {"--control-url", *controlURL},
		{"--identity-file", *identityFile}, {"--forwarding-keys-file", *keysFile}, {"--control-token-file", *tokenFile}} {
		if required.value == "" {
			return gitservice.Config{}, fmt.Errorf("%s is required", required.name)
		}
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return gitservice.Config{}, errors.New("--tls-cert and --tls-key are given together")
	}
	interval, err := time.ParseDuration(*poll)
	if err != nil || interval < 0 || interval > time.Hour {
		return gitservice.Config{}, errors.New("--poll-interval is a duration between 0 and 1h")
	}
	keyData, err := os.ReadFile(*keysFile)
	if err != nil {
		return gitservice.Config{}, fmt.Errorf("forwarding keys: %w", err)
	}
	public, err := serve.ParsePublicKeys(keyData)
	if err != nil {
		return gitservice.Config{}, err
	}
	ring, err := gitjwt.NewPublicKeyRing(public)
	if err != nil {
		return gitservice.Config{}, err
	}
	verifier, err := gitjwt.NewVerifier(gitjwt.VerifierConfig{Issuer: serve.TokenIssuer, Forwarding: ring})
	if err != nil {
		return gitservice.Config{}, err
	}
	token, err := readSecretFile(*tokenFile)
	if err != nil {
		return gitservice.Config{}, fmt.Errorf("control token: %w", err)
	}
	if _, err := readSecretFile(*identityFile); err != nil {
		return gitservice.Config{}, fmt.Errorf("worker identity: %w", err)
	}
	tenantID := domain.TenantID(*tenant)
	// The identity token is re-read at every call, so a renewed token file is
	// picked up without a restart.
	identity := func(context.Context) (string, error) { return readSecretFile(*identityFile) }
	control, err := gitservice.NewControlPlaneClient(*controlURL, tenantID, identity, nil)
	if err != nil {
		return gitservice.Config{}, err
	}
	return gitservice.Config{
		TenantID: tenantID, DataRoot: *dataRoot, ListenAddr: *listen,
		ControlPlane: control, Assertions: verifier, Callers: serve.ControlCallers(token),
		TLSCertFile: *tlsCert, TLSKeyFile: *tlsKey, PollInterval: interval, HookBinary: *hook,
	}, nil
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

func runGitServe(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var usage strings.Builder
	cfg, err := parseGitServe(args, getenv, &usage)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usage.String())
			return 0
		}
		if usage.Len() > 0 {
			_, _ = io.WriteString(stderr, usage.String())
		} else {
			fmt.Fprintln(stderr, "kodama git serve:", err)
		}
		return 2
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(stderr, nil))
	cmd, err := gitservice.ServeCommand(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "kodama git serve:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg.Logger.Info("git worker listening", "tenant", cfg.TenantID, "address", cfg.ListenAddr)
	if err := cmd.Run(ctx); err != nil {
		fmt.Fprintln(stderr, "kodama git serve:", err)
		return 1
	}
	return 0
}

// runGitHook is `kodama git-hook <name> [args...]`, the command of the worker's
// hook shims. It reads no configuration: the worker hands the hook its loopback
// address and per-receive token in the environment, and the hook reaches the
// control plane only through the worker.
func runGitHook(args []string, stdin io.Reader, stdout, stderr io.Writer, environ []string) int {
	return gitservice.HookMain(args, stdin, stdout, stderr, environ)
}
