---
id: T-034
milestone: M2
status: done
depends_on: [T-010, T-015, T-020]
decision_blockers: []
owns: [internal/gates/, internal/execution/gate-step.go, api/gates.yaml]
check: make check-go
resources: [go]
---
# T-034 — Canonical human gates

## Goal
Pause progression on one attributable human decision without compute.

## Contract
Use the single subject in [delivery](../specs/delivery-and-quality-gates.md). Source commit plus content/definition/evidence/deployment configuration are required; current authorization is rechecked at decision and consumption.

## Deliverables
Gate table, canonicalization fixtures, decision idempotency/version checks, pending-step integration, invalidation/applicability, audit/events, API fragment.

Validate agent candidate conformance artifacts against T-003's schema, expected candidate/baseline, pinned required-check coverage, and disposition. Bind their artifact IDs/digests through the existing gate subject; required evidence is not a new policy/override resource.

## Acceptance
`canonical_subject`, `concurrent_decision`, `changed_spec_source_config`, `reviewer_revoked`, and `gate_restart_no_job` cover DQ-01–DQ-06/DQ-11 and PX-13.

`conformance_required` and `conformance_stale_or_blocked` cover DQ-14/SD-08: absent, wrong-candidate, wrong-baseline, missing-check, or unresolved-deviation reports block approval and progression; fresh applicable evidence permits normal human review.

## Out of scope
Quorums, generic overrides, or allowing an agent identity to approve.
