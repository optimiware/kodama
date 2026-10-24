---
id: T-066
milestone: M2
status: done
depends_on: [T-020, T-035, T-077, T-080]
decision_blockers: []
owns: [internal/deliverycleanup/, internal/execution/cleanup-run.go]
check: make check-go PKGS="./internal/deliverycleanup/... ./internal/execution/..."
resources: [go]
acceptance: [cleanup_after_work_done, cleanup_scope, cleanup_duplicate_claim]
---
# T-066 — Deployment expiry and cleanup-run admission

## Goal
Schedule ordinary deployment-bound cleanup independently of business work completion.

## Contract
Consume T-035's record/authorization port, T-020's `AdmitCleanup` semantics and T-077's environment/credential cleanup binding. Cleanup has no work item/workspace, consumes no business WIP and cannot target arbitrary resources. Use stable deployment/action keys and retained configuration. Preserve explicit unknown-outcome reconciliation.

## Deliverables
Small expiry/reconciliation capability, cleanup-run contribution and constructor/worker hooks for T-072. Keep deployment records with T-035 and central worker activation with the assembly owner. Use existing execution ports; if a required change falls outside the owned contribution, request an ordered prerequisite.

## Acceptance
`cleanup_after_work_done`, `cleanup_scope`, and `cleanup_duplicate_claim` exercise current authorization, duplicate claims, controlled time, retry and unknown outcomes using record/execution port doubles. Preserve DQ-09/DQ-10 and PX-06/PX-07 policy. T-067 owns actual non-owner-role persistence, rollback/restart and restricted deletion/outcome lookup.

## Out of scope
A deployment engine, work-item WIP mutation, image edits or deletion based solely on caller labels.
