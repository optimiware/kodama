# Pipeline-runtime simplification plan

**Status:** Proposed implementation plan under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38).
**Baseline:** `7eeff9b5463ae1a725d7c9dd53be2c5e985546d1`.
**Scope:** Code and specification cleanup after the ADR 0013 amendment. This document schedules no task and claims no implemented capability.

## Outcome

Kodama has one process definition and one execution implementation:

1. **Kernel:** a standalone pipeline runtime for steps, containers, workspaces, services, secrets, waits, cancellation, and recovery.
2. **Shell:** native services for identity, Git, tasks, persistence, API/MCP/CLI, and the central NATS bus.
3. **Agentic services:** sessions, harnesses, tools, AI gateway, and external MCP proxy.
4. **External behavior:** pipelines, scripts, images, and hooks.

A task selects a project-unique pipeline. Its progress is a projection of that pipeline's runs. No separate project workflow, gate engine, or second execution stack remains.

## Baseline findings

| Finding | Evidence | Consequence |
| --- | --- | --- |
| The kernel is a product composition, not an isolated runtime. | `internal/kernel/runner/runner.go` requires a concrete SQL store, key provider, and Git preparation; `internal/kernel/serve/serve.go` composes SQL, HTTP, tasks, Git, and NATS. | Extract ports before moving packages. Directory renaming alone does not create a boundary. |
| Two execution stacks exist. | `internal/execution` has about 42,600 tracked Go lines including tests; `internal/kernel` has about 17,700. `internal/app` still imports the legacy assemblies. | Port retained lifecycle behavior once, then delete the legacy consumers. Do not preserve two runtimes indefinitely. |
| The project workflow is real code and SQL. | `internal/tasks/workflow.go`, `internal/tasks/task.go`, task stores, project APIs, and `projects.task_workflow`. | Remove the workflow engine with the task projection change. |
| Pipeline-name lookup is ambiguous. | `001_schema.sql` permits duplicate `(tenant, project, repo, name)` rows; `internal/kernel/store/resources.go` uses `ORDER BY repo_id NULLS FIRST LIMIT 1`. | Enforce `(tenant_id, project_id, name)` uniqueness and reject conflicting definition sources. |
| Git integration depends on task approval. | `internal/kernel/store/git.go` derives approval from task/request rows; `internal/kernel/api/gitpolicy.go` checks it during protected pushes. | Replace it with run/step/wait authority before removing task workflow fields. |
| The small runner does not yet replace mature lifecycle behavior. | Blocking `Backend.Execute`, a polling scheduler, and no durable wait or restart adoption. | Implement durable progression and waits before retiring the legacy runtime. |
| The product schema has 16 tables. | The baseline has 15; `003_git_hosting.sql` adds `git_credentials`. | Remove a workflow column and behavior, not a table. Do not repeat the stale 14/15 count. |
| Legacy imports hide inside new code. | `contracts/git/v1` imports `contracts/runtime/v1`; `internal/gitauth/jwt` imports legacy `gitauth`; the new secret envelope reuses `internal/secrets/envelope`. | Extract shared helpers, then delete the legacy owners. |
| Documentation is inconsistent. | The Phase 3 handoff still lists Git/NATS as unimplemented; the event catalog mixes current and legacy events; delivery text retains gate/deployment requirements while saying those subsystems do not exist. | Reconcile the owning documents before dependent implementation. |

## Contract checkpoints

Freeze these before moving code. Each checkpoint updates its owning specification and contract fixtures.

### C1 — Pipeline identity and authoring

- `pipelines` is project-scoped and project-unique by name.
- A Git definition, an API definition, or a conflicting repository source cannot silently replace another. The conflict is recorded and refused.
- A task resolves a pipeline name to an immutable pipeline ID at creation and keeps that reference.
- The pipeline document is the only persisted process definition. Trigger, source, and inbox metadata are derived views, not a second editable copy.
- The exact wait and condition syntax is frozen here.

### C2 — Run and wait lifecycle

- Admission snapshots the definition, inputs, sources, secret references, and resources.
- A run progresses through durable intent: claim, launch, observe, settle, choose the next step.
- A wait is a control step without an image or workload. It persists a unique identity, correlation, expected input, deadline, consumed signal identity, result, and version.
- A signal for a closed, cancelled, stale, or foreign wait cannot advance execution.
- Commit the wait before announcing it. Duplicate or lost delivery converges on the same consumed result.
- A rerun creates a new run and a new workspace. It does not reset completed step rows.
- The kernel owns run/step truth. Task progress is derived and rebuildable.

### C3 — Task projection and inbox

- A task requires a same-project pipeline.
- A task may have several runs; exactly one is its current execution.
- Projection fields are versioned so an older run's event cannot overwrite a newer run's task view.
- Inbox entries are tasks whose current wait is assigned to the caller's role. Authorization checks the current wait, run, and actor.
- Resolution records an immutable input and actor against the wait. The pipeline decides the next branch.
- Remove `task_workflow`, workflow transitions, `parked_at`, `resume_state`, `blocked_by_id`, and special decision/promotion/incident lifecycle machinery.
- Preserve generic task labels or metadata for business categorization. Do not recreate a custom state machine under another name.

### C4 — Git integration authority

- Replace task-status approval with a shell-owned check of the run's configured pipeline, its eligible integration step, and any required satisfied wait/input.
- Issue integration authority only to that step for the exact ref and expected-old/new commit pair.
- Keep the native worker's fast-forward, current-authority, and expected-old-value checks.
- A task field, bare NATS event, or container-supplied `approved=true` is not authority.
- A pipeline may be the complete SDLC process. Earlier steps do not inherit integration authority.

### C5 — Messaging and persistence

- PostgreSQL remains authoritative; state and outbox intent commit together.
- NATS is the deployed shell's central bus. Consumers tolerate duplicate and out-of-order delivery.
- Reconciliation repairs missed wakeups and rebuilds task projections from run/step records.
- The kernel has memory ports for tests and no NATS or SQL client dependency.
- Deployed `test` and `production` profiles require NATS.

## Code map

| Current code | Action | Destination |
| --- | --- | --- |
| `internal/kernel/pipeline` | Keep pure definitions, validation, canonical form, and ordering. Move Git trigger interpretation out. | Kernel |
| `internal/kernel/runner` | Refactor into a durable engine with injected state, runtime, resources, secrets, workspace preparation, logs, and clock. | Kernel |
| `internal/kernel/api`, `bus`, `dispatch` | Move native transport, events, and triggers behind shell use cases. | Shell |
| `internal/kernel/store`, `secret` | Keep models/ports; move concrete SQL and custody adapters out. Extract envelope/key helpers. | Kernel + shell adapters |
| `internal/kernel/serve` | Fold composition into one reduced `internal/app`. | Composition |
| `internal/tasks` | Replace workflow engine with task tracking and execution projection. | Shell |
| `internal/gitservice` | Keep worker, hooks, fencing, journal, and tenant hosting. Rehome only if it improves ownership. | Shell |
| `internal/gitauth`, `gitpush`, `gitops`, `repository` | Extract live worker, signing, and protocol helpers; delete superseded grant, push-record, and workspace-association code. | Shell |
| `internal/execution`, legacy `pipeline`, `entrypoint`, `results`, `runtimeauth` | Port retained lifecycle and workload identity behavior; delete the legacy contract versions and consumers. | Kernel + shell |
| `internal/runtime`, `internal/kube` | Reuse qualified backend behavior behind kernel-owned contracts. Remove legacy execution/gate/workspace dependencies. | Kernel adapters |
| `internal/work`, `workspace`, `gates`, `delivery`, `deliverycleanup`, `metrics`, `qa` | Delete superseded business engines; express retained behavior as task queries, pipeline recipes, and external tools. Port relevant tests. | Shell + Layer 4 |
| `internal/artifacts`, `secrets`, `logs`, `observation`, `events`, `integrations` | Keep metadata registry, custody, logs, monitoring, native Git/event path, and current interfaces. Remove catalog/release-receipt, duplicate-broker, flow/governance, and provider orchestration leftovers. | Shell |
| `internal/agents`, `gateway`, `mcpproxy`, `productmcp` | Keep agentic functionality in Layer 3; keep the native product MCP server in Layer 2. Adapt to public shell operations and current run context. | Layer 3 + shell |
| `internal/cli`, `api/`, `contracts/` | Publish served interfaces only; regenerate the client; remove retired fragments, aliases, schemas, and fixtures. | Interfaces |
| `deploy/`, `dev/`, Make/CI targets | Update to the one composition and current settings; remove retired entrypoints and targets. | Operations |

Hooks are bounded commands in the declared workload image with normal execution results. They are not dynamically loaded controller plugins.

## Ordered phases

### P0 — Governing documents

1. Amend ADR 0013 and its index entry.
2. Record O-38 and update C-24/O-35 in the decision register.
3. Update the glossary, system architecture, domain/lifecycle chapter, repository layout, database schema, and owning specifications.
4. Replace contradictory current requirements; retain retired IDs with a supersession mapping.
5. Add stable requirement and acceptance IDs for waits, task projection, pipeline identity, and NATS coordination.
6. Update examples, status, traceability, task graph, and AGENTS reading paths.
7. Verify with `make docs-check validate-tasks` and `git diff --check`. Obtain independent review before dependent work.

### P1 — Kernel isolation

1. Define runtime-owned models and ports for state, execution, resources, secrets, workspace preparation, logs, and time.
2. Add a memory state implementation and one shared contract suite.
3. Replace blocking run execution with durable progression and reconciliation.
4. Implement waits, signals, conditions, cancellation, deadlines, restart adoption, services, and confirmed cleanup.
5. Port retained adapter behavior and regressions; do not import the legacy execution tree.
6. Prove a plain multi-step pipeline, a waiting pipeline, failure, and recovery without SQL, NATS, Git, HTTP, tasks, or agents.

### P2 — Persistence and native shell

1. Remove `projects.task_workflow`; add task pipeline/current-run/projection fields.
2. Enforce project-unique pipeline names in schema, API, and Git indexing.
3. Persist waits and generic control-step shapes in run/step records.
4. Wire SQL, secret custody, source preparation, Git worker, NATS outbox, triggers, and task projection outside the kernel.
5. Provide task create/start/rerun/inbox/resolve through common shell use cases; retire workflow/transition/park APIs.
6. Replace task-based Git approval with the C4 contract.
7. Require NATS in deployed profiles; add scoped send/listen/watch interfaces.
8. Prove duplicate/out-of-order delivery and projection rebuild.

Use a new isolated database for an intentional reset, or an explicit append-only migration if a particular database must be preserved. Never let the runner delete or transform existing state.

### P3 — Composition and interfaces

1. Complete Process, Podman, and Kubernetes behind the single kernel contract.
2. Wire one normal server composition and consistent environment selection; retire competing `core` and `kernel` product assemblies at cutover.
3. Port workload callbacks, identity, logs, cancellation, and cleanup.
4. Publish only served routes; regenerate the client; port CLI and MCP callers.
5. Update chart, development settings, and runtime qualification entrypoints.
6. Prove task → pipeline → steps → wait → authorized signal → continuation, plus independent Git-triggered and task-free runs.

### P4 — Deletion

1. Delete the workflow engine and its schema consumers.
2. Remove obsolete engines and composition wiring in dependency order.
3. Remove retired pipeline/runtime versions, gate/release compatibility contracts, aliases, generated operations, and legacy-only fixtures.
4. Preserve tests for retained lifecycle, security, and authority behavior under their new owners.
5. Remove unused dependency pins, settings, Make/CI targets, and misleading documentation; run `go mod tidy` only after import cleanup.
6. Verify transitive imports so renamed packages do not conceal the old stack.

### P5 — Externalization and layer completion

1. Adapt retained agent/session/gateway/proxy functionality to shell APIs and current execution context.
2. Separate the product MCP server from the external MCP proxy.
3. Move retained SDLC, delivery, review, QA, notification, and hook policy into a small copyable recipe/example set.
4. Keep Git Markdown knowledge and skill conventions as recipe/agent concerns.
5. Update status to distinguish working, retained, and deferred functionality.

## Verification

| Scenario | Required evidence |
| --- | --- |
| Standalone runtime | Two container steps share files and declared secrets with no shell or agent service. |
| Pipeline identity | A task resolves one project pipeline; duplicate names and cross-project references are refused. |
| Durable wait | A wait survives restart; an authorized correlated result resumes the same run exactly once across lost publication or acknowledgment. |
| Wait outcomes | Rejection and answers take their declared branch and cannot authorize protected integration or promotion. |
| Resource bounds | A waiting run retains only declared resources within its deadline; cancellation and resource loss settle and clean up confirmed resources. |
| Cancellation and uncertainty | Active cancellation stops workloads; uncertain launch or stop never causes blind replay or workspace handoff. |
| Messaging | State and outbox intent are atomic; NATS outage, redelivery, and reordering cannot duplicate runs or regress task progress; reconciliation repairs a missed wakeup. |
| Git | Create → seed push → exact-commit indexing → run still works, including journal recovery and protected integration. |
| Layer absence | Plain pipelines run with Layer 3 absent; agent steps use the same container contract when enabled. |
| Dependency direction | Kernel transitive imports contain no shell, task, Git-hosting, SQL/NATS/HTTP client, or agent package. |

Checks:

- Documentation: `make docs-check validate-tasks`, plus `git diff --check`.
- Go: `make check-go PKGS="<owned packages>"` and relevant shared contract suites.
- SQL/shell: real PostgreSQL with non-owner RLS roles and real JetStream. A skipped test is not evidence.
- Runtime: Process lifecycle suite, real Podman scenarios, then bounded Kubernetes/chart qualification for changed behavior. Reuse historical stage evidence only for unchanged, still-applicable properties.
- Interfaces/configuration: `make api-check`, `make config-check`, and relevant chart tests.
- Batch validation: `make lint-go test-go-unit` every five merges and at queue drain. Publication to `main` is a separate explicit decision.

## Completion report

Record before and after counts for:

- non-generated production Go lines and test Go lines;
- internal packages and transitive dependencies;
- product tables, columns, indexes, and policies;
- supported wire and pipeline versions;
- server compositions and CLI entrypoints;
- generated API and contract fixture volume.

Do not claim a table reduction from removing a JSON workflow column or a code reduction from moving files alone.
