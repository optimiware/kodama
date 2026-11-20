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
		usage:   "kodema login [--device | --no-browser]",
		summary: "Sign in via OIDC and persist a human session",
		description: `Discovers the Kodema OAuth/OIDC configuration from /.well-known/kodema-configuration
and signs in using browser PKCE or the headless RFC 8628 device flow.

Flags:
  --device, --no-browser   Force the headless OAuth device authorization flow`,
	},
	"logout": {
		usage:       "kodema logout",
		summary:     "Revoke the refresh token and delete the local session",
		description: `Revokes the stored OIDC refresh token when bound to the configured server and deletes the local session file.`,
	},
	"refresh": {
		usage:       "kodema refresh",
		summary:     "Rotate the stored human session before expiry",
		description: `Refreshes the stored human OIDC session under the cross-process session lock.`,
	},
	"session": {
		usage:       "kodema session",
		summary:     "Show current human session issuer, server URL, and expiry",
		description: `Displays non-secret metadata about the persisted human session.`,
	},
	"whoami": {
		usage:   "kodema whoami [--offline]",
		summary: "Show current principal identity, kind, scope, and roles",
		description: `Displays the caller's Kodema principal identity (oidc-<hash>), kind, tenant,
project, and roles. Queries principal.whoami on the server; a failed query is an
error. --offline derives oidc-<hash> from the saved session / JWT claims instead.

Flags:
  --offline   Derive principal identity locally from session/JWT without calling the server`,
	},
	"logs": {
		usage:   "kodema logs <execution-id> [--action <id>] [--stream stdout|stderr] [-f|--follow]",
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
		usage:   "kodema inbox [--project <id>]",
		summary: "List pending request approvals, waiting tasks, and failed tasks",
		description: `Queries task.inbox for the project (defaulting to --project or KODEMA_PROJECT)
and renders a table of actionable items (KIND, TASK, EXECUTION, ACTION, PROMPT/SUMMARY).

Flags:
  --project <id>   Project ID (defaults to global --project or KODEMA_PROJECT)`,
	},
	"approve": {
		usage:   "kodema approve <execution-id> [--action <id>] [--comment <text>] [--output <k=v>]",
		summary: "Approve a waiting execution request action",
		description: `Resolves a waiting request action on <execution-id> with decision "pass".
When --action is omitted, inspects the execution and selects the single waiting request action.

Flags:
  --action <id>      Waiting request action ID (auto-detected when omitted)
  --comment <text>   Optional approval comment output
  --output <k=v>     Declared request response output pair (repeatable)`,
	},
	"reject": {
		usage:   "kodema reject <execution-id> [--action <id>] --reason <text> [--output <k=v>]",
		summary: "Reject a waiting execution request action",
		description: `Resolves a waiting request action on <execution-id> with decision "fail".
When --action is omitted, inspects the execution and selects the single waiting request action.

Flags:
  --action <id>     Waiting request action ID (auto-detected when omitted)
  --reason <text>   Required rejection reason
  --output <k=v>    Declared request response output pair (repeatable)`,
	},
	"task": {
		usage:   "kodema task <subcommand> [arguments]",
		summary: "Create, run, list, inspect, reply to, and manage tasks",
		description: `Subcommands:
  create   kodema task create --project <id> --title <t> [--description <d>] [--pipeline <id>]
  run      kodema task run --project <id> --title <t> --pipeline <id> [--description <d>]
  list     kodema task list --project <id>
  show     kodema task show <task-id>
  start    kodema task start <task-id> [--expected-version <v>]
  reply    kodema task reply <task-id> --answer <text>
  cancel   kodema task cancel <task-id> [--expected-version <v>]`,
	},
	"task create": {
		usage:   "kodema task create [--project <id>] --title <t> [--description <d>] [--pipeline <id>] [--input <k=v>]",
		summary: "Create an open task in a project",
		description: `Invokes task.create with an auto-generated idempotency key.

Flags:
  --project <id>       Project ID (defaults to global --project or KODEMA_PROJECT)
  --title <text>       Task title (required)
  --description <text> Optional task description
  --pipeline <id>      Optional selected pipeline ID
  --input <k=v>        Pipeline input key=value pair (repeatable)`,
	},
	"task run": {
		usage:   "kodema task run [--project <id>] --title <t> --pipeline <id> [--description <d>] [--input <k=v>]",
		summary: "Create and immediately start a task on a pipeline",
		description: `Invokes task.create_and_start with an auto-generated idempotency key.

Flags:
  --project <id>       Project ID (defaults to global --project or KODEMA_PROJECT)
  --title <text>       Task title (required)
  --pipeline <id>      Pipeline ID to run (required)
  --description <text> Optional task description
  --input <k=v>        Pipeline input key=value pair (repeatable)`,
	},
	"task list": {
		usage:       "kodema task list [--project <id>]",
		summary:     "List tasks in a project",
		description: `Lists tasks for --project (or KODEMA_PROJECT).`,
	},
	"task show": {
		usage:       "kodema task show <task-id>",
		summary:     "Read a task's state, versions, and outstanding question",
		description: `Invokes task.read for <task-id>. --output json prints the full task, including its messages.`,
	},
	"task start": {
		usage:       "kodema task start <task-id> [--expected-version <v>]",
		summary:     "Start a new execution for an existing task",
		description: `Reads the current lifecycle version when --expected-version is omitted and invokes task.start.`,
	},
	"task reply": {
		usage:       "kodema task reply <task-id> --answer <text>",
		summary:     "Answer a waiting task's outstanding question and resume execution",
		description: `Reads the waiting question ID and versions from task.read when omitted and invokes task.reply.`,
	},
	"task cancel": {
		usage:       "kodema task cancel <task-id> [--expected-version <v>]",
		summary:     "Cancel a task",
		description: `Reads the current lifecycle version when --expected-version is omitted and invokes task.cancel.`,
	},
	"member": {
		usage:   "kodema member <subcommand> [arguments]",
		summary: "List, grant, and revoke project memberships",
		description: `Subcommands:
  list     kodema member list --project <id>
  grant    kodema member grant <principal-or-subject> --project <id> --role <role>[,<role>...]
  revoke   kodema member revoke <principal-or-subject> --project <id> [--expected-version <v>]

Zero-hash principal derivation:
  Targets starting with "oidc-" are used directly. Otherwise, pass a subject,
  --subject <sub>, or --github <login> (mapped to "github:<lowercase-login>"),
  and kodema automatically discovers the OIDC issuer and derives oidc-<hash>.`,
	},
	"member list": {
		usage:       "kodema member list [--project <id>]",
		summary:     "List project memberships and versions",
		description: `Invokes membership.list for --project (or KODEMA_PROJECT).`,
	},
	"member grant": {
		usage:   "kodema member grant [<principal-or-subject>] [--subject <sub> | --github <login>] [--project <id>] --role <role>[,<role>...]",
		summary: "Grant project roles to a principal or OIDC subject",
		description: `Grants the complete role set (admin, engineer, reviewer, operator, viewer) via membership.grant.
Automatically derives oidc-<hash> when given an OIDC subject or --github <login>.`,
	},
	"member revoke": {
		usage:   "kodema member revoke [<principal-or-subject>] [--subject <sub> | --github <login>] [--project <id>] [--expected-version <v>]",
		summary: "Revoke a principal's project membership",
		description: `Revokes a project membership via membership.revoke. Looks up the current
membership version via membership.list automatically when --expected-version is omitted.`,
	},
	"mcp": {
		usage:   "kodema mcp <stdio|install> [arguments]",
		summary: "Bridge MCP over stdio or configure Claude Code, Cursor, or Codex",
		description: `Subcommands:
  stdio     Bridge newline-delimited JSON-RPC 2.0 on stdin/stdout to <API_URL>/mcp
  install   Print or write MCP server configuration for claude, cursor, or codex`,
	},
	"mcp stdio": {
		usage:   "kodema mcp stdio",
		summary: "Run the authenticated JSON-RPC 2.0 stdio-to-HTTP MCP bridge",
		description: `Reads newline-delimited JSON-RPC 2.0 messages from stdin, forwards each to
<API_URL>/mcp with Bearer authentication (auto-refreshing OIDC sessions as needed)
and --ca-file TLS trust, and writes non-empty JSON-RPC responses as single lines to stdout.
A request that cannot be delivered is answered with a JSON-RPC error; the bridge keeps running.`,
	},
	"mcp install": {
		usage:   "kodema mcp install [claude|cursor|codex] [--name kodema] [--config-file <path>] [--local] [--print]",
		summary: "Print or install the kodema MCP server entry for an AI coding tool",
		description: `Configures an MCP client to invoke 'kodema --url <url> [--ca-file <path>] mcp stdio'.

Targets:
  claude   ~/.claude.json (or .mcp.json with --local)
  cursor   ~/.cursor/mcp.json
  codex    ~/.codex/config.toml

Flags:
  --name <name>          MCP server name (default: kodema)
  --config-file <path>   Explicit config file path to update
  --local                Write .mcp.json in current directory (claude target only)
  --print                Print configuration snippet to stdout without writing files`,
	},
	"pipeline": {
		usage:   "kodema pipeline apply <directory|file>",
		summary: "Apply the .yaml/.yml pipeline definitions in a directory, or one file",
		description: `Applies every non-hidden .yaml/.yml file in <directory>, or the one .yaml/.yml <file>, to the
configured project via pipeline.apply and prints each applied pipeline's ID for 'task run --pipeline'.`,
	},
	"run": {
		usage:   "kodema run <file> [--project <id>] [--input <k=v>] [-d|--detach]",
		summary: "Run one local pipeline file and follow it to its end",
		description: `Starts the pipeline YAML in <file> inline via pipeline.start (nothing is applied or stored)
and follows the execution to its end unless --detach is set. With --detach, prints the
execution ID to stdout and exits 0 immediately after admission.

When following, the execution ID, action-state transitions (including waiting notices for
service/* requests), any failing container's stderr tail, and the final outcome go to
stderr, keeping stdout clean for container stdout (printed in action order once settled).

Exit codes:
  0   Execution succeeded (or admitted with --detach)
  1   Execution failed, was cancelled, or paused at a human role/* request gate
      (leaves the execution running; answer via 'kodema inbox' and 'kodema approve')
  2   Usage or validation error

Flags:
  --project <id>   Project ID (defaults to global --project or KODEMA_PROJECT)
  --input <k=v>    Pipeline input key=value pair (repeatable)
  -d, --detach     Print the execution ID and exit 0 immediately without waiting`,
	},
	"inspect": {
		usage:       "kodema inspect <execution-id>",
		summary:     "Inspect an execution and its actions",
		description: `Invokes execution.inspect once for <execution-id>.`,
	},
	"watch": {
		usage:       "kodema watch <execution-id>",
		summary:     "Poll execution.inspect until the execution reaches a terminal status",
		description: `Polls execution.inspect until succeeded (exit 0) or failed/cancelled (exit 1).`,
	},
	"call": {
		usage:       "kodema call <operation> [--input=<json>] [--expected-version=<n>] [--idempotency-key=<k>]",
		summary:     "Invoke any registered Kodema API operation directly",
		description: `Dispatches a raw public operation envelope over the configured REST client.`,
	},
	"git": {
		usage:   "kodema git <subcommand> [arguments]",
		summary: "Git credential helper for registered trusted origins",
		description: `Subcommands:
  credential   kodema git credential <get|store|erase> [<url>]`,
	},
	"git credential": {
		usage:   "kodema git credential <get|store|erase> [<url>]",
		summary: "Git credential helper for registered trusted origins",
		description: `Speaks Git's credential-helper protocol: Git passes the operation as the
argument and the request (protocol, host, path) on standard input. Configure it
as the only helper of one origin, so no keychain or store keeps a Kodema token:
  git config --global credential.https://<host>.helper ''
  git config --global --add credential.https://<host>.helper \
    '!kodema --trusted-origin https://<host> git credential'
It answers get with the current credential (the human session, or the execution
token inside a container action) for a trusted origin only; any other request
gets an empty answer. When --server is omitted, the session file's api_url (or
the running local default instance) is used. Credential or TLS errors for a
trusted origin are reported on stderr while keeping stdout empty for Git.
store and erase do nothing. A <url> argument replaces the request on standard
input. Inside a container action Kodema configures it itself.

Global Flags:
  --trusted-origin <url>   Registered trusted Git HTTPS origin (repeatable)
  --session <path>         Human session file ($KODEMA_SESSION)
  --ca-file <path>         PEM CA bundle for TLS trust ($KODEMA_CA_FILE)
  --server <url>           Kodema server URL (defaults to session api_url or default instance)`,
	},
	"output": {
		usage:   "kodema output <subcommand> [arguments]",
		summary: "Submit the outputs of the running container action",
		description: `Subcommands:
  push   kodema output push [--dir <path>] [--execution <id>] [--action <name>]`,
	},
	"output push": {
		usage:   "kodema output push [--dir <path>] [--execution <id>] [--action <name>]",
		summary: "Submit the files of the action's output directory as its outputs",
		description: `Reads every file of /kodema/outputs/<action> and submits them in one request as the
outputs of the running container action: the file name is the output key and the
content its value, with one trailing newline removed. Kodema runs it as the last
command of a container action that declares outputs; an empty or absent directory
submits nothing.

Flags:
  --dir <path>         Output directory (default: /kodema/outputs/<action>)
  --execution <id>     Execution ID (default: $KODEMA_EXECUTION)
  --action <name>      Action name (default: $KODEMA_ACTION)`,
	},
	"version": {
		usage:       "kodema version",
		summary:     "Print kodema version and source revision",
		description: `Prints build version, VCS revision, Go toolchain version, and platform.`,
	},
	"help": {
		usage:       "kodema help [command [subcommand]]",
		summary:     "Show usage for kodema or a specific command",
		description: `Prints top-level or command-specific usage information.`,
	},
}

// writeCommandSummary writes the concise overview of kodema global flags and
// commands to out.
func writeCommandSummary(out io.Writer) {
	fmt.Fprint(out, `Usage:
  kodema [global flags] <command> [arguments]

Commands:
  login       Sign in via OIDC and persist a human session
  logout      Revoke the refresh token and delete the local session
  refresh     Rotate the stored human session before expiry
  session     Show current human session status
  whoami      Show current principal identity, scope, and roles
  run         Run one local pipeline file and follow it to its end
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
  git         Git credential helper for trusted origins
  output      Submit the outputs of the running container action
  version     Print version and source revision
  help        Show help for kodema or a command

Global Flags:
  --server, --server-url, --url <url>   Kodema server URL ($KODEMA_SERVER / $KODEMA_URL)
  --ca-file <path>                      PEM CA bundle for TLS trust ($KODEMA_CA_FILE)
  --project <id>                        Default project ID ($KODEMA_PROJECT)
  --output <table|json>                 Output format ($KODEMA_OUTPUT)
  --session, --session-file <path>      Human session file ($KODEMA_SESSION)
  --credential-file <path>              Mounted execution token file ($KODEMA_CREDENTIAL_FILE)
  --trusted-origin <url>                Registered trusted Git origin for git credential (repeatable)
  --config <path>                       Configuration file ($KODEMA_CONFIG)
  -h, --help                            Show help and exit

Run 'kodema help <command>' for details on a specific command.
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
