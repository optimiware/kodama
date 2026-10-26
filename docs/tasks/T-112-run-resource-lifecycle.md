---
id: T-112
milestone: production
status: planned
depends_on: [T-110]
decision_blockers: []
owns: [internal/execution/, api/execution.yaml]
check: make check-go PKGS='./internal/execution/...'
resources: [go]
acceptance: []
---
# T-112 — P2: run-resource lifecycle and persistence

## Goal
Implement [P2](../implementation/run-workspace-services-builders.md#p2--run-resource-lifecycle-and-persistence), independently of P1 after the contract checkpoint. Freeze the execution-owned companion seam before adapter work.

## Contract
ADR 0010/O-28 and immutable T-109/T-110 candidates. Keep the existing six-method attempt runtime port. Extend guarantees safely beyond the existing six-used-bit `uint8`; keep optional launch-history proof separate. Provider types remain adapter-private. Preserve quota/work → run/attempt → authority → workspace → event lock order and audit/outbox atomicity.

## Deliverables
- Run-resource DTOs/ensure-observe-logs-stop-remove port and memory/PostgreSQL persistence of pre-effect intent, immutable scope/backend/policy/spec identity, UID, preparation/readiness, deadlines, stop/removal evidence and safe reasons; no plaintext/scratch persistence.
- Transactional ordered workspace-holder admission separate from logical writer admission; readiness/preparation barrier; reserve service/active-attempt/PVC capacity atomically and release only after verified settlement.
- Restart adoption, duplicate/reordered wakeups, cancel/create races, quota safety, terminal/orphan cleanup and run deadline including gates; no silent scratch/service reconstruction. Pre-command-only automatic retries and explicit unknown-effect barriers.
- Safe observation/API contributions without new business run enums or fake attempts/checkpoints. Keep new-format admission closed unless the selected runtime declares the complete required companion capabilities.

## Acceptance
Scoped check, shared memory/PostgreSQL contract suite and `make check-go-integration DEV_SCOPE=t112 DEV_STACK=services PKGS='./internal/execution/postgres/...'` before final acceptance. Prove actual RLS/rollback/races, unknown launch, cleanup restart and existing writer/late-result regressions; pin test names before review. Unit evidence alone cannot satisfy real SQL claims.

## Out of scope
Provider adapters, authority/publication changes owned by T-113, assembly/client generation and deployment.
