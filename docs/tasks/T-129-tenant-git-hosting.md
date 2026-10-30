---
id: T-129
milestone: production
status: planned
depends_on: [T-126]
decision_blockers: []
owns: [internal/kube/gitworker/]
check: make check-go PKGS='./internal/kube/gitworker/...'
resources: [go]
acceptance: [unit_gitworker_render_per_tenant, unit_gitworker_single_pod_volume, unit_gitworker_network_ingress_from_control_plane_only, unit_gitworker_no_platform_credentials, unit_gitworker_adopt_verified_only, unit_gitworker_volume_retained_on_replace]
---
# T-129 — Tenant Git hosting on Kubernetes

## Goal
Provision one Git worker and one durable volume per tenant in a Kodama-managed service namespace (GR-03, GR-04, P-17).

## Contract
ADR 0011 §3, seams S2/S3, existing `internal/kube` client, preflight and ownership-verification patterns.

## Deliverables
- Render and reconcile per tenant: namespace (Pod Security `restricted`), service account without an API token, `ReadWriteOncePod` volume with a retain policy, single-replica `Recreate` Deployment, ClusterIP Service, NetworkPolicies admitting only the control plane, and the worker identity secret.
- Adoption only of objects verified by ownership labels, fingerprint and UID; never touch foreign objects; preflight refusal when the storage class cannot provide single-Pod access.
- Volume lifecycle independent of worker replacement and of every run; deletion only through tenant offboarding after retention.

## Acceptance
Declared check with every named test passing; kind conformance for two tenants under `make check-go-integration DEV_SCOPE=t129 DEV_STACK=cluster PKGS='./internal/kube/gitworker/...'` (GR-A03).

## Out of scope
Chart values and server wiring (T-132), stage qualification (T-133).
