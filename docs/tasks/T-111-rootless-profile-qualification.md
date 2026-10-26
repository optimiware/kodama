---
id: T-111
milestone: production
status: planned
depends_on: [T-110]
decision_blockers: []
owns: [test/integration/kube/, test/integration/runtime/, images/, docs/acceptance/run-resource-profile.md]
check: make check-go-integration DEV_SCOPE=t111 DEV_STACK=cluster PKGS='./test/integration/kube/... ./test/integration/runtime/...'
resources: [go, integration]
acceptance: []
---
# T-111 — P1: qualify the rootless run-resource profile

## Goal
Supply actual, retained evidence for [P1](../implementation/run-workspace-services-builders.md#p1--qualify-and-freeze-the-deployable-profile). Unit/template tests do not satisfy this checkpoint.

## Contract
ADR 0010/O-28, design `0b12febef5b9e092982af74e2c951afe2dd80c19`, and immutable T-109/T-110 checkpoints. Preserve trusted-engineer scope; no unrelated hosted/untrusted/live-agent work is scheduled. Never modify the shared stage installation or node policy without an explicit installation handoff. Orchestrator owns dev-scope teardown.

## Deliverables
- Inventory exact kernel/Kubernetes/CRI/CNI/CSI/filesystem/cgroups/LSM/seccomp/user-mapping versions and image digests. Prove sequential RWOP sharing across images/UIDs, idmapped ownership, deletion and actual mount release.
- Actually build and push with ko and Buildah (`RUN`, multi-UID ownership, dependency egress); qualify VFS and only advertise overlay/FUSE with evidence. Try normal rootless BuildKit; qualify one explicit supported mode and full process-tree teardown. Never silently enable host privilege.
- Prove non-root PostgreSQL readiness/data roundtrip and browser sandbox/bounded shared memory; test nested networking under actual outer default deny.
- Freeze the support matrix, settings, destinations, credential flows, executable commands and negative controls in the report. Missing installation prerequisites become explicit T-114/T-116 handoffs; unavailable infrastructure pauses qualification, not a fabricated pass.

## Acceptance
The scoped real integration command plus retained build/push/storage/network/teardown receipts for the exact target. Pin semantic test names before review. A skipped test or `--version` is not qualification.

## Out of scope
Product lifecycle/assembly/chart changes, permissive fallbacks, shared stage rollout and claiming untested platform combinations.
