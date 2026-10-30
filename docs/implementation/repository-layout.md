# Repository layout and package ownership

Following [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), Kodama is a single Go module (`github.com/optimiware/kodama`) organized around two functional layers: **Layer 1 (The Core Kernel)** and **Layer 2 (The Agentic Software Factory)**.

## Target package layout (Phase 3 consolidation)

```text
cmd/
  kodama/                    # server, git serve, step, agent entrypoints
  kodactl/                   # unified CLI (or subcommand of kodama)
internal/
  app/                       # composition root; selects adapters by KODAMA_ENV
  domain/                    # shared IDs, tenant/project scope, error codes
  storage/                   # pgx, WithTenant RLS, migration runner, 15-table 001_schema.sql
  memdb/                     # in-memory transaction kernel for development/unit tests
  natsbus/                   # NATS JetStream pub/sub for git triggers, tasks, steps, releases
  # --- Layer 1: Core Kernel ---
  identity/                  # external OAuth/OIDC, user RBAC, project-space workload tokens
  git/                       # native Git HTTP service, Gerrit <tenant>/<project>/* hierarchy, Knowledge Base
  pipeline/                  # .kodama/<process>.yaml parser, runs, run_steps, shared /workspace orchestration
  services/                  # container services at tenant, project, or pipeline scope
  tasks/                     # per-project SQL state machine, parked tasks, and role-inbox resolution
  secrets/                   # KMS/keyfile encrypted tenant/project secrets
  artifacts/                 # metadata-only tracking for external production artifacts
  runtime/                   # container runtime adapters (Kubernetes, Podman, Process)
  # --- Layer 2: Agentic Software Factory ---
  agent/                     # native kodama agent runtime (skills, guardrails, decisions, git/task tools)
  gateway/                   # OpenAI-compatible AI Gateway and token usage tracking
  mcp/                       # MCP Gateway (Kodama core MCP tools + external mcp_servers proxy)
  # --- Access Surfaces ---
  api/                       # HTTP REST handlers and SSE stream
  cli/                       # kodactl commands
deploy/helm/kodama/          # Helm chart
dev/                         # local Podman dependency stack and repoctl checks
```

## Parallel boundaries

Capability packages own their domain entities, use cases, ports/adapters, SQL queries, HTTP/MCP handlers, and unit tests. `internal/app` explicitly wires concrete adapters according to `KODAMA_ENV` (`development`, `test`, `production`).

The database schema is a single baseline owned by `internal/storage` (`001_schema.sql` and `002_grants.sql`) implementing the [15-table model](database-schema.md).

## Package seam contracts

| Seam / authority owner | Transaction ownership | Failure and replay |
| --- | --- | --- |
| `identity` → RBAC & project-space grants | Verified OAuth principal + `WithTenant` RLS context | Fail closed on missing/revoked membership or expired run token |
| `git` → `repositories`, `pipelines`, `natsbus` | Auto-links `<tenant>/<project>/*` to `project_id`; indexes `.kodama/*.yaml` into `pipelines` | Idempotent on repository path and commit SHA; emits `git.push` on NATS |
| `tasks` → `projects.task_workflow`, `natsbus` | Validates state transition and role-inbox resolution in SQL; unblocks `parked` tasks atomically | Emits `task.transitioned` / `task.resolved` via `audit_events` + NATS |
| `pipeline` & `services` → `runtime` (`kube`, `podman`, `process`) | Commits `runs`, `run_steps`, and `services` state in SQL; invokes container runtime outside SQL | Reconciles running containers against `run_steps` and `services` on restart |
| `agent` (Layer 2) → `gateway`, `mcp`, Layer 1 APIs | Uses project-space run token; persists `agent_sessions` and token counts | Parks task and creates role-inbox `decision` task when human input is required |

### Transaction composition checkpoint

State changes commit with `WithTenant` RLS predicates and append an `audit_events` record before publishing to NATS JetStream. External container, Git, and model calls never run inside open PostgreSQL transactions.

See [task ownership rules](../tasks/README.md#ownership-and-integration) for worktree and merge requirements.
