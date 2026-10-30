---
id: T-140
milestone: production
status: deferred
depends_on: [T-116]
decision_blockers: []
plan: true
owns: [contracts/artifacts/, contracts/delivery/, contracts/runtime/, contracts/pipeline/, contracts/events/, api/artifacts.yaml, api/delivery.yaml, api/runtime.yaml, docs/contracts/artifacts-and-releases.md]
check: make check-go PKGS='./contracts/...' && make docs-check
resources: [go]
acceptance: [unit_release_manifest_canonical, unit_generic_delivery_contract, unit_sdlc_contract_compatibility]
---
# T-140 — Container-first SDLC executable contract checkpoint

## Goal and baseline
Freeze the smallest executable extension of [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) and its [semantic contract](../contracts/artifacts-and-releases.md) before consumers change. Prepared for the owner's plan-mode pass; scheduling/dependency reconciliation follows the [handoff](../implementation/container-sdlc-refinement.md#coordination-with-native-git-and-active-run-resource-work).

## Deliverables
- Schemas/DTOs/fixtures for canonical release manifests, publication receipts, scoped input fetch and generic environment/deployment messages; exact runtime actions and public/CLI operation handoff.
- Versioned provenance/source-set and gate-subject compatibility, preserving existing definitions/context/result/subject hashes. Unsupported consumers refuse new messages; no silent v1/v2 reinterpretation.
- Define how release references resolve into existing artifact inputs, exact input retention and workload grants, environment/step secret composition and new-run credential pins.
- Define canonicalization, unique keys/idempotency, ownership/error mapping, immutable metadata versus version labels, optional external observations and legacy projection/drain boundaries. Reuse one gate and one container primitive.

## Acceptance
Named contract vectors cover equivalent ordering, conflicting release contents, absent/foreign inputs, source-free/mixed-source manifests, same/new subject versions and unchanged historical hash vectors. Map AF-01–AF-10, DQ-17–DQ-21, PX-23/PX-24, AR-15/AR-16 and VS-15 to consumer fixtures. Record native Git contract dependency before scheduling.

## Out of scope
Server handlers/storage/assembly, new tool-specific step kinds, a provider plugin system, cluster changes or support claims.
