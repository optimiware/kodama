---
id: T-013
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/workspace/, api/workspace.yaml, test/support/git/]
---
# T-013 — Git workspaces and controlled publication

## Goal
Make commits the durable checkpoints without a shared writable filesystem.

## Contract
Implement [workspace protocol](../specs/workspaces.md). Credential and attempt checks are injected; only the publisher receives write authority. Export provision/sync/archive/restore, writer reservation, publication/reconciliation, and retained-ref operations.

## Deliverables
Workspace table, portable work branches, private clone/bundle verification, pending publication fields, expected-tip CAS, source retention and HTTP fragment.

## Acceptance
Real Git tests `human_push_race`, `revoked_writer`, `push_ack_loss`, `retained_commit`, and `archive_publish_race` cover WS-01–WS-12.

## Out of scope
PVC checkpoints, editor sessions, automatic merges, or Pod-deletion-as-fencing.
