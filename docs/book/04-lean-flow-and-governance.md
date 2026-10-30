# 4. Autonomous flow and role inbox

Kodama maximizes autonomous SDLC execution while keeping human intervention simple, explicit, and asynchronous.

## Core mechanisms

| Mechanism | How it works |
| --- | --- |
| **Per-project state machine** | Each project defines its states, valid transitions, and pipeline triggers in `projects.task_workflow` in PostgreSQL. |
| **NATS coordination bus** | Every Git push/tag/merge, task state transition, inbox resolution, and release signal is published to NATS JetStream, triggering the matching `.kodama/<processname>.yaml` pipeline. |
| **Layer 2 validation & guardrails** | Instead of pausing for humans at every step, native `kodama agent` steps validate plans, code diffs, tests, and security guardrails autonomously, looping tasks back (`review -> implement`) when fixes are needed. |
| **Parked tasks & Role Inbox** | When human judgment is genuinely required (ambiguous requirements, production promotion, or incident triage), the task is set to `parked` and linked via `blocked_by_id` to a new task assigned to a role inbox (`assigned_role`). |

## Why Role Inbox replaces a separate Gate engine

Traditional CI/CD platforms bolt on a separate approval/gate subsystem that holds pipeline runs in limbo or requires complex digest matching. In Kodama:
1. A pipeline run finishes cleanly after parking the task and creating the blocking inbox task. No container or run slot stays idle waiting for a human.
2. Humans inspect their inbox (`kodactl tasks inbox --role reviewer`, MCP, or API), where decision requests, promotion approvals, and incident actions appear in one uniform queue with links to the commit, diff, and external artifact metadata.
3. Resolving the inbox task records the human's decision in PostgreSQL and emits `task.resolved` on NATS, which unblocks the parent task and starts the next pipeline (such as `.kodama/promote.yaml`).
