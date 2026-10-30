---
id: T-152
milestone: production
status: deferred
depends_on: [T-115, T-129, T-150, T-151]
decision_blockers: []
owns: [internal/kube/, internal/runtime/podman/, internal/runtime/process/, internal/runtime/runtimetest/]
check: make check-go PKGS='./internal/kube/... ./internal/runtime/...'
resources: [go]
acceptance: [unit_kube_source_set_preparation, unit_kube_attempt_git_credential_delivery, unit_kube_fingerprint_includes_sources, unit_podman_source_set_preparation, unit_process_source_set_preparation, unit_runtime_contract_source_sets]
---
# T-152 — Runtime adapters for source sets and Git credentials

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
Kubernetes, Podman and Process deliver the new context, multiple sources and attempt Git credentials.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Preserve fingerprints/adoption rules, one preparation, verified stop and cleanup; cleanup never touches tenant Git storage. T-129's `gitworker` files are not edited.

## Deliverables
- Preparation and attempt rendering for source sets; credential delivery outside `/workspace`; fingerprints include the source set.
- Runtime contract suite cases for source sets on every backend.

## Acceptance
Declared check; kind conformance under `make check-go-integration DEV_SCOPE=t152 DEV_STACK=cluster PKGS='./test/integration/kube/...'` where available.

## Out of scope
Stage acceptance (T-133).
