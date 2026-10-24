---
id: T-016
milestone: M1
status: done
depends_on: [T-003]
decision_blockers: []
owns: [internal/entrypoint/, test/integration/entrypoint/]
---
# T-016 — In-Pod step entrypoint

## Goal
Run ordinary commands through one source/result lifecycle.

## Contract
Implement [kodama step](../implementation/step-entrypoint.md) against frozen HTTP fixtures. Expose a CLI registration contribution; composition owns main dispatch.

## Deliverables
Admission/refresh client, exact checkout, argv/env launch, process-group cancellation, bounded outputs/diagnostics, bundle proposal, idempotent final result.

## Acceptance
`exit_zero_missing_output`, `path_escape`, `cancel_child_tree`, `duplicate_result`, and `source_conflict` cover PX-08/PX-10/PX-14, WS-04/WS-06, and AR-06 using a contract server and real child commands/Git.

## Out of scope
Canonical Git write credentials, harness-specific flags, or a direct database/broker client.
