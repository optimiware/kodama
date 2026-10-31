---
id: T-194
milestone: kernel
status: done
depends_on: [T-184]
decision_blockers: []
owns: [docs/implementation/pipeline-runtime-handoff.md, docs/status.md, docs/implementation/assembly-status.md]
check: make docs-check
resources: []
acceptance: []
---
# T-194 — Post-P2 handoff for P3–P5

## Goal
Give the next session a short, accurate entry point for continuing after this branch is published, without re-deriving the phase breakdown or the current state.

## Contract
O-39 in the [decision register](../decisions.md), the [simplification plan](../implementation/pipeline-runtime-simplification.md) (P3–P5, parallel execution model), [status](../status.md) and [assembly status](../implementation/assembly-status.md).

## Deliverables
- `docs/implementation/pipeline-runtime-handoff.md`, kept brief (roughly one to two pages):
  - where the work stands and what P2 delivered, with the check and evidence that support it;
  - what P3, P4 and P5 each require, as a handful of bullets and an exit gate per phase, linking to the plan rather than restating it;
  - the first three P3 tasks to create, with proposed owned paths and prerequisites;
  - the known gaps and deferred items a successor must not rediscover: production `Tips`/`Keys`/vault `Source` implementations, `Runnable` including legacy-composition rows, the dispatcher cursor reset, the outbox index and the legacy schema contraction that waits for cutover;
  - the publication checklist: branch shape, batch evidence, the explicit owner decision, and the fact that pushing `main` is part of that decision.
- Update `docs/status.md` and `docs/implementation/assembly-status.md` so they describe the post-P2 state accurately: kernel and shell packages implemented and tested, composition still the transitional assembly, P3 cutover pending.

## Acceptance
`make docs-check` passes, and every claim in the handoff is supported by a merged task, a recorded check or an explicit decision. Do not claim capability that is not wired.

## Out of scope
Creating P3–P5 task records, changing the plan or the decision register, and any code change.
