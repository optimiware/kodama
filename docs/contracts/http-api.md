# HTTP and MCP interface contract

**Status:** Proposed REST and MCP interface under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). The current transitional assemblies implement only part of it.

## Conventions

Base URL is `/api/v1`. Let `T` mean `/tenants/{tenantKey}` and `P` mean `/tenants/{tenantKey}/projects/{projectKey}`.
- **Authentication:** Bearer OAuth access token for users/operators (governed by RBAC `memberships`); Bearer `KODAMA_TOKEN` for in-run steps and agents (governed by project-space run authority).
- **Kodama MCP endpoint:** `POST /api/v1/mcp` (Streamable HTTP Model Context Protocol exposing native shell operations).
- **AI Gateway endpoint:** `POST /api/v1/ai/v1/chat/completions` (Layer 3 OpenAI-compatible proxy for agents and steps).

## Routes

### Identity and tenancy
| Route | Purpose |
| --- | --- |
| `GET /me`, `GET /tenants` | Verified caller identity and accessible tenants |
| `GET, POST T/memberships`, `DELETE T/memberships/{id}` | Manage tenant and project RBAC role bindings |
| `GET, POST T/projects`, `GET, PATCH P` | List, create, and update projects and settings |

### Layer 1: Pipeline runtime
| Route | Purpose |
| --- | --- |
| `GET, POST P/pipelines`, `GET, PUT, DELETE P/pipelines/{name}` | List or register project-unique `.kodama/<processname>.yaml` definitions |
| `GET, POST P/runs`, `GET P/runs/{id}` | List or trigger pipeline runs; inspect runs and `run_steps` |
| `POST P/runs/{id}/cancel` | Cancel an active pipeline run |
| `GET P/runs/{id}/logs` | Stream or fetch step logs |

### Layer 2: Native shell
| Route | Purpose |
| --- | --- |
| `GET, POST T/repositories`, `GET, POST P/repositories` | List or create repositories (`<tenant>/context` or `<tenant>/<project>/<name>`) |
| `GET, POST P/tasks`, `GET, PATCH P/tasks/{id}` | List, create, and update tasks; `filter`: `status`, `kind`, `inbox_role` |
| `POST P/tasks/{id}/start`, `POST P/tasks/{id}/rerun` | Admit or rerun the task's selected pipeline |
| `POST P/tasks/{id}/resolve` | Resolve the task's current wait with an authorized input (emits a wait signal on NATS) |
| `GET, POST T/services`, `GET, POST P/services`, `DELETE P/services/{name}` | Manage scoped container services (`tenant`, `project`, or `pipeline`) |
| `GET, POST P/artifacts`, `GET P/artifacts/{id}` | Register and query metadata for external production artifacts |
| `GET, POST T/secrets`, `GET, POST P/secrets`, `DELETE P/secrets/{name}` | Set, list (metadata only), or delete encrypted secrets |

### Layer 3: Agentic services
| Route | Purpose |
| --- | --- |
| `POST /api/v1/ai/v1/chat/completions` | OpenAI-compatible AI Gateway |
| `GET, POST P/agent-sessions`, `GET P/agent-sessions/{id}` | Record and inspect `kodama agent` sessions, messages, and token usage |
| `GET, POST T/mcp-servers`, `GET, POST P/mcp-servers`, `DELETE P/mcp-servers/{id}` | Register or remove external MCP servers used by the agent proxy |

## Runtime and observation

- **Git smart HTTP:** Served at `/git/<tenant>/<path>.git` (for example `/git/acme/context.git` and `/git/acme/erp/backend.git`).
- **Event stream (SSE):** `GET P/events` streams live NATS JetStream signals (`git.*`, `task.*`, `run.*`, `artifact.*`) for authorized callers and `kodactl run watch`.
