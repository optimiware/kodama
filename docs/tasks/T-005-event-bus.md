---
id: T-005
milestone: foundation
status: done
depends_on: [T-002, T-003]
decision_blockers: []
owns: [internal/events/, contracts/events/]
---
# T-005 — Durable event bus

## Goal
Preserve outbox → JetStream → inbox delivery and repairable reconciliation.

## Contract
Implement the [event catalog](../contracts/event-catalog.md), transaction-local event append, durable publisher/consumer helpers, safe claims, retained observation sequence, and periodic wakeups.

## Deliverables
Outbox/inbox migrations and schemas; publish acknowledgment/retry, consumer dedup/local-effect transaction, quarantine/replay audit, bounded retention/backpressure.

## Acceptance
`publish_ack_crash`, `duplicate_consumer`, `reordered_events`, `broker_outage`, and `quarantine_replay` use real JetStream/PostgreSQL. Verify PX-05 and AC-06/AC-08.

## Out of scope
Replacing NATS with wakeup-only messages or assuming exactly-once external effects.
