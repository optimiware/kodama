# API, `kodactl` CLI, and MCP

**Scope:** native shell access surfaces (HTTP API, Kodama MCP server, `kodactl` CLI) and the central NATS JetStream bus ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Unified access model

Users, operators, and in-container steps/agents interact with Kodama through three symmetrical interfaces backed by the same domain use cases:

1. **HTTP REST API (`/api/v1`):**
   - JSON endpoints for tenants, projects, memberships, repositories, pipelines, runs/steps, services, tasks and waits, artifacts, secrets, agent sessions, and MCP servers.
   - Server-Sent Events (SSE) for live run/step logs and NATS event observation.
2. **Kodama MCP server (`/api/v1/mcp`):**
   - Streamable HTTP MCP endpoint exposing Kodama's native shell operations. The external MCP proxy for agent tools is Layer 3 and is a separate surface.
3. **`kodactl` CLI (and `kodama` subcommands):**
   - Uses external OAuth/OIDC with PKCE for interactive users and `KODAMA_TOKEN` for headless/container steps.
   - Core command groups:
     - `kodactl auth` / `kodactl context`
     - `kodactl project` (`list`, `create`)
     - `kodactl repo` (`list`, `create`, `clone`)
     - `kodactl task` (`list`, `create`, `get`, `start`, `rerun`, `inbox`, `resolve`)
     - `kodactl pipeline` (`list`, `apply`, `trigger`)
     - `kodactl run` (`list`, `get`, `logs`, `watch`, `cancel`)
     - `kodactl service` (`list`, `up`, `down`, `logs`)
     - `kodactl secret` (`list`, `set`, `delete`)
     - `kodactl artifact` (`list`, `register`, `get`)

Task commands operate on the task's selected pipeline and projected execution. There is no workflow, transition, or park command group.

## NATS JetStream system bus

NATS JetStream is the native shell's central bus. PostgreSQL remains authoritative; state changes and outbox intent commit together, and the shell publishes to tenant-scoped subjects (`kodama.v1.<tenant>.<project>.<event>`). The bus carries:
- Git receives and pipeline indexing (`git.refs_updated`, `pipeline.indexed`)
- Task creation and projection (`task.created`, `task.progress_changed`)
- Run, step, wait, and service observations (`run.*`, `run_step.*`, `wait.*`, `service.*`)
- Artifact and configuration changes
- External signals and monitoring subscriptions

The kernel emits execution events through its ports and is tested without a broker. Deployed `test` and `production` profiles require NATS. See the [event catalog](../contracts/event-catalog.md).

## Requirements

| ID | Requirement |
| --- | --- |
| AC-01 | HTTP API, Kodama MCP server, and `kodactl` MUST enforce the same OAuth RBAC (for users) and project-space run authority (for steps/agents). |
| AC-02 | `kodactl` MUST support both interactive human workflows (OAuth PKCE) and in-container script/agent workflows (`KODAMA_TOKEN` + `--json`). |
| AC-03 | NATS JetStream MUST carry committed Git triggers, pipeline indexing, task projection, wait resolutions, and run/service observations, and deployed profiles MUST require it. |
| AC-04 | Task operations MUST use the task's selected pipeline and execution projection; workflow transition, park, and separate blocking-task commands MUST NOT be exposed. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AC-A01 | A user configures a project, secrets, and task via `kodactl`; an in-run step reads the task and registers an artifact using `kodactl` with `KODAMA_TOKEN` | AC-01, AC-02 |
| AC-A02 | Pushing a branch or resolving a pipeline wait emits a NATS signal that starts or resumes the configured `.kodama/<processname>.yaml` pipeline | AC-03 |
| AC-A03 | The same task operation through HTTP, MCP, and CLI has the same authority and idempotency, and no workflow/transition/park operation is served | AC-01, AC-04 |
