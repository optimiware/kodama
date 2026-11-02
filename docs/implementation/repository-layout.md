# Repository layout and package ownership

Following [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), Kodama is a single Go module (`github.com/optimiware/kodama`) organized around four functional layers. This is the **target** layout from the [pipeline-runtime simplification plan](pipeline-runtime-simplification.md); the current tree still contains legacy packages until their replacements are executable.

## Target package layout

```text
cmd/
  kodama/                    # server, git serve, step, agent entrypoints
  kodactl/                   # unified CLI (or subcommand of kodama)
internal/
  app/                       # composition root; selects adapters by KODAMA_ENV
  domain/                    # shared IDs, scope, error codes
  storage/                   # pgx, WithTenant RLS, migration runner, baseline SQL
  memdb/                     # in-memory transaction kernel for development/unit tests
  natsbus/                   # NATS JetStream adapter for the shell's central bus
  # --- Layer 1: Pipeline Runtime kernel ---
  kernel/                    # pipeline model, durable engine, waits, conditions
    runtime/                 # kernel-owned workload/resource ports and value types
  # --- Layer 2: Native Shell ---
  shell/
    identity/                # OAuth/OIDC, RBAC, project-space workload authority
    git/                     # native Git worker, hooks, ref policy, events
    pipelines/               # project pipeline registry, source indexing, triggers
    tasks/                   # task tracking and execution-progress projection
    secrets/                 # custody, envelope encryption, key providers
    services/                # long-lived tenant/project container services
    artifacts/               # metadata-only external artifacts
    api/                     # HTTP API and SSE
    mcp/                     # native Kodama MCP server
    cli/                     # kodactl commands
  # --- Kernel runtime adapters ---
  runtime/
    process/                 # development/test lifecycle adapter
    podman/                  # local end-to-end adapter
  kube/                      # Kubernetes runtime and run-resource adapter
  # --- Layer 3: Agentic services ---
  agents/                    # native agent runtime, harnesses, tools, skills
  gateway/                   # OpenAI-compatible AI gateway
  mcpproxy/                  # external MCP proxy
deploy/charts/               # Helm charts: kodama (product), cluster-issuer (stage-only)
dev/                         # local Podman dependency stack and repoctl checks
```

The package names above are targets. Moving a file does not create a boundary: the import rules below are the acceptance condition.

## Layer dependency rules

| Layer | May import | Must not import |
| --- | --- | --- |
| Kernel (`internal/kernel`) | Standard library, kernel-owned ports and value types, pure shared domain IDs | Shell, Git hosting, tasks, API/MCP/CLI, SQL/NATS/HTTP clients, OAuth, agents |
| Native shell (`internal/shell`, shell adapters) | Kernel contracts, shared domain/storage/bus packages | Agent packages that add execution semantics |
| Agentic (`internal/agents`, `gateway`, `mcpproxy`) | Shell and kernel public contracts | Kernel internals or private shell state |
| Composition (`internal/app`) | All selected adapters | Domain policy or a second execution engine |

Ports are consumer-owned and narrow. Infrastructure adapters are wired explicitly by `internal/app`. A shared package is extracted only where two owners genuinely share a contract, not to avoid defining a boundary.

## Package seam contracts

| Seam / authority owner | Transaction ownership | Failure and replay |
| --- | --- | --- |
| Kernel engine → state, runtime, resources, secrets, logs | Claims and run/step transitions commit with outbox intent; runtime calls run outside SQL | Launch/observe by stable identity; uncertain outcomes are reconciled, never blindly replayed |
| Shell identity → RBAC and project-space grants | Verified OAuth principal plus `WithTenant` RLS context | Fail closed on missing or revoked membership or expired run authority |
| Shell Git → `repositories`, pipeline registry, NATS | Authorizes each ref update against current authority; one `git.refs_updated` per applied receive | Worker journal plus `repositories.journal_ack` recover a lost outcome |
| Shell tasks → run/step projection, NATS | Validates the selected project pipeline and current-run reference; updates the projection with monotonic run version | Duplicate or out-of-order events converge; projection is rebuildable from runs and steps |
| Shell pipeline registry → Git indexer and API | One project-unique definition per name; conflicting sources are refused | Indexing and admission are idempotent on event, ref, and commit identity |
| Shell API/MCP/CLI → use cases | Common authorization and idempotency | The same operation through any surface has the same authority and replay behavior |
| Agentic services → shell APIs, gateways | Uses project-space run authority; persists sessions and usage | Parks by requesting a pipeline wait, never by mutating task workflow state |

## Transaction composition checkpoint

State changes commit with `WithTenant` RLS predicates and append an `audit_events` record before publishing to NATS JetStream. External container, Git, and model calls never run inside open PostgreSQL transactions.

See [task ownership rules](../tasks/README.md#ownership-and-integration) for worktree and merge requirements.
