---
id: T-115
milestone: production
status: planned
depends_on: [T-111, T-112]
decision_blockers: []
owns: [internal/runtime/podman/, internal/runtime/process/, internal/runtime/runtimetest/]
check: make check-go PKGS='./internal/runtime/podman/... ./internal/runtime/process/... ./internal/runtime/runtimetest/...'
resources: [go]
acceptance: []
---
# T-115 — P5: Podman parity and Process fixtures

## Goal
Implement [P5](../implementation/run-workspace-services-builders.md#p5--podman-parity-and-process-fixtures) with a shared exported lifecycle/resource contract.

## Contract
ADR 0010/O-28 and immutable prerequisites. Process advertises no image/network/builder guarantee. Podman advertises only enforced/real-tested policy: a bridge alone is not default deny. T-113 owns shared bootstrap.

## Deliverables
- Run-owned volume/network/services/readiness/endpoints with durable identity, idempotent restart/adoption/stop/remove and private credential/output tmpfs.
- Qualified rootless mappings/storage; no host engine socket or implicit broad network. Refuse policies requiring absent guarantees.
- Export backend-neutral resource/lifecycle contract and decorators for Process, real Podman and Kubernetes without runtime-specific DTO leakage.

## Acceptance
Scoped check and mandatory `make test-podman`: real multi-image volume/service roundtrip, mappings/image execution, cancel/restart, capability refusal and zero leaked volumes/networks/containers. Pin actual names before review. Process-only evidence cannot certify Podman or Kubernetes.

## Out of scope
Shared bootstrap, execution lifecycle, chart/assembly and production fallbacks.
