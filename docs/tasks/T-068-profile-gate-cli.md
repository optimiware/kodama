---
id: T-068
milestone: M2
status: done
depends_on: [T-025, T-030, T-034]
decision_blockers: []
owns: [internal/cli/agent/, internal/cli/gate/]
check: make check-go PKGS="./internal/cli/agent/... ./internal/cli/gate/..."
resources: [go]
---
# T-068 — Profile and evidence-bound gate CLI

## Goal
Deliver profile/review commands without waiting for deployment tools or cleanup.

## Contract
Use T-025's client/output conventions and T-030/T-034's module API contributions. Display current subject/version and evidence; a decision requires an explicit reason and public version/idempotency semantics. Export command registration for T-072.

## Deliverables
Profile list/register, gate inspect/decide and JSON/error/replay fixtures. T-037 retains only environment/deployment commands; root command and aggregate public-client updates belong to assembly.

## Acceptance
`stale_gate_cli`, `decision_retry`, and `json_review_evidence` preserve the original T-037 AC-01/AC-02/AC-05 and DQ-03/DQ-04 obligations. Command tests cross public HTTP boundaries, preserve stale/conflict exit semantics and stable retry keys, and never treat reading evidence as approval.

## Out of scope
Client-side authorization, root wiring or deployment commands.
