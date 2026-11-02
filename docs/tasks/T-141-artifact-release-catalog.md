---
id: T-141
milestone: production
status: deferred
depends_on: [T-140]
decision_blockers: []
plan: true
owns: [internal/artifacts/, internal/domain/, internal/storage/migrations/]
check: make check-go PKGS='./internal/artifacts/... ./internal/domain/... ./internal/storage/...'
resources: [go]
acceptance: [unit_release_identity_conflict, unit_release_reference_retention, unit_artifact_catalog_scope]
---
# T-141 — Native artifact catalogue and immutable releases

## Goal
Extend the existing artifact capability under AF-01–AF-05 and AF-08/AF-09, using T-140's frozen contract and the [ADR 0012 handoff](../implementation/container-sdlc-refinement.md).

## Deliverables
- Scoped bounded artifact/release/publication discovery and exact release name/version resolution; retain files/OCI references through existing storage.
- Immutable release manifests over usable accepted outputs, including mixed-run/source and explicit source-free provenance; labels are caller-supplied, not metadata versions or an automatic SemVer allocator.
- Creation idempotency/conflict, retirement and retention-pin lifecycle; reconcile commit uncertainty without releasing live references. Keep tombstones/history and implement O-11 eligibility without silently pruning referenced content.
- Capability-local public/runtime handlers with denied/unavailable behavior until real authority and verifier ports are bound. Publication verification implementation follows T-144.
- Append migration(s), including least-privilege grants, after the integrated baseline; never rewrite historical migrations. Supply manifest wiring handoff to T-145.

## Acceptance
Shared memory/unit contract proves unique name/version under contention, bad/foreign/diagnostic/unfinalized references, pin/deletion ordering and lost-ack reconciliation. T-146 qualifies actual SQL/RLS/S3 races. Existing reserve/complete/seal and diagnostic retrieval remain valid.
