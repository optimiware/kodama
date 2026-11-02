---
id: T-133
milestone: production
status: deferred
depends_on: [T-117, T-132, T-138, T-139, T-152, T-153, T-157]
decision_blockers: []
owns: [test/integration/git-service/, test/integration/kube/stage/, docs/acceptance/git-service.md, docs/status.md, docs/traceability.md]
check: test/integration/kube/stage/verify-git-service.sh
resources: [go, integration]
acceptance: [stage_git_bootstrap_by_push, stage_git_default_branch_protected, stage_git_tenant_isolation, stage_git_provisioning_recovery, stage_git_push_journal_recovery, stage_git_concurrent_agents_integrate, stage_git_revocation, stage_git_multi_repository_run, stage_git_developer_push_cicd, stage_git_durability_and_restore, stage_git_native_install_no_external_git, stage_git_external_compatibility]
---
# T-133 — Internal Git service acceptance

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
Accept the integrated candidate: locally against real Git/PostgreSQL, then on an isolated two-tenant Kubernetes installation.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), GR-A01–GR-A11, GR-A13–GR-A15. Stage credentials never enter reports. Do not weaken runtime guarantees to pass a local backend.

## Deliverables
- The scenarios named in `acceptance`, with retained receipts.
- Status and traceability updated only for what passed.

## Acceptance
Declared real check with every named scenario passing.

## Out of scope
Publication to `main` (an explicit owner decision).
