---
id: T-111
milestone: production
status: planned
depends_on: [T-110]
decision_blockers: []
owns: [test/integration/kube/, test/integration/runtime/, images/, docs/acceptance/run-resource-profile.md]
check: make check-go PKGS='./test/integration/kube/runprofile/...'
resources: [go]
acceptance: [unit_runprofile_guard, unit_runprofile_configuration, unit_runprofile_pins, unit_runprofile_verdicts, unit_runprofile_redaction, unit_runprofile_quiet_output, unit_runprofile_dependency_archive, unit_runprofile_lease]
---
# T-111 — P1: qualification driver and recorded platform findings

## Goal
Deliver the bounded real-profile qualification driver and honestly record the attempted platform qualification. The owner explicitly transferred Kubernetes configuration and full P1 qualification to planned T-120/T-121 before closing this task. Closing T-111 means reviewed driver/evidence delivery, **not** a qualified builder/browser profile.

## Contract
ADR 0010/O-28, design `0b12febef5b9e092982af74e2c951afe2dd80c19`, normative T-109 checkpoint `9db322ffd37a4279f8a7ac1a404a06e46533527d` and executable T-110 checkpoint `6343b9e5f75b20e41e28b62b418069bb1811950f`. Preserve trusted-engineer scope; no unrelated hosted/untrusted/live-agent work is scheduled. Never modify the shared stage installation or node policy without an explicit installation handoff. Orchestrator owns dev-scope teardown.

## Deliverables
- Inventory exact kernel/Kubernetes/CRI/CNI/CSI/filesystem/cgroups/LSM/seccomp/user-mapping versions and image digests. Prove sequential RWOP sharing across images/UIDs, idmapped ownership, deletion and actual mount release.
- Retain the existing build/push, service, sandbox, storage, network and teardown probes, with digest pins, scoped guards, redacted receipts and conservative verdicts. Do not weaken the real qualification assertion: Buildah, one selected BuildKit mode and browser sandbox must still work before P1 can pass in T-121.
- Record partial successes separately from blocked/unproven combinations, the exact historical candidate/inputs and installation handoffs. Do not retarget old receipts to a squashed or changed driver candidate.
- Keep the driver in `test/integration/kube/runprofile/`, separate from legacy kind-only attempt tests. The infrastructure-free `-tags unit` layer must explicitly skip the real probe and run all offline guard/configuration/pin/verdict/redaction/archive/lease tests. Normal explicit-target qualification must continue to fail, never skip, when the target or required evidence is absent.
- T-121 owns the single real integration lease and retained full qualification; no new live probe or cluster change is required to close this driver task.

## Acceptance
The scoped offline check must pass every named `Test_<name>` acceptance and documentation checks. Review the real driver, safety boundaries, cleanup and receipt claims; the report must continue to say full qualification is blocked. The skipped real test in the unit layer is not qualification evidence. Full `Test_run_resource_profile_qualification` acceptance is transferred intact to T-121 after T-120 configures Kubernetes.

## Infrastructure handoff
Read-only source preflight found that `dev/internal/devenv/cluster.go` creates stock kind, not a CSI/RWOP/idmap-qualified target. `test/integration/kube/cluster_test.go` assumes a local kind container, imports images into its containerd and derives a host callback IP; merely changing its kubeconfig does not make it stage-compatible. Add owned, explicit qualification fixtures with published pinned images and an in-cluster controlled callback/traffic topology, or obtain a separately owned disposable suitable cluster. Do not run dev provisioning/registry rewrites against shared stage. The coordinator's read-only stage inventory found Cilium and Scaleway CSI, but it proves no RWOP/userns/build capability. Shared node/runtime/storage/network changes require an installation-owner handoff. Existing stage receipts do not qualify this new profile.

## Out of scope
Product lifecycle/assembly/chart changes, permissive fallbacks, shared stage rollout and claiming untested platform combinations.
