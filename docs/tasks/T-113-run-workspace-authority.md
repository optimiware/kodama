---
id: T-113
milestone: production
status: planned
depends_on: [T-112, T-121]
decision_blockers: []
owns: [internal/entrypoint/, internal/runtime/bootstrap/, internal/runtimeauth/, internal/secrets/, internal/workspace/, internal/results/, internal/logs/, internal/agents/]
check: make check-go PKGS='./internal/entrypoint/... ./internal/runtime/bootstrap/... ./internal/runtimeauth/... ./internal/secrets/... ./internal/workspace/... ./internal/results/... ./internal/logs/... ./internal/agents/...'
resources: [go]
acceptance: []
---
# T-113 — P3: one preparation, scoped authority and cumulative publication

## Goal
Implement [P3](../implementation/run-workspace-services-builders.md#p3--entrypoint-scoped-authority-and-publication) against frozen resource/policy contracts.

## Contract
ADR 0010/O-28/O-29, immutable T-109/T-110/T-112 checkpoints and the qualified T-121 profile. Closing T-111's driver task does not satisfy that profile dependency. Reuse publisher-owned expected-tip CAS and original result hashes. Preparation/service identities are not attempts and cannot acquire result/source/model/tool authority. Platform remains company-core authority.

## Deliverables
- Controller-selected preparation with read-only Git authority, trusted bound receipt and pre-command idempotency; new-context steps verify existing scratch without clone/reset/clean. Source-free runs have no Git authority. Keep outputs/secrets/identities private.
- Resource-specific committed-UID credential exchange, revocation/renewal and just-in-time declared secret delivery outside manifests/shared scratch, with safe private builder auth/session cleanup.
- Cumulative-writer lineage/CAS/replay/unknown-publication handling, original immutable source input, dirty/generated provenance and gate/candidate binding. Include relevant earlier run secret disclosures in source/output detection.
- Harden publication Git against retained config/hooks/helpers/filters/symlink/ownership attacks; control-plane validation stays authoritative. Keep bounded preparation/service diagnostics and correct finalization ordering.

## Acceptance
Declared scoped check plus checkout-once/restart/generated-file/non-Git-reader fixtures, cumulative writers, human push conflict/lost response, foreign-resource grant denial, earlier-step secret copy, candidate provenance and finalization regressions. Pin actual names and real integration requirements before review.

## Out of scope
Adapter resources/policy, execution lifecycle, shared assembly/client and hosted qualification.
