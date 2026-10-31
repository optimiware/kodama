---
id: T-193
milestone: kernel
status: planned
depends_on: [T-188]
decision_blockers: []
owns: [internal/shell/store/dispatch.go, internal/shell/store/gitmemory_dispatch.go, internal/shell/store/dispatch_postgres_test.go, internal/shell/outbox/]
check: make check-go PKGS='./internal/shell/outbox/... ./internal/shell/store/...'
resources: [go, integration]
acceptance: [unit_outbox_cursor_covers_concurrent_commits]
---
# T-193 — Outbox cursor must not skip concurrently committed events

## Goal
Close an ordering gap T-182 found: the publisher pages by `(created_at, id)` while `created_at` is assigned from `clock_timestamp()`, so a transaction that commits after a page was read can carry an earlier timestamp and be skipped.

## Contract
C5 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), EC-01 in the [event catalog](../contracts/event-catalog.md), and the bounded `UnpublishedEvents` query added by T-188.

## Background
T-188 replaced a full scan with `(created_at, id) > (cursor) ORDER BY created_at, id LIMIT n`. Because `created_at` comes from the clock at insert time rather than commit order, a long transaction can commit an event whose timestamp precedes a page already read. Reconcile only revisits rows older than a grace period, so a young skipped row can wait indefinitely.

## Deliverables
- Make the page cursor order match commit visibility: either a monotonic sequence assigned at commit, or a documented grace window with a reconciliation guarantee that covers rows committed late. Choose one and state why in the package documentation.
- Keep the query bounded and indexed; do not return to a backlog scan.
- A regression with two connections: one commits a row with an earlier `clock_timestamp` after the other's page has been read, and the publisher still delivers it exactly once.
- Keep the existing deduplication and replay behaviour.

## Acceptance
`make check-go` with `Test_unit_outbox_cursor_covers_concurrent_commits` passing, plus real-PostgreSQL evidence for the two-connection case.

## Out of scope
The event contract shape, the consumer side, and any change to `internal/natsbus`.
