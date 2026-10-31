---
id: T-180
milestone: kernel
status: done
depends_on: [T-172]
decision_blockers: []
owns: [docs/traceability.md]
check: make docs-check
resources: []
acceptance: []
---
# T-180 — Traceability for the frozen pipeline-runtime contract

## Goal
Connect the requirements and acceptance scenarios frozen in T-172 to the workstream tasks that implement them, so coverage is auditable from one place.

## Contract
[Traceability](../traceability.md) conventions, the frozen requirements in [pipeline format](../contracts/pipeline-format.md), [pipeline execution](../specs/pipeline-execution.md), [work management](../specs/work-management.md), [Git repositories](../specs/git-repositories.md) and the [event catalog](../contracts/event-catalog.md), and the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- Extend the capability rows for work management, durable execution, Git integration and the event catalog with the IDs frozen by T-172: PF-01–PF-06 and PF-A01–PF-A06, PX-35–PX-43 and PX-A28–PX-A38, WM-19–WM-22 and WM-A08–WM-A12, GR-21/GR-22 and GR-A16/GR-A17, EC-01–EC-05 and EC-A01–EC-A06.
- Name the owning tasks: T-171 contract, T-172 decisions, T-173 relocation, T-174 state suite, T-175 engine, T-176–T-178 backends, T-179 schema, and the later shell, composition and deletion tasks by phase.
- Add one row for the four-layer pipeline-runtime architecture that points at [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38).
- Keep retired and superseded IDs labelled rather than deleted, and do not renumber anything.
- Do not claim any capability is implemented; the row records planned ownership only.

## Acceptance
`make docs-check` passes, and every new requirement range in the table resolves to a document that actually defines it.

## Out of scope
Editing specifications or contracts, `docs/implementation/`, and task records other than this one.
