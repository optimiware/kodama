# Event catalog

**Status:** proposed domain-event contract. PostgreSQL state/outbox is authoritative; NATS JetStream provides at-least-once transport.

## Envelope

```json
{
  "id": "evt_01",
  "schemaVersion": 1,
  "type": "work_item.transitioned",
  "occurredAt": "2026-10-19T10:05:00Z",
  "tenantId": "t_acme",
  "projectId": "p_inventory",
  "resource": {"type": "work_item", "id": "wi_1042", "version": 7},
  "actor": {"type": "user", "id": "usr_engineer"},
  "correlationId": "req_01",
  "causationId": null,
  "data": {"from": "ready", "to": "active", "transitionId": "tr_01"}
}
```

`id` is globally unique and stable across publish retries. `occurredAt` is the committed domain transition time. `projectId` is omitted for tenant-wide events. Actor types are `user`, `service`, `workload`, and `operator`; automated actors retain the initiating/cause reference where applicable.

`resource.version` identifies the committed aggregate observation. `correlationId` connects one user/integration operation; `causationId` identifies the preceding event when an event caused the operation. Payloads use references to content rather than embedding secret values, source files, or transcripts.

## Subject and access model

Proposed NATS subject shape:

```text
factory.v1.tenant.<tenant-subject-token>.<event-type>
```

For example: `factory.v1.tenant.t_acme.work_item.transitioned`. Tenant subject tokens are server-assigned, restricted to safe non-wildcard token characters, and mapped to canonical tenant IDs. Untrusted strings cannot construct subjects.

Only authorized platform roles publish/consume domain events. Tenant workloads use scoped result and product interfaces; they cannot publish trusted events directly. Tenant-facing observation streams are authorized projections through the API.

## Initial event types

| Type | Aggregate | Required data references/fields |
| --- | --- | --- |
| `work_item.created` | Work item | Kind, initial state |
| `work_item.updated` | Work item | Changed field names, content revision where applicable |
| `work_item.transitioned` | Work item | Previous/new state, transition ID |
| `workspace.state_changed` | Workspace | Previous/new state, reason code |
| `workspace.checkpoint_created` | Workspace | Repository, previous/new commit, publishing attempt or human synchronization |
| `run.created` | Run | Work item, definition ID, input-snapshot digest |
| `run.state_changed` | Run | Previous/new state, classified reason |
| `step_attempt.state_changed` | Attempt | Run/step IDs, previous/new state, classified reason |
| `gate.requested` | Gate | Subject digest, content revision, reviewer scope, deadline |
| `gate.decided` | Gate | Actor, decision, resulting state, subject digest |
| `gate.invalidated` | Gate | Subject digest, reason, superseding commit/revision when present |
| `deployment.state_changed` | Deployment | Environment, previous/new state, source/artifact references |
| `membership.changed` | Membership | User ID, changed binding references, effective version |
| `project.settings_changed` | Project | Changed setting names, version |
| `configuration.changed` | Variable/secret | Resource kind, value/wrapping version, change kind, scope |
| `usage.recorded` | Usage record | Run/attempt, model, observed or estimated usage, uncertainty |

Gate expiry is an applicability check against its deadline, not a status transition. Agent results and defects use attempt/work-item events. No separate session, candidate, finding, or policy subsystem is introduced.

## Publication and consumption

1. Commit state, audit, and outbox rows in one PostgreSQL transaction.
2. Publish the row with its stable event ID and wait for durable broker acknowledgment.
3. Mark publication complete. A crash before this mark may cause redelivery.
4. A consumer validates schema and scope, then claims `(consumer, eventId)` in its inbox.
5. Commit its database effect and inbox result atomically where the effect is local. External effects use a durable intent and their own idempotency/reconciliation contract.
6. Acknowledge only after the durable local outcome. Retry classified transient failures; quarantine poison events with operator-visible reason and replay history.

Broker duplicate suppression is an optimization, not the correctness boundary. Inbox retention and replay policy must be aligned; replaying beyond the deduplication window requires an explicit safe replay procedure.

## Ordering and reconciliation

Global ordering is not promised. Current-state consumers reload the aggregate and use versions to avoid applying stale observations. Historical consumers such as audit/export or notifications process unique events even when older than the current resource state. Multiple event types may legitimately describe the same aggregate version.

Missing notifications do not prevent progress because periodic reconciliation scans durable desired state. Business commands never trust an event payload as authorization to perform an action without current policy checks.

## Retention and recovery

Define independent retention for outbox publication history, broker streams, consumer inboxes, audit, and API observation cursors. Events are not the complete backup of the product database. After restore, rotate workload signing keys, fence old controllers, and reconcile active external effects before replaying side-effecting consumers.

Manual replay requires scope, event range, consumer identity, and an audit record. Dry-run classification should identify potential external effects before replay is enabled for a consumer.

## Compatibility and validation

Payload additions are optional within a schema version. Breaking meaning/type changes require a new supported version and documented migration. Schemas validate envelopes and per-type payloads; acceptance tests cover duplicate delivery, reordering, unknown versions, poison events, and restore/replay behavior.
