---
id: T-024
milestone: M1
status: done
depends_on: [T-004, T-005, T-010]
decision_blockers: []
owns: [internal/observation/, api/observation.yaml]
---
# T-024 — Authorized durable observation streams

## Goal
Let clients observe work and runs without depending on raw NATS access.

## Contract
Use retained event sequence and a scoped opaque cursor. Bind filters/tenant, support Last-Event-ID, snapshot-plus-cursor resync, bounded buffering and disconnect.

## Deliverables
Project/run SSE handlers, backlog/live handoff, expiry response, current authorization checks, OpenAPI examples.

## Acceptance
`snapshot_stream_race`, `cursor_expiry`, `foreign_cursor`, and `slow_client` cover AC-04/AC-06 and IT-03/IT-11 using real event persistence.

## Out of scope
Treating broker sequence alone as a tenant-authorized public cursor.
