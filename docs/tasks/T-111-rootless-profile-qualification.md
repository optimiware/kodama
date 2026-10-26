---
id: T-111
milestone: production
status: planned
depends_on: [T-110]
decision_blockers: []
owns: [test/integration/kube/, test/integration/runtime/, images/, docs/acceptance/run-resource-profile.md]
check: make check-go-integration DEV_SCOPE=t111 DEV_STACK=cluster PKGS='./test/integration/kube/... ./test/integration/runtime/...'
resources: [go, integration]
acceptance: [run_resource_profile_qualification]
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
The scoped real integration command must report `Test_run_resource_profile_qualification` passed, covering the actual build/push/storage/network/teardown profile rather than existing restricted-job cases alone. Retain receipts for the exact target. A skipped test or `--version` is not qualification; the absence of a usable profile pauses this checkpoint, not permission to substitute unit/template evidence.

## Infrastructure handoff
Read-only source preflight found that `dev/internal/devenv/cluster.go` creates stock kind, not a CSI/RWOP/idmap-qualified target. `test/integration/kube/cluster_test.go` assumes a local kind container, imports images into its containerd and derives a host callback IP; merely changing its kubeconfig does not make it stage-compatible. Add owned, explicit qualification fixtures with published pinned images and an in-cluster controlled callback/traffic topology, or obtain a separately owned disposable suitable cluster. Do not run dev provisioning/registry rewrites against shared stage. The coordinator's read-only stage inventory found Cilium and Scaleway CSI, but it proves no RWOP/userns/build capability. Shared node/runtime/storage/network changes require an installation-owner handoff. Existing stage receipts do not qualify this new profile.

## Out of scope
Product lifecycle/assembly/chart changes, permissive fallbacks, shared stage rollout and claiming untested platform combinations.
