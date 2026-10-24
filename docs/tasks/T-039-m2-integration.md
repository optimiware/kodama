---
id: T-039
milestone: M2
status: done
depends_on: [T-031, T-032, T-033, T-034, T-035, T-036, T-037, T-068, T-072]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [m2_composition_smoke, m2_contract_compatibility]
---
# T-039 — M2 composition

## Goal
Merge runtime/gateway/gate/delivery contributions into one server and CLI.

## Contract
Consume frozen module contributions; maintain M1 compatibility. Full M2 acceptance includes T-036's selected native Git receive-hook integration under resolved O-03. Manual triggers remain usable independently; GitHub or another provider adapter is not a prerequisite for M2.

T-072 unit-tests manual-trigger agent/review/delivery wiring independently of native Git triggers. Consume those constructors/fixtures and join T-036; T-073/T-067/T-038 then qualify actual transaction/delivery behavior. No unit slice waives O-03 or the M1 checkpoint. Live provider/wrapper evidence is explicitly deferred to T-085.

## Deliverables
Route/migration/CLI/resolver wiring, gateway/MCP configuration, aggregate contracts and unit smoke checks using deterministic model/tool responses. Real service joins are T-038; live-provider/image qualification is T-085.

Regenerate T-027's public client alongside the aggregate contract; include generation drift and supported-client behavior in `m2_contract_compatibility`.

## Acceptance
`m2_composition_smoke` and `m2_contract_compatibility` verify AC-10/AC-12, PX-01/PX-13, and GM-12 before T-038.

## Out of scope
Parallel changes to shared root files outside this integration owner.
