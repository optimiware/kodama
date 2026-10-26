---
id: T-114
milestone: production
status: planned
depends_on: [T-112, T-121]
decision_blockers: []
owns: [internal/kube/]
check: make check-go PKGS='./internal/kube/...'
resources: [go]
acceptance: []
---
# T-114 — P4: Kubernetes run resources and execution policy

## Goal
Implement [P4](../implementation/run-workspace-services-builders.md#p4--kubernetes-resources-and-policy) using the exact qualified T-121 profile and T-112 seam. T-111 driver/evidence delivery does not qualify the target.

## Contract
ADR 0010/O-28 and immutable prerequisite candidates. Preserve one Job/primary container per executable attempt, `backoffLimit: 0`, no token automount, scoped ownership and actual default deny. No arbitrary pipeline Pod controls or unqualified capability claims.

## Deliverables
- Adapter-private typed PVC/Service/service Pod/NetworkPolicy/preparation Job operations, run RWOP claim, private attempt volumes and qualified user mappings/LSM/mount/network settings.
- Verify actual security fields/volume/service UID and scope bindings on create/adopt/delete, not labels/fingerprints alone. Scope role-specific DNS/egress and declared same-run edges; refuse broad peers/host/public/platform access.
- Bind bootstrap/service secret authority and RBAC; reconcile unknown creates, stop/remove and actual process/mount release; inventory all resources including PVC/PV and selected isolated workers. Never hand off uncertain scratch.
- Export bounded constructors/conformance fixtures for T-116 without editing chart/assembly.

## Acceptance
Scoped check and real CSI/CNI contract evidence through T-111/P7 fixtures: traffic denial, adoption negative controls, restart/cancel/create races, UID collision, unknown termination and complete teardown. Pin names and real command before review; mocks do not qualify isolation.

## Out of scope
Chart/installation integration, shared app/client, other runtimes and shared stage changes.
