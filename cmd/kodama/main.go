// Command kodama is the single composition entrypoint. It dispatches one
// explicit subcommand and never starts an unrelated process: `kodama git serve`
// does not open the Shell store and `kodama agent` does not start a server.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usageText = `usage: kodama <command> [flags]

commands:
  dev           run the instant single-origin local dev server (zero containers)
  up            start the single-origin container stack (Podman or Docker)
  down          stop the single-origin container stack (--purge removes state)
  status        inspect the single-origin container stack
  server        run the Shell composition (--profile development, test or production)
  git serve     run the independent Git smart-HTTP service
  git hook      run one native Git hook callback
  git provision-tenant  create the tenant's trusted admin repository
  git provision-repository  initialize a repository and commit its default access rules
  auth serve    run the built-in OIDC provider and social login broker
  auth principal-id  derive the human principal id for an OIDC issuer and subject
  agent         run one bounded native-agent run
  services      run one independent service adapter
  migrate       apply database migrations and set role passwords (administrator)
  bootstrap-admin  create the first tenant and administrator from an OIDC token
  version       print the version and source revision of this binary

Run "kodama <command> --help" for command flags.
`

// runner is one dispatched subcommand.
type runner func(context.Context, []string, io.Writer, io.Writer) int

// deps are the injectable subcommand runners. Tests use them to prove dispatch
// isolation: one command never starts another command's process.
type deps struct {
	dev                    runner
	up                     runner
	down                   runner
	status                 runner
	server                 runner
	gitServe               runner
	gitHook                runner
	gitProvision           runner
	gitProvisionRepository runner
	authServe              runner
	authPrincipalID        runner
	agent                  runner
	services               runner
	migrate                runner
	bootstrap              runner
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps()))
}

func defaultDeps() deps {
	return deps{
		dev:                    runDevEntry,
		up:                     runUpEntry,
		down:                   runDownEntry,
		status:                 runStatusEntry,
		server:                 runServerEntry,
		gitServe:               runGitServeEntry,
		gitHook:                runGitHookEntry,
		gitProvision:           runGitProvisionEntry,
		gitProvisionRepository: runGitProvisionRepositoryEntry,
		authServe:              runAuthServeEntry,
		authPrincipalID:        runAuthPrincipalIDEntry,
		agent:                  runAgentEntry,
		services:               runServicesEntry,
		migrate:                runMigrateEntry,
		bootstrap:              runBootstrapAdminEntry,
	}
}

// run dispatches one command and returns its exit code. It never calls os.Exit.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps deps) int {
	if len(args) == 0 {
		usage(stderr, "a command is required")
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "version", "--version":
		printVersion(stdout)
		return 0
	case "dev":
		return deps.dev(ctx, args[1:], stdout, stderr)
	case "up":
		return deps.up(ctx, args[1:], stdout, stderr)
	case "down":
		return deps.down(ctx, args[1:], stdout, stderr)
	case "status":
		return deps.status(ctx, args[1:], stdout, stderr)
	case "server":
		return deps.server(ctx, args[1:], stdout, stderr)
	case "git":
		if len(args) < 2 {
			usage(stderr, "git requires a subcommand")
			return 2
		}
		switch args[1] {
		case "serve":
			return deps.gitServe(ctx, args[2:], stdout, stderr)
		case "hook":
			return deps.gitHook(ctx, args[2:], stdout, stderr)
		case "provision-tenant":
			return deps.gitProvision(ctx, args[2:], stdout, stderr)
		case "provision-repository":
			return deps.gitProvisionRepository(ctx, args[2:], stdout, stderr)
		default:
			usage(stderr, fmt.Sprintf("unknown git subcommand %q", args[1]))
			return 2
		}
	case "auth":
		if len(args) < 2 {
			usage(stderr, "auth requires a subcommand")
			return 2
		}
		switch args[1] {
		case "serve":
			return deps.authServe(ctx, args[2:], stdout, stderr)
		case "principal-id":
			return deps.authPrincipalID(ctx, args[2:], stdout, stderr)
		default:
			usage(stderr, fmt.Sprintf("unknown auth subcommand %q", args[1]))
			return 2
		}
	case "agent":
		return deps.agent(ctx, args[1:], stdout, stderr)
	case "services":
		return deps.services(ctx, args[1:], stdout, stderr)
	case "migrate":
		return deps.migrate(ctx, args[1:], stdout, stderr)
	case "bootstrap-admin":
		return deps.bootstrap(ctx, args[1:], stdout, stderr)
	default:
		usage(stderr, fmt.Sprintf("unknown command %q", args[0]))
		return 2
	}
}

// usage writes one error line and the usage block to stderr.
func usage(stderr io.Writer, message string) {
	fmt.Fprintf(stderr, "kodama: %s\n%s", message, usageText)
}
