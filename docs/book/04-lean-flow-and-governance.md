# 4. Autonomous flow and role inbox

Kodama maximizes autonomous SDLC execution while keeping human intervention simple, explicit, and asynchronous.

## Core mechanisms

| Mechanism | How it works |
| --- | --- |
| **Pipeline-owned process** | A task selects one project-unique pipeline. That pipeline defines planning, implementation, review, test, deployment, rework, and human waits. There is no separate `projects.task_workflow`. |
| **NATS coordination bus** | Git receives, pipeline indexing, wait resolutions, task projection, and run/service observations are committed to PostgreSQL and published to NATS JetStream. |
| **Agentic guardrails** | Native `kodama agent` steps validate plans, diffs, tests, and security rules autonomously, and select a pipeline branch or request a wait when judgment is needed. |
| **Durable waits and Role Inbox** | When human judgment is required, the pipeline pauses the same run at a wait step. The role inbox lists tasks whose current wait is assigned to the caller's role. |

## Why a pipeline wait replaces a separate Gate engine

A separate approval subsystem duplicates the process definition and holds execution in limbo. In Kodama:

1. The pipeline declares the wait, its expected input, its assignment, and its timeout. No container or worker stays busy while the run waits.
2. Humans inspect their inbox (`kodactl task inbox --role reviewer`, MCP, or API), where decisions and promotions appear in one queue with links to the candidate and artifact metadata.
3. Resolution verifies current authority and records an immutable input against the exact run, step, and wait. The signal resumes the same run, and the pipeline selects the next branch.
4. A rejection or clarification answer cannot take an approval branch unless the pipeline explicitly expresses that path. A changed candidate requires a new wait and decision.

## Flow example

```text
task selects sdlc pipeline
  -> plan
  -> implement
  -> review wait (reviewer)
       approved -> test -> integrate
       rejected -> rework -> review wait
  -> deploy recipe (Layer 4)
  -> task progress projection
```

The pipeline is the single process definition. The task is the durable work record and progress view. Git, NATS, and the shell carry source, events, authority, and integration; the kernel executes the steps and waits.

See [pipeline execution](../specs/pipeline-execution.md), [work management](../specs/work-management.md), and [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md).
