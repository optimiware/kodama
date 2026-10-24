---
id: T-080
milestone: M1
status: done
depends_on: [T-020, T-030, T-034, T-062, T-076, T-086, T-087]
decision_blockers: []
owns: [internal/execution/]
check: make check-go PKGS=./internal/execution/...
resources: [go]
acceptance: [unit_dispatch_replay, unit_cancel_fencing, unit_admission_pins, unit_execution_store_contract]
---
# T-080 — Execution core unit qualification

## Goal
Finish and test execution policy without a database, cluster or broker.

## Contract
Preserve the existing execution interfaces and entity transitions. `executiontest.Env` runs the PostgreSQL store today, so the unit layer skips it: build an in-memory `execution.Store` over `memdb` (T-087), with the peer joins (work content, workspace reservations, secret pins, events) passing the same `Tx`. Run execution on the Process runtime wrapped in `runtimetest.Decorator`, or `runtimetest.Manual`, with controlled time and fake authority. Add a small consumer-owned seam only where a real external effect currently prevents unit testing; no generic repository, transaction or worker framework.

## Deliverables
The in-memory store with an exported `execution.Contract` that the PostgreSQL store also runs (explicitly, in the `test` environment), then focused tests and bounded fixes for admission, dispatch/retry, cancellation and retained configuration, running on the in-memory store. Keep persistence/provider semantics unchanged. Report any real-adapter gap to T-026 rather than provisioning infrastructure during this task.

## Acceptance
`unit_dispatch_replay` proves repeated/recovered dispatch keeps attempt/action identity and handles unknown effects safely. `unit_cancel_fencing` proves cancelled/revoked work cannot launch or accept stale progress. `unit_admission_pins` proves definition/source/configuration inputs are immutable and rejected on scope/version mismatch. `unit_execution_store_contract` runs the contract on the memory store with no service (SD-A11). Cover PX-01/PX-05/PX-06/PX-07 with no service fixture.

## Out of scope
Root wiring, real SQL/RLS or Kubernetes qualification, provider adapters and production claims.
