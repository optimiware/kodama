// Command devctl manages one scoped development environment.
//
// It replaces scripts/dev/environment.py and scripts/dev/teardown_check.py.
// The Makefile is the documented entry point:
//
//	make dev-up DEV_SCOPE=t026 DEV_STACK=services
//	make dev-preflight DEV_SCOPE=t026
//	make dev-down DEV_SCOPE=t026
//	make tools
//
// A services scope may host Git with the kernel's own workers instead of the
// compatibility fixture. It is selected explicitly, once per scope, and each
// tenant's worker is a container started on its own:
//
//	make dev-up DEV_SCOPE=t026 DEV_STACK=services DEV_GIT=native
//	go run ./dev/cmd/devctl git-worker --scope t026 --tenant t_acme [--control-url http://host.containers.internal:8080]
package main

import (
	"fmt"
	"os"

	"github.com/optimiware/kodama/dev/internal/devenv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Development environment failed: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devctl <up|down|preflight|integration|smoke|runtime|diagnostics|teardown-check|git-worker|tools> [flags]")
	}
	command := args[0]
	if command == "tools" {
		return tools(args[1:])
	}
	options, err := parseScopeFlags(args[1:])
	if err != nil {
		return err
	}
	scope, provider := options.scope, options.provider
	if len(options.rest) > 0 {
		return fmt.Errorf("unexpected arguments: %v", options.rest)
	}
	if command != "git-worker" && (options.tenant != "" || options.controlURL != "") {
		return fmt.Errorf("--tenant and --control-url belong to the git-worker command")
	}
	requested, err := devenv.RequestedStack(options.stack, os.Environ())
	if err != nil {
		return err
	}
	requestedGit, err := devenv.RequestedGitHosting(options.git, os.Environ())
	if err != nil {
		return err
	}
	release, err := devenv.LockScope(scope)
	if err != nil {
		return err
	}
	defer release()
	environment, err := devenv.Load(scope, provider, requested)
	if err != nil {
		return err
	}
	environment.RequestedGit = requestedGit
	err = execute(environment, command, options)
	if err != nil && command != "diagnostics" && command != "preflight" {
		if diagnosticErr := environment.Diagnostics(); diagnosticErr != nil {
			fmt.Fprintln(os.Stderr, "Diagnostic collection failed: "+diagnosticErr.Error())
		}
	}
	return err
}

func execute(e *devenv.Environment, command string, options flags) error {
	switch command {
	case "git-worker":
		if options.tenant == "" {
			return fmt.Errorf("git-worker needs --tenant <tenant-id>")
		}
		return e.GitWorker(options.tenant, options.controlURL)
	case "up":
		return e.Up()
	case "down":
		return e.Down()
	case "preflight":
		return e.Preflight()
	case "integration":
		return e.Integration()
	case "smoke":
		return e.Smoke()
	case "runtime":
		return e.RuntimeAcceptance()
	case "diagnostics":
		return e.Diagnostics()
	case "teardown-check":
		return devenv.TeardownCheck(e.Scope, e.Provider)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// flags are the command line of every devctl command.
type flags struct {
	scope, provider, stack, git string
	tenant, controlURL          string
	rest                        []string
}

func parseScopeFlags(args []string) (options flags, err error) {
	options.scope = envOr("DEV_SCOPE", "t-001-local")
	options.provider = envOr("DEV_PROVIDER", "podman")
	for index := 0; index < len(args); index++ {
		value := args[index]
		next := func() (string, error) {
			if index+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", value)
			}
			index++
			return args[index], nil
		}
		switch value {
		case "--scope":
			if options.scope, err = next(); err != nil {
				return
			}
		case "--provider":
			if options.provider, err = next(); err != nil {
				return
			}
		case "--stack":
			if options.stack, err = next(); err != nil {
				return
			}
		case "--git":
			if options.git, err = next(); err != nil {
				return
			}
		case "--tenant":
			if options.tenant, err = next(); err != nil {
				return
			}
		case "--control-url":
			if options.controlURL, err = next(); err != nil {
				return
			}
		default:
			options.rest = append(options.rest, value)
		}
	}
	if err = devenv.ValidateScope(options.scope); err == nil {
		err = devenv.ValidateProvider(options.provider)
	}
	return
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func tools(args []string) error {
	if len(args) == 0 {
		for _, name := range []string{"kind", "kubectl"} {
			path, err := devenv.InstallTool(name)
			if err != nil {
				return err
			}
			fmt.Println(path)
		}
		return nil
	}
	switch args[0] {
	case "path":
		if len(args) != 2 {
			return fmt.Errorf("usage: devctl tools path <kind|kubectl>")
		}
		path, err := devenv.InstallTool(args[1])
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	case "image":
		if len(args) != 2 || args[1] != "node" {
			return fmt.Errorf("usage: devctl tools image node")
		}
		lock, err := devenv.Versions()
		if err != nil {
			return err
		}
		fmt.Println(lock.Images.Node)
		return nil
	default:
		return fmt.Errorf("unknown tools command %q", args[0])
	}
}
