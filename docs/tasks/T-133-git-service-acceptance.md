---
id: T-133
milestone: production
status: blocked
depends_on: [T-117, T-132]
decision_blockers: [O-32]
owns: [test/integration/git-service/, test/integration/kube/stage/, docs/acceptance/git-service.md, docs/status.md, docs/traceability.md]
check: test/integration/kube/stage/verify-git-service.sh
resources: [go, integration]
acceptance: [stage_git_tenant_isolation, stage_git_provisioning_recovery, stage_git_push_journal_recovery, stage_git_concurrent_agents_integrate, stage_git_revocation, stage_git_multi_repository_run, stage_git_developer_push_cicd, stage_git_durability_and_restore, stage_git_import_and_compatibility]
---
# T-133 — Kubernetes acceptance and cutover

## Goal
Accept internal Git hosting on two tenants on the stage cluster with the merged image and chart, and cut over new repositories to it.

## Contract
ADR 0011, GR-A01–GR-A13, the [migration sequence](../implementation/internal-git-service.md#migration-and-cutover). Stage credentials never enter reports.

## Deliverables
- The scenarios named in `acceptance`, with retained receipts: isolation, provisioning and push-journal recovery, racing agents and integrations, revocation, a multi-repository run, a developer push driving CI/CD, worker replacement, maintenance and restore, import and compatibility of admitted runs.
- Cutover: internal hosting default for new repositories; external ones imported or explicitly kept external. The publisher stays for repositories kept external; its retirement is not part of this task and needs a separate owner decision.
- Status and traceability updated only for what passed.

## Acceptance
Declared real check with every named scenario passing on stage.

## Before scheduling
Import scope needs O-32.
