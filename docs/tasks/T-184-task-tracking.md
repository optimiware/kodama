---
id: T-184
milestone: kernel
status: planned
depends_on: [T-179, T-182]
decision_blockers: []
owns: [internal/shell/tasks/]
check: make check-go PKGS='./internal/shell/tasks/...'
resources: [go, integration]
acceptance: [unit_tasks_pipeline_selection, unit_tasks_projection_versioning]
---
# T-184 — Task tracking and execution projection

## Goal
Replace the project workflow engine with tasks that select a pipeline and expose execution progress projected from run and step records.

## Contract
C3 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), [work management](../specs/work-management.md), and the T-179 schema.

## Deliverables
- Task create, get, list, start, rerun, inbox and resolve as shell use cases over the kernel state port.
- A task resolves its pipeline name to an immutable project pipeline ID at creation; a task requires a same-project pipeline.
- Exactly one current run after first execution; a rerun creates a new run without resetting completed step rows.
- Projection fields are updated from run/step records and rejected when the event's run identity or version is stale, so an older run cannot overwrite a newer view. The projection is rebuildable from runs and steps.
- Inbox lists tasks whose current wait is assigned to the caller's role; resolution records an immutable input and actor against that exact run, step and wait through the kernel signal path.
- Authorization for task content, pipeline selection and wait resolution.
- Remove transition, park, blocking-request and resolution-marker operations; do not recreate a state machine under another name.

## Acceptance
`make check-go` with `Test_unit_tasks_pipeline_selection` and `Test_unit_tasks_projection_versioning` passing, plus real-PostgreSQL evidence for the projection and inbox queries.

## Out of scope
HTTP/MCP/CLI handlers, NATS consumers, and the Git authority decision.
