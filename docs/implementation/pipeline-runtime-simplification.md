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

### P1 — Kernel isolation and durable execution

**Goal:** make the kernel a standalone pipeline runtime that can execute, pause, recover, and clean up without PostgreSQL, NATS, Git, HTTP, tasks, identity, or agents.

#### P1.0 Boundary relocation

One ordered owner moves the shell responsibilities out of the kernel before parallel replacement work starts. Relocation alone does not prove isolation.

- Move `internal/kernel/{api,bus,dispatch,store,secret,serve}` into explicitly enumerated shell packages, and keep the existing blocking runner under an explicit transitional owner until the replacement engine is ready.
- Keep pure pipeline definitions in `internal/kernel/pipeline`; move Git trigger interpretation to the shell.
- Include every affected consumer in the relocation manifest: `internal/app`, `dev/internal/devenv`, and any test or tooling import.
- Freeze the destination manifest so later tasks have stable owned paths; do not refactor runner or store behavior in the same change.

#### P1.1 Kernel-owned model and ports

Define a small `internal/kernel` API that is independent of the shell:

- **Model:** canonical pipeline plan, execution order, step kind (`container`, `wait`, `condition`), run state, step state, wait specification/state, resource specification/state, runtime binding, secret reference, result/reason codes.
- **Ports:**
  - `StateStore`: load a run, claim a run/step, commit a transition with outbox intent, list runnable work, rebuild a run view.
  - `Runtime`: `Start`, `Observe`, `Logs`, `Stop`, `Remove`, `Capabilities` for one workload attempt.
  - `ResourceManager`: `Ensure`, `Observe`, `Logs`, `Stop`, `Remove`, `Capabilities` for run-owned workspaces, preparation, and services.
  - `SecretResolver`: resolve only declared references into a private materialization.
  - `WorkspacePreparer`: prepare or adopt a run workspace from pinned inputs.
  - `LogSink` and `Clock`.
- **Rules:** no concrete `pgx`, NATS, HTTP, Git, task, identity, or agent types. No environment reads. No shell composition in the kernel package.
- **Deliverable:** exported kernel contract, compile-time dependency check, and a documented transaction/effect boundary.

#### P1.2 Memory state and contract suite

- Implement the state port over the existing `memdb` transaction kernel.
- Add one exported contract suite covering claim races, optimistic versions, duplicate transitions, outbox intent, cancellation, wait consumption, and projection reads.
- Keep the memory adapter test-only; it is not a production fallback.

#### P1.3 Durable progression engine

Replace blocking all-in-one execution with explicit progression:

1. Claim a run with a durable owner/lease and version.
2. Load the immutable plan and current step.
3. Commit command intent before an external effect.
4. Launch or adopt a workload by stable identity.
5. Observe until settled; persist state and outbox intent.
6. Select the next eligible step, wait, condition, or terminal state.
7. Reconcile after restart, lost response, or expired lease.

Required behavior:

- One executable step holds the workspace at a time.
- A prior step must settle and release the workspace before the next starts.
- A claim is a lease: owner identity, expiry, renewal, takeover and a monotonic fence. A stale writer cannot commit a transition after takeover.
- Database fencing is not enough for external effects: workloads carry a stable identity and immutable specification identity, launch evidence is conservative, and delayed creates are rejected.
- Retries are allowed only when launch is affirmatively known not to have reached the command; uncertain outcomes are reconciled, not replayed.
- Cancellation blocks new execution claims but still permits stop, removal and reconciliation claims.
- Execution outcome and cleanup are tracked separately. A run is terminal only after owned resources are confirmed stopped and removed, or cleanup remains visibly pending with retained resource identities and bounded retries.
- Authority for a step is revoked when its workload settles, not only when the run ends.
- Fail-fast and dependency ordering are deterministic and tested.

#### P1.4 Waits, signals, and conditions

- Freeze the wait contract: identity, assignment/correlation, expected input, accepted schema, deadline, consumed signal identity, result, and continuation.
- Freeze a bounded condition grammar over declared inputs, prior step results, and wait outcomes. It must not execute arbitrary code or read shell state.
- Persist the wait before announcing it.
- Make consumption idempotent: duplicate, late, stale, cancelled, or foreign signals cannot advance a run twice. The guarantee is one committed consumption with idempotent continuation; command side effects are not exactly-once.
- Define deadline behavior: expire the wait and take the declared timeout branch, or fail the run when no branch exists.
- Resolution, expiry and cancellation race on one version-checked decision. A duplicate request returns the recorded result instead of creating a second continuation.
- Define workspace/service lifetime during a wait. Loss of required state fails the run; it never silently replays prior commands.
- Add tests for restart between open/announce/consume, lost acknowledgment, and concurrent resolution.

#### P1.5 Runtime and resource lifecycle

- Port retained lifecycle behavior from `internal/execution` and `internal/runtime` behind the new ports.
- Preserve stable workload identity, adoption, fenced stop, removal confirmation, orphan reconciliation, and capability-based admission.
- Implement run-scoped service ensure/readiness/stop/remove and workspace preparation.
- Keep provider-specific types out of kernel contracts.
- Define what each backend advertises; a backend must not claim isolation, networking, or cleanup guarantees it cannot enforce.

#### P1.6 Logs, secrets, and workspace preparation

- Materialize only declared secret references into a directory outside `/workspace`.
- Define rotation/revocation behavior and redaction boundaries.
- Capture bounded logs and retain them through the shell-owned log sink.
- Support source-free runs and injected source preparation without native Git knowledge in the kernel.
- Test that no secret value enters events, logs, or retained metadata.

#### P1.7 Exit gate

- A plain multi-step pipeline runs with memory state and the Process adapter.
- A waiting pipeline reconstructs from retained state and continues after one committed consumption; duplicate and lost signals converge on the same result.
- Cancellation, timeout, service loss, uncertain launch and cleanup recovery are covered.
- Kernel transitive imports contain no shell, SQL/NATS/HTTP client, Git-hosting, task, identity, or agent package.
- The kernel contract suite passes with no service and no container.
- **Evidence distinction:** memory tests prove reconstruction over retained state. A real controller-crash restart needs PostgreSQL and a recovery-capable backend and is qualified at P3, not claimed here.
- **Merge slices:** P1.0/P1.1 together; P1.2; P1.3/P1.4 (one engine owner); P1.5 per backend; P1.6; P1.7 verification. Each slice is independently reviewable and leaves the current executable composition intact until P3 cutover.

### P2 — Persistence and native shell

**Goal:** replace the project workflow with pipeline-selected tasks, persist waits and projections, and wire the shell around the kernel.

#### P2.1 Schema contribution

Use one ordered schema owner for migration numbering, the manifest, RLS, grants, constraints and schema documentation. Other workstreams submit reviewed requirements; they do not add migrations.

Sequence the schema in three reviewed stages instead of dropping columns up front:

1. **Expand.** Append nullable/new fields, indexes and compatible grants. Cover the complete P1 execution representation: lease owner/expiry/fence, command intent, workload and resource identities, workspace exclusion, wait state and cleanup evidence. Add `tasks.pipeline_id`, `current_run_id` and the projection fields. Keep `projects.task_workflow` and the existing task state columns readable by the current composition.
2. **Validate and map.** Detect duplicate `(tenant_id, project_id, name)` pipeline rows and conflicting definition sources. Require an explicit task-to-pipeline mapping for existing tasks; refuse ambiguous mappings rather than inventing a pipeline. Enforce `UNIQUE (tenant_id, project_id, name)` on `pipelines` only after duplicates are resolved.
3. **Contract.** After the replacement composition and consumers are live and active executions are drained or explicitly settled, enforce required references and remove `projects.task_workflow`, `state`, `parked_at`, `resume_state`, `blocked_by_id`, `resolution`, `resolved_at` and their obsolete grants and indexes.

A fresh isolated database is an explicit owner choice that skips stages 1–2; it is not permission to rewrite a deployed checksum.

Required final shape:

- `tasks`: required same-project `pipeline_id`; `current_run_id` constrained to belong to **that task and project**, not merely the project; `status`, `current_step_key`, `waiting_for`, `assigned_role`, `result`, `progress_version`, `progress_run_version`; title/body, parent, repository/branch, context, assignment and optimistic content version retained. A task has no current run before its first execution and at most one afterwards.
- `run_steps`: `step_kind` (`container`, `wait`, `condition`); wait identity, assignment, correlation, expected input, state, result, deadline, consumed signal and version; image/command requirements relaxed for non-container steps; indexes for due waits and task current-run lookup.
- RLS/grants: preserve tenant predicates, composite ownership, narrow runtime column grants, forced RLS, and narrow grants over immutable run snapshots and wait results.
- Migration tests for duplicate pipeline names, cross-project and cross-task references, wait-state constraints, and removed-column absence at the contract stage.

#### P2.2 Pipeline registry and task use cases

- Resolve project pipeline names to immutable IDs at task creation.
- Reject duplicate names and conflicting Git/API definition sources.
- Implement task create, get, list, start, rerun, inbox, and resolve as shell use cases.
- Build the task projection from run/step records; reject stale `progress_run_version` updates.
- Remove transition, park, and blocking-request operations.
- Define one-current-run semantics and rework/rerun behavior.
- Add authorization checks for task content, pipeline selection, and wait resolution.

#### P2.3 Shell state, outbox, and NATS

- Implement PostgreSQL state and task-projection adapters behind kernel/shell ports.
- Keep state mutation and outbox intent in one transaction.
- Publish `git.refs_updated`, pipeline indexing, task projection, run/step/wait/service, artifact, and configuration events.
- Define durable consumer names and deduplication keys.
- Require NATS in deployed `test` and `production` profiles; keep in-process delivery for development tests only.
- Implement reconciliation for missed wakeups and projection rebuild.
- Test broker outage, duplicate delivery, reordering, and restart.

#### P2.4 Git integration authority

- Remove `TaskApproved` and task-state-based integration checks.
- Issue integration authority only when:
  - the run's pipeline is the repository's configured integration pipeline;
  - the step is the eligible integration step;
  - any required wait/input for the exact candidate is satisfied;
  - the ref is the protected default branch;
  - the expected old commit and new candidate match;
  - the update is a fast-forward.
- Keep the native worker's current-authority and expected-old-value checks.
- Test that ordinary earlier steps, stale candidates, task status strings, and bare events cannot integrate.

#### P2.5 Source, secret, and workspace adapters

- Move Git checkout, context repositories, run-scoped Git credentials, and knowledge-base preparation into shell adapters.
- Keep credentials outside `/workspace`, rotate/revoke them with the run, and never expose them in logs or events.
- Bind secret custody and version resolution to the kernel resolver port.
- Preserve source-free and multi-repository run behavior.
- Test revoked credentials, unavailable secret versions, and lost workspace state.

#### P2.6 API, CLI, and MCP adapters

P2 implements transport adapters and their package tests only. P3 owns route/command registration and public interface generation.

- Implement task operations: create, get, list, start, rerun, inbox, resolve.
- Remove workflow, transition, park, and blocking-request handlers.
- Add wait inspection and resolution through the same shell use cases.
- Implement `kodactl` command handlers and the native Kodama MCP tools over those use cases.
- Hand constructors and operation contracts to the composition owner; do not regenerate the client or edit app registration here.

#### P2.7 Cutover and exit gate

- Use a new isolated database for a reset, or an explicit append-only migration when a database must be preserved.
- If existing tasks are migrated, map each task to a pipeline explicitly; fail closed rather than inventing a pipeline.
- Prove duplicate/out-of-order delivery, projection rebuild, wait resolution, Git integration, and tenant isolation on real PostgreSQL.
- **Exit:** a task can select a pipeline, start a run, wait, resolve, resume, and project progress without the workflow engine.

### P3 — Composition and interfaces

**Goal:** one executable product composition over the new kernel and shell, with all supported interfaces and runtime backends aligned.

#### P3.1 Runtime backend convergence

- Implement Process, Podman, and Kubernetes behind the kernel runtime/resource contracts.
- Preserve the qualified behavior worth keeping: image pinning, non-root execution, resource limits, network policy, workload identity, adoption, stop fencing, and cleanup.
- Declare capabilities per backend and refuse runs a backend cannot satisfy.
- Remove duplicate blocking execution paths and provider-specific branches from domain code.
- Run the shared lifecycle contract against each real backend; do not use mocks as acceptance evidence.

#### P3.2 One server composition

- Reduce `internal/app` to one normal composition for the kernel, shell, adapters, workers, and HTTP listener.
- Select adapters by `KODAMA_ENV`; keep `production` refusal of development/test adapters.
- Remove `--assembly core` versus `--assembly kernel` as competing product engines at cutover.
- Make startup ordering explicit: database/RLS, keys, identity, bus, Git workers, runtime, workers, listener.
- Fail closed when a required shell dependency is missing.

#### P3.3 Workload identity, callbacks, and logs

- Port only the current workload identity and callback contract.
- Give each step scoped run authority, exact source, declared secrets, and bounded logs.
- Remove legacy runtime v1/v2/v3 callback routes that no current client uses.
- Keep result/artifact registration narrow and idempotent.
- Test revoked run authority, lost callbacks, and restart adoption.

#### P3.4 Interface publication

- Publish only served HTTP routes in `api/openapi.yaml`; the served-route inventory must match the document.
- Regenerate `api/client` and verify with `make api-check`.
- Register CLI and native MCP commands over the P2 use cases and retire legacy registrations.
- Remove operations for workflow transitions, gates, release receipts, and legacy delivery.
- Add contract tests for error envelopes, idempotency, tenant isolation and served-interface parity.

#### P3.5 Deployment and developer workflow

- Update the Helm chart and settings document for the one composition.
- Update `dev/` scopes, preflight, runtime acceptance, and Git worker startup.
- Update Make/CI targets to the current checks; remove targets for retired assemblies.
- Document the supported development, test, and production profiles.
- Keep stage deviations explicit and refused without acknowledgement.

#### P3.6 Exit gate

- A task runs end to end: select pipeline → start → container steps → wait → authorized signal → continuation.
- A Git-triggered run and a task-free run work independently.
- Restart, cancellation, cleanup, and broker outage are exercised on real adapters.
- The product starts from one composition and refuses an incomplete environment.
- **Merge slices:** runtime backends; composition; workload identity/logs; API/client/CLI/MCP; chart/dev tooling; end-to-end acceptance.

### P4 — Deletion of the superseded implementation

**Goal:** remove the second execution stack and obsolete product engines after the replacement is executable and reviewed.

#### P4.1 Deletion order

Deletion is dependency-driven, not phase-driven. A package becomes deletable only when its replacement is executable, every retained consumer (including agentic services, adapters, tests and fixtures) has migrated, required recovery and security regressions have moved, and no admitted execution still needs it.

Record a retirement inventory with one row per retired component:

| Retired component | Replacement | Retained consumers migrated | Regression evidence | Deletion owner |
| --- | --- | --- | --- | --- |

Delete in dependency order, with imports and tests checked after each slice:

1. Workflow/task transition code and its schema consumers.
2. Legacy execution engine, attempt/workspace associations, and old runtime contract versions.
3. Gate, release-receipt, deployment-record, and delivery-cleanup engines.
4. Legacy work-item, workspace, metrics, and QA engines whose behavior moved to tasks, pipelines, or recipes.
5. Legacy composition wiring and settings.
6. Retired API fragments, generated client operations, contract schemas, aliases, and fixtures.

Independent deletion tasks may run on disjoint packages. Shared cleanup stays ordered: composition and API cleanup belong to the composition owner, schema contraction to the schema owner, configuration and chart cleanup to the operations owner, and `go.mod`/`go.sum` cleanup to one final dependency owner after import deletion.

#### P4.2 Preserve before deleting

Before removing a package, identify and port:

- lifecycle and recovery regressions that still describe required behavior;
- tenant/RLS and authority tests;
- Git worker, hooks, fencing, journal, and signing helpers;
- secret envelope/key helpers;
- bounded log capture and artifact metadata registration;
- monitoring/event behavior that remains in the shell.

Do not keep a package only because it has tests; port the behavior that still belongs to the target design.

#### P4.3 Dependency and interface cleanup

- Remove unused configuration keys, secret files, chart values, and environment variables.
- Remove obsolete Make/CI targets and dev scopes.
- Remove unused Go dependencies only after import cleanup and `go list` checks; then run `go mod tidy`.
- Remove retired contract versions and compatibility aliases only when no admitted run or published client needs them.
- Recheck transitive imports so a renamed package does not conceal the legacy stack.

#### P4.4 Documentation cleanup

- Mark historical handoffs and acceptance reports as historical, not current.
- Remove current-looking descriptions of gates, deployments, workspaces, workflows, and runtime versions.
- Keep historical evidence links where they explain past qualification.
- Update status and assembly status to the one composition.

#### P4.5 Exit gate

- No production import reaches a deleted engine.
- The kernel import boundary check passes.
- The migration baseline is the only schema source; no legacy migration directory remains.
- The generated API and client contain no retired operations.
- The codebase reports the before/after package, line, table, and wire-version counts from the completion report.
- Scoped checks pass for each deletion task; the repository batch (`make lint-go test-go-unit`) runs every five merges and at queue drain. A failed batch pauses further merges until repaired.

### P5 — Externalization and layer completion

**Goal:** finish the layer boundaries and move specialized behavior out of core code.

P5 splits into two portions so deletion is not blocked by late adaptation:

- **P5-A, retained-consumer adaptation.** Starts once kernel/shell contracts are stable and may run alongside P2 and P3. It gates deletion of the legacy packages those consumers import.
- **P5-B, recipes, knowledge and closeout.** Authors may prepare examples once authoring and public-operation contracts freeze; execution claims wait for real integrated evidence.

#### P5.1 Agentic services on shell contracts

- Adapt the native agent runtime, sessions, harnesses, tools, and skills to current shell APIs and run context.
- Replace task transitions/parking with pipeline waits and projected progress reads.
- Keep agent work inside ordinary kernel container steps.
- Remove agent-specific execution semantics from the kernel and shell.

#### P5.2 AI gateway and MCP separation

- Keep the OpenAI-compatible AI gateway in Layer 3 with server-side provider credentials, budgets, and usage recording.
- Keep the native Kodama MCP server in Layer 2.
- Keep the external MCP proxy in Layer 3 with tool allow-lists and server-side `mcp_servers` credentials.
- Remove the old combined "MCP gateway" surface and any native-tool routing from the proxy.
- Test authority, tool visibility, credential isolation, and usage attribution.

#### P5.3 Recipes and examples

Provide a small, copyable Layer 4 set:

- full SDLC pipeline: plan → implement → review wait → test → integrate;
- rejection/rework branch;
- preview and promotion wait;
- build/publish with external tools;
- deploy, rollback, and cleanup recipes;
- notification/webhook recipe;
- hook conventions inside workload images;
- multi-repository candidate example;
- source-free maintenance run.

Each recipe must state required images, secrets, shell APIs, and which behavior is a convention rather than a Kodama guarantee.

#### P5.4 Knowledge and skills

- Keep context, skills, blueprints, and engineering guidance as Markdown in Git.
- Document repository conventions (`<tenant>/context`, `<tenant>/<project>/context`, component and deploy repositories).
- Do not add a database-backed knowledge subsystem or a runtime library extraction step.

#### P5.5 Status and acceptance

- Update status to distinguish working, retained, deferred, and historical functionality.
- Add acceptance evidence for the full task journey and each Layer 4 recipe that is claimed.
- Do not claim production, hosted, or stage qualification without the named real evidence.
- Record remaining deferred work explicitly rather than leaving stale "current" descriptions.

#### P5.6 Exit gate

- Kernel, shell, and agentic imports follow the layer rules.
- A plain pipeline runs with Layer 3 absent.
- An agentic pipeline uses the same container and wait contracts.
- Delivery, QA, and notification behavior is expressed in recipes, not core engines.
- Status and traceability match the implemented surface.
- **Merge slices:** agent adaptation; gateway/proxy separation; recipe set; knowledge conventions; status/acceptance closeout.

## Parallel execution model

P0–P5 are acceptance milestones. Implementation runs as dependency-gated workstreams that reintegrate into one branch, so several agents can work at once without competing for the same paths.

```text
P0 reviewed documentation
          │
P1.0 relocation + P1.1 executable contract checkpoint
          │
          ├─ kernel state suite → progression/waits/recovery/cleanup
          ├─ PostgreSQL schema and native-shell capabilities
          ├─ Process / Podman / Kubernetes adapters
          ├─ HTTP / MCP / CLI adapters
          └─ P5-A retained-consumer adaptation + recipe preparation
                         │
                P3 composition and qualification
                         │
                     Cutover
                         │
          P4 deletion as each dependency is released
                         │
             P5-B recipe evidence and final closeout
```

A workstream starts when its actual prerequisites are merged, not when an entire numbered phase finishes. P2 may proceed while P1 adapters are still being ported, provided the kernel contract is frozen. P3 cutover needs P1 lifecycle evidence and P2 shell evidence. P4 deletion is not a substitute for implementation and waits for every retained consumer, including P5-A. P5-B closes status and evidence last.

### Reintegration protocol

The integration branch is `task/adr13-pipeline-runtime` in `.work/adr13-pipeline-runtime`. Child tasks branch from a recorded integration checkpoint and reintegrate there; publication to `main` remains a separate explicit decision.

1. The coordinator records the integration checkpoint commit and the task's `owns`, dependencies, check, resources and acceptance names.
2. The implementer works only in its own worktree and owned paths, runs the declared check and commits. It never pushes, rebases, merges or touches another worktree.
3. The coordinator rebases the clean candidate onto the current integration tip and renews affected evidence if relevant inputs moved.
4. An independent reviewer approves the exact candidate diff and the recorded checks.
5. One serialized merge lane reintegrates the approved candidate, then the coordinator updates the task record and removes only that task's worktree and branch.
6. `make lint-go test-go-unit` runs on the integration tip every five merges and at queue drain; a failed batch pauses further merges until repaired.

### Workstream ownership

One writer per path. A relocation task owns both source and destination. Existing mixed packages stay read-only to parallel replacement tasks until their extraction or deletion owner acts. Shared interfaces change through a bounded prerequisite task.

| Workstream | Exclusive ownership (target) | Notes |
| --- | --- | --- |
| Boundary extraction | `internal/kernel/{api,bus,dispatch,store,secret,serve}` and their consumers; transitional runner | One ordered owner before broad fan-out |
| Kernel contract | `internal/kernel/pipeline`, new kernel model/port packages, `internal/kernel/runtime` | Freezes C1–C5 and the contract decision table |
| Kernel engine | `internal/kernel/runner` | One owner delivers progression, waits, recovery and cleanup sequentially |
| State persistence | `internal/shell/runstate` (memory and PostgreSQL split by subdirectory), `test/support/kernelcontracts` | Shared suites have one owner; backends consume them |
| Pipeline registry | `internal/shell/pipelines` | Project-unique names, indexing, source conflicts |
| Task tracking | `internal/shell/tasks` | Content, current execution, projection, inbox |
| Messaging | `internal/natsbus`, `internal/shell/outbox` | Outbox, deduplication, reconciliation |
| Git authority | `internal/shell/git`, explicitly assigned `internal/gitservice` and `contracts/git` files | Exact step, candidate and ref authority |
| Preparation and custody | `internal/shell/sources`, `internal/shell/secrets` | Pinned preparation and scoped credentials |
| Runtime backends | `internal/runtime/process`, `internal/runtime/podman`, `internal/kube` | Each consumes the shared lifecycle suites |
| Transports | `internal/shell/api`, `internal/shell/mcp`, `internal/shell/cli` | Adapters and package tests only |
| Composition and interfaces | `internal/app` (including `apispec`), `cmd/kodama`, `cmd/kodactl`, `api/openapi.yaml`, `api/client`, shared API fragments | One ordered owner; P2 hands over constructors |
| Operations | `deploy/`, `dev/`, Make/CI targets | Changes to app settings registration pass through composition |
| Schema | `internal/storage`, schema documentation | Sole migration numbering and grants owner |
| Agentic adaptation | `internal/agents`, `internal/gateway`, `internal/mcpproxy` | P5-A; gates deletion of legacy consumers |

### Contract decisions to freeze before fan-out

Parallel agents must not independently decide these. The listed resolution is the proposed initial scope; the owning specification records the final choice before parser, engine, SQL and agent adapters implement it.

| Decision | Proposed initial scope |
| --- | --- |
| Rework loops | Keep execution acyclic; express rework as declared forward steps or an explicit new run with explicit source/output continuity, never reuse of a discarded workspace |
| Agent-requested waits | The container settles, records a bounded request/result, then a declared wait consumes it; no mid-command suspension |
| Meaning of resume | Continue the same run through a declared next step with persisted references; do not replay the previous command |
| Conditions | Use bounded `when` predicates over declared inputs, prior step results and wait outcomes; add a condition step kind only when a requirement demands it |
| Rejection and joins | A rejected input is a recorded outcome, not infrastructure failure; skipped prerequisites and joins have explicit semantics; protected steps cannot be reached unconditionally |
| Deadline races | One atomic version-checked decision across resolution, expiry and cancellation; duplicates return the recorded result |
| Assignment and correlation | Represent separately; every changed candidate needs a fresh wait identity and decision |

### Capacity

Start with four implementers and scale to six after the contract checkpoint. Reserve reviewer capacity so approved work does not queue behind integration. Give each concurrent session its own `DEV_SCOPE`; serialize jobs that share a service stack or cluster.

Every work package is one or more task-sized merges with its own owned paths, scoped checks, and independent review. A phase gate is not satisfied by a green unit run alone when the phase claims real PostgreSQL, NATS, container, or Kubernetes behavior.

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
