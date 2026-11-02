---
id: T-158
milestone: production
status: deferred
depends_on: [T-116, T-127, T-130, T-136, T-137, T-147, T-148, T-149]
decision_blockers: []
owns: [api/openapi.yaml, api/client/, api/pipeline.yaml, api/runtime.yaml, internal/app/apispec/]
check: make check-go PKGS='./internal/app/apispec/...' && make api-check
resources: [go]
acceptance: []
---
# T-158 — API aggregate and client for the Git service

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
Assemble the module fragments into the published API and regenerate the client so CLI work can start.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Additive changes only; compatibility tests against earlier client baselines.

## Deliverables
- Register the new fragments in `internal/app/apispec` (`CoreFragments`) with fragment-registration and compatibility tests.
- Pipeline/runtime fragment additions from T-131; aggregate regeneration with the repository, credential, push, trigger, workspace, run, gate, delivery and artifact fragments; client regeneration.

## Acceptance
`make api-check` and existing compatibility tests pass.

## Out of scope
Route registration (T-132).
