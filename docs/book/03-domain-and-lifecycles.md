# 3. Domain and lifecycles

Use the [glossary](../../CONTEXT.md) for terms and [schema sketch](../implementation/database-schema.md) for persistence. Work item, workspace, run, and review are separate lifecycles.

```mermaid
erDiagram
    TENANT ||--o{ PROJECT : owns
    PROJECT ||--o{ REPOSITORY : hosts
    PROJECT ||--o{ WORK_ITEM : tracks
    WORK_ITEM ||--o| WORKSPACE : pairs
    WORKSPACE }o--o{ REPOSITORY : "task branches in"
    REPOSITORY ||--o{ PUSH_RECORD : journals
    WORK_ITEM ||--o{ RUN : motivates
    PUSH_RECORD ||--o{ RUN : triggers
    RUN }o--o{ REPOSITORY : "pins commits of"
    PIPELINE_DEFINITION ||--o{ RUN : instantiates
    RUN ||--o{ STEP_RUN : contains
    STEP_RUN ||--o{ ATTEMPT : executes
    STEP_RUN ||--o| GATE : waits
    ATTEMPT ||--o{ ARTIFACT : produces
    ENVIRONMENT ||--o{ DEPLOYMENT : receives
```

Repositories are project resources; under the designed internal hosting of [ADR 0011](../adr/0011-internal-tenant-git-service.md) Kodama serves them itself, records every accepted push, and starts runs from those pushes as well as from work items. A run pins a source set: one exact commit per selected repository. Source checkpoints/candidates are Git commits, not separate storage aggregates. Under the designed `run_shared` model a run also owns disposable **run resources** (one shared scratch `/workspace`, preparation, support services, a network boundary); they are execution bookkeeping with their own evidence, not a lifecycle status, an aggregate or a checkpoint ([run resources](../contracts/run-resources.md)). Evidence is artifact metadata/provenance. An agent profile expands into an ordinary executable step. A defect is a work item. A plan is part of the specification.

## States

| Resource | Persisted status | Authority |
| --- | --- | --- |
| Work item | `backlog`, `ready`, `active`, `review`, `done`, `cancelled` | [Work management](../specs/work-management.md) |
| Repository | `provisioning`, `ready`, `archived` | [Git repositories](../specs/git-repositories.md) |
| Push record | `authorized`, `applied`, `refused`, `unknown` | Git repositories |
| Workspace | `provisioning`, `available`, `archived` | [Workspaces](../specs/workspaces.md) |
| Run | `queued`, `running`, `succeeded`, `failed`, `cancelled` | [Execution](../specs/pipeline-execution.md) |
| Step | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | Execution |
| Attempt | `running`, `succeeded`, `failed`, `cancelled` | Execution |
| Gate | `pending`, `approved`, `rejected` | [Delivery](../specs/delivery-and-quality-gates.md) |
| Deployment | `pending`, `running`, `succeeded`, `failed` | Delivery |

Waiting, startup, timeout, unknown outcome, cancellation request, expiry, and deletion use reasons/timestamps. They are not additional status enums. This preserves uncertainty instead of hiding it behind an assumed success/failure.

```mermaid
stateDiagram-v2
    [*] --> backlog
    backlog --> ready
    ready --> backlog
    ready --> active
    active --> ready
    active --> review
    review --> active
    review --> done
    done --> active
    backlog --> cancelled
    ready --> cancelled
    active --> cancelled
    review --> cancelled
```

All entries to active check WIP capacity. Completion is a human decision against current content/source. A passing run does not move the card automatically. A failed run leaves the work branch and published commits available. A gate wait retains a database record without a Job; in the current `attempt_private` model it retains no compute, while in `run_shared` the run's services and scratch persist through the wait, bounded by the run's total deadline.

Content revision changes only when specification/acceptance changes; resource version changes on every mutation. Runs pin content revision and source commit. Gates additionally bind evidence and deployment configuration. Historical runs/decisions remain immutable after rework.
