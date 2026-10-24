---
id: T-020
milestone: M1
status: done
depends_on: [T-005, T-011, T-012, T-013, T-014]
decision_blockers: []
owns: [internal/execution/, api/runs.yaml, internal/workspace/migrations/T020_001_attempt_fk.sql]
---
# T-020 — Durable run orchestration

## Goal
Plan/advance runs transactionally before invoking Kubernetes.

## Contract
Freeze run admission, claim, cancellation, observation, and result-commit interfaces from [execution](../specs/pipeline-execution.md). Treat executor and profile/gate hooks as narrow seams; no plugin framework.

## Deliverables
Run/step/attempt tables, pinned inputs, DAG progression, quotas/writer reservation, retry classification/backoff, failure drain, work/cleanup purpose distinction, API fragment.

## Acceptance
`dispatch_race`, `cancel_before_claim`, `quota_double_claim`, `unknown_effect_no_retry`, and `terminal_immutable` verify PX-02/PX-05–PX-09/PX-11/PX-12/PX-14 against PostgreSQL.

## Out of scope
Kubernetes network calls inside transactions, actual agent/gate execution, or assuming normal commands are idempotent.
