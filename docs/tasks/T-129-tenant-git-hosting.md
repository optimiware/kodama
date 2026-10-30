---
id: T-129
milestone: production
status: done
depends_on: [T-126]
decision_blockers: []
owns: [internal/kube/gitworker/, internal/kube/kubernetes/gitworker_types.go, internal/kube/kubernetes/gitworker_client.go, internal/kube/kubernetes/gitworker_client_test.go]
check: make check-go PKGS='./internal/kube/gitworker/... ./internal/kube/kubernetes/...'
resources: [go]
acceptance: [unit_gitworker_render_per_tenant, unit_gitworker_single_pod_volume, unit_gitworker_network_ingress_from_control_plane_only, unit_gitworker_no_platform_credentials, unit_gitworker_adopt_verified_only, unit_gitworker_volume_retained_on_replace, unit_gitworker_service_discovery]
---
# T-129 — Tenant Git hosting on Kubernetes

## Goal
One Git worker and one durable volume per tenant in a Kodama-managed service namespace (GR-03, GR-04, P-17, IT-17).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S2/S3, existing `internal/kube` client, preflight and ownership-verification patterns. New client operations live in the listed new files only.

## Deliverables
- Render and reconcile per tenant: namespace (Pod Security `restricted`), service account without an API token, `ReadWriteOncePod` PVC with retain policy, single-replica `Recreate` Deployment, ClusterIP Service, NetworkPolicies admitting only the control plane, worker identity Secret.
- Kubernetes client additions for namespaces, service accounts, Deployments and Services in the new files.
- Adoption only of objects verified by labels, fingerprint and UID; never touch foreign objects; preflight refusal when the storage class cannot provide single-Pod access.
- Worker discovery for the control plane (tenant → Service endpoint) and volume lifecycle independent of worker replacement and runs.

## Acceptance
Declared check; kind conformance for two tenants under `make check-go-integration DEV_SCOPE=t129 DEV_STACK=cluster PKGS='./internal/kube/gitworker/...'` (GR-A03, IT-A13).

## Out of scope
Chart values and wiring (T-156, T-132), runtime Jobs (T-152).

## Completion evidence
Implemented by a subagent; the first coordinator kind run failed (registry-port image references refused). Independent review requested changes twice (image references; non-CSI storage cannot prove single-Pod access; readiness kept after drift; unmodelled Deployment fields; private key material; worker created before isolation checks; then PEM blocks skipped by the decoder) and approved `e80ae374`. Coordinator checks: `make check-task TASK=T-129` PASS (7 acceptance names); kind `make check-go-integration DEV_SCOPE=t129 DEV_STACK=cluster PKGS='./internal/kube/gitworker/...'` PASS (`Test_conformance_gitworker_two_tenants`). Coordinator decision: kind proves namespaces, network isolation, retention and no-write reconcile but not single-Pod exclusivity; `Test_conformance_gitworker_rwop_exclusive` (skipped without `KODAMA_TEST_GIT_STORAGE_CLASS`) belongs to T-133 on a qualified CSI class. For T-132/T-156: worker environment names `KODAMA_GIT_*`, `RWOPProvisioners` allow-list and `IdentityProvider` wiring.
