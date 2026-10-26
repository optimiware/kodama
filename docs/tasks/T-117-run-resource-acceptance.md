---
id: T-117
milestone: production
status: planned
depends_on: [T-116]
decision_blockers: []
owns: [test/integration/assembly-execution/, test/integration/kube/, test/integration/runtime/, images/, docs/acceptance/run-resource-profile.md, docs/acceptance/run-resources.md, docs/status.md, docs/implementation/assembly-status.md, docs/traceability.md]
check: make check-go-integration DEV_SCOPE=t117 DEV_STACK=cluster PKGS='./test/integration/kube/... ./test/integration/runtime/... ./test/integration/assembly-execution/...'
resources: [go, integration]
acceptance: []
---
# T-117 — P7: integrated acceptance and staged rollout

## Goal
Qualify every row of the [P7 acceptance matrix](../implementation/run-workspace-services-builders.md#acceptance-matrix-and-stable-id-proposal) against the assembled product and actual target.

## Contract
ADR 0010/O-28 and immutable prerequisites. This is not satisfied by template/unit/component results. Reuse valid P1/P4/P5 evidence with explicit candidate/profile binding. Obtain an installation handoff before changing a shared stage environment; stage credentials and values never enter reports.

## Deliverables
- Real ko/Buildah/selected BuildKit build-push-provenance, database/browser and generated/ignored/untracked-file flow across different images.
- Same-run and service-to-service allowed traffic, other-run/same-tenant/foreign-tenant/platform denial, scoped credential/publication/candidate evidence.
- Faults at preparation/readiness/commands/gates/publication/service loss/restart/cancel/create/storage deletion; crash-surviving cleanup inventories and no unsafe retries/reconstruction.
- Retained exact commit/images/tools/kernel/CSI/CNI/profile/commands/evidence, multiple concurrent tenants/runs, old-run drain and bounded trusted-engineer staged rollout with admission-stop/drain/reconcile rollback.
- Update status/traceability only for qualified combinations; unresolved installation/owner decisions park affected rollout, not unrelated implementation.

## Acceptance
Declared real check plus each stable requirement/acceptance row, retained receipts and cleanup. Pin all real semantic test names before review. Queue-drain main validation follows repository cadence, not an extra full suite per task.

## Out of scope
Unrelated deferred hosted/live-agent/untrusted-code/release work and broader supported-platform claims.
