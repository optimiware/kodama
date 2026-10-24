---
id: T-035
milestone: M2
status: done
depends_on: [T-012, T-020, T-022, T-076]
decision_blockers: []
owns: [internal/delivery/, api/delivery.yaml]
check: make check-go PKGS=./internal/delivery/...
resources: [go]
---
# T-035 — Environment and deployment records

## Goal
Support build/preview/stage bookkeeping without a deployment-engine abstraction.

## Contract
Use namespace-scoped kubeconfig secrets, explicit environment/configuration snapshots, pre-apply deployment rows, stable action keys, verified image/source outputs, and cleanup runs from [delivery](../specs/delivery-and-quality-gates.md).

O-19 allows an ordinary build/deployment script to explicitly bind an authorized pinned secret to its child environment; use the existing secrets/entrypoint delivery contract, protected files where appropriate, and no plaintext Kubernetes manifest fields. Do not transfer those values to an agent step implicitly. A missing delivery seam is an owner prerequisite, not permission to edit entrypoint/runtime contracts outside this task. Use O-11's configurable 24-hour preview default and record cleanup exceptions.

Follow the [environment-pinning checkpoint](../implementation/environment-pinning.md): implement the concrete execution-owned `EnvironmentResolver`/`EnvironmentRequest`/`EnvironmentSnapshot` contract from T-076 in a delivery adapter, and consume an explicit pinned-context reader. Reject callbacks with absent/foreign/mismatched pins; never resolve current settings as a substitute. Component tests may inject immutable pins at that reader boundary with real record/provenance adapters. T-077 qualifies the real resolver/transaction/cleanup bindings after T-035/T-062; T-035 does not wait for that downstream integration.

## Deliverables
Environment/deployment routes/tables, runtime-scoped registration/result checks, explicit configuration snapshots, pre-apply/action-key receipts and outcome lookup/update contract. Export constructor/routes/migrations and fixtures for ordinary scripts, annotation-based reconciliation and deployment-bound cleanup authorization.

T-065 owns the tools image/scripts, T-066 the TTL scheduler and cleanup-run contribution, and T-067 their real qualification including T-077's real environment pinning binding. Keep cleanup scheduling outside this record package; T-072 owns central activation. Component persistence tests must verify RLS, concurrent action-key uniqueness and state/audit/outbox rollback before this task completes.

## Acceptance
`duplicate_deployment_key` and `wrong_image_source` verify DQ-07–DQ-10 with real record/provenance adapters, including rejection of missing/mismatched immutable-pin fixtures. The apply_ack_loss check moves to T-067; cleanup_after_work_done and cleanup_scope move to T-066 and are rechecked in T-067 (PX-06/PX-07). T-076 establishes the execution-side contract; the real delivery pinning check belongs to T-077 and the T-067 join. All remain required before M2 acceptance.

## Out of scope
Generic cloud deployment adapters, privileged builders, or treating output text as verified readiness.
