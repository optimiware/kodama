---
id: T-067
milestone: M2
status: done
depends_on: [T-021, T-022, T-035, T-065, T-066, T-077, T-091, T-092, T-093]
decision_blockers: []
owns: [test/integration/delivery/, test/integration/environment-pinning-runtime/]
check: make check-go-integration PKGS="./test/integration/delivery/... ./test/integration/environment-pinning-runtime/..."
resources: [go, integration]
acceptance: [apply_ack_loss, deployment_cleanup_join, deployment_provenance_join, duplicate_deployment_key, wrong_image_source, cleanup_after_work_done, cleanup_scope, cleanup_duplicate_claim, environment_pinned_at_admission, environment_edit_preserves_pin, environment_pin_rollback, environment_pin_restart_scope]
---
# T-067 — Deployment apply/reconcile/cleanup qualification

## Goal
Join records, ordinary scripts and cleanup through real dependencies before M2 composition.

## Contract
Construct the merged modules, including T-077's implemented delivery/execution binding, and qualify its actual SQL behavior here. Persist action/configuration before apply; unknown responses reconcile recorded annotations rather than repeat arbitrary effects. Unit fixtures/doubles are not proof of this joined path.

## Deliverables
Real kind/PostgreSQL/runtime-output suite (steps run on the Podman runtime; kind is the deployment target and the only reason this task declares it) with scoped kubeconfigs, verified image/source/configuration, apply-response loss, restart, preview expiry and cleanup after business completion. T-072 owns process registration; T-038 retains the whole agent/delivery tracer.

## Acceptance
Own real environment/credential-pin checks transferred from T-077: `environment_pinned_at_admission`, `environment_edit_preserves_pin`, `environment_pin_rollback`, `environment_pin_restart_scope`. Use actual delivery/execution/secret persistence, non-owner roles, failure rollback, restart and deployment-bound cleanup. Unit policy evidence cannot satisfy SQL checks.

`apply_ack_loss`, `deployment_cleanup_join`, and `deployment_provenance_join` cover DQ-07–DQ-10 and PX-06/PX-07. Recheck `duplicate_deployment_key`, `wrong_image_source`, `cleanup_after_work_done` and `cleanup_scope` through the assembled real path. Verify no cross-tenant delete, no duplicate logical deployment and no inferred readiness from stdout.

`deployment_provenance_join` also verifies DQ-15/DQ-A11 through real admission → persisted snapshot → runtime context → delivery: an environment/credential edit after admission leaves the admitted configuration unchanged; missing/mismatched pins fail, and cleanup remains bound to the recorded deployment configuration.

## Out of scope
Root composition changes, production cluster qualification or claiming M2 accepted.
