# Pipeline execution and services

**Scope:** Layer 1 pipeline-runtime kernel under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38). This specification owns pipeline documents, runs, steps, durable waits, workspaces, and container services. The [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) owns the transition from the current executable compositions.

## Model

1. **Pipeline (`pipelines`):**
   - Defined in `.kodama/<processname>.yaml` in a repository or registered through the API.
   - The whole parsed definition is persisted once in `pipelines.definition` (JSONB).
   - Names are unique within a project. The repository is the definition source, not a second naming scope; a conflicting definition source is refused.
   - Each run stores an immutable definition snapshot; later edits cannot change it.
2. **Run (`runs`):**
   - One execution of a pipeline against pinned inputs and optional source commits.
   - States: `queued`, `running`, `succeeded`, `failed`, `cancelled`.
   - Allocates one shared `/workspace` filesystem for its steps. A task may accumulate several runs; one is its current execution.
3. **Step (`run_steps`):**
   - One row per step per run, created when the run is admitted.
   - Container states: `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled`.
   - Wait states: `pending`, `waiting`, `succeeded`, `failed`, `cancelled`.
   - Runs the declared container `image` and `commands` with declared secret references, environment variables, and the shared `/workspace`. There is no agent-specific SQL kind.
4. **Wait:**
   - A non-container control step that pauses the same run until an authorized correlated signal or deadline arrives.
   - Persists a unique identity, expected input, accepted result, deadline, and continuation position.
   - Holds no worker and runs no command. A signal is not approval by itself.
5. **Condition:**
   - A bounded expression over declared inputs, prior step results, and wait outcomes that selects whether a step runs.
   - The executable grammar is frozen by the contract checkpoint; arbitrary code execution in the engine is not introduced.
6. **Scoped container services (`services`):**
   - Support containers such as PostgreSQL, Valkey, mock APIs, or preview servers.
   - **`pipeline` scope:** started with a run and stopped when it finishes.
   - **`project` scope:** long-lived support shared across a project's runs (`project_id` set, `run_id IS NULL`).
   - **`tenant` scope:** long-lived support shared across a tenant's projects (`project_id` and `run_id` null).
   - States: `starting`, `ready`, `failed`, `stopped`.

## Kernel boundary

The kernel executes a prepared plan. It receives the immutable pipeline snapshot, pinned inputs and sources, declared secret references, and resource requirements through kernel-owned ports. It does not read Git, call the Kodama API, query tasks, publish NATS subjects, or open SQL transactions.

The shell owns:

- resolving a project pipeline and preparing its run admission;
- indexing definitions from Git and matching triggers;
- source checkout and knowledge-base preparation;
- secret custody and authorization;
- task projection and wait-inbox authorization;
- durable state and outbox persistence;
- NATS publication and signal delivery;
- API, MCP, and CLI transport.

Concrete runtime, state, secret, workspace-preparation, log, and clock adapters are wired by the composition root. The kernel's unit and contract tests use memory implementations without a database, broker, or container.

## Execution

1. Admission snapshots the definition, inputs, sources, secret references, and resource requirements, and records one step row per step.
2. A worker claims the run and progresses one executable step at a time. A prior container must settle and release the workspace before the next executable step starts.
3. The backend launches a workload with a stable identity. Observation and recovery use that identity; an uncertain launch or stop is reconciled and never blindly replayed.
4. A wait commits its durable state before its signal is announced. An authorized result consumes the wait and advances the same run.
5. Cancellation blocks new claims, stops active workloads, and preserves evidence needed to settle uncertain effects.
6. A run is terminal only after every owned resource is verified stopped and removed, or after an explicit bounded failure records why cleanup remains pending.
7. A rerun creates a new run, a new workspace, and new history. It does not reset completed step rows.

## Shared workspace and services

Every run uses one disposable shared `/workspace`:
- The shell prepares the pinned source and context through an injected preparation step or adapter before user commands run.
- Steps execute in deterministic topological order, sharing generated files, caches, and commits.
- Secret material is mounted outside `/workspace`; a step explicitly granted a secret may copy it into shared files, and publication filters must account for that disclosure.
- When the run finishes, the workspace is removed. Durable outputs are Git commits and metadata-only artifacts.
- Run services start with the run and are ready before dependent commands run. Loss of an essential service fails the run; it is not silently recreated with empty state.

## Signals and coordination

NATS is the shell's central bus. The shell publishes committed events and routes signals, triggers, and monitoring subscriptions. The kernel emits execution events through its ports; it does not depend on NATS types or subjects.

- **Git triggers:** `git.refs_updated` matches pipeline triggers and admits a run pinned to the pushed commit.
- **Task and wait signals:** an authorized resolution emits the signal for the exact run, step, and wait.
- **Run and service observations:** committed run/step transitions feed the task projection and monitoring consumers.
- **External signals:** a pipeline may wait for an integration callback with an explicit correlation and deadline.

Delivery is at least once. Consumers deduplicate against run, step, wait, event, or trigger identity. A missed wakeup is repaired by reconciliation.

## Requirements

| ID | Requirement |
| --- | --- |
| PX-28 | The kernel MUST execute a pipeline snapshot through kernel-owned state, runtime, resource, secret, workspace-preparation, log, and clock ports; it MUST NOT import shell, task, Git-hosting, SQL/NATS/HTTP client, identity, or agent packages. |
| PX-29 | A project MUST have at most one pipeline per name; conflicting definition sources MUST be refused rather than silently replacing one another. |
| PX-30 | A run MUST snapshot its definition and inputs at admission and MUST create one step row per step; a later pipeline edit MUST NOT change an admitted run. |
| PX-31 | A wait MUST pause the same run without a running workload and persist its identity, expected input, deadline, accepted result, and continuation; a duplicate or late signal MUST NOT advance it twice. |
| PX-32 | A run MUST progress through durable intent and reconciliation: workload identity is stable, uncertain launch/stop is not replayed blindly, and a run is terminal only after owned resources are confirmed stopped and removed or cleanup remains visibly pending. |
| PX-33 | The shell MUST publish committed execution events to NATS and consumers MUST deduplicate and reconcile; the kernel MUST remain runnable without a broker. |
| PX-34 | Run-scoped services MUST reach `ready` before dependent steps and MUST NOT be silently recreated after loss; tenant- and project-scoped services MUST outlive individual runs. |

PX-01–PX-05 are retired by this amendment. They described the previous run model, project workflow triggers, and kernel composition. Historical acceptance reports that cite them remain historical evidence for the superseded model.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| PX-A24 | A plain multi-step pipeline runs with memory/Process adapters and no SQL, NATS, Git, HTTP, task, or agent service; two steps share workspace files and declared secrets | PX-28, PX-30 |
| PX-A25 | A project refuses a duplicate pipeline name from another repository; an admitted run keeps its snapshot after the definition changes | PX-29, PX-30 |
| PX-A26 | A run waits across controller restart without a worker; one authorized signal resumes it once, a duplicate and a late signal change nothing, and a run-scoped service is not silently recreated after loss | PX-31, PX-32, PX-34 |
| PX-A27 | With the broker unavailable, state commits and the outbox retains the event; after recovery, consumers process it once, a redelivery changes nothing, and reconciliation repairs a missed wakeup | PX-32, PX-33 |

## run_shared model

This anchor is retained for historical contracts. The `run_shared` name and its `v1alpha2`/`v1alpha3` documents describe the superseded dual-format execution model. The current specification has one shared-workspace model, described above; the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) removes the legacy formats and consumers.
