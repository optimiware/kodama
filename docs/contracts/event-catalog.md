# Event catalog (NATS JetStream)

**Status:** Internal message bus contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md).

NATS JetStream is the internal nervous system of Kodama. It captures and relays signals across Git repositories, the state-machine task tracker, pipeline steps, container services, agents, releases, and promotions.

## Subject hierarchy

```text
kodama.v1.<tenant-key>.<project-key>.<domain>.<action>
```
*(Tenant-wide events use `_` for `<project-key>`.)*

## Envelope

```json
{
  "id": "evt_01J8Z9X2",
  "occurredAt": "2026-10-30T12:00:00Z",
  "tenant": "acme",
  "project": "erp",
  "type": "task.transitioned",
  "actor": {"type": "agent", "id": "run_01/review"},
  "resource": {"type": "task", "id": "tsk_1042"},
  "data": {
    "from": "review",
    "to": "deploy_preview",
    "triggerPipeline": "preview"
  }
}
```

## Initial event types

| Event type | Emitted when | Key payload fields & coordination role |
| --- | --- | --- |
| `git.push` | Commits pushed to native Git service | `repoId`, `path`, `branch`, `oldSha`, `newSha`, `originRunId`; indexes `.kodama/*.yaml` and triggers matching pipelines |
| `git.merge` | Branch merged into target branch | `repoId`, `sourceBranch`, `targetBranch`, `commitSha` |
| `git.tag` | Release tag created | `repoId`, `tag`, `commitSha`; triggers release pipelines |
| `task.created` | New task or inbox item created | `taskId`, `kind`, `state`, `assignedRole`; triggers initial state pipeline if configured |
| `task.transitioned` | Task moves between states in `projects.task_workflow` | `taskId`, `from`, `to`, `triggerPipeline`; starts `.kodama/<trigger>.yaml` |
| `task.parked` | Task is parked waiting on a role-inbox task | `taskId`, `blockedByTaskId`, `assignedRole`, `kind` (`decision`\|`promotion`\|`incident`) |
| `task.resolved` | Human resolves a blocking role-inbox task | `inboxTaskId`, `unblockedTaskId`, `decision`, `resumeState`; unblocks parent task and triggers next pipeline |
| `run.queued` / `run.started` / `run.completed` | Pipeline run lifecycle changes | `runId`, `pipelineName`, `taskId`, `status`, `commitSha` |
| `step.completed` | Individual `run_steps` container finishes | `runId`, `stepKey`, `status`, `exitCode`, `outputs` |
| `service.ready` / `service.stopped` | Scoped container service changes state | `serviceId`, `scope` (`tenant`\|`project`\|`pipeline`), `name`, `endpoint` |
| `artifact.published` | Step records external artifact metadata | `artifactId`, `name`, `kind`, `uri`, `digest`, `runId`, `taskId` |
| `release.promoted` | Promotion pipeline completes | `taskId`, `runId`, `artifactId`, `target` |

## Run resource events

For backward reference with existing runtime adapters during Phase 3 consolidation, container/service lifecycle observations map to `service.ready` / `service.stopped` and `step.completed` on the tenant's NATS stream. Append-only audit records are written to `audit_events` in PostgreSQL.
