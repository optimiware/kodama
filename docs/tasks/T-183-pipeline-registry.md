---
id: T-183
milestone: kernel
status: planned
depends_on: [T-179]
decision_blockers: []
owns: [internal/shell/pipelines/]
check: make check-go PKGS='./internal/shell/pipelines/...'
resources: [go, integration]
acceptance: [unit_pipelines_project_unique, unit_pipelines_source_conflict]
---
# T-183 — Project pipeline registry

## Goal
Own the project-scoped pipeline registry: project-unique names, immutable IDs, source indexing, and refusal of conflicting definitions.

## Contract
C1 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), [pipeline format](../contracts/pipeline-format.md), [Git repositories](../specs/git-repositories.md), and the T-179 schema.

## Deliverables
- Resolve a pipeline name to an immutable ID within a project; a duplicate name is refused in the registry, not only by the database.
- Index a definition from Git or the API. A conflicting definition from another repository or source is recorded and refused; neither silently replaces the other. Pipeline renaming stays out of scope.
- Persist the document as the single process definition; trigger and source metadata are derived views.
- Refuse a definition that fails `internal/kernel/pipeline` validation, surfacing stable diagnostic codes.
- Indexing is idempotent on event, ref and commit identity; a repeated event does not create a second definition.
- Tenant and project scope enforced; a foreign project is a not-found.

## Acceptance
`make check-go` with `Test_unit_pipelines_project_unique` and `Test_unit_pipelines_source_conflict` passing, with real-PostgreSQL evidence for the uniqueness and conflict paths.

## Out of scope
Task creation, run admission, Git transport, and the HTTP/MCP/CLI surfaces.
