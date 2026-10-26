---
id: T-110
milestone: production
status: planned
depends_on: [T-109]
decision_blockers: []
owns: [contracts/pipeline/, contracts/runtime/, internal/pipeline/, api/pipeline.yaml, api/runtime.yaml]
check: make check-go PKGS='./contracts/... ./internal/pipeline/...'
resources: [go]
acceptance: []
---
# T-110 — P0: versioned pipeline and runtime wire contracts

## Goal
Implement the executable shape/semantic/canonical checkpoint for [P0](../implementation/run-workspace-services-builders.md#p0--contract-checkpoint) after T-109 freezes the owning specifications.

## Contract
Design baseline `0b12febef5b9e092982af74e2c951afe2dd80c19`, executable baseline `7a66d3493f2bc69d197406f757841e603efbb0c1`, plus the immutable merged T-109 contract candidate. Conform to PX/WS/IT/VS/DQ requirements introduced there. Keep all old golden bytes/digests and historical result hashes unchanged. No runtime may accidentally execute a new format under old per-attempt semantics: registration/validation must expose an explicit opt-in capability, off by default until T-116 wires supported admission. New schema representability is not installation support.

## Deliverables
- Versioned pipeline schema/Go DTOs and strict YAML/JSON parsing, normalization and canonical fixtures for shared scratch, services, bounded total deadline, resources/storage, probes, declared network edges and endpoint expressions. Validate immutable step/service images and reject unknown/raw Pod controls, invalid ports/probes/references, step-output service dependencies and new-format `workspace.from`.
- Stable topological-order fixtures with declaration-order tie breaks and gate positions; preserve explicit logical/output dependencies.
- New immutable runtime context version with run-resource/workspace/policy/endpoint/source-provenance pins and source-free shape, compatible reference to unchanged result/source-proposal envelopes as defined by T-109. Keep v1 readers/tests intact.
- Module API fragment contributions and README fixtures only; aggregate/client generation belongs to T-116. Request an integration prerequisite if any existing aggregate drift test requires that regeneration before publication.

## Acceptance
Run the declared scoped check and `make docs-check`; test old golden compatibility, new shape/semantic/canonical fixtures, capability refusal, source-free/source-backed defaults, invalid expressions, deterministic order and sensitive-value-free diagnostics. Record real test names in handoff for the coordinator to pin before merge.

## Out of scope
Execution/runtime resource effects, production enablement, shared dependency files and generated aggregate/client.
