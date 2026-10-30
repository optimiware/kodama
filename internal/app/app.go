// Package app is the composition root for the Kodama process.
package app

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/optimiware/kodama/internal/entrypoint"
)

const usageText = `Usage: kodama <command> [flags]

Commands:
  server     Run the HTTP process (--assembly scaffold|core|kernel)
  seed       Create the first tenant and administrator (operator only)
  migrate    Apply the core schema through the owner database identity
  bootstrap  Provision the first tenant and administrator (operator only)
  step       Run one attempt's command inside its workload (used by the runtimes)
  prepare    Check out the run's pinned commit once into the shared workspace (run_shared preparation Job)
  service-run  Deliver a run service's pinned secrets and run its command (run_shared services with secrets)

Command groups of the command-line client:
  context, auth, work, workspace, pipeline, run, variable, secret, agent, gate, environment, deployment
  (see kodama <group> help)

Use kodama server --help for server options.`

// Main returns a process exit code so startup failures are testable without exit.
func Main(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var output bytes.Buffer
	root := flag.NewFlagSet("kodama", flag.ContinueOnError)
	root.SetOutput(&output)
	root.Usage = func() { fmt.Fprintln(&output, usageText) }
	if len(args) == 1 && args[0] == "help" {
		args = []string{"--help"}
	}
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.Copy(stdout, &output)
			return 0
		}
		_, _ = io.Copy(stderr, &output)
		return 2
	}
	command := root.Args()
	if len(command) > 0 && isCLIGroup(command[0]) {
		return runCLI(ctx, command, getenv, stdout, stderr)
	}
	if len(command) > 0 {
		// The in-workload commands (the attempt entrypoint and, for run_shared, the
		// preparation and the service supervisor) read no environment selection and
		// build no adapter: they are clients of the runtime API (SD-12 concerns the
		// server).
		for _, c := range workloadCommands() {
			if command[0] == c.Name {
				return c.Run(ctx, command[1:], getenv, stdout, stderr)
			}
		}
	}
	if len(command) == 0 || (command[0] != "server" && command[0] != "migrate" && command[0] != "bootstrap" && command[0] != "seed") {
		root.Usage()
		_, _ = io.Copy(stderr, &output)
		return 2
	}
	switch command[0] {
	case "migrate":
		return runMigrate(ctx, command[1:], getenv, stdout, stderr)
	case "bootstrap":
		return runBootstrap(ctx, command[1:], getenv, stdout, stderr)
	case "seed":
		return runSeed(ctx, command[1:], getenv, stdout, stderr)
	}
	cfg, err := parseServerConfig(command[1:], getenv, &output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.Copy(stdout, &output)
			return 0
		}
		if output.Len() > 0 {
			_, _ = io.Copy(stderr, &output)
		} else {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	switch cfg.Assembly {
	case AssemblyCore:
		return runCore(ctx, cfg, getenv, logger, stderr)
	case AssemblyKernel:
		return runKernel(ctx, cfg, getenv, logger, stderr)
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		logger.Error("listen failed", "error", err)
		return 1
	}
	logger.Info("server listening", "address", listener.Addr().String())
	if err := Serve(ctx, listener, cfg, logger); err != nil {
		logger.Error("server stopped", "error", err)
		return 1
	}
	logger.Info("server stopped")
	return 0
}

// workloadCommands are the commands that run inside a workload the server
// created: `step` (an attempt), `prepare` (a run's one checkout) and
// `service-run` (a service with pinned secrets). The Kubernetes runtime injects
// this same binary into every one of those workloads.
func workloadCommands() []entrypoint.Command {
	return []entrypoint.Command{entrypoint.StepCommand(), entrypoint.PrepareCommand(), entrypoint.ServiceRunCommand()}
}

func parseServerConfig(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	setting := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	flags := flag.NewFlagSet("kodama server", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: kodama server [flags]\n\nFlags override environment settings:")
		flags.PrintDefaults()
	}
	address := flags.String("http-addr", setting("KODAMA_HTTP_ADDR", ":8080"), "HTTP listen address (KODAMA_HTTP_ADDR)")
	level := flags.String("log-level", setting("KODAMA_LOG_LEVEL", "INFO"), "slog level (KODAMA_LOG_LEVEL)")
	timeout := flags.String("shutdown-timeout", setting("KODAMA_SHUTDOWN_TIMEOUT", "10s"), "drain duration, greater than 0 and at most 1m (KODAMA_SHUTDOWN_TIMEOUT)")
	assembly := flags.String("assembly", setting("KODAMA_ASSEMBLY", AssemblyScaffold), "composition: scaffold (health only), core or kernel (KODAMA_ASSEMBLY)")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("kodama server accepts flags only, not positional arguments")
	}
	if *address == "" || *level == "" || *timeout == "" || *assembly == "" {
		return Config{}, fmt.Errorf("server flag values must not be empty")
	}
	values := map[string]string{"KODAMA_HTTP_ADDR": *address, "KODAMA_LOG_LEVEL": *level, "KODAMA_SHUTDOWN_TIMEOUT": *timeout, "KODAMA_ASSEMBLY": *assembly}
	return LoadConfig(func(name string) string { return values[name] })
}
