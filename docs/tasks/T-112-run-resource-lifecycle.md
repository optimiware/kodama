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
ADR 0010/O-28, normative T-109 checkpoint `9db322ffd37a4279f8a7ac1a404a06e46533527d` and executable T-110 checkpoint `6343b9e5f75b20e41e28b62b418069bb1811950f`. Keep the existing six-method attempt runtime port; add the reviewed `run_resources` marker without overflowing the existing six-used-bit `uint8`, and keep finer resource capabilities in a separate named set. Optional launch-history proof stays separate. Provider types remain adapter-private. Preserve quota/work → run/attempt → authority → workspace → event lock order and audit/outbox atomicity.

## Deliverables
- Run-resource DTOs/ensure-observe-logs-stop-remove port and memory/PostgreSQL persistence of pre-effect intent, immutable scope/backend/policy/spec identity, UID, preparation/readiness, deadlines, stop/removal evidence and safe reasons; no plaintext/scratch persistence.
- Transactional ordered workspace-holder admission separate from logical writer admission; readiness/preparation barrier; reserve service/active-attempt/PVC capacity atomically and release only after verified settlement.
- Restart adoption, duplicate/reordered wakeups, cancel/create races, quota safety, terminal/orphan cleanup and run deadline including gates; no silent scratch/service reconstruction. Pre-command-only automatic retries and explicit unknown-effect barriers.
- Safe observation/API contributions without new business run enums or fake attempts/checkpoints. Keep new-format admission closed unless the selected runtime declares the complete required companion capabilities.
- Freeze the exact preparation-receipt digest object and verify its workspace UID binding. Extend the frozen step codec with round-trip evidence before allowing any new-format admission; replace T-110's deliberate v1-only codec regression in that same change. Pure runtime-v2 derivations do not replace persisted command-begin/cancellation/replay guards.

## Acceptance
Scoped check, shared memory/PostgreSQL contract suite and `make check-go-integration DEV_SCOPE=t112 DEV_STACK=services PKGS='./internal/execution/postgres/...'` before final acceptance. Prove actual RLS/rollback/races, unknown launch, cleanup restart and existing writer/late-result regressions; pin test names before review. Unit evidence alone cannot satisfy real SQL claims.

Hold the single manual integration lease `/tmp/opencode/kodama-run-resources-integration.lock` around the real SQL check, serialized with T-111 qualification. This does not prevent parallel unit work. Orchestrator owns scope teardown; never tear down another task's resources.

## Out of scope
Provider adapters, authority/publication changes owned by T-113, assembly/client generation and deployment.
