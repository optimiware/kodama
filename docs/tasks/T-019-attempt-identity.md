---
id: T-019
milestone: M1
status: done
depends_on: [T-002, T-003, T-010]
decision_blockers: []
owns: [internal/runtimeauth/]
review: human
---
# T-019 — Attempt admission and runtime tokens

## Goal
Give each Job bounded authority independent of human tokens.

## Contract
Implement admission/refresh from the [runtime contract](../implementation/step-entrypoint.md). Use the wire contract frozen in [`contracts/runtime`](../../contracts/runtime/README.md). Define the active-attempt/Job ownership lookup as a consumer-owned interface in this module; T-003 freezes shapes, not that interface.

## Deliverables
One-time admission credentials, short-lived audience-bound tokens, current-attempt/cancellation checks, signing-key rotation, action scopes, safe logging.

## Acceptance
`cancelled_admission`, `wrong_job`, `wrong_audience`, `refresh_after_revoke`, and `rotated_key` cover IT-06/IT-08 and PX-08.

## Out of scope
Using Kubernetes API deletion as evidence that external effects ended or granting human review authority to workloads.
