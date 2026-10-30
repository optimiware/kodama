---
id: T-156
milestone: production
status: deferred
depends_on: [T-116, T-129]
decision_blockers: []
owns: [deploy/helm/kodama/, docs/operations/]
check: make test-chart
resources: [go]
acceptance: []
---
# T-156 — Chart and operations for native Git

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
Install the Git endpoint, tenant hosting settings and storage without external Git credentials.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). External-repository settings remain an explicit option.

## Deliverables
- Chart values and schema for the Git host, storage class, worker image and limits, control-plane RBAC for tenant service namespaces; native profile renders without Git token Secrets.
- Operations documents: enabling hosting, bootstrap by push, backup/restore, maintenance, offboarding, external repositories.

## Acceptance
`make test-chart` passes; T-132's chart test consumes the values.

## Out of scope
Stage scripts (T-157).
