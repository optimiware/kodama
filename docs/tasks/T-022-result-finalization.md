---
id: T-022
milestone: M1
status: done
depends_on: [T-015, T-019, T-020]
decision_blockers: []
owns: [internal/results/, test/integration/results/]
---
# T-022 — Runtime results and source finalization

## Goal
Accept valid attempt outputs once, without trusting arbitrary workload IDs.

## Contract
Implement [runtime artifact/source/result routes](../implementation/step-entrypoint.md) using artifacts, workspace publisher, auth, and execution interfaces. Freeze result acceptance ordering before e2e composition.

Owner resolution: call `runtimeauth.CheckSession`. `result.submit` stays authorized after the attempt leaves `active` when the grant is not revoked and the binding still matches, so an accepted-result retry reaches execution and returns the original `200` or `409 result_conflict`. Explicit revocation still rejects the callback. Do not bypass `CheckSession` or treat a terminal attempt as active.

## Deliverables
Digest/type/provenance validation, idempotent result hash, source proposal/publication coordination, late-result rejection, failure diagnostics, required-output gating.

## Acceptance
`conflicting_result`, `foreign_artifact`, `missing_output`, `cancel_finalization_race`, and `published_git_db_failure` cover PX-10/PX-12, WS-04/WS-06/WS-11, and IT-06.

## Out of scope
Inferring success from stdout/exit zero or storing checkpoint manifests as durable source.
