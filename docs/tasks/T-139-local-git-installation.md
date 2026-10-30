---
id: T-139
milestone: production
status: deferred
depends_on: [T-132, T-165]
decision_blockers: []
owns: [dev/compose.yaml, dev/probe/, dev/internal/devenv/, dev/gitserver/, dev/README.md, dev/configuration.md, test/integration/assembly-core/]
check: make check-devtools && make check-go-integration DEV_SCOPE=t139 DEV_STACK=services PKGS='./test/integration/assembly-core/...'
resources: [go, integration]
acceptance: [unit_devenv_native_git_stack, assembly_core_native_git_roundtrip, assembly_core_external_git_compat]
---
# T-139 — Local development and test installation with native Git

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
The `test` environment boots with Kodama's own Git service and no external Git credentials; the external fixture stays as an explicit compatibility profile.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md).

## Deliverables
- Native-Git service stack (one `kodama git serve` container per tenant from the built image, replacing `dev/gitserver` as the default), probes and endpoint files; external `git-http-backend` fixture selected explicitly for compatibility tests.
- Assembly-core startup and a native bootstrap/push roundtrip; documentation.

## Acceptance
Declared check.

## Out of scope
Stage (T-157).
