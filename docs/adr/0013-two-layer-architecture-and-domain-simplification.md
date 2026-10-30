---
status: accepted
---

# ADR 0013: Pipeline-runtime kernel and four-layer architecture

**Date:** 2026-10-30
**Status:** Accepted. This amendment replaces the original two-layer decision with a four-layer functional architecture and makes the pipeline the sole process definition for a task.
**Supersedes:** [ADR 0004](0004-logical-workspaces.md), [ADR 0005](0005-git-backed-workspaces.md), [ADR 0012](0012-container-sdlc-and-artifact-releases.md); simplifies [ADR 0002](0002-postgres-state-and-events.md), [ADR 0010](0010-run-workspace-services-builders.md), and [ADR 0011](0011-internal-tenant-git-service.md).
**Amends:** C-24 and O-35 in the [decision register](../decisions.md); the resolved refinement is [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38).
**Implementation:** [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md).

## Context

Kodama is pre-release. Its founding abstraction is small: a named pipeline executes container steps against a shared workspace with declared secrets and services. A task asks for work; a pipeline describes how that work progresses.

Successive implementation passes grew two competing ideas on top of that primitive:

1. A per-project task workflow with its own states, edges, parking fields, and state-to-pipeline trigger map. A task therefore had a process definition separate from the pipeline it ran, and the two could disagree.
2. A core kernel that included Git hosting, task tracking, SQL, HTTP, OAuth, NATS, and agent services. That made the execution primitive impossible to implement or test without the whole product.

The database and package count were symptoms. The governing problem was ownership: process progression, execution, native integration, and agentic behavior were mixed together.

Because Kodama is pre-release, one authoring model and one executable model can replace the accumulated compatibility layers. Existing databases are never silently dropped or reinterpreted.

## Decision

### 1. Four functional layers

| Layer | Responsibility |
| --- | --- |
| **1. Pipeline Runtime — Kernel** | Pipeline execution semantics: run and step lifecycle, container dispatch and observation, shared run workspaces, run-scoped support containers, declared secret delivery, generic waits and signals, cancellation, deadlines, recovery, logs, and outputs. |
| **2. Native Services — Shell** | Tenant and project identity, authorization, project pipeline registry, task tracker, native Git, secret custody, metadata-only artifacts, long-lived tenant/project services, HTTP API, Kodama MCP server, CLI, persistence and runtime adapters, and the central NATS system bus. |
| **3. Agentic Services** | Native agent sessions and runtime, harness integrations, agent tools and skills, AI gateway, and external MCP proxy. Agent work runs through ordinary kernel container steps and uses shell services. |
| **4. External Behavior** | SDLC process definitions, release/deployment/rollback recipes, QA policy, notifications, organizational conventions, scripts, and container hooks. These may live entirely in tenant or external repositories and images. |

These are dependency boundaries, not four mandatory deployments. Kodama remains one Go module with explicit composition.

The kernel depends only on small runtime-owned ports and value types. It has no dependency on tasks, Git hosting, HTTP or MCP, NATS, SQL drivers, OAuth, or agent packages. Concrete adapters and process configuration are assembled outside it. Secrets remain an execution primitive through declared references and a resolver/materialization port; encryption, key custody, scope authorization, and rotation belong to shell adapters. Workspace preparation is injected or expressed as ordinary preparation steps, so the kernel does not know native Git credentials or knowledge-base repository conventions.

The Kodama MCP server exposes native operations in Layer 2. The proxy to external MCP servers and agent-specific tool policy belong to Layer 3.

### 2. The pipeline is the process definition

```text
tenant
  project
    pipelines (project-unique names)
      runs
        run_steps
    tasks (each selects a pipeline; records current execution progress)
```

Each task selects one project pipeline by name. Persistence resolves the name to an immutable pipeline ID. Pipeline names are unique within a project; a repository is the definition's source, not a second naming scope. A conflicting definition from another repository or from the API is rejected instead of silently taking precedence. Pipeline renaming is not part of the initial interface.

A run snapshots its pipeline definition and inputs at admission. A task may accumulate several runs through explicit reruns or rework, with one current execution selected for its progress view. Git-triggered and API-triggered runs may exist without a task.

Remove `projects.task_workflow`, independently configurable task transition graphs, and the state-to-pipeline trigger map. Business phases such as planning, implementation, review, test, and deployment are step names or labels in pipeline YAML. They are not a second state machine in SQL.

Tasks retain ordinary tracking data and add queryable execution fields: the selected pipeline, current run, execution status, current step, waiting reason and assignment, result reference, and optimistic version. Run and step records own execution truth. Cached task progress is updated idempotently from versioned runtime events and can be rebuilt from those records.

### 3. Durable generic waits

A pipeline can pause at a non-container wait step and resume the same run after a correlated signal. The kernel persists the wait identity, expected input, accepted result, and continuation position. Authorization and human-facing inbox operations belong to the shell; approval, rejection, and rework behavior is expressed in the pipeline.

A waiting step holds no worker and runs no command. The run workspace and retained support services have explicit bounded lifetimes. Existing run services may continue consuming resources during a wait; stopping and recreating them is never silently treated as preserving their state. Loss of required workspace or service state fails the run rather than replaying earlier commands.

A role inbox lists tasks whose current pipeline step waits for that role. Resolving an inbox entry records the authorized input against that exact run, step, and wait, then emits its signal. A successful signal delivery is not itself approval: the recorded outcome determines the pipeline's next branch. This replaces creating a separate blocking task solely to drive workflow resumption.

### 4. NATS is the shell's central system bus

NATS JetStream carries native system events, execution observations, external signals, pipeline triggers, and monitoring subscriptions. Git, the task tracker, APIs, CLI and MCP, agents, and external integrations connect through this common contract. In deployed `test` and `production` profiles, NATS is a required shell service.

PostgreSQL remains the authoritative durable state. State changes and event-publication intent commit together; the shell publishes the outbox to JetStream. Consumers tolerate duplicate and out-of-order delivery and deduplicate commands against the affected run, step, wait, or trigger identity. Periodic reconciliation repairs missed wakeups. This retains [ADR 0002](0002-postgres-state-and-events.md)'s authority model while making NATS central to coordination.

The kernel receives commands and signals and emits execution events through its ports. Its unit and contract tests use memory implementations without a broker or database. NATS subject names and client types do not enter its execution model.

### 5. Minimal persistence and one supported model

Keep relational ownership, identities, queryable status, and concurrency keys. Use JSON for pipeline documents, immutable run inputs, wait payloads, outputs, and metadata. Keep separate `runs` and `run_steps` records to preserve execution history and support source-free runs. A task is not a substitute execution record.

Store generic waiting state and results with run steps. An independent workflow, gate, or approval engine is unnecessary. Keep metadata-only external artifacts. Agent session and MCP registration persistence belongs to Layer 3 and is not a dependency of the kernel or native shell behavior.

The fixed table-count promise is withdrawn in favor of an explicit ownership inventory. The current baseline has 16 product tables; this refinement primarily removes duplicate state, fields, and code. Any later table consolidation must follow a concrete lifecycle or query need.

Kodama converges on one authoring contract, one execution implementation, and one server composition. Retired formats, compatibility aliases, and obsolete consumers are removed as their replacements become executable. A fresh-database cutover is explicit; the migration runner never deletes or transforms an existing database.

### 6. Dependency direction

- Layer 1 imports no shell, transport, persistence driver, broker, identity, Git-hosting, task, or agent package.
- Layer 2 builds on kernel contracts and may host native transport, persistence, Git, tasks, identity, and messaging adapters.
- Layer 3 builds on shell and kernel public contracts. It does not add execution semantics or private shell state access.
- Layer 4 uses public shell APIs, Git, NATS signals, and ordinary container images. It cannot require a controller plugin or a kernel code change.

Ports are consumer-owned and narrow. Infrastructure adapters are wired explicitly. A shared package is extracted only where two owners genuinely share a contract, not to avoid defining a boundary.

## Consequences

- A plain pipeline can be implemented and tested without Git, task tracking, SQL, NATS, HTTP, or agents.
- One pipeline describes a task's process, including human waits and conditional progression.
- Native Git and messaging remain first-class shell services with their own durability and authorization responsibilities.
- Specialized delivery, QA, notification, and governance behavior moves to pipelines and containers.
- Kernel isolation does not remove runtime obligations: durable identity, cancellation, stop confirmation, workspace exclusion, resource cleanup, and conservative treatment of unknown command outcomes remain execution semantics.
- The original two-layer wording and the project workflow model are superseded. ADRs [0001](0001-native-kubernetes-jobs.md), [0003](0003-shared-control-plane.md), [0006](0006-hexagonal-domain-design.md), [0007](0007-platform-core-and-agent-layer.md), [0008](0008-runtime-seam.md), and [0009](0009-environments.md) continue to govern their applicable runtime, tenancy, dependency, and deployment responsibilities. ADRs [0002](0002-postgres-state-and-events.md), [0010](0010-run-workspace-services-builders.md), and [0011](0011-internal-tenant-git-service.md) are refined at the boundaries described here. ADRs [0004](0004-logical-workspaces.md), [0005](0005-git-backed-workspaces.md), and [0012](0012-container-sdlc-and-artifact-releases.md) remain historical and superseded as previously recorded.
- Implementation is staged: amend the governing contracts first, isolate and complete the standalone kernel, simplify persistence and rebuild the native shell, converge runtime adapters and composition, delete superseded implementation, then externalize specialized behavior. The [simplification plan](../implementation/pipeline-runtime-simplification.md) owns the ordered work, verification, and completion criteria.

## Historical note

The original ADR 0013 (2026-10-30) introduced two layers and a 14-domain-table model plus audit/outbox. It reduced a 74-table schema but retained a project task state machine, a workflow-to-pipeline trigger map, and a kernel that composed native services. Its Phase 2 schema and kernel slice remain implemented history. This amendment replaces that direction; it does not retroactively claim that the earlier work was delivered as the four-layer design.
