---
id: T-003
milestone: foundation
status: done
depends_on: [T-001]
decision_blockers: []
owns: [internal/domain/, contracts/runtime/, api/runtime.yaml]
---
# T-003 — Freeze runtime contract fixtures

## Goal
Let entrypoint, auth, executor, and result agents implement independently.

## Contract
Translate [entrypoint](../implementation/step-entrypoint.md) into JSON schemas, examples, Go DTOs, and internal OpenAPI. Shared domain types contain IDs/scope/errors/version only; consumer interfaces live with consumers.

## Deliverables
Admission/context/token, artifact/source proposal, result schemas; canonical result-hash fixtures; request/error examples; exported primitive types.

Freeze pinned design-baseline reference serialization and the [conformance report](../implementation/design-conformance.md#agent-report-contract) schema/canonical digest fixtures with the existing runtime context. Share them with T-030/T-031/T-034; no separate conformance resource is introduced.

## Acceptance
`runtime_contract_fixtures` validates positive/negative payloads including foreign IDs, conflicting result hashes, questions, and nullable source. Covers PX-10, AR-06, AC-12.

`design_context_fixtures` covers immutable reference encoding, deterministic baseline digest, report disposition/check coverage, and wrong-candidate/baseline reports for AR-14/DQ-14/SD-08.

## Out of scope
Runtime servers or a central interface/plugin registry. Contract changes require dependent-task agreement.
