---
id: T-127
milestone: production
status: planned
depends_on: [T-126]
decision_blockers: []
owns: [internal/repository/, api/repositories.yaml, internal/storage/migrations/, internal/storage/manifest.go, internal/storage/manifest_test.go]
check: make check-go PKGS='./internal/repository/... ./internal/storage/...'
resources: [go]
acceptance: [unit_repository_create_idempotent, unit_repository_name_unique_per_project, unit_repository_provisioning_reconciles, unit_repository_archive_restore, unit_repository_seed_permission, unit_repository_external_mapping, unit_repository_memory_contract]
---
# T-127 — Repository lifecycle and persistence

## Goal
Make repositories project-owned resources with an idempotent, recoverable provisioning lifecycle (GR-01, GR-02, GR-14).

## Contract
ADR 0011, [Git repositories](../specs/git-repositories.md#provisioning), seams S1/S2, T-126 DTOs.

## Deliverables
- `internal/repository`: entity and transitions (`provisioning → ready ↔ archived`, reasons), repository policy (`directPushToDefault`, required on creation with no default until O-33), create/read/list/archive/restore use cases, current-authority checks, the `Hosts` port (S2) and a reconciler for interrupted provisioning.
- PostgreSQL adapter with tenant predicates and RLS; one appended migration contribution (`repositories`, audit/outbox integration) after the baseline; memory adapter over `memdb` with one shared contract suite.
- Mapping of each project's existing configured repository to a `hosting: external` record, without changing current workspace behavior.

## Acceptance
Declared check with every named test passing; real SQL under `make check-go-integration DEV_SCOPE=t127 DEV_STACK=services PKGS='./internal/repository/...'` (GR-A01, GR-A02 persistence side).

## Out of scope
Git processes (T-128), Kubernetes (T-129), route registration, aggregate API and CLI (T-132). The module fragment `api/repositories.yaml` and its handler belong here.
