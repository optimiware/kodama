# Kodama domain language

Kodama is a four-layer software factory ([ADR 0013](docs/adr/0013-two-layer-architecture-and-domain-simplification.md)):
- **Layer 1 (Pipeline Runtime kernel):** pipelines, runs, steps, containers, shared workspaces, support services, declared secrets, durable waits, cancellation, and recovery.
- **Layer 2 (Native Shell):** identity, projects, the project pipeline registry, tasks, native Git, secret custody, metadata artifacts, long-lived services, HTTP API, Kodama MCP server, CLI, persistence/runtime adapters, and the central NATS bus.
- **Layer 3 (Agentic services):** sessions, harnesses, tools, AI gateway, and external MCP proxy.
- **Layer 4 (External behavior):** pipeline YAML, scripts, container images, hooks, and delivery/QA/notification recipes.

## Tenancy and permissions

**Platform core**: Optimi's company-wide authority for tenant identity, entitlements, and core operations ([platform integration](docs/specs/platform-integration.md)).

**Tenant**: An isolated organization owning projects, memberships, tenant-level services, and a tenant-level knowledge base (`<tenant>/context`).
_Avoid_: Account, customer workspace.

**Project**: A tenant-owned space grouping repositories (`<tenant>/<project>/*`), project-unique pipelines, tasks, services, secrets, and artifacts.

**Membership**: A user's role binding (`admin`, `engineer`, `reviewer`, `operator`, `viewer`) at tenant or project scope, authenticated via external OAuth/OIDC.

**Project-space permission model**: User RBAC controls what a person can configure on Kodama (projects, pipelines, secrets, tasks RW, inbox resolution). Agents and pipeline steps executing inside a project automatically have access to what has been provisioned and configured in that project space.

## Layer 1: Pipeline runtime

**Pipeline**: The sole process definition for a task or trigger. A pipeline is defined in `.kodama/<processname>.yaml` in a repository or registered through the API, and is persisted as one complete document. Pipeline names are unique within a project.

**Run**: One execution of a pipeline against pinned inputs and optional source commits. A run snapshots its pipeline definition and inputs at admission. A task may accumulate several runs through reruns or rework.

**Step (`run_step`)**: One position in a run's execution order. A container step executes an image and commands; a wait step pauses the run without a workload; a control step evaluates declared conditions. Steps share the run workspace.

**Wait**: A durable, non-container step that pauses a run until an authorized correlated signal or deadline arrives. It records its identity, expected input, accepted result, and continuation position. A signal is not itself approval.

**Workspace**: The disposable filesystem shared by the steps of one run. It is removed when the run finishes and is never the durable source authority.

**Service**: A support container with a tenant, project, or run lifetime. Run-scoped services start and stop with the run; their state is never silently recreated after loss.

**Secret reference**: A named credential a step may request. The kernel delivers only declared references; encryption, custody, scope authorization, and rotation belong to the shell.

## Layer 2: Native shell

**Task**: A tracked unit of work or request. A task selects one project pipeline and records execution progress: current run, status, current step, waiting assignment, and result. Its progress is derived from run and step records, not a separate project workflow.
_Avoid_: Ticket, card, workflow state machine, parked blocker.

**Role inbox**: A query of tasks whose current pipeline wait is assigned to the caller's role. Resolution records an authorized input against the exact run, step, and wait; the pipeline decides whether the outcome approves, rejects, or requests rework.

**Repository**: A Git repository hosted in Kodama's native tenant Git service using a Gerrit-style hierarchical path (`<tenant>/context` or `<tenant>/<project>/<name>`). Repositories under `<tenant>/<project>/*` are automatically associated with `<project>`.

**Knowledge base**: Markdown repositories in the native Git service storing skills, context, blueprints, and engineering guidelines (`<tenant>/context`, `<tenant>/<project>/context`, `<tenant>/<project>/<component>`, `<tenant>/<project>/deploy`).

**Artifact**: Metadata recorded in PostgreSQL (`name`, `kind`, `uri`, `digest`, `metadata`) pointing to build or release outputs published to external registries or services.

**NATS system bus**: The central JetStream bus carrying native system events, execution observations, external signals, pipeline triggers, and monitoring subscriptions. PostgreSQL remains the authoritative state; the outbox publishes to NATS.

**Kodama MCP server**: The shell's native MCP surface exposing Kodama operations. It is distinct from the Layer 3 proxy to external MCP servers.

**`kodactl`**: The unified CLI for users, operators, and in-container scripts to interact with the Kodama HTTP API, MCP endpoint, and bus signals.

## Layer 3: Agentic services

**Container harness step**: An ordinary Layer 1 container step running an external coding harness (`opencode`, `claude-code`, `pi`) with mounted secrets and the shared workspace, configured through pipeline YAML.

**Kodama Agent runtime**: A container step running the native `kodama` runtime (`kodama agent`) with access to shell operations, tools, skills, and gateways. It may request a wait when human input is required.

**Agent session**: The persisted record (`agent_sessions`) of a native `kodama agent` execution, including model usage, messages or transcript reference, and structured outcome.

**AI gateway**: Kodama's OpenAI-compatible HTTP proxy that mediates model calls, injects server-side provider credentials, and tracks token usage per run and session.

**External MCP proxy**: Kodama's proxy to registered external MCP servers, with tool visibility and policy specific to agent work.

## Layer 4: External behavior

**Recipe**: Pipeline YAML, scripts, container images, and conventions that implement delivery, rollback, QA, notification, or organizational policy outside the kernel and shell code.

**Hook**: A bounded command in a declared workload image, invoked at a pipeline or container lifecycle point with normal execution results. A hook is not a dynamically loaded controller plugin.
