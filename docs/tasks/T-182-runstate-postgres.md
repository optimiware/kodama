---
id: T-182
milestone: kernel
status: done
depends_on: [T-174, T-179, T-185, T-186, T-189, T-192]
decision_blockers: []
owns: [internal/shell/runstate/postgres/]
check: make check-go PKGS='./internal/shell/runstate/postgres/...'
resources: [go, integration]
acceptance: [unit_runstate_postgres_contract, unit_runstate_postgres_scope]
---
# T-182 — PostgreSQL kernel state adapter

## Goal
Implement the frozen kernel `StateStore` over PostgreSQL so the engine runs on authoritative durable state, and prove it passes the same contract suite as the memory adapter.

## Contract
`internal/kernel/runtime` ports, the T-174 contract suite in `test/support/kernelcontracts`, the T-179 schema, and P2.1–P2.3 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- A PostgreSQL adapter behind the kernel port, in `internal/shell/runstate/postgres`, built on the existing `internal/storage` transaction helpers and the 004 schema.
- State mutation and outbox intent commit in one transaction; a failed commit leaves neither.
- Tenant predicates and RLS on every operation; a foreign scope is a not-found.
- Claim, lease renewal, fence takeover, transition, wait decision and signal lookup use the durable columns and guards added by T-179 — no second source of truth in Go.
- The T-174 contract suite runs unchanged against the real database; no adapter-specific expectation is added to the suite.
- A skipped database is not evidence: the acceptance run uses `KODAMA_REQUIRE_POSTGRES=1` and a non-owner role.

## Acceptance
`make check-go` with `Test_unit_runstate_postgres_contract` and `Test_unit_runstate_postgres_scope` passing, plus recorded real-PostgreSQL evidence in the handoff.

## Parked
T-182 is parked on [T-189](T-189-wait-and-materialization-schema.md). T-185 closed the first round of gaps; implementing the adapter then proved two more against a real database: the wait-decision constraints forbid a decided wait on an unsettled step, and `RunRecord.Materializations` has no table. Resume once T-189 merges.

## Follow-ups from review
- Read and write the decision reason from `signal_records` once T-192 adds the column, and delete the `wait_reason` fallback so replay is retention-independent.
- Use one canonical time representation for stored deadlines and derived opening times; independently truncating each rejects a legal later decision for a timeout not divisible by a microsecond.

## Completion evidence
Candidate `8d7267dce2f6129a1db4e15d70afd77f0acb6c05`, clean tree. `make check-task TASK=T-182` PASS (2 acceptance names, no database). Real PostgreSQL: `KODAMA_TEST_POSTGRES_ADMIN_URL=... KODAMA_REQUIRE_POSTGRES=1 go test -count=1 -race -v ./internal/shell/runstate/postgres/...` on a disposable Podman `postgres:17.11` — 127 PASS, 0 FAIL, 0 SKIP, including all 58 shared suite cases. The runtime role is a non-owner without `BYPASSRLS`; RLS is enabled and forced on all 8 kernel tables.

## Out of scope
The engine, NATS publication, task projection, and any change to the schema or the contract suite.
