package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/up"
)

// runUpStack, runDownStack and runStatusStack are small injectable seams so CLI
// command flag and dispatch tests can exercise `kodama up/down/status` without
// starting containers.
var (
	runUpStack     = up.Up
	runDownStack   = up.Down
	runStatusStack = up.Status
	serveGateway   = up.ServeStackGateway
)

// runUpEntry runs `kodama up`.
func runUpEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODAMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODAMA_UP_STATE_DIR", ""), "stack state directory (default ~/.kodama/instances/<name>)")
	listen := flags.String("listen", envOr("KODAMA_UP_LISTEN", up.DefaultListenAddr), "single-origin HTTPS listen address")
	socket := flags.String("socket", envOr("KODAMA_UP_SOCKET", ""), "container runtime Unix socket override")
	tenant := flags.String("tenant", envOr("KODAMA_UP_TENANT", up.DefaultTenantID), "bootstrap tenant identity")
	project := flags.String("project", envOr("KODAMA_UP_PROJECT", up.DefaultProjectID), "bootstrap project identity")
	image := flags.String("image", envOr("KODAMA_UP_IMAGE", up.DefaultKodamaImage), "kodama container image")
	foreground := flags.Bool("foreground", false, "keep the single-origin TLS gateway in the foreground")
	gatewayOnly := flags.Bool("gateway-only", false, "serve only the single-origin TLS gateway from an existing stack.json")
	allowUnshared := flags.Bool("allow-unshared-state-dir", false, "allow --state-dir outside $HOME on macOS")
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
			fmt.Fprintf(stderr, "kodama: %v\n", err)
			return 1
		}
		return 0
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
	listenAddr, _, err := up.PublicOriginFromListen(*listen)
	if err != nil {
		usage(stderr, err.Error())
		return 2
	}
	warnOpenSignIn(stderr, listenAddr)

	_, err = runUpStack(ctx, up.UpConfig{
		Instance:              *name,
		StateDir:              *stateDir,
		ListenAddr:            *listen,
		Socket:                *socket,
		AllowUnsharedStateDir: *allowUnshared,
		TenantID:              *tenant,
		ProjectID:             *project,
		KodamaImage:           *image,
		Foreground:            *foreground,
	}, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// warnOpenSignIn says what a listen address beyond loopback exposes: the
// built-in sign-in of `kodama dev` and `kodama up` asks for no credential.
func warnOpenSignIn(stderr io.Writer, listenAddr string) {
	if !loopbackListener(listenAddr) {
		fmt.Fprintf(stderr, "kodama: warning: %s is not a loopback address. The built-in sign-in asks for no credential: anyone who can reach this port can obtain an administrator token.\n", listenAddr)
	}
}

// runDownEntry runs `kodama down`.
func runDownEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("down", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODAMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODAMA_UP_STATE_DIR", ""), "stack state directory")
	socket := flags.String("socket", envOr("KODAMA_UP_SOCKET", ""), "container runtime Unix socket override")
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
		fmt.Fprintf(stderr, "kodama: %v\n", err)
		return 1
	}
	return 0
}

// runStatusEntry runs `kodama status`.
func runStatusEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", envOr("KODAMA_INSTANCE", up.DefaultInstanceName), "stack instance name")
	stateDir := flags.String("state-dir", envOr("KODAMA_UP_STATE_DIR", ""), "stack state directory")
	socket := flags.String("socket", envOr("KODAMA_UP_SOCKET", ""), "container runtime Unix socket override")
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
		fmt.Fprintf(stderr, "kodama: %v\n", err)
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
