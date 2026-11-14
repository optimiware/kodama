package cli

import (
	"fmt"
	"io"
	"strings"
)

// commandHelp holds concise usage and details for one command or subcommand.
type commandHelp struct {
	usage       string
	summary     string
	description string
}

var helpTopics = map[string]commandHelp{
	"login": {
		usage:   "kodactl login [--device | --no-browser]",
		summary: "Sign in via OIDC and persist a human session",
		description: `Discovers the Kodama OAuth/OIDC configuration from /.well-known/kodama-configuration
and signs in using browser PKCE or the headless RFC 8628 device flow.

Flags:
  --device, --no-browser   Force the headless OAuth device authorization flow`,
	},
	"logout": {
		usage:       "kodactl logout",
		summary:     "Revoke the refresh token and delete the local session",
		description: `Revokes the stored OIDC refresh token when bound to the configured server and deletes the local session file.`,
	},
	"refresh": {
		usage:       "kodactl refresh",
		summary:     "Rotate the stored human session before expiry",
		description: `Refreshes the stored human OIDC session under the cross-process session lock.`,
	},
	"status": {
		usage:       "kodactl status",
		summary:     "Show current human session issuer, server URL, and expiry",
		description: `Displays non-secret metadata about the persisted human session.`,
	},
	"whoami": {
		usage:   "kodactl whoami [--offline]",
		summary: "Show current principal identity, kind, scope, and roles",
		description: `Displays the caller's Kodama principal identity (oidc-<hash>), kind, tenant,
project, and roles. Queries principal.whoami on the server; a failed query is an
error. --offline derives oidc-<hash> from the saved session / JWT claims instead.

Flags:
  --offline   Derive principal identity locally from session/JWT without calling the server`,
	},
	"logs": {
		usage:   "kodactl logs <execution-id> [--action <id>] [--stream stdout|stderr] [-f|--follow]",
		summary: "Read or stream decoded execution action logs",
		description: `Reads retained container logs for an execution action, automatically decoding
base64 log chunks into raw bytes in text mode (or structured JSON with --output json).
When --action is omitted, selects the currently running action or the last started action.

Flags:
  --action <id>             Action ID (or action name) to read logs from
  --stream stdout|stderr    Log stream to read (default: stdout)
  -f, --follow              Poll until the action/execution reaches a terminal state`,
	},
	"inbox": {
		usage:   "kodactl inbox [--project <id>]",
		summary: "List pending request approvals, waiting tasks, and failed tasks",
		description: `Queries task.inbox for the project (defaulting to --project or KODAMA_PROJECT)
and renders a table of actionable items (KIND, TASK, EXECUTION, ACTION, PROMPT/SUMMARY).

Flags:
  --project <id>   Project ID (defaults to global --project or KODAMA_PROJECT)`,
	},
	"approve": {
		usage:   "kodactl approve <execution-id> [--action <id>] [--comment <text>] [--output <k=v>]",
		summary: "Approve a waiting execution request action",
		description: `Resolves a waiting request action on <execution-id> with decision "pass".
When --action is omitted, inspects the execution and selects the single waiting request action.

Flags:
  --action <id>      Waiting request action ID (auto-detected when omitted)
  --comment <text>   Optional approval comment output
  --output <k=v>     Declared request response output pair (repeatable)`,
	},
	"reject": {
		usage:   "kodactl reject <execution-id> [--action <id>] --reason <text> [--output <k=v>]",
		summary: "Reject a waiting execution request action",
		description: `Resolves a waiting request action on <execution-id> with decision "fail".
When --action is omitted, inspects the execution and selects the single waiting request action.

Flags:
  --action <id>     Waiting request action ID (auto-detected when omitted)
  --reason <text>   Required rejection reason
  --output <k=v>    Declared request response output pair (repeatable)`,
	},
	"task": {
		usage:   "kodactl task <subcommand> [arguments]",
		summary: "Create, run, list, inspect, reply to, and manage tasks",
		description: `Subcommands:
  create   kodactl task create --project <id> --title <t> [--description <d>] [--pipeline <id>]
  run      kodactl task run --project <id> --title <t> --pipeline <id> [--description <d>]
  list     kodactl task list --project <id>
  show     kodactl task show <task-id>
  start    kodactl task start <task-id> [--expected-version <v>]
  reply    kodactl task reply <task-id> --answer <text>
  cancel   kodactl task cancel <task-id> [--expected-version <v>]`,
	},
	"task create": {
		usage:   "kodactl task create [--project <id>] --title <t> [--description <d>] [--pipeline <id>] [--input <k=v>]",
		summary: "Create an open task in a project",
		description: `Invokes task.create with an auto-generated idempotency key.

Flags:
  --project <id>       Project ID (defaults to global --project or KODAMA_PROJECT)
  --title <text>       Task title (required)
  --description <text> Optional task description
  --pipeline <id>      Optional selected pipeline ID
  --input <k=v>        Pipeline input key=value pair (repeatable)`,
	},
	"task run": {
		usage:   "kodactl task run [--project <id>] --title <t> --pipeline <id> [--description <d>] [--input <k=v>]",
		summary: "Create and immediately start a task on a pipeline",
		description: `Invokes task.create_and_start with an auto-generated idempotency key.

Flags:
  --project <id>       Project ID (defaults to global --project or KODAMA_PROJECT)
  --title <text>       Task title (required)
  --pipeline <id>      Pipeline ID to run (required)
  --description <text> Optional task description
  --input <k=v>        Pipeline input key=value pair (repeatable)`,
	},
	"task list": {
		usage:       "kodactl task list [--project <id>]",
		summary:     "List tasks in a project",
		description: `Lists tasks for --project (or KODAMA_PROJECT).`,
	},
	"task show": {
		usage:       "kodactl task show <task-id>",
		summary:     "Read a task's state, versions, and outstanding question",
		description: `Invokes task.read for <task-id>. --output json prints the full task, including its messages.`,
	},
	"task start": {
		usage:       "kodactl task start <task-id> [--expected-version <v>]",
		summary:     "Start a new execution for an existing task",
		description: `Reads the current lifecycle version when --expected-version is omitted and invokes task.start.`,
	},
	"task reply": {
		usage:       "kodactl task reply <task-id> --answer <text>",
		summary:     "Answer a waiting task's outstanding question and resume execution",
		description: `Reads the waiting question ID and versions from task.read when omitted and invokes task.reply.`,
	},
	"task cancel": {
		usage:       "kodactl task cancel <task-id> [--expected-version <v>]",
		summary:     "Cancel a task",
		description: `Reads the current lifecycle version when --expected-version is omitted and invokes task.cancel.`,
	},
	"member": {
		usage:   "kodactl member <subcommand> [arguments]",
		summary: "List, grant, and revoke project memberships",
		description: `Subcommands:
  list     kodactl member list --project <id>
  grant    kodactl member grant <principal-or-subject> --project <id> --role <role>[,<role>...]
  revoke   kodactl member revoke <principal-or-subject> --project <id> [--expected-version <v>]

Zero-hash principal derivation:
  Targets starting with "oidc-" are used directly. Otherwise, pass a subject,
  --subject <sub>, or --github <login> (mapped to "github:<lowercase-login>"),
  and kodactl automatically discovers the OIDC issuer and derives oidc-<hash>.`,
	},
	"member list": {
		usage:       "kodactl member list [--project <id>]",
		summary:     "List project memberships and versions",
		description: `Invokes membership.list for --project (or KODAMA_PROJECT).`,
	},
	"member grant": {
		usage:   "kodactl member grant [<principal-or-subject>] [--subject <sub> | --github <login>] [--project <id>] --role <role>[,<role>...]",
		summary: "Grant project roles to a principal or OIDC subject",
		description: `Grants the complete role set (admin, engineer, reviewer, operator, viewer) via membership.grant.
Automatically derives oidc-<hash> when given an OIDC subject or --github <login>.`,
	},
	"member revoke": {
		usage:   "kodactl member revoke [<principal-or-subject>] [--subject <sub> | --github <login>] [--project <id>] [--expected-version <v>]",
		summary: "Revoke a principal's project membership",
		description: `Revokes a project membership via membership.revoke. Looks up the current
membership version via membership.list automatically when --expected-version is omitted.`,
	},
	"mcp": {
		usage:   "kodactl mcp <stdio|install> [arguments]",
		summary: "Bridge MCP over stdio or configure Claude Code, Cursor, or Codex",
		description: `Subcommands:
  stdio     Bridge newline-delimited JSON-RPC 2.0 on stdin/stdout to <API_URL>/mcp
  install   Print or write MCP server configuration for claude, cursor, or codex`,
	},
	"mcp stdio": {
		usage:   "kodactl mcp stdio",
		summary: "Run the authenticated JSON-RPC 2.0 stdio-to-HTTP MCP bridge",
		description: `Reads newline-delimited JSON-RPC 2.0 messages from stdin, forwards each to
<API_URL>/mcp with Bearer authentication (auto-refreshing OIDC sessions as needed)
and --ca-file TLS trust, and writes non-empty JSON-RPC responses as single lines to stdout.
A request that cannot be delivered is answered with a JSON-RPC error; the bridge keeps running.`,
	},
	"mcp install": {
		usage:   "kodactl mcp install [claude|cursor|codex] [--name kodama] [--config-file <path>] [--local] [--print]",
		summary: "Print or install the kodactl MCP server entry for an AI coding tool",
		description: `Configures an MCP client to invoke 'kodactl --url <url> [--ca-file <path>] mcp stdio'.

Targets:
  claude   ~/.claude.json (or .mcp.json with --local)
  cursor   ~/.cursor/mcp.json
  codex    ~/.codex/config.toml

Flags:
  --name <name>          MCP server name (default: kodama)
  --config-file <path>   Explicit config file path to update
  --local                Write .mcp.json in current directory (claude target only)
  --print                Print configuration snippet to stdout without writing files`,
	},
	"pipeline": {
		usage:   "kodactl pipeline apply <directory|file>",
		summary: "Apply the .yaml/.yml pipeline definitions in a directory, or one file",
		description: `Applies every non-hidden .yaml/.yml file in <directory>, or the one .yaml/.yml <file>, to the
configured project via pipeline.apply and prints each applied pipeline's ID for 'task run --pipeline'.`,
	},
	"inspect": {
		usage:       "kodactl inspect <execution-id>",
		summary:     "Inspect an execution and its actions",
		description: `Invokes execution.inspect once for <execution-id>.`,
	},
	"watch": {
		usage:       "kodactl watch <execution-id>",
		summary:     "Poll execution.inspect until the execution reaches a terminal status",
		description: `Polls execution.inspect until succeeded (exit 0) or failed/cancelled (exit 1).`,
	},
	"call": {
		usage:       "kodactl call <operation> [--input=<json>] [--expected-version=<n>] [--idempotency-key=<k>]",
		summary:     "Invoke any registered Kodama API operation directly",
		description: `Dispatches a raw public operation envelope over the configured REST client.`,
	},
	"helper": {
		usage:       "kodactl helper <get|store|erase> <url>",
		summary:     "Git credential helper for registered trusted origins",
		description: `Provides Git HTTPS credentials strictly for configured trusted origins.`,
	},
	"version": {
		usage:       "kodactl version",
		summary:     "Print kodactl version and source revision",
		description: `Prints build version, VCS revision, Go toolchain version, and platform.`,
	},
	"help": {
		usage:       "kodactl help [command [subcommand]]",
		summary:     "Show usage for kodactl or a specific command",
		description: `Prints top-level or command-specific usage information.`,
	},
}

// writeCommandSummary writes the concise overview of kodactl global flags and
// commands to out.
func writeCommandSummary(out io.Writer) {
	fmt.Fprint(out, `Usage:
  kodactl [global flags] <command> [arguments]

Commands:
  login       Sign in via OIDC and persist a human session
  logout      Revoke the refresh token and delete the local session
  refresh     Rotate the stored human session before expiry
  status      Show current human session status
  whoami      Show current principal identity, scope, and roles
  inbox       List pending approvals, waiting tasks, and failed tasks
  approve     Approve a waiting execution request action
  reject      Reject a waiting execution request action
  logs        Read or stream decoded execution action logs
  inspect     Inspect an execution
  watch       Poll an execution until it reaches a terminal state
  task        Create, run, list, show, start, reply to, and cancel tasks
  pipeline    Apply local pipeline YAML files
  member      List, grant, and revoke project memberships
  mcp         Run the MCP stdio bridge or install editor configuration
  call        Invoke any registered public API operation directly
  helper      Git credential helper for trusted origins
  version     Print version and source revision
  help        Show help for kodactl or a command

Global Flags:
  --server, --server-url, --url <url>   Kodama server URL ($KODAMA_SERVER / $KODAMA_URL)
  --ca-file <path>                      PEM CA bundle for TLS trust ($KODAMA_CA_FILE)
  --project <id>                        Default project ID ($KODAMA_PROJECT)
  --output <table|json>                 Output format ($KODAMA_OUTPUT)
  --session, --session-file <path>      Human session file ($KODAMA_SESSION)
  --credential-file <path>              Mounted execution token file ($KODAMA_CREDENTIAL_FILE)
  --config <path>                       Configuration file ($KODAMA_CONFIG)
  -h, --help                            Show help and exit

Run 'kodactl help <command>' for details on a specific command.
`)
}

// runHelp renders top-level or command-specific help to out and exits 0, or
// reports an unknown help topic on errOut with ExitUsage.
func runHelp(out, errOut io.Writer, args []string) int {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			continue
		}
		filtered = append(filtered, arg)
	}
	if len(filtered) == 0 {
		writeCommandSummary(out)
		return ExitSuccess
	}
	if len(filtered) >= 2 {
		compound := filtered[0] + " " + filtered[1]
		if topic, ok := helpTopics[compound]; ok && len(filtered) == 2 {
			writeTopicHelp(out, topic)
			return ExitSuccess
		}
	}
	if len(filtered) == 1 {
		if topic, ok := helpTopics[filtered[0]]; ok {
			writeTopicHelp(out, topic)
			return ExitSuccess
		}
	}
	return usage(errOut, fmt.Sprintf("unknown help topic %q", strings.Join(filtered, " ")))
}

func writeTopicHelp(out io.Writer, topic commandHelp) {
	fmt.Fprintf(out, "%s\n\nUsage:\n  %s\n", topic.summary, topic.usage)
	if topic.description != "" {
		fmt.Fprintf(out, "\n%s\n", topic.description)
	}
}
