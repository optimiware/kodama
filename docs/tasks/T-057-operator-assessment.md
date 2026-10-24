---
id: T-057
milestone: M4
status: deferred
depends_on: [T-056]
decision_blockers: []
owns: [docs/implementation/operator-assessment.md]
---
# T-057 — Assess operator need

## Goal
Resolve O-16 from real installation/upgrade operating evidence.

## Contract
O-16 is explicitly deferred by the owner, who does not currently expect an operator to be necessary. Keep this assessment unscheduled; ordinary Helm/Job operations remain the baseline. Only when the owner revisits O-16, inventory repeated manual lifecycle work and compare a small operator against existing Helm/Job reconciliation.

## Deliverables
Evidence-based decision proposal, costs/failure/ownership analysis, and—only if justified—exact resource/reconciliation contract for T-058.

## Acceptance
`operator_decision_review` names observed problems, alternatives, and measurable benefit. Owner acceptance records O-16; a negative decision closes T-058 as not applicable.

## Out of scope
Building an operator solely because Kubernetes is in the stack.
