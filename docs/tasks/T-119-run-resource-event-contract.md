---
id: T-119
milestone: production
status: planned
depends_on: [T-110]
decision_blockers: []
owns: [contracts/events/v1/, docs/contracts/event-catalog.md, docs/contracts/run-resources.md]
check: make check-go PKGS='./contracts/events/v1/...'
resources: [go]
acceptance: [unit_run_resource_event_contract]
---
# T-119 — Run-resource event contract prerequisite

## Goal
Supply the event contract required for P2's transactional resource transitions. T-112's first candidate recorded history/audit only because this independently owned event type was absent; that omission blocks its merge rather than moving a required P2 guarantee to P6.

## Contract
Normative checkpoint `9db322ffd37a4279f8a7ac1a404a06e46533527d`, executable checkpoint `6343b9e5f75b20e41e28b62b418069bb1811950f`, PX run-resource requirements and the existing event catalog. Resource observations carry safe references only and never authorize workload actions. Preserve existing event types, validation and schema version.

## Deliverables
- Add `TypeRunResourceStateChanged = "run_resource.state_changed"`, typed `RunResourceStateChanged` payload, catalog registration, schema and fixtures. Envelope resource type is `run_resource`; project is required. Payload fields are `runId`, `kind`, `key`, `from`, `to`, `reason`, `specFingerprint`, `uidBound`, all required. Kinds/keys/phases follow the resource contract; `from: none` is permitted for creation. Check kind/key consistency, stable safe reason syntax, digest and explicit boolean presence. No endpoints, provider UID, logs, credentials or scratch content.
- Freeze a domain-valid, nonambiguous audit/event resource ID: `rr-` plus the first 40 lowercase hex characters of SHA-256 over the length-prefixed UTF-8 tuple `(tenantId, projectId, runId, key)`. Each length is an unsigned 4-byte big-endian byte count. A raw `RUN_ID/KEY` violates the ID domain; concatenating run/key with underscores can collide and is not accepted as identity framing.
- Clarify the same 4-byte big-endian framing for resource names and endpoint hashes in `run-resources.md`, and update the event catalog's ID description. No lifecycle, authority or qualification semantics change.
- `Test_unit_run_resource_event_contract`: valid envelope/payload and schema/Go agreement, all resource kinds/phases, missing/foreign/mismatched/unsafe fields and secrecy projection, plus compatibility with existing event fixtures. This validates the contract, not an outbox implementation; T-112 emits it atomically and proves rollback/replay on both stores.

## Acceptance
Declared scoped check and `make docs-check`, independent exact-candidate review. Retain historical event fixtures unchanged where possible; adding a supported optional event type does not re-version prior envelopes. Do not regenerate public API/client or edit execution code.

## Out of scope
Execution producer/adapters, business lifecycle changes, API/client, deployment and real profile qualification. T-112 consumes the event after this prerequisite merges.
