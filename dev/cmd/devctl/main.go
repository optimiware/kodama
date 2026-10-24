// Command devctl manages one scoped development environment.
//
// It replaces scripts/dev/environment.py and scripts/dev/teardown_check.py.
// The Makefile is the documented entry point:
//
//	make dev-up DEV_SCOPE=t026 DEV_STACK=services
//	make dev-preflight DEV_SCOPE=t026
//	make dev-down DEV_SCOPE=t026
//	make tools
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
		return fmt.Errorf("usage: devctl <up|down|preflight|integration|smoke|runtime|diagnostics|teardown-check|tools> [flags]")
	}
	command := args[0]
	if command == "tools" {
		return tools(args[1:])
	}
	scope, provider, stack, rest, err := parseScopeFlags(args[1:])
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected arguments: %v", rest)
	}
	requested, err := devenv.RequestedStack(stack, os.Environ())
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
	err = execute(environment, command)
	if err != nil && command != "diagnostics" && command != "preflight" {
		if diagnosticErr := environment.Diagnostics(); diagnosticErr != nil {
			fmt.Fprintln(os.Stderr, "Diagnostic collection failed: "+diagnosticErr.Error())
		}
	}
	return err
}

func execute(e *devenv.Environment, command string) error {
	switch command {
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

func parseScopeFlags(args []string) (scope, provider, stack string, rest []string, err error) {
	scope = envOr("DEV_SCOPE", "t-001-local")
	provider = envOr("DEV_PROVIDER", "podman")
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
			if scope, err = next(); err != nil {
				return
			}
		case "--provider":
			if provider, err = next(); err != nil {
				return
			}
		case "--stack":
			if stack, err = next(); err != nil {
				return
			}
		default:
			rest = append(rest, value)
		}
	}
	if err = devenv.ValidateScope(scope); err == nil {
		err = devenv.ValidateProvider(provider)
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
