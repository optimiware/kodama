---
id: T-110
milestone: production
status: planned
depends_on: [T-109, T-118]
decision_blockers: []
owns: [contracts/pipeline/, contracts/runtime/, internal/pipeline/, internal/execution/pipeline_version_v2_test.go]
check: make check-go PKGS='./contracts/... ./internal/pipeline/... ./internal/execution/...'
resources: [go]
acceptance: []
---
# T-110 — P0: versioned pipeline and runtime wire contracts

## Goal
Implement the executable shape/semantic/canonical checkpoint for [P0](../implementation/run-workspace-services-builders.md#p0--contract-checkpoint) after T-109 freezes the owning specifications.

## Contract
Design direction `0b12febef5b9e092982af74e2c951afe2dd80c19`, original executable baseline `7a66d3493f2bc69d197406f757841e603efbb0c1`, and immutable merged T-109 contract checkpoint `9db322ffd37a4279f8a7ac1a404a06e46533527d`. Its authoritative owners are [pipeline v1alpha2](../contracts/pipeline-format-v1alpha2.md), [runtime v2](../contracts/runtime-v2.md) and [run resources](../contracts/run-resources.md), including the reviewed resource-specific envelopes, `resource_unbound` error, argv omission rules, begin/lineage and mandatory network guarantees. Conform to PX/WS/IT/VS/DQ requirements introduced there. Keep all old golden bytes/digests and historical result hashes unchanged. No runtime may accidentally execute a new format under old per-attempt semantics: registration/validation must expose an explicit opt-in capability, off by default until T-116 wires supported admission. New schema representability is not installation support.

## Deliverables
- Versioned pipeline schema/Go DTOs and strict YAML/JSON parsing, normalization and canonical fixtures for shared scratch, services, bounded total deadline, resources/storage, probes, declared network edges and endpoint expressions. Validate immutable step/service images and reject unknown/raw Pod controls, invalid ports/probes/references, step-output service dependencies and new-format `workspace.from`.
- Stable topological-order fixtures with declaration-order tie breaks and gate positions; preserve explicit logical/output dependencies.
- New immutable runtime context version with run-resource/workspace/policy/endpoint/source-provenance pins and source-free shape, compatible reference to unchanged result/source-proposal envelopes as defined by T-109. Keep v1 readers/tests intact.
- Consume T-118's independently landed old-executor admission guard; add real v2-definition regression tests in the explicitly owned test file for ordinary and source-free cleanup admission, including resolver/storage definitions. Registration opt-in alone is not an execution fence. T-112 later extends the guard only with its complete run-resource capability checks.
- Supply versioned schema/README fixtures and the module API handoff for T-116. Keep existing `api/pipeline.yaml` and `api/runtime.yaml` unchanged in P0: the former is bundled into the generated aggregate/client and the latter is byte-checked against frozen v1 operations. T-116 owns source-fragment integration and regeneration together; do not publish a drifting aggregate or repoint a v1 operation to v2.

## Acceptance
Run the declared scoped check and `make docs-check`; test old golden compatibility, new shape/semantic/canonical fixtures, registration and execution capability refusal, source-free/source-backed defaults, invalid expressions, deterministic order and sensitive-value-free diagnostics. Record real test names in handoff for the coordinator to pin before merge.

## Out of scope
Execution/runtime resource effects, production enablement, shared dependency files and generated aggregate/client.
