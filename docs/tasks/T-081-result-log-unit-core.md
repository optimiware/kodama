---
id: T-081
milestone: M1
status: done
depends_on: [T-022, T-023, T-062, T-064, T-086, T-087]
decision_blockers: []
owns: [internal/results/, internal/logs/]
check: make check-go PKGS="./internal/results/... ./internal/logs/..."
resources: [go]
acceptance: [unit_result_replay, unit_result_revocation, unit_log_scope, unit_results_logs_contract]
---
# T-081 — Result and log core unit qualification

## Goal
Make result finalization and log policy independently testable before executable assembly.

## Contract
Exercise the existing guarded finalization, source publication and log-retention interfaces with in-memory stores over `memdb`, fake authority, and temporary Git and in-memory object-store adapters. Preserve durable receipt and tenant semantics; doubles cannot establish real transaction atomicity.

## Deliverables
Bounded missing tests/fixes for duplicate callbacks, revoked attempts, publish failure/unknown outcome, bounded tails and retained-log selection. Add memory repositories for the results and log ports with an exported `Contract` shared with their PostgreSQL adapters (SD-13). Reuse existing test helpers. Actual SQL/Git/S3/Kubernetes joins remain T-026's checkpoint.

## Acceptance
`unit_result_replay` proves duplicate callbacks converge and unknown publication does not fabricate success. `unit_result_revocation` denies stale/foreign/revoked attempts before effects. `unit_log_scope` proves tenant/run selectors, bounds and fallback selection without a service. `unit_results_logs_contract` runs those contracts on the memory adapters with no service. Cover PX-05/PX-07, IT-03/IT-06 and AC-06.

## Out of scope
Root composition, new logging infrastructure or reimplementation of already-working adapters.
