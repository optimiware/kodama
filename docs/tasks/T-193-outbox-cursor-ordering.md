---
id: T-193
milestone: kernel
status: done
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
T-188 replaced a full scan with `(created_at, id) > (cursor) ORDER BY created_at, id LIMIT n`. Because `created_at` defaults to `now()` — transaction start — a long transaction can commit an event whose timestamp precedes a page already read.

**Corrected premise (review 2026-10-31).** The omission is bounded to the one pass whose cursor moved past the row before it committed, not indefinite: every `Reconcile` pass restarts from the zero cursor with a fresh cutoff, and `Drain` uses the cursorless `Pending` query. The accepted resolution is therefore a stated guarantee plus regressions rather than a query change; a commit-ordered sequence would need a schema change or a serialization point on every write.

## Deliverables
- Make the page cursor order match commit visibility: either a monotonic sequence assigned at commit, or a documented grace window with a reconciliation guarantee that covers rows committed late. Choose one and state why in the package documentation.
- Keep the query bounded and indexed; do not return to a backlog scan.
- A regression with two connections: one commits a row with an earlier `clock_timestamp` after the other's page has been read, and the publisher still delivers it exactly once.
- Keep the existing deduplication and replay behaviour.

## Resolution
Coverage is the next eligible *completed* reconciliation pass, conditional on continued scheduling and store access; there is no wall-clock or next-poll delivery promise. Exactly-once holds only for a single publisher with a working store and broker; crashes, lost acknowledgments, failed marks and a second publisher still republish under the same event id, so at-least-once and consumer deduplication are unchanged. Pagination is indexed keyset paging returning at most `Batch` rows per query; a full reconciliation walks the eligible backlog and `LIMIT` does not bound physical work.

## Acceptance
`make check-go` with `Test_unit_outbox_cursor_covers_concurrent_commits` passing, plus real-PostgreSQL evidence for the two-connection case.

## Out of scope
The event contract shape, the consumer side, and any change to `internal/natsbus`.
