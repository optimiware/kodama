---
id: T-029
milestone: production
status: deferred
depends_on: [T-028, T-044, T-105]
decision_blockers: []
owns: [test/e2e/recovery/, internal/app/recovery.go, docs/operations/upgrades-backup-and-recovery.md]
---
# T-029 — Production source/data/key recovery

## Goal
Qualify recovery of the complete business loop with production KMS.

## Contract
Run after M3 integration to serialize shared composition edits. Implement the owner-approved O-11 [retention/recovery baseline](../operations/upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline): 15-minute RPO, 4-hour RTO, 30-day PITR, Git mirrors at most 15 minutes behind and coordinated artifact/image/KMS retention. These are acceptance objectives until measured by the drill.

## Deliverables
Consume T-105's production KMS startup wiring. Deliver Git retention/mirror tests, database/blob/registry restore drill, old-controller fencing/token/credential rotation, orphan reconciliation, measured report including supported recovery points and any unmet objective. Required refs, objects, images and key versions cannot expire before their supported database recovery point.

## Acceptance
`restore_full_factory`, `stale_publisher_after_restore`, `missing_retained_object`, and `old_token_denied` verify VS-10, WS-04/WS-08/WS-12, IT-06, PX-07.

## Out of scope
Treating a successful database import or key rotation alone as complete recovery.

## Future shaping after O-11
O-26 extracts startup wiring into T-105 and live deployment acceptance into T-104. Before scheduling the remaining recovery work, consider bounded retention/mirror fixtures, restore tooling and the final live-profile drill as separate tasks. Preserve every acceptance above and assign exact paths for further shared composition edits.
