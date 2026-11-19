// Command kodema is the single entrypoint: the control plane and its
// companions, the client on a workstation and the toolbox inside a container
// action. It dispatches one explicit subcommand and never starts an unrelated
// process: `kodema git serve` does not open the Shell store, `kodema agent`
// does not start a server, and a client command does neither.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/optimiware/kodema/cmd/kodema/internal/clientcmd"
)

const usageText = "usage: kodema <command> [flags]\n\n" + installationText

// installationText lists the commands that run an installation. Every other
// command is a client command, listed by `kodema help`.
const installationText = `installation commands:
  up            start a local Kodema on Podman (then: kodema run <pipeline.yaml>)
  down          stop the local Kodema (--purge removes its state)
  status        inspect the local Kodema
  serve         run the Shell composition (--profile development, test or production)
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

Run "kodema <command> --help" for the flags of an installation command.
`

// runner is one dispatched subcommand.
type runner func(context.Context, []string, io.Writer, io.Writer) int

// deps are the injectable subcommand runners. Tests use them to prove dispatch
// isolation: one command never starts another command's process.
type deps struct {
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
	// client runs every command the installation does not own: the client
	// commands over the public API and the toolbox commands of an action.
	client runner
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps()))
}

func defaultDeps() deps {
	return deps{
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
		client:                 runClientEntry,
	}
}

// runClientEntry runs one client command. It opens no store and no listener.
func runClientEntry(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return clientcmd.Run(ctx, args, stdout, stderr, printVersion)
}

// run dispatches one command and returns its exit code. It never calls os.Exit.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, deps deps) int {
	if len(args) == 0 {
		usage(stderr, "a command is required")
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) > 1 {
			return deps.client(ctx, append([]string{"help"}, args[1:]...), stdout, stderr)
		}
		if code := deps.client(ctx, []string{"help"}, stdout, stderr); code != 0 {
			return code
		}
		fmt.Fprint(stdout, "\n"+installationText)
		return 0
	case "version", "--version":
		printVersion(stdout)
		return 0
	case "up":
		return deps.up(ctx, args[1:], stdout, stderr)
	case "down":
		return deps.down(ctx, args[1:], stdout, stderr)
	case "status":
		return deps.status(ctx, args[1:], stdout, stderr)
	case "serve", "server":
		return deps.server(ctx, args[1:], stdout, stderr)
	case "git":
		if len(args) > 1 {
			switch args[1] {
			case "serve":
				return deps.gitServe(ctx, args[2:], stdout, stderr)
			case "hook":
				return deps.gitHook(ctx, args[2:], stdout, stderr)
			case "provision-tenant":
				return deps.gitProvision(ctx, args[2:], stdout, stderr)
			case "provision-repository":
				return deps.gitProvisionRepository(ctx, args[2:], stdout, stderr)
			}
		}
		// `git credential` is the client's.
		return deps.client(ctx, args, stdout, stderr)
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
		return deps.client(ctx, args, stdout, stderr)
	}
}

// usage writes one error line and the usage block to stderr.
func usage(stderr io.Writer, message string) {
	fmt.Fprintf(stderr, "kodema: %s\n%s", message, usageText)
}
