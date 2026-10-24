---
id: T-058
milestone: M4
status: deferred
depends_on: [T-057]
decision_blockers: []
owns: [cmd/kodama-operator/, internal/operator/, deploy/operator/, test/e2e/operator/, docs/operations/operator.md]
---
# T-058 — Implement justified operator automation

## Goal
Automate only the lifecycle actions approved by T-057.

## Contract
O-16 and this implementation are explicitly deferred; no operator is scheduled or assumed necessary. Reactivation requires an owner-approved T-057 assessment and explicit approval to implement its bounded contract. Preserve PostgreSQL as product-state authority and Helm compatibility. Obtain any shared dependency change through an integration owner.

## Deliverables
Approved controller/resource definitions, migration/install path, idempotent reconciliation, ownership/cleanup rules, runbook.

## Acceptance
`operator_reconcile_restart`, `operator_ownership`, and `operator_upgrade` demonstrate the specific accepted T-057 criteria and preserve IT-02/PX-05.

## Out of scope
A second workflow authority or unapproved management of tenant data services.
