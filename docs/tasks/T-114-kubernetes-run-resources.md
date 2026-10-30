---
id: T-114
milestone: production
status: done
depends_on: [T-112, T-122]
decision_blockers: []
owns: [internal/kube/]
check: make check-go PKGS='./internal/kube/...'
resources: [go]
acceptance: [unit_kube_run_workspace_claim_lifecycle, unit_kube_workspace_release_fence, unit_kube_preparation_job_lifecycle, unit_kube_service_pod_and_endpoint, unit_kube_run_network_policies_tenant_model, unit_kube_run_network_policies_run_model, unit_kube_shared_attempt_template, unit_kube_shared_attempt_adoption_verifies_security, unit_kube_resource_collision_never_adopted, unit_kube_resource_unknown_is_never_absent, unit_kube_resource_capabilities, unit_kube_preflight_accepts_owned_run_policies, unit_kube_verify_resource_pod]
---
# T-114 — P4: Kubernetes run resources and execution policy

## Goal
Implement [P4](../implementation/run-workspace-services-builders.md#p4--kubernetes-resources-and-policy): the Kubernetes `execution.RunResources` adapter and `run_shared` attempt Jobs, against seams S2–S6 and S8 of [T-122](T-122-o30-scope-amendment.md).

## Contract
ADR 0010 as amended by O-30 and the run-resources contract. Preserve one Job/primary container per executable attempt, `backoffLimit: 0`, no token automount, scoped ownership and verified adoption by scope, name, fingerprint and UID. No arbitrary pipeline Pod controls. Provider types stay inside `internal/kube`.

## Deliverables
- Typed client/fake support for PersistentVolumeClaim, PersistentVolume/VolumeAttachment observation, Service, Pod and NetworkPolicy.
- `RunResources`: `network` (per-run NetworkPolicies of seam S8 for the pinned model), `workspace` (RWOP PVC of the policy's storage class and size; `Released` only when no Pod uses the claim and no VolumeAttachment of its PV remains), `preparation` (one Job, seam S4, read-only checkout credential through the existing checkout port), `service` (Pod with `restartPolicy: Never`, bounded storage/`/dev/shm`, readiness probes, ClusterIP Service at the endpoint host). Resource admission credentials via a consumer-owned registrar port (seam S5). `Capabilities` advertise only what this adapter enforces; `builder_service`/`builder_isolated_worker` never.
- `run_shared` attempt Jobs from `AttemptSpec.Shared` (seams S1–S3): mount the bound workspace claim, attempt-private dirs, the pinned step role; adoption verifies those fields and the claim UID.
- `VerifyResourcePod` (seam S6) and namespace preflight accepting exactly the owned run policies.

## Acceptance
Declared check with every named test passing on the fake cluster. Real CSI/CNI evidence belongs to T-121/T-117; mocks do not qualify isolation.

## Out of scope
Chart, `internal/app`, other runtimes and cluster changes.

## Completion evidence
Implemented on `task/T-114-kubernetes-run-resources` (`ab121643`…`47631c2a`). A coordinator check before review found that a workspace ready only when `Bound` deadlocks on the stage `WaitForFirstConsumer` class; repaired (`2b2c343a`). Two independent reviews: jobs/network (`ses_e767481d1ffeL9JzaZNC1RTjuE`) requested forged-policy verification, a preflight cache key fix and legacy Job goldens, then approved `04e257e9` and confirmed `47631c2a`; lifecycle (`ses_e7674a98bffeULZacvMMP6tmvt`) requested changes three times (companion creates after removal, resurrection after removal, released without identity, metadata-only deletion, PV naming assumption, fabricated evidence after record loss, concurrent-Ensure uncertainty, tombstone recheck, persisted late UIDs, lost delete response) and approved `47631c2a`. Coordinator check on the final candidate: `make check-go PKGS='./internal/kube/...'` exit 0, 178 `Test_unit_kube_*` PASS including all 13 pinned acceptances, no FAIL. Fake-cluster component evidence only: real CSI/CNI qualification is T-121/T-117. T-116 must wire `kube.NewRunResources`, the `PolicyCatalog`, the registrar and checkout bridges, `VerifyResourcePod`, `Dependencies.OwnedPolicies`, and RBAC: PVC/Service/NetworkPolicy/Pod create/delete in tenant namespaces; get/list persistentvolumes; list volumeattachments; get storageclasses. Squashed to one commit; nothing pushed.
