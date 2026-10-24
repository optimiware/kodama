---
id: T-004
milestone: foundation
status: done
depends_on: [T-002, T-003]
decision_blockers: []
owns: [internal/httpapi/, api/common.yaml]
---
# T-004 — HTTP conventions and concurrency

## Goal
Implement shared transport behavior without duplicating domain logic.

## Contract
Use [HTTP](../contracts/http-api.md) on `net/http`: deny-by-default authorization injection, JSON/error envelopes, ETags, scoped cursors, request IDs, rate-limit interface, keyed create transaction helper. Do not add chi or an ORM. Generate common schemas with `oapi-codegen`.

## Deliverables
Router/middleware helpers, idempotency table/migrations, common OpenAPI schemas, representative test handlers.

## Acceptance
`idempotency_changed_body` proves the request hash is compared rather than included in the unique key; `stale_edit`, `cursor_scope`, and `replay_after_revocation` cover AC-02–AC-04 and IT-03.

## Out of scope
Product routes, actual identity provider, or global idempotency requirements on every PATCH.
