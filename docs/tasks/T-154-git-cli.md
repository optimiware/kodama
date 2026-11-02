---
id: T-154
milestone: production
status: deferred
depends_on: [T-116, T-158]
decision_blockers: []
owns: [internal/cli/]
check: make check-go PKGS='./internal/cli/...'
resources: [go]
acceptance: [unit_cli_repository_commands, unit_cli_repository_seed, unit_cli_git_credential_helper_protocol, unit_cli_run_shows_source_set, unit_cli_git_serve_not_shadowed]
---
# T-154 — CLI for repositories and Git credentials

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
`kodama repo …`, `kodama repo seed` and the `kodama git credential` helper (AC-13, AC-14).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). `kodama git serve` (worker role, registered by T-132) must not be shadowed by the `git` command group.

## Deliverables
- Repository create/list/get/patch/archive/restore/refs/pushes; seed request; Git credential helper (`get`/`store`/`erase`); run display of source sets.

## Acceptance
Declared check.

## Out of scope
Server wiring (T-132).
