---
id: T-044
milestone: M3
status: done
depends_on: [T-038, T-046]
decision_blockers: []
owns: [test/e2e/m3/, test/integration/assembly-business-loop/, docs/acceptance/m3.md]
check: make check-go-integration PKGS="./test/e2e/m3/... ./test/integration/assembly-business-loop/..."
resources: [go, integration]
acceptance: [m3_business_loop, m3_rejection, m3_notification_outage, m3_dependencies, m3_metrics, business_loop_assembly, business_loop_gate_links, business_loop_event_replay, chatops_gate_integration]
---
# T-044 — Full SDLC acceptance

## Goal
Prove stage, tests, QA, review, rework, and notification as one user journey.

## Contract
Use [staging example](../examples/workflows/staging-and-acceptance.md), installed local composition and real dependencies, with deterministic QA/model responses. Human review uses an authenticated reviewer fixture distinct from the QA workload. Browser/live-agent and blueprint expansion remain deferred T-069/T-047/T-085 scope.

## Deliverables
Accepted/rejected business paths, linked defect/rework, stage-image provenance, notification outage/replay, WIP/dependency/metrics checks, release evidence.

## Acceptance
Own `business_loop_assembly`, `business_loop_gate_links`, `business_loop_event_replay` from T-075 and `chatops_gate_integration` from T-046. Verify actual transaction/notification/replay behavior, not just wiring doubles.

`m3_business_loop`, `m3_rejection`, `m3_notification_outage`, `m3_dependencies`, and `m3_metrics` cover DQ-A04–DQ-A07/DQ-A09, WM-A03–WM-A05/WM-A08, AC-A06/AC-A07.

## Out of scope
Claiming production or untrusted-hosted qualification from development kind tests.
