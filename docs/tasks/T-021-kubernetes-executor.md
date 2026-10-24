---
id: T-021
milestone: M1
status: done
depends_on: [T-016, T-019, T-020]
decision_blockers: []
owns: [internal/kube/, test/integration/kube/]
check: make check-go
resources: [go]
---
# T-021 — Native Job executor and reconciliation

## Goal
Turn durable attempts into restricted, observable Kubernetes Jobs.

## Contract
Consume the run-service seam from T-020. Build deterministic Jobs with injected entrypoint, private scratch, admission token, exact image, quotas, labels/spec fingerprint, and `backoffLimit: 0`.

## Deliverables
Create/observe/delete reconciler, startup/timeout handling, orphan/late-create cleanup, tenant namespace/service-account policy checks, real-cluster fixtures.

## Acceptance
`create_ack_crash`, `foreign_job_collision`, `cancel_create_race`, and `restricted_job` cover PX-03/PX-04/PX-08/PX-09 and IT-07/IT-08 in kind.

## Out of scope
Source publication fencing through Pod deletion or an alternative Tekton executor.
