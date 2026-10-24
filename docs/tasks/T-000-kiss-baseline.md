---
id: T-000
milestone: foundation
status: done
depends_on: []
decision_blockers: []
owns: [README.md, AGENTS.md, CONTEXT.md, docs/]
---
# T-000 — KISS documentation baseline

## Goal
Apply the approved simplifications and create an implementation-ready dependency graph.

## Contract
The [decision register](../decisions.md), [runtime protocol](../implementation/step-entrypoint.md), and module ownership guide are the documentation baseline; implementation tasks freeze their machine-readable counterparts.

## Deliverables
Revised book/specs/contracts/examples/operations; review findings, schema/layout/testing design, and this backlog. Keep the full NATS outbox/JetStream/inbox contract.

## Acceptance
Links, examples, active requirement coverage, task graph, and unordered-path-conflict checks pass. Only this documentation task is marked done; software capability claims remain planned.

## Out of scope
Application implementation and unapproved provider/license selection.
