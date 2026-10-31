---
id: T-175
milestone: kernel
status: planned
depends_on: [T-171, T-172, T-174]
decision_blockers: []
owns: [internal/kernel/runner/]
check: make check-go PKGS='./internal/kernel/runner/...'
resources: [go]
acceptance: [unit_kernel_engine_progression, unit_kernel_engine_waits, unit_kernel_engine_uncertain_launch, unit_kernel_engine_cleanup]
---
# T-175 — Durable progression engine

## Goal
Replace the blocking all-in-one runner with explicit, recoverable progression over the T-171 ports: claim, commit intent, launch or adopt, observe, settle, choose the next step, and reconcile.

## Contract
[ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), P1.3–P1.4 of the [simplification plan](../implementation/pipeline-runtime-simplification.md), and the frozen decisions in [T-172](T-172-contract-decisions.md).

## Deliverables
- One engine owner delivers these in order, each as its own reviewed commit:
  1. **Progression.** Claim with lease and fence, immutable plan snapshot, intent before effect, launch or adopt by stable identity, settle, choose the next eligible step, reconcile after restart or expired lease.
  2. **Waits and conditions.** Persist the wait before announcing it; one committed consumption with idempotent continuation; resolution, expiry and cancellation race on one version-checked decision; the frozen condition grammar.
  3. **Cancellation and uncertainty.** Cancellation blocks new execution claims but not stop/remove/reconciliation; an uncertain launch or stop is reconciled, never replayed.
  4. **Cleanup.** Execution outcome and cleanup are separate; a run is terminal only after owned resources are confirmed removed or cleanup remains visibly pending with retained identities and bounded retries.
- One executable step holds the workspace at a time; the prior step settles and releases before the next starts.
- Deterministic fail-fast and dependency ordering, tested with the memory state adapter and the Process backend.
- Mark the blocking `Backend.Execute` path as transitional. Its deletion belongs to the P3 cutover, because `internal/shell/serve` still calls `Runner`, `Backend.Execute` and `Process.Execute` outside this task's paths. It must not gain behaviour or be presented as a supported second engine.

## Acceptance
`make check-go` with `Test_unit_kernel_engine_progression`, `Test_unit_kernel_engine_waits`, `Test_unit_kernel_engine_uncertain_launch` and `Test_unit_kernel_engine_cleanup` passing. Memory state plus Process proves reconstruction, not process-crash durability; that is qualified later on PostgreSQL.

## Out of scope
PostgreSQL, NATS, Git, tasks, HTTP, agent packages, and any change to `internal/app`.
