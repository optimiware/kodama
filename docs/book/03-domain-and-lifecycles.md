# 3. Domain and lifecycles

Use the [glossary](../../CONTEXT.md) for terms and [database schema](../implementation/database-schema.md) for persistence.

## Core entity hierarchy

At the core, Kodama's relational domain model is **15 tables** (14 domain tables plus audit/outbox) spanning Identity, Layer 1 (Core Primitives), and Layer 2 (Agentic Factory):

```mermaid
erDiagram
    TENANT ||--o{ MEMBERSHIP : grants
    TENANT ||--o{ PROJECT : owns
    TENANT ||--o{ REPOSITORY : "hosts (tenant/context)"
    TENANT ||--o{ SERVICE : "runs (tenant scope)"
    TENANT ||--o{ SECRET : "stores (tenant scope)"
    PROJECT ||--o{ REPOSITORY : "auto-links (tenant/project/*)"
    PROJECT ||--o{ PIPELINE : "defines (.kodama/*.yaml)"
    PROJECT ||--o{ SERVICE : "runs (project scope)"
    PROJECT ||--o{ SECRET : "stores (project scope)"
    PROJECT ||--o{ TASK : tracks
    TASK ||--o| TASK : "blocked_by (role inbox)"
    PIPELINE ||--o{ RUN : executes
    TASK ||--o{ RUN : motivates
    RUN ||--o{ RUN_STEP : "executes (shared /workspace)"
    RUN ||--o{ SERVICE : "runs (pipeline scope)"
    RUN_STEP ||--o{ ARTIFACT : "records metadata"
    RUN_STEP ||--o{ AGENT_SESSION : "tracks (Layer 2)"
    PROJECT ||--o{ MCP_SERVER : "registers (Layer 2)"
```

## Resource lifecycles

| Resource | States | Authority |
| --- | --- | --- |
| **Task** | Project-defined in `projects.task_workflow` (default: `backlog`, `plan`, `implement`, `review`, `test`, `deploy_preview`, `promote`, `done`, `parked`, `cancelled`) | [Task tracker](../specs/work-management.md) |
| **Run** | `queued`, `running`, `succeeded`, `failed`, `cancelled` | [Pipeline execution](../specs/pipeline-execution.md) |
| **Step (`run_step`)** | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | [Pipeline execution](../specs/pipeline-execution.md) |
| **Service** | `starting`, `ready`, `failed`, `stopped` | [Pipeline execution](../specs/pipeline-execution.md) |
| **Agent session** | `running`, `completed`, `parked`, `failed`, `cancelled` | [Agent runtime](../specs/agent-runtime.md) |

## Task state machine and Role Inbox

Each project defines its task state machine in `projects.task_workflow` (stored in PostgreSQL), mapping states and transitions to `.kodama/<processname>.yaml` pipelines via NATS events:

```mermaid
stateDiagram-v2
    [*] --> backlog
    backlog --> plan : triggers .kodama/plan.yaml
    plan --> implement : triggers .kodama/implement.yaml
    implement --> review : triggers .kodama/review.yaml
    review --> implement : rework needed
    review --> test : triggers .kodama/test.yaml
    test --> deploy_preview : triggers .kodama/preview.yaml
    deploy_preview --> parked : creates promotion task in role inbox
    parked --> promote : inbox task resolved (.kodama/promote.yaml)
    promote --> done
    plan --> parked : creates decision task in role inbox
    implement --> parked : creates decision task in role inbox
    parked --> plan : inbox task resolved
    parked --> implement : inbox task resolved
```

The diagram illustrates workflow behavior. In the Phase 2 model, parking preserves the business state and sets `parked_at`, `blocked_by_id` and `resume_state`. Inbox states are project-defined. Resolution records an explicit outcome; an applicable approval or clarification answer can resume the task, while rejection cannot promote it. See the [task specification](../specs/work-management.md).
