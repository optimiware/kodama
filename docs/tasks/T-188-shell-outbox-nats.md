---
id: T-188
milestone: kernel
status: done
depends_on: [T-174]
decision_blockers: []
owns: [internal/natsbus/, internal/shell/outbox/, internal/shell/bus/jetstream.go, internal/shell/bus/jetstream_test.go, internal/shell/bus/publisher.go, internal/shell/bus/bus_test.go, internal/shell/serve/events.go, internal/shell/serve/events_test.go, internal/shell/serve/events_internal_test.go, internal/shell/serve/dispatch_test.go, internal/shell/serve/closeout_postgres_test.go, internal/shell/store/dispatch.go, internal/shell/store/gitmemory_dispatch.go, internal/shell/store/dispatch_postgres_test.go]
check: make check-go PKGS='./internal/natsbus/... ./internal/shell/outbox/... ./internal/shell/bus/... ./internal/shell/serve/... ./internal/shell/store/...'
resources: [go, integration]
acceptance: [unit_outbox_atomic_publish, unit_outbox_redelivery]
---
# T-188 — Shell outbox and NATS bus

## Goal
Make NATS the shell's central bus with PostgreSQL as the authority: state and publication intent commit together, and delivery tolerates duplicates, reordering and outages.

## Contract
C5 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), [ADR 0002](../adr/0002-postgres-state-and-events.md) as refined by [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), and the [event catalog](../contracts/event-catalog.md) (identity and deduplication keys).

## Deliverables
- Move the JetStream adapter out of `internal/shell/bus` into `internal/natsbus`; keep the port and the in-process implementation for development tests only.
- A shell outbox publisher that drains committed intent: state mutation and intent in one transaction, publication afterwards, at-least-once delivery.
- Stable event identity derived from type plus deduplication key as the event catalog defines; a consumer can deduplicate on the affected run, step, wait or trigger identity.
- Durable consumer names and replay/ack rules; duplicate, out-of-order and replayed delivery converge without duplicating a run or regressing a projection.
- Reconciliation for a missed wakeup: a periodic pass that finds committed but unpublished intent and republishes it.
- Deployed `test` and `production` profiles require NATS; a missing broker is a startup failure, not a silent in-process fallback.

## Acceptance
`make check-go` with `Test_unit_outbox_atomic_publish` and `Test_unit_outbox_redelivery` passing, plus recorded real-JetStream evidence for outage, redelivery and reconciliation.

## Out of scope
Task projection, Git events, and the HTTP/MCP/CLI surfaces.
