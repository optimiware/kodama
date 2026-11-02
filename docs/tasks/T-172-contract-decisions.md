---
id: T-172
milestone: kernel
status: done
depends_on: []
decision_blockers: []
owns: [docs/contracts/pipeline-format.md, docs/contracts/event-catalog.md, docs/specs/pipeline-execution.md, docs/specs/work-management.md, docs/specs/git-repositories.md]
check: make docs-check
resources: []
acceptance: []
---
# T-172 — Freeze the pipeline runtime contract decisions

## Goal
Settle the authoring and execution decisions that parser, engine, SQL and agent adapters would otherwise each invent, and record them in the owning documents.

## Contract
[ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38), and the contract decision table in the [simplification plan](../implementation/pipeline-runtime-simplification.md#contract-decisions-to-freeze-before-fan-out).

## Deliverables
Record one decision per row, with stable requirement and acceptance identifiers:

1. **Rework loops.** Execution stays acyclic. Rework is a declared forward step or an explicit new run with explicit source/output continuity; a discarded workspace is never reused.
2. **Agent-requested waits.** The container settles and records a bounded request/result; a declared downstream wait consumes it. No mid-command suspension.
3. **Meaning of resume.** The same run continues through a declared next step with persisted references. The previous command is not replayed.
4. **Conditions.** A bounded predicate over declared inputs, prior step results and wait outcomes, with fixed operators, missing-value behaviour and reference visibility. A condition step kind exists only if a requirement needs one.
5. **Rejection and joins.** A rejected input is a recorded outcome, not infrastructure failure. Skipped prerequisites and joins have explicit semantics, and a protected step cannot be reached unconditionally.
6. **Deadline races.** Resolution, expiry and cancellation are one atomic version-checked decision. A duplicate request returns the recorded result.
7. **Assignment and correlation.** Represented separately. Every changed candidate needs a fresh wait identity and decision.

Update the pipeline format for the wait and condition syntax, the execution specification for lifecycle, deadlines and joins, the work-management specification for inbox and resolution, the Git specification for integration authority, and the event catalog for the signal and outcome events that carry these decisions. Retire contradictory current requirements with a supersession note rather than leaving both.

## Acceptance
`make docs-check` passes. Review confirms every decision is stated once, in its owning document, with no conflicting restatement elsewhere.

## Out of scope
Implementation, schema, and any change to `docs/implementation/`.
