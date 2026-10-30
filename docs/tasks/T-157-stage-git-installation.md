---
id: T-157
milestone: production
status: deferred
depends_on: [T-132, T-156]
decision_blockers: []
owns: [deploy/stage/, test/support/stage/, test/e2e/kube-full/, test/e2e/staging/]
check: deploy/stage/scripts/test_scripts.sh
resources: [go, integration]
acceptance: []
---
# T-157 — Stage installation with native Git

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
New stage installations and qualification sessions use the tenant Git service instead of Gitea, copied tokens and publisher credentials.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Do not remove existing Gitea data or invalidate earlier receipts; an explicit legacy profile remains for external repositories.

## Deliverables
- Stack and qualification changes: no Gitea dependency or token copying for native sessions; repository provisioning through Kodama; stage Git helper using the Kodama API.
- Kubernetes acceptance and staging startup updated; offline script tests.

## Acceptance
`deploy/stage/scripts/test_scripts.sh` passes; T-133 runs the live gates.

## Out of scope
Live acceptance (T-133).
