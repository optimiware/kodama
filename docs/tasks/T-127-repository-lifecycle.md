---
id: T-127
milestone: production
status: done
depends_on: [T-126]
decision_blockers: []
owns: [internal/repository/, api/repositories.yaml]
check: make check-go PKGS='./internal/repository/...'
resources: [go]
acceptance: [unit_repository_create_idempotent, unit_repository_name_unique_per_project, unit_repository_policy_defaults_deny, unit_repository_provisioning_reconciles, unit_repository_archive_restore, unit_repository_cross_tenant_hidden, unit_repository_external_mapping, unit_repository_memory_contract]
---
# T-127 — Repository lifecycle and persistence

## Goal
Repositories as project resources with an idempotent, recoverable provisioning lifecycle (GR-01, GR-02, GR-14, GR-19).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S1/S2, T-126 DTOs. Migration `007_git_repositories.sql` in `internal/repository/postgres/migrations/`, exported as a contribution that T-132 registers.

## Deliverables
- Entity and transitions (`provisioning → ready ↔ archived`, reasons), policy with `directPushToDefault` defaulting to `false`, `defaultBranch` changes, emptiness tracking for seeding (reported by the worker).
- Use cases: create/read/list/patch/archive/restore with current authorization; provisioning via the `Hosts` port with a reconciler; audit and outbox events `repository.created` / `repository.state_changed`.
- PostgreSQL adapter with tenant predicates and RLS; memory adapter over `memdb`; one shared `Contract` suite.
- Read-only mapping of each project's existing configured repository to a `hosting: external` record, without changing workspace behavior.
- HTTP handler and `api/repositories.yaml` fragment (repository resource and refs read).

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t127 DEV_STACK=services PKGS='./internal/repository/...'` (GR-A01, GR-A02 persistence side).

## Out of scope
Git processes, Kubernetes, route registration, CLI.

## Completion evidence
Implemented by a subagent; independent review requested changes once (a two-field PATCH stepping the version twice and failing the SQL guard; unchanged refs observations not advancing the emptiness watermark) and approved `0acc7975`. Coordinator checks: `make check-task TASK=T-127` PASS (8 acceptance names); real SQL `make check-go-integration DEV_SCOPE=t127 DEV_STACK=services PKGS='./internal/repository/...'` PASS. Open for T-132: map the `repository.*` actions to identity permissions; register migration `007`.
