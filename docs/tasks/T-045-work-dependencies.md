---
id: T-045
milestone: M3
status: done
depends_on: [T-011]
decision_blockers: []
owns: [internal/work/dependencies/, internal/work/dependency-admission.go, api/work-dependencies.yaml]
---
# T-045 — Work dependency graph

## Goal
Add business dependency edges after the core work model is established.

## Contract
Same-project directed edges, transactional cycle rejection, authorized CRUD, and a dependency-admission contribution used by active transitions. Do not confuse business edges with pipeline step DAGs.

## Deliverables
Owned migration/queries, cycle/admission service, API fragment, history/events, permission-aware errors.

## Acceptance
`concurrent_cycle_insert`, `foreign_dependency`, `dependency_not_done`, and `dependency_deleted_before_admission` verify WM-01/WM-03/WM-05 and IT-11.

## Out of scope
Cross-project dependencies, a generic workflow language, or concurrent edits to T-011's existing files without an ownership transfer.
