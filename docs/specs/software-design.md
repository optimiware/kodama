# Software design

**Scope:** Single Go module (`github.com/optimiware/kodama`) implementing the four-layer architecture and the pipeline-runtime kernel ([ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md)).

## Model and responsibilities

- **Four functional layers:**
  - **Layer 1 (Pipeline Runtime kernel):** pipeline definitions, runs, steps, waits, containers, shared workspaces, run services, declared secrets, and recovery. It has no shell, SQL/NATS/HTTP client, Git-hosting, task, identity, or agent dependency.
  - **Layer 2 (Native Shell):** identity, projects, project-unique pipeline registry, tasks and execution projection, native Git, secret custody, metadata artifacts, long-lived services, HTTP API, Kodama MCP server, CLI, persistence/runtime adapters, and the central NATS bus.
  - **Layer 3 (Agentic services):** sessions, harnesses, tools, AI gateway, and external MCP proxy. Agent work executes through ordinary kernel container steps.
  - **Layer 4 (External behavior):** pipeline YAML, scripts, container images, hooks, and delivery/QA/notification recipes.
- **Hexagonal ports and adapters:**
  - Capability packages own their domain structs, state transitions, and small consumer-owned Go interfaces.
  - Infrastructure details (PostgreSQL `pgx`, NATS JetStream, native Git CLI, Kubernetes/Podman/Process runtimes, KMS, OIDC) live in adapters wired explicitly by `internal/app`.
- **Standard-library first:**
  - HTTP uses `net/http`, CLI uses `flag`, logs use `log/slog`, YAML uses `go.yaml.in/yaml/v3`, SQL uses `pgx` without an ORM, and MCP uses `github.com/modelcontextprotocol/go-sdk` ([P-16](../decisions.md#implementation-baseline)).

## Requirements

| ID | Requirement |
| --- | --- |
| SD-01 | Modules SHOULD use lightweight hexagonal ports and adapters with inward dependencies wired by `internal/app`. |
| SD-02 | Layer 1 MUST NOT depend on shell, transport, persistence-driver, broker, identity, Git-hosting, task, or agent packages. Layer 2 and Layer 3 MUST build on public lower-layer contracts. |
| SD-03 | Persistence adapters MUST enforce the [current schema](../implementation/database-schema.md), composite `(tenant_id, project_id)` ownership, and forced PostgreSQL RLS (`WithTenant`). |
| SD-04 | `internal/app` MUST select outbound adapters from `KODAMA_ENV` (`development`, `test`, `production`) and refuse development/test adapters in `production`. |
| SD-05 | Ports with both memory (`memdb`) and PostgreSQL adapters MUST pass a shared contract test suite. |

## Authorization, transactions, and recovery

Use cases verify OAuth RBAC (for users) or project-space run tokens (for steps and agents) before calling persistence ports. Database state mutations and `audit_events` commit together inside `storage.WithTenant` transactions; external container, Git, NATS, and model calls execute outside SQL transactions.

## Environments

`KODAMA_ENV` selects the adapter profile ([ADR 0009](../adr/0009-environments.md)):
- `development`: In-memory `memdb` store and `Process` runtime for fast unit tests.
- `test`: Real PostgreSQL, NATS JetStream, native Git, and `Podman` container runtime.
- `production`: Kubernetes runtime, PostgreSQL, NATS JetStream, and cloud KMS.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| SD-A01 | Layer 1 packages compile and pass unit tests without Layer 2 dependencies | SD-01, SD-02 |
| SD-A02 | Memory and PostgreSQL adapters pass the same contract suite with strict tenant isolation | SD-03, SD-05 |
| SD-A03 | `KODAMA_ENV=production` refuses in-memory stores, `Process`/`Podman` runtimes, and local keyfiles | SD-04 |

## Decisions

Recorded in [C-16](../decisions.md#confirmed-direction), [C-24](../decisions.md#confirmed-direction), [ADR 0006](../adr/0006-hexagonal-domain-design.md), and [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md).
