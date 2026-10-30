---
id: T-143
milestone: production
status: deferred
depends_on: [T-142]
decision_blockers: []
plan: true
owns: [internal/delivery/, internal/deliverycleanup/, internal/domain/, internal/storage/migrations/, docs/implementation/environment-pinning.md]
check: make check-go PKGS='./internal/delivery/... ./internal/deliverycleanup/... ./internal/storage/...' && make docs-check
resources: [go]
acceptance: [unit_tool_neutral_deployment, unit_generic_delivery_unknown_effect, unit_rollback_credentials_and_config, unit_legacy_delivery_read]
---
# T-143 — Tool-neutral deployment and cleanup records

## Goal
Implement DQ-19–DQ-21 with T-140's versioned [generic delivery contract](../contracts/artifacts-and-releases.md#generic-environment-and-deployment).

## Deliverables
- Generic target/configuration/secret bindings without mandatory namespace, kubeconfig, `preview|stage` taxonomy or single OCI image. Accept exact release/artifact sets and infrastructure-only delivery.
- Small tracked-deployment pre-apply/action/outcome protocol with optional bounded external observations; scripts own readiness and reconciliation. Ordinary untracked commands require no callback.
- Remove catalogue-based automatic effect replay from the new path; preserve unknown outcomes across timeout/cancellation/lost callbacks. Provide an attributable authorized reconciliation operation/run binding.
- Optional expiry → cleanup definition admission remains; cleanup/rollback uses recorded target/config/artifacts and newly authorized credential pins. Never resurrect revoked historical credentials or mutate old outcomes.
- Separate new generic configuration identity (including logical credential bindings) from immutable per-run value-version pins. Project legacy version-bearing snapshots into explicitly linked new snapshots without changing old digests.
- Legacy record projection/drain support and appended migrations; hand T-145 an exact inventory of remaining legacy adapters/routes/callers for removal.

## Acceptance
Unit contracts cover non-Kubernetes and no-image deployments, duplicate intent, mismatched pins, unknown apply, late reports, explicit settlement, cleanup scope, rotated/revoked credentials and immutable history. Real SQL/admission joins and provider-facing recipes are T-146 evidence, not inferred from mocks.
