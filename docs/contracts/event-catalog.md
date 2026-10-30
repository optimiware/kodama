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
    "signalId": "sig_01",
    "outcome": "approved"
  }
}
```

## Initial event types

Each event has a stable identity derived from its type and the deduplication key below, so republishing an outbox row yields the same event `id`. Consumers deduplicate on that key.

| Event type | Emitted when | Key payload fields and coordination role | Deduplication key |
| --- | --- | --- | --- |
| `git.refs_updated` | An accepted receive applied an update | `receiveId`, repository, actor, origin run, and applied `(ref, old, new, kind)` updates inline; indexing and trigger admission consume it | `receiveId` |
| `pipeline.indexed` | A definition at an exact commit was accepted, refused, or removed | Pipeline name, repository, commit, source, version, refusal code | pipeline name, repository, commit |
| `run.created` | A run is admitted | Pipeline, optional task, optional continued run, definition/input snapshot digest, trigger identity | trigger key `(pipeline, push event, ref)` for triggered runs, otherwise run ID |
| `run.state_changed` | A run changes state | Previous/new state, bounded reason, version | run ID, run version |
| `run_step.state_changed` | A step changes state | Run, step key, previous/new state, bounded reason code, version. A skipped or failed step carries the bounded reason code and, where evaluated, the recorded `when` result defined by [pipeline execution](../specs/pipeline-execution.md#eligibility-skipping-and-joins) | run ID, step key, step version |
| `wait.opened` | A wait step commits its durable state | Run, step key, wait identity, assignment, correlation keys and digest, request reference, expected input kind, outcomes, deadline, wait version | wait ID |
| `wait.resolved` | An authorized result decides a wait | Run, step key, wait identity, deciding `signalId`, actor, declared outcome (a rejection is an ordinary outcome), answer reference | wait ID |
| `wait.expired` | The deadline decides a wait | Run, step key, wait identity, deadline, `onExpiry` (`fail` or `continue`) | wait ID |
| `wait.cancelled` | Cancellation or failure of the run decides a wait | Run, step key, wait identity, reason (`run_cancelled` or `run_failed`) | wait ID |
| `wait.reassigned` | A wait's assignment changes | Wait identity, previous and new assignment, actor, new wait version | wait ID, wait version |
| `signal.refused` | An authenticated signal was not applied | Target run, step and wait when resolvable, `signalId`, bounded reason (`decided`, `version_conflict`, `invalid_outcome`, `unknown_wait`, `ambiguous_correlation`, `unauthorized`), the recorded decision it lost to | `signalId`, wait ID |
| `service.state_changed` | A run, project, or tenant service changes state | Scope, run, service key, previous/new phase, bounded reason | scope, service ID, version |
| `task.created` | A task is created with its selected pipeline | Task, pipeline, initial execution | task ID |
| `task.progress_changed` | A task projection advances | Task, current run, status, current step, waiting assignment, projection version | task ID, projection version |
| `repository.created` | A repository is provisioned | Name, default branch, hosting | repository ID |
| `repository.state_changed` | A repository changes status | Previous/new status, bounded reason | repository ID, version |
| `membership.changed` | A membership changes | User, changed binding, effective version | membership ID, version |
| `project.settings_changed` | Project settings change | Changed setting names, version | project ID, version |
| `configuration.changed` | A variable or secret changes | Resource kind, scope, wrapping version, change kind | resource, version |
| `artifact.registered` | A step registers external artifact metadata | Artifact, run, step, task, kind, URI, digest | artifact ID |
| `usage.recorded` | Agent or gateway usage is observed | Run/session, model, observed or estimated usage, uncertainty | usage record ID |

## Delivery rules

- Delivery is at least once. Consumers deduplicate on a stable event identity or on the resource identity they own.
- A state change and its outbox row commit in one transaction. Publication runs outside that transaction.
- A consumer that cannot safely apply an event leaves it for redelivery; a malformed or permanently irrelevant event is recorded and settled.
- Consumers tolerate reordering. A projection compares the run version and never lets an older event overwrite a newer view.
- Reconciliation repairs a missed wakeup from authoritative state.
- A `wait.resolved` event is a notification of a committed result, not the authority to approve anything. Authorization is checked when the result is recorded and again when a protected effect is issued.
- A wait has at most one of `wait.resolved`, `wait.expired` and `wait.cancelled`; the decision rules are owned by [pipeline execution](../specs/pipeline-execution.md#wait-decisions). A rejection is a `wait.resolved` with the declared outcome and is never a failure event; its effect on later steps appears as `run_step.state_changed` with the skip or failure reason.
- A repeat of the winning signal emits nothing. A losing or invalid authenticated signal emits one `signal.refused` carrying references and a bounded reason, never the answer text. A signal that names another tenant's wait is reported to its caller as not found and emits no event.
- Payloads carry references and bounded metadata only: no secret values, credentials, workspace contents, or full logs.

## Requirements

| ID | Requirement |
| --- | --- |
| EC-01 | Every event type MUST define a deduplication key, and the event `id` MUST be derived from the type and that key so republication is idempotent. |
| EC-02 | A wait MUST emit at most one of `wait.resolved`, `wait.expired` and `wait.cancelled`, and a rejection MUST be carried as a declared outcome of `wait.resolved`, not as a failure event. |
| EC-03 | A repeated winning signal MUST emit no event; a losing or invalid authenticated signal MUST emit one `signal.refused` with a bounded reason and no answer content, and a foreign tenant's wait MUST emit none. |
| EC-04 | An assignment change MUST emit `wait.reassigned` with the new wait version and MUST NOT change the wait identity or correlation carried by other events. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| EC-A01 | Republishing an outbox row after a lost acknowledgment delivers the same event `id`, and a consumer applies it once | EC-01 |
| EC-A02 | Concurrent resolution, expiry and cancellation of one wait yield exactly one decision event; a rejection yields `wait.resolved` with the rejected outcome and a skipped successor, and no failure event | EC-02 |
| EC-A03 | A duplicate of the winning signal emits nothing; a late signal and an undeclared outcome each emit one `signal.refused` without the answer text; a signal naming a foreign tenant's wait emits no event | EC-03 |
| EC-A04 | Reassigning a wait emits `wait.reassigned` and later events carry the unchanged identity and correlation digest | EC-04 |

## Retired event names

`work_item.*`, `workspace.*`, `gate.*`, `deployment.state_changed`, `step_attempt.state_changed`, and `run_resource.state_changed` described the superseded workflow, workspace, gate, deployment, and dual-runtime models. They are retained here only as historical names; current implementations must not emit them as product contracts.

## Run resource events

This anchor is retained for historical contracts. `run_resource.state_changed` and the companion run-resource lifecycle described the superseded dual-runtime model. Current execution observations use `run.state_changed`, `run_step.state_changed`, `wait.*`, and `service.state_changed`; the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md) removes the legacy contracts.
