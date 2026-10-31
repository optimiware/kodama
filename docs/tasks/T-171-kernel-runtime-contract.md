---
id: T-171
milestone: kernel
status: done
depends_on: []
decision_blockers: []
owns: [internal/kernel/runtime/]
check: make check-go PKGS='./internal/kernel/runtime/...'
resources: [go]
acceptance: [unit_kernel_runtime_contract, unit_kernel_runtime_dependency_boundary]
---
# T-171 — Kernel runtime contract and dependency boundary

## Goal
Create the consumer-owned contract the standalone pipeline runtime is built on: the execution model, the ports the engine depends on, and a checked dependency boundary. This is additive; the current composition keeps running unchanged.

## Contract
[ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md), [O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38), [pipeline execution](../specs/pipeline-execution.md), and P1.1 of the [simplification plan](../implementation/pipeline-runtime-simplification.md).

## Deliverables
- A new `internal/kernel/runtime` package containing only value types and ports:
  - run, step and step-kind (`container`, `wait`), `when` predicates, wait specification and wait state, resource specification and state, runtime binding, secret reference, result/reason codes;
  - a claim with owner identity, expiry and monotonic fence, and a command intent;
  - explicit run outcome and cleanup state kept separate;
  - `StateStore`, `Runtime`, `ResourceManager`, `SecretResolver`, `WorkspacePreparer`, `LogSink` and `Clock` ports.
- Ports are narrow and consumer-owned. Tenant and project scope is carried through commands, handles, observations and events rather than read from ambient context.
- One owner for workspace preparation lifecycle: `WorkspacePreparer` supplies pinned preparation inputs and `ResourceManager` owns ensure/observe/stop/remove. The two do not independently create or adopt a workspace.
- Model invariants as tests: legal step-kind and wait transitions, fence comparison, terminal versus cleanup-pending, and rejection of a stale or duplicate consumption.
- A boundary test in the style of `internal/artifacts/boundaries_test.go` that parses this package's imports and fails on SQL, broker, HTTP, Git-hosting, task, identity, agent or shell packages.
- Package documentation stating the transaction/effect boundary: state commits with outbox intent, external effects run outside transactions, uncertain outcomes are reconciled.

## Acceptance
`make check-go` over the new package, with `Test_unit_kernel_runtime_contract` and `Test_unit_kernel_runtime_dependency_boundary` passing. The tests prove the model invariants and the import boundary; they do not claim a working engine.

## Out of scope
Memory or PostgreSQL state adapters, the engine, runtime backends, moving existing packages, pipeline YAML syntax, and any change to `internal/app`.
