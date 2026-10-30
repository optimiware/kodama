# HTTP and MCP interface contract

**Status:** Proposed Phase 3 REST and MCP interface for the 15-table model under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md). Phase 2 implements persistence, not these routes.

## Conventions

Base URL is `/api/v1`. Let `T` mean `/tenants/{tenantKey}` and `P` mean `/tenants/{tenantKey}/projects/{projectKey}`.
- **Authentication:** Bearer OAuth access token for users/operators (governed by RBAC `memberships`); Bearer `KODAMA_TOKEN` for in-run steps and agents (governed by project-space permissions).
- **MCP Endpoint:** `POST /api/v1/mcp` (Streamable HTTP Model Context Protocol exposing core tools and federated `mcp_servers`).
- **AI Gateway Endpoint:** `POST /api/v1/ai/v1/chat/completions` (OpenAI-compatible proxy for agents and steps).

## Routes

### Identity & Tenancy
| Route | Purpose |
| --- | --- |
| `GET /me`, `GET /tenants` | Verified caller identity and accessible tenants |
| `GET, POST T/memberships`, `DELETE T/memberships/{id}` | Manage tenant and project RBAC role bindings |
| `GET, POST T/projects`, `GET, PATCH P` | List, create, and update projects (including `task_workflow` state machine and `settings`) |

### Layer 1: Core Primitives
| Route | Purpose |
| --- | --- |
| `GET, POST T/repositories`, `GET, POST P/repositories` | List or create repositories (`<tenant>/context` or `<tenant>/<project>/<name>`) |
| `GET, POST P/pipelines`, `GET, PUT, DELETE P/pipelines/{name}` | List or register `.kodama/<processname>.yaml` definitions |
| `GET, POST P/runs`, `GET P/runs/{id}` | List or trigger pipeline runs; inspect run and `run_steps` |
| `POST P/runs/{id}/cancel` | Cancel an active pipeline run |
| `GET P/runs/{id}/logs` | Stream or fetch step logs |
| `GET, POST T/services`, `GET, POST P/services`, `DELETE P/services/{name}` | Manage scoped container services (`tenant`, `project`, or `pipeline`) |
| `GET, POST P/tasks`, `GET, PATCH P/tasks/{id}` | List, create, and update tasks (`filter`: `state`, `kind`, `inbox_role`) |
| `POST P/tasks/{id}/transition` | Transition task state along `projects.task_workflow` (emits `task.transitioned` on NATS) |
| `POST P/tasks/{id}/park` | Park a task (`state: parked`) and create a blocking `decision`/`promotion`/`incident` task in a role inbox |
| `POST P/tasks/{id}/resolve` | Resolve a role-inbox task, unblocking the parked parent task (emits `task.resolved` on NATS) |
| `GET, POST P/artifacts`, `GET P/artifacts/{id}` | Register and query metadata for external production artifacts |
| `GET, POST T/secrets`, `GET, POST P/secrets`, `DELETE P/secrets/{name}` | Set, list (metadata only), or delete encrypted secrets |

### Layer 2: Agentic Software Factory
| Route | Purpose |
| --- | --- |
| `POST /api/v1/ai/v1/chat/completions` | OpenAI-compatible AI Gateway |
| `POST /api/v1/mcp` | MCP Gateway for native Kodama tools and registered `mcp_servers` |
| `GET, POST P/agent-sessions`, `GET P/agent-sessions/{id}` | Record and inspect `kodama agent` sessions, messages, and token usage |
| `GET, POST T/mcp-servers`, `GET, POST P/mcp-servers`, `DELETE P/mcp-servers/{id}` | Register or remove external MCP servers |

## Runtime and observation

- **Git smart HTTP:** Served at `/git/<tenant>/<path>.git` (for example `/git/acme/context.git` and `/git/acme/erp/backend.git`).
- **Event stream (SSE):** `GET P/events` streams live NATS JetStream signals (`git.*`, `task.*`, `run.*`, `artifact.*`) for authorized callers and `kodactl run watch`.
