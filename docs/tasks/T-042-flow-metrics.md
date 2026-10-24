---
id: T-042
milestone: M3
status: done
depends_on: [T-011]
decision_blockers: []
owns: [internal/metrics/, api/work-summaries.yaml]
---
# T-042 — SQL flow summaries

## Goal
Expose useful lean observations without a metrics product subsystem.

## Contract
Derive named lead/active/blocked/review/reopen intervals from immutable work history. Export summary query contribution to existing work lists; keep tenant scope in SQL/RLS.

## Deliverables
Views/queries, documented interval/window semantics, summary DTO/OpenAPI contribution, indexes justified by query plans.

## Acceptance
`blocked_intervals`, `reopen_history`, `incomplete_window`, and `metrics_tenant_scope` verify WM-06/WM-12 and IT-03.

## Out of scope
New flow-policy or improvement-proposal resources, billing, and unmeasured SLO claims.
