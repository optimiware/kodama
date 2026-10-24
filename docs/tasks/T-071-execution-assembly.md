---
id: T-071
milestone: M1
status: done
depends_on: [T-070, T-016, T-018, T-021, T-022, T-064, T-080, T-081, T-082, T-086, T-088]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [execution_wiring, execution_shutdown, execution_guard_binding, env_selection]
---
# T-071 — Incremental container execution assembly

## Goal
Wire the server → executor → entrypoint → result/log interfaces with deterministic Go tests before M1 integration.

## Contract
Extend T-070's explicit composition with T-021's executor, T-019 runtime admission, T-022 guarded result/source/artifact adapters and T-064 qualified logs. Preserve T-062's transaction/lock-order bindings, runtime receipt exception and denial on missing limits/authority. Wire `kodama step` and controller/worker shutdown deliberately.

## Deliverables
`KODAMA_ENV` parsing and the construction guards of [SD-12](../specs/software-design.md) (no default; `production` refuses in-memory stores, the Process runtime, the development KEK and the OIDC fixture), the per-environment constructor table in `internal/app`, executable container-only wiring, runtime routes, cancellation/reconciliation workers and log API, updated aggregate/client. Test constructor selection, worker lifecycle and callbacks with injected adapters; production constructors select the actual guarded adapters and fail closed. T-027 finishes the CLI/client joins. T-026 owns the installed Podman/kind roundtrip and real cross-module failure checks.

## Acceptance
`env_selection` proves an unset or unknown `KODAMA_ENV` stops startup and `production` refuses each development or test adapter (SD-A10). `execution_wiring`, `execution_shutdown`, and `execution_guard_binding` verify constructor selection, controlled worker startup/shutdown, cancellation routing and guarded result bindings without services. The original `execution_assembly_roundtrip`, `execution_assembly_cancel_restart`, and `execution_assembly_result_guard` transfer to T-026, retaining PX-03/PX-05/PX-07/PX-10/PX-12, IT-03/IT-06 and AC-06/AC-12. They are required before M1 acceptance, not before this contribution merges.

## Out of scope
Declaring M1 accepted, agent/gate execution, production images or editing module contracts silently.
