package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/userland/up"
)

// runUpStack, runDownStack and runStatusStack are small injectable seams so CLI
// command flag and dispatch tests can exercise `kodema up/down/status` without
// starting containers.
var (
	runUpStack     = up.Up
	runDownStack   = up.Down
	runStatusStack = up.Status
	serveGateway   = up.ServeStackGateway
)

// runUpEntry runs `kodema up`.
func runUpEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODEMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODEMA_UP_STATE_DIR", ""), "stack state directory (default ~/.kodema/instances/<name>)")
	listen := flags.String("listen", envOr("KODEMA_UP_LISTEN", up.DefaultListenAddr()), "single-origin HTTPS listen address")
	socket := flags.String("socket", envOr("KODEMA_SOCKET", ""), "Podman API Unix socket (default: found; env KODEMA_SOCKET)")
	tenant := flags.String("tenant", envOr("KODEMA_UP_TENANT", up.DefaultTenantID), "bootstrap tenant identity")
	project := flags.String("project", envOr("KODEMA_UP_PROJECT", up.DefaultProjectID), "bootstrap project identity")
	image := flags.String("image", envOr("KODEMA_UP_IMAGE", up.KodemaImageRepository+":"+version), "kodema container image (default: the image of this binary's version)")
	foreground := flags.Bool("foreground", false, "keep the single-origin TLS gateway in the foreground")
	gatewayOnly := flags.Bool("gateway-only", false, "serve only the single-origin TLS gateway from an existing stack.json")
	allowUnshared := flags.Bool("allow-unshared-state-dir", false, "allow --state-dir outside $HOME on macOS")
	defaultAllowProjectSecrets, allowSecretsEnvErr := envBoolStrict("KODEMA_UP_ALLOW_PROJECT_SECRETS", true)
	allowProjectSecrets := flags.Bool("allow-project-secrets", defaultAllowProjectSecrets, "allow project-stored workload secrets in the local stack (env KODEMA_UP_ALLOW_PROJECT_SECRETS)")
	var allowSecrets stringList
	flags.Var(&allowSecrets, "allow-secret", "allow only this exact workload secret name in the local project (repeatable; overrides allow-all)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("up: unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if *gatewayOnly {
		if strings.TrimSpace(*stateDir) == "" {
			usage(stderr, "up --gateway-only requires --state-dir")
			return 2
		}
		if err := serveGateway(ctx, *stateDir); err != nil {
			fmt.Fprintf(stderr, "kodema: %v\n", err)
			return 1
		}
		return 0
	}
	if allowSecretsEnvErr != nil {
		usage(stderr, allowSecretsEnvErr.Error())
		return 2
	}
	if !*allowProjectSecrets && len(allowSecrets) > 0 {
		usage(stderr, "up: cannot combine --allow-project-secrets=false with --allow-secret")
		return 2
	}
	for _, entry := range allowSecrets {
		if _, err := id.ParseSecretName(strings.TrimSpace(entry)); err != nil {
			usage(stderr, fmt.Sprintf("invalid --allow-secret %q: %v", entry, err))
			return 2
		}
	}
	if err := up.ValidateInstanceName(*name); err != nil {
		usage(stderr, err.Error())
		return 2
	}
	if _, err := id.ParseTenantID(strings.TrimSpace(*tenant)); err != nil {
		usage(stderr, fmt.Sprintf("invalid --tenant: %v", err))
		return 2
	}
	if _, err := id.ParseProjectID(strings.TrimSpace(*project)); err != nil {
		usage(stderr, fmt.Sprintf("invalid --project: %v", err))
		return 2
	}
	if _, _, err := up.PublicOriginFromListen(*listen); err != nil {
		usage(stderr, err.Error())
		return 2
	}

	_, err := runUpStack(ctx, up.UpConfig{
		Instance:              *name,
		StateDir:              *stateDir,
		ListenAddr:            *listen,
		Socket:                *socket,
		AllowUnsharedStateDir: *allowUnshared,
		TenantID:              *tenant,
		ProjectID:             *project,
		KodemaImage:           *image,
		Foreground:            *foreground,
		DenyProjectSecrets:    !*allowProjectSecrets,
		AllowSecrets:          allowSecrets,
	}, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "kodema: %v\n", err)
		return 1
	}
	return 0
}

// runDownEntry runs `kodema down`.
func runDownEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("down", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODEMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODEMA_UP_STATE_DIR", ""), "stack state directory")
	socket := flags.String("socket", envOr("KODEMA_SOCKET", ""), "Podman API Unix socket (default: found; env KODEMA_SOCKET)")
	purge := flags.Bool("purge", false, "remove volumes and the instance state directory")
	allowUnshared := flags.Bool("allow-unshared-state-dir", false, "allow --state-dir outside $HOME on macOS")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("down: unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if err := up.ValidateInstanceName(*name); err != nil {
		usage(stderr, err.Error())
		return 2
	}

	if err := runDownStack(ctx, up.DownConfig{
		Instance:              *name,
		StateDir:              *stateDir,
		Socket:                *socket,
		AllowUnsharedStateDir: *allowUnshared,
		Purge:                 *purge,
	}, stdout); err != nil {
		fmt.Fprintf(stderr, "kodema: %v\n", err)
		return 1
	}
	return 0
}

// runStatusEntry runs `kodema status`.
func runStatusEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODEMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODEMA_UP_STATE_DIR", ""), "stack state directory")
	socket := flags.String("socket", envOr("KODEMA_SOCKET", ""), "Podman API Unix socket (default: found; env KODEMA_SOCKET)")
	asJSON := flags.Bool("json", false, "print status report as JSON")
	allowUnshared := flags.Bool("allow-unshared-state-dir", false, "allow --state-dir outside $HOME on macOS")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		usage(stderr, fmt.Sprintf("status: unexpected argument %q", flags.Arg(0)))
		return 2
	}
	if err := up.ValidateInstanceName(*name); err != nil {
		usage(stderr, err.Error())
		return 2
	}

	report, err := runStatusStack(ctx, up.StatusConfig{
		Instance:              *name,
		StateDir:              *stateDir,
		Socket:                *socket,
		AllowUnsharedStateDir: *allowUnshared,
	})
	if err != nil {
		fmt.Fprintf(stderr, "kodema: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprintf(stdout, "instance:    %s\n", report.Instance)
		fmt.Fprintf(stdout, "state_dir:   %s\n", report.StateDir)
		fmt.Fprintf(stdout, "runtime:     %s (%s, %s)\n", report.Runtime.Kind, report.Runtime.Provider, report.Runtime.SocketPath)
		fmt.Fprintf(stdout, "public_url:  %s\n", report.PublicURL)
		fmt.Fprintf(stdout, "gateway:     alive=%t (pid=%d)\n", report.GatewayAlive, report.GatewayPID)
		fmt.Fprintf(stdout, "readyz:      %t\n", report.Ready)
		for _, comp := range []string{"postgres", "nats", "server", "git"} {
			info := report.Containers[comp]
			fmt.Fprintf(stdout, "container %-8s %s\n", comp+":", info.Status)
		}
	}
	if !report.Healthy() {
		return 1
	}
	return 0
}
