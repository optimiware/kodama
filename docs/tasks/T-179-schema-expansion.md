---
id: T-179
milestone: kernel
status: planned
depends_on: [T-172]
decision_blockers: []
owns: [internal/storage/, docs/implementation/database-schema.md]
check: make check-go PKGS='./internal/storage/...'
resources: [go, integration]
acceptance: [unit_storage_pipeline_runtime_expand, unit_storage_pipeline_name_unique]
---
# T-179 — Pipeline runtime schema expansion

## Goal
Add the durable execution representation the kernel and shell need, without breaking the current composition: expand first, validate and map second, contract later.

## Contract
P2.1 stages 1–2 of the [simplification plan](../implementation/pipeline-runtime-simplification.md), [database schema](../implementation/database-schema.md), and the frozen decisions in [T-172](T-172-contract-decisions.md).

## Deliverables
- One appended migration with a new number, plus its manifest entry and RLS/grants, adding:
  - run lease owner, expiry and fence; command intent; workload and resource identity; workspace exclusion; cleanup state and retained resource identities;
  - wait columns on `run_steps` (identity, assignment, correlation, expected input, state, result, deadline, consumed signal, version), `step_kind`, and relaxed image/command requirements for non-container steps;
  - `tasks.pipeline_id`, `current_run_id`, and projection fields (`status`, `current_step_key`, `waiting_for`, `assigned_role`, `result`, `progress_version`, `progress_run_version`), all nullable at this stage;
  - indexes for due waits and task current-run lookup.
- Validation that refuses duplicate `(tenant_id, project_id, name)` pipeline rows and ambiguous task-to-pipeline mappings; it never invents a pipeline or picks a repository arbitrarily.
- A composite constraint proving `current_run_id` belongs to that task and project, not merely the project.
- Nothing is dropped in this task. `projects.task_workflow`, `state`, `parked_at`, `resume_state`, `blocked_by_id`, `resolution` and `resolved_at` stay readable until the contract stage.
- Migration tests for the new constraints, forced RLS under a non-owner role, and rollback.

## Acceptance
`make check-go` with `Test_unit_storage_pipeline_runtime_expand` and `Test_unit_storage_pipeline_name_unique` passing, plus real PostgreSQL evidence in the handoff. An in-memory schema check is not sufficient for RLS.

## Out of scope
Dropping legacy columns, the shell adapters, and the contract-stage migration.
