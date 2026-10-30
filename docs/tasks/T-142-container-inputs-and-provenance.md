---
id: T-142
milestone: production
status: deferred
depends_on: [T-141]
decision_blockers: []
plan: true
owns: [internal/execution/, internal/entrypoint/, internal/runtimeauth/, internal/results/, internal/agents/, internal/gates/, internal/workspace/, internal/domain/, internal/storage/migrations/]
check: make check-go PKGS='./internal/execution/... ./internal/entrypoint/... ./internal/runtimeauth/... ./internal/results/... ./internal/agents/... ./internal/gates/... ./internal/workspace/... ./internal/storage/...'
resources: [go]
acceptance: [unit_admitted_artifact_fetch, unit_step_environment_secret_union, unit_direct_harness_container, unit_artifact_gate_provenance]
---
# T-142 — Container inputs, secrets and honest output provenance

## Goal
Implement AF-05, PX-26/PX-27, AR-16/AR-17, VS-15 and DQ-18/DQ-19 on the ordinary attempt/entrypoint path.

## Deliverables
- Admit/retain exact artifact/release input identities; authorize bounded runtime fetch of only those inputs, verify bytes before local use, no implicit archive extraction or source overwrite.
- Compose step/environment secret bindings without loss or collisions; current authorization/version pins/revocation apply to every use. Inspect T-113 first and repair only remaining secret-using writer/disclosure gaps.
- Direct harness commands work with a secret and ordinary result/output collection without managed profile/gateway/MCP setup. Keep optional profile expansion and accurate mediated/direct control claims.
- Record artifact-input digests, source/workspace observations and producer lineage. Apply T-140's source versus exact-artifact/release gate semantics; generated outputs need no source commit, unknown/modified source never becomes clean by assertion.
- Keep cancellation/begin fences, one shared holder, conservative command retries, result finalization and hosting-specific Git authority intact; append migrations where required.

## Acceptance
Scoped checks prove copied/foreign/revoked input denial, digest mismatch, restart-stable pins, collision rejection, direct harness execution and valid managed expansion. Verify generated-file chains, changed source/refused stale approvals, immutable artifact approval after unrelated scratch edits and legacy subject/hash compatibility. Real fetch/cross-run joins belong to T-146.
