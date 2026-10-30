# 7. Delivery roadmap

Following [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), Kodama is executing a three-phase simplification plan to align the documentation, database schema, and Go codebase with the two-layer architecture.

## Simplification roadmap

### Phase 1: Specification and contract reset (`docs/`) — In progress
- Establish [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) as the architectural baseline.
- Rewrite the domain glossary ([CONTEXT.md](../../CONTEXT.md)), book chapters, specifications, contracts, and [15-table database schema design](../implementation/database-schema.md).
- Retire legacy multi-version contracts (`v1alpha1`/`v1alpha2`/`v1alpha3`), S3 Git-bundle workspace publishing, Kanban WIP locks, the canonical-JSON gate engine, and the 12-table PostgreSQL event quarantine/replay subsystem.

### Phase 2: Database schema and domain reset — implemented on the task branch
- Replaced `001_schema.sql` and `002_grants.sql` with **15 product tables** (14 domain tables plus `audit_events`) while preserving PostgreSQL RLS and scoped ownership constraints.
- Added shared IDs/owners, the project-defined task domain, PostgreSQL/memory task stores and mutable-value isolation in `memdb`.
- Updated the application migration manifest. New-schema server wiring follows in Phase 3. [Implementation and verification handoff](../implementation/simplification-phase-2.md).

### Phase 3: Kernel execution slice — first cutover delivered
- `kodama server --assembly kernel` runs over the 15-table schema: identity/RBAC,
  projects and workflows, API pipelines, envelope secrets, sequential runs on a
  shared workspace (Podman/Process), retained logs, artifact metadata and the
  task/role-inbox loop. See [the handoff](../implementation/simplification-phase-3.md).
- Remaining: native Git and `.kodama` indexing, NATS dispatch, long-lived
  services, agents/gateways, `kodactl`, and removal of the legacy packages.

### Phase 3 (remaining): codebase consolidation (`internal/` & `cmd/`)
- **Layer 1 Core Kernel:**
  - `internal/identity`: External OAuth/OIDC + tenant/project RBAC + project-space workload tokens.
  - `internal/git`: Native Git HTTP service with Gerrit-style `<tenant>/<project>/*` auto-association, Knowledge Base checkout, and `.kodama/<processname>.yaml` indexing.
  - `internal/pipeline`: Drone-inspired YAML parser, `pipelines` / `runs` / `run_steps` orchestrator with shared `/workspace`, and `services` manager across `tenant`, `project`, and `pipeline` scopes.
  - `internal/tasks`: Per-project SQL state machine, NATS transition triggers, and parked tasks with role-inbox decision/promotion tasks.
  - `internal/secrets` & `internal/artifacts`: KMS/keyfile encrypted secrets and metadata-only external artifact tracking.
  - `internal/natsbus`: Clean NATS JetStream pub/sub for Git triggers, task transitions, step/agent coordination, releases, and promotions.
- **Layer 2 Agentic Software Factory:**
  - `internal/agent`: Native `kodama agent` runtime (skills, guardrails, validation, Git ops, task transitions, knowledge-base reader) + Mode A container harness recipes.
  - `internal/gateway` & `internal/mcp`: OpenAI-compatible AI Gateway, MCP Gateway, and `agent_sessions`.
- **Unified Interfaces:**
  - `internal/api` & `internal/cli`: Unified HTTP API, MCP server, and `kodactl` CLI.

## Environments

Every phase preserves the three execution environments ([ADR 0009](../adr/0009-environments.md)):
- `development`: In-memory adapters and Process runtime for fast unit tests.
- `test`: Real PostgreSQL, NATS JetStream, native Git, and Podman containers on a developer machine.
- `production`: Kubernetes, PostgreSQL, NATS JetStream, and cloud KMS (AWS, GCP, or Scaleway).
