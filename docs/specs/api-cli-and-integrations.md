# API, `kodactl` CLI, and MCP

**Scope:** Unified access surfaces (HTTP API, MCP server, `kodactl` CLI) and NATS JetStream event bus ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Unified access model

Users, operators, and in-container steps/agents interact with Kodama through three symmetrical interfaces backed by the same domain use cases:

1. **HTTP REST API (`/api/v1`):**
   - JSON endpoints for tenants, projects, memberships, repositories, pipelines, runs/steps, services, tasks/role-inbox, artifacts, secrets, agent sessions, and MCP servers.
   - Server-Sent Events (SSE) for live run/step logs and NATS event observation.
2. **MCP Server (`/api/v1/mcp`):**
   - Streamable HTTP MCP endpoint exposing Kodama's core operations and federated `mcp_servers` to external AI clients and in-run agents.
3. **`kodactl` CLI (and `kodama` subcommands):**
   - Uses external OAuth/OIDC with PKCE for interactive users and `KODAMA_TOKEN` for headless/container steps.
   - Core command groups:
     - `kodactl auth` / `kodactl context`
     - `kodactl project` (`list`, `create`, `workflow`)
     - `kodactl repo` (`list`, `create`, `clone`)
     - `kodactl task` (`list`, `create`, `get`, `transition`, `park`, `inbox`, `resolve`)
     - `kodactl pipeline` (`list`, `apply`, `trigger`)
     - `kodactl run` (`list`, `get`, `logs`, `watch`, `cancel`)
     - `kodactl service` (`list`, `up`, `down`, `logs`)
     - `kodactl secret` (`list`, `set`, `delete`)
     - `kodactl artifact` (`list`, `register`, `get`)

## NATS JetStream internal message bus

NATS JetStream is the internal nervous system of Kodama. Every state change publishes a structured signal on tenant-scoped subjects (`kodama.v1.<tenant>.<project>.<event>`):
- `git.push`, `git.branch`, `git.merge`, `git.tag`
- `task.created`, `task.transitioned`, `task.parked`, `task.resolved`
- `run.queued`, `run.started`, `run.completed`, `step.completed`
- `service.ready`, `service.stopped`
- `artifact.published`, `release.promoted`

## Requirements

| ID | Requirement |
| --- | --- |
| AC-01 | HTTP API, MCP server, and `kodactl` MUST enforce the same OAuth RBAC (for users) and project-space token permissions (for steps/agents). |
| AC-02 | `kodactl` MUST support both interactive human workflows (OAuth PKCE) and in-container script/agent workflows (`KODAMA_TOKEN` + `--json`). |
| AC-03 | NATS JetStream MUST capture and relay Git triggers, task state transitions, inbox resolutions, and release/promotion signals to start matching `.kodama/<processname>.yaml` pipelines. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| AC-A01 | A user configures a project, secrets, and task via `kodactl`; an in-run step transitions the task and registers an artifact using `kodactl` with `KODAMA_TOKEN` | AC-01, AC-02 |
| AC-A02 | Pushing a branch or transitioning a task emits a NATS signal that triggers the configured `.kodama/<processname>.yaml` pipeline | AC-03 |
