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
| `work_item.created` | Work item | Kind, initial state |
| `work_item.updated` | Work item | Changed field names, content revision where applicable |
| `work_item.transitioned` | Work item | Previous/new state, transition ID |
| `workspace.state_changed` | Workspace | Previous/new state, reason code |
| `workspace.checkpoint_created` | Workspace | Repository, previous/new commit, publishing attempt or human synchronization |
| `run.created` | Run | Work item, definition ID, input-snapshot digest |
| `run.state_changed` | Run | Previous/new state, classified reason |
| `step_attempt.state_changed` | Attempt | Run/step IDs, previous/new state, classified reason |
| `run_resource.state_changed` | Run resource (`run_shared` runs; envelope resource type `run_resource`; resource ID [`rr-…`](#run-resource-events); project required) | Run ID, kind (`network`, `workspace`, `preparation`, `service`), key, previous/new phase, safe reason code ([catalog](run-resources.md#reasons)), spec fingerprint, whether a runtime UID is bound |
| `repository.created` | Repository (designed internal hosting) | Name, default branch, hosting |
| `repository.state_changed` | Repository | Previous/new status, reason code |
| `git.refs_updated` | An accepted receive (`resource.type` `git_push`, `resource.id` the payload's `receiveId`: the push is its receive); exactly one per receive that applied an update | `receiveId`, repository, actor, origin run and the applied `(ref, old, new, kind)` updates inline, never a server ref ([Git service](git-service.md#push-events)); trigger admission and pipeline indexing consume it ([triggers](git-service.md#trigger-configuration)) |
| `gate.requested` | Gate | Subject digest, content revision, reviewer scope, deadline |
| `gate.decided` | Gate | Actor, decision, resulting state, subject digest |
| `gate.invalidated` | Gate | Subject digest, reason, superseding commit/revision when present |
| `deployment.state_changed` | Deployment | Environment, previous/new state, source/artifact references |
| `membership.changed` | Membership | User ID, changed binding references, effective version |
| `project.settings_changed` | Project | Changed setting names, version |
| `configuration.changed` | Variable/secret | Resource kind, value/wrapping version, change kind, scope |
| `usage.recorded` | Usage record | Run/attempt, model, observed or estimated usage, uncertainty |

`run_resource.state_changed` carries references only: no endpoints beyond the logical key, logs, credentials, secret values or scratch contents, and scratch changes never produce `workspace.checkpoint_created`. Gate expiry is an applicability check against its deadline, not a status transition; a run deadline that expires while a gate is pending invalidates it with reason `run_deadline_exceeded`. Agent results and defects use attempt/work-item events. No separate session, candidate, finding, or policy subsystem is introduced.

## Run resource events

For backward reference with existing runtime adapters during Phase 3 consolidation, container/service lifecycle observations map to `service.ready` / `service.stopped` and `step.completed` on the tenant's NATS stream. Append-only audit records are written to `audit_events` in PostgreSQL.
