---
id: T-075
milestone: M3
status: done
depends_on: [T-039, T-040, T-041, T-042, T-045]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [business_loop_wiring]
---
# T-075 — Incremental business-loop assembly

## Goal
Join evidence mapping, real gate notifications and flow/dependency capabilities before the full QA image/blueprint milestone join.

## Contract
Continue T-039's app wiring with T-040/T-041/T-042/T-045. Keep human completion explicit and current authority on gate links. Browser-agent QA and blueprint delivery are deferred T-069/T-047 work.

## Deliverables
Constructor/routes/migrations/worker/command contributions, updated API/client and unit tests with controlled adapters. Production constructors select real authority. T-046 succeeds this central owner; T-044 owns the actual business-loop joins.

## Acceptance
`business_loop_wiring` verifies constructor/worker selection and event routing using deterministic adapters. Original `business_loop_assembly`, `business_loop_gate_links`, and `business_loop_event_replay` transfer to T-044, preserving WM-01/WM-05/WM-12, AC-08/AC-09/AC-12, DQ-06/DQ-10 and SD-11.

## Out of scope
Treating mapping fixtures as real exploratory QA, marking M3 accepted or changing release/hosted decision gates.
