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
| `run_resource.state_changed` | Run resource (`run_shared` runs; envelope resource type `run_resource`; resource ID [`rr-…`](#run-resource-events); project required) | Run ID, kind (`network`, `workspace`, `preparation`, `service`), key, previous/new phase, safe reason code ([catalog](run-resources.md#reasons)), spec fingerprint, whether a runtime UID is bound |
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

`run_resource.state_changed` is an additive v1 type (`contracts/events/v1`): earlier envelopes, types, fixtures and digests are unchanged and the schema version stays 1. It is an observation of execution-owned state; it never authorizes a workload action, and consumers rely on current policy and the execution record, not the payload.

| Member | Rule |
| --- | --- |
| Envelope | `resource.type` is `run_resource`; `projectId` is required and `tenantId`/`projectId` are the run's scope. `resource.version` is the committed run-resource record version (at least 1) |
| `resource.id` | `rr-` plus the first 40 lowercase hex characters of SHA-256 over the length-prefixed UTF-8 tuple `(tenantId, projectId, runId, key)`: each element is preceded by its byte length as an unsigned 4-byte big-endian integer. The result is 43 characters and satisfies the ID domain; a raw `RUN_ID/KEY` does not, and an underscore join is ambiguous across tuple boundaries (tenant `t_a` with project `p` aliases tenant `t` with project `a_p`; run `run_x` with key `service.network` aliases run `run_x_service` with key `network` once the key's dot becomes `_`), so neither is accepted. The Go contract derives and checks it from the envelope scope and payload; `eventsv1.RunResourceID` is the single implementation. Golden vector: `(t_acme, p_inventory, run_01, network)` → `rr-b6db60ce0677ae6f334067ebe0214c4795279151` |
| `runId` | Run ID domain |
| `kind`, `key` | `network`/`network`, `workspace`/`workspace`, `preparation`/`preparation`, `service`/`service.NAME` with `NAME` as in the pipeline service-name syntax (`^[a-z][a-z0-9-]{0,29}$`); any other pairing is invalid |
| `from`, `to` | A [phase](run-resources.md#phases-and-evidence): `pending`, `ready`, `failed`, `stopped`, `absent` or `unknown`. `from` may also be `none` for the creation edge; `to` never is. The phase may be unchanged (`from` equal to `to`): an observation also records a changed reason, UID binding, intent or evidence; whether an unchanged observation is emitted at all is execution's decision, not this contract's |
| `reason` | A stable safe code (`^[a-z][a-z0-9_]{0,63}$`), normally from the [reason catalog](run-resources.md#reasons); never prose, a message or a log line |
| `specFingerprint` | `sha256:` and 64 lowercase hex characters |
| `uidBound` | Required boolean (an explicit `false` is meaningful; omitted or null is invalid): whether a provider identity is bound. `to: ready` requires `true` because readiness is verified against the bound identity |

All eight members are required. The payload carries no provider UID, endpoint, log, credential or scratch content: a producer serializes only these members, and the typed projection (`RunResourceStateChanged`) exposes nothing else. Additional unknown members follow the general rule below (tolerated on decode, never part of the typed projection) and are not a place for such data. The envelope's tenant/project validity is checked here; whether a producer may publish for that scope is established by the execution transaction that commits the event with its state, not by envelope validation.

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
