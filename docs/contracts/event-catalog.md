# Event catalog (NATS JetStream)

**Status:** Internal system-bus contract under [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38).

NATS JetStream is the native shell's central bus. It carries committed system events, execution observations, external signals, Git triggers, and monitoring subscriptions. PostgreSQL remains authoritative; the outbox publishes to NATS. The pipeline-runtime kernel emits execution events through its ports and does not depend on NATS types.

## Subject hierarchy

```text
kodama.v1.<tenant-key>.<project-key>.<domain>.<action>
```

Tenant-wide events use `_` for `<project-key>`.

## Envelope

```json
{
  "id": "evt_01J8Z9X2",
  "occurredAt": "2026-10-30T12:00:00Z",
  "tenant": "acme",
  "project": "erp",
  "type": "wait.resolved",
  "actor": {"type": "user", "id": "usr_17"},
  "resource": {"type": "run_step", "id": "run_01/review"},
  "data": {
    "runId": "run_01",
    "stepKey": "review",
    "waitId": "wait_01",
    "outcome": "approved"
  }
}
```

## Initial event types

| Event type | Emitted when | Key payload fields and coordination role || --- | --- | --- |
| `git.refs_updated` | An accepted receive applied an update | `receiveId`, repository, actor, origin run, and applied `(ref, old, new, kind)` updates inline; indexing and trigger admission consume it |
| `pipeline.indexed` | A definition at an exact commit was accepted, refused, or removed | Pipeline name, repository, commit, source, version, refusal code |
| `run.created` | A run is admitted | Pipeline, optional task, definition/input snapshot digest, trigger identity |
| `run.state_changed` | A run changes state | Previous/new state, bounded reason, version |
| `run_step.state_changed` | A step changes state | Run, step key, previous/new state, bounded reason, version |
| `wait.opened` | A wait step commits its durable state | Run, step key, wait identity, assignment/correlation, expected input, deadline |
| `wait.resolved` | An authorized result consumes a wait | Run, step key, wait identity, actor, outcome, answer reference |
| `service.state_changed` | A run, project, or tenant service changes state | Scope, run, service key, previous/new phase, bounded reason |
| `task.created` | A task is created with its selected pipeline | Task, pipeline, initial execution |
| `task.progress_changed` | A task projection advances | Task, current run, status, current step, waiting assignment, projection version |
| `repository.created` | A repository is provisioned | Name, default branch, hosting |
| `repository.state_changed` | A repository changes status | Previous/new status, bounded reason |
| `membership.changed` | A membership changes | User, changed binding, effective version |
| `project.settings_changed` | Project settings change | Changed setting names, version |
| `configuration.changed` | A variable or secret changes | Resource kind, scope, wrapping version, change kind |
| `artifact.registered` | A step registers external artifact metadata | Artifact, run, step, task, kind, URI, digest |
| `usage.recorded` | Agent or gateway usage is observed | Run/session, model, observed or estimated usage, uncertainty |

## Delivery rules

- Delivery is at least once. Consumers deduplicate on a stable event identity or on the resource identity they own.
- A state change and its outbox row commit in one transaction. Publication runs outside that transaction.
- A consumer that cannot safely apply an event leaves it for redelivery; a malformed or permanently irrelevant event is recorded and settled.
- Consumers tolerate reordering. A projection compares the run version and never lets an older event overwrite a newer view.
- Reconciliation repairs a missed wakeup from authoritative state.
- A `wait.resolved` event is a notification of a committed result, not the authority to approve anything. Authorization is checked when the result is recorded and again when a protected effect is issued.
- Payloads carry references and bounded metadata only: no secret values, credentials, workspace contents, or full logs.

## Retired event names

`work_item.*`, `workspace.*`, `gate.*`, `deployment.state_changed`, `step_attempt.state_changed`, and `run_resource.state_changed` described the superseded workflow, workspace, gate, deployment, and dual-runtime models. They are retained here only as historical names; current implementations must not emit them as product contracts.

## Run resource events

This anchor is retained for historical contracts. `run_resource.state_changed` and the companion run-resource lifecycle described the superseded dual-runtime model. Current execution observations use `run.state_changed`, `run_step.state_changed`, `wait.*`, and `service.state_changed`; the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) removes the legacy contracts.
