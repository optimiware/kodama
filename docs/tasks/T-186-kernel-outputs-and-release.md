---
id: T-186
milestone: kernel
status: planned
depends_on: [T-175]
decision_blockers: []
owns: [internal/kernel/runtime/, test/support/kernelcontracts/, internal/shell/runstate/memory.go, internal/shell/runstate/commit.go, internal/shell/runstate/clone.go, internal/shell/runstate/doc.go, internal/shell/runstate/memory_test.go]
check: make check-go PKGS='./internal/kernel/runtime/... ./internal/shell/runstate/... ./test/support/kernelcontracts/...'
resources: [go]
acceptance: [unit_kernel_runtime_outputs, unit_kernel_state_release]
---
# T-186 — Step outputs and claim release in the kernel contract

## Goal
Close the two gaps the engine found: a run's step outputs are not part of the durable record, and a run parked on a wait cannot release its lease, so resumption waits for expiry.

## Contract
`internal/kernel/runtime`, the T-174 contract suite, `docs/specs/pipeline-execution.md` (step outputs and wait continuation), and P1.3–P1.4 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- **Step outputs.** Add a durable representation of a settled step's declared outputs to `RunRecord`, so recovery does not depend on an engine-side port the shell may not provide. The engine's `Outputs` port may remain as the writer, but the record is the authority after commit. Bound the size and reject an output that is not declared by the plan.
- **Claim release.** Add a `StateStore` operation that releases a claim held by the caller without ending the run — used when a run parks on an open wait — so the next claim does not wait for lease expiry. It must check scope, owner and fence, keep the fence, and be a no-op for a run that is no longer claimed by the caller.
- Extend the contract suite with both behaviours, including a broken-adapter variant for each, and update the memory adapter.
- Keep the memory suite and every existing kernel test passing; no assertion weakened.

## Acceptance
`make check-go` with `Test_unit_kernel_runtime_outputs` and `Test_unit_kernel_state_release` passing.

## Out of scope
The engine's scheduling loop, PostgreSQL (T-182), and the P3 cutover.
