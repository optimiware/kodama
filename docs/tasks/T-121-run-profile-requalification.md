---
id: T-121
milestone: production
status: planned
depends_on: [T-111, T-120]
decision_blockers: []
owns: [test/integration/kube/runprofile/, images/, docs/acceptance/run-resource-profile.md]
check: test/integration/kube/runprofile/verify.sh
resources: [go, integration]
acceptance: [run_resource_profile_qualification]
---
# T-121 — Verify the configured Kubernetes run-resource profile

## Goal
Complete the original full P1 qualification after T-120 configures the target. The owner explicitly deferred this work; **do not execute during closeout**. T-111 delivered the driver and a blocked report, not this acceptance.

## Contract
O-29 and the reviewed T-120 policy/installation receipt. Keep `Test_run_resource_profile_qualification` intact as a real acceptance: no skip, mock, `--version` substitute or weakened success criteria.

## Deliverables
- Add the bounded explicit-target check wrapper `verify.sh`: run the real Go qualification test without `-tags unit`, capture its JSON outcomes into `FORGE_RESULTS`, and take the manual integration lease exactly once (the driver already takes it). Do not use the generic dev-stack bootstrap as a substitute for the selected Kubernetes target. Refuse missing inputs and preserve credential redaction.
- Actually build and push with ko and Buildah (`RUN`, multiple UID/GID ownership, authorized dependencies) and qualify one selected BuildKit mode including authentication and whole-process-tree/cache teardown. Prove browser sandbox and bounded shared memory, PostgreSQL readiness/data roundtrip, CSI RWOP/idmapped multi-image sharing and actual release/Delete reclaim.
- Verify affected same-run, service-to-service, other-run/tenant, platform/metadata and external default-deny controls on the actual CNI with live positive controls; exercise cleanup, cancellation and failure paths. Reuse unchanged historical evidence only with explicit candidate/profile compatibility reasoning.
- Retain exact commit, image/tool/kernel/CRI/CSI/CNI/security-profile versions, commands, receipts, hashes and checked cleanup inventories. Update only the combinations actually qualified; missing prerequisites remain blocked, never a pass.

## Acceptance
The declared real check must report `Test_run_resource_profile_qualification` passed and no required row skipped. Review support-matrix and cleanup evidence before accepting P1. This is platform qualification, not assembled product rollout (T-117).

## Out of scope
Running verification now, silently escalating privilege, changing unowned node/global policy, replacing sandboxed-browser acceptance, or treating T-111 closure as qualification.
