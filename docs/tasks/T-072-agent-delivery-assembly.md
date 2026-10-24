---
id: T-072
milestone: M2
status: done
depends_on: [T-027, T-030, T-031, T-032, T-033, T-034, T-035, T-037, T-066, T-068, T-077, T-083, T-084, T-092, T-093]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [agent_delivery_wiring, gate_wiring, delivery_worker_lifecycle]
---
# T-072 — Incremental manual agent/review/delivery assembly

## Goal
Assemble the manual-trigger agent-to-preview slice independently of T-036's selected native Git receive-hook implementation.

## Contract
Continue M1's wiring; consume agent/gateway/MCP interfaces, canonical gates, delivery/cleanup and CLI contributions. Test orchestration with deterministic model/tool adapters. Production constructors select actual adapters and fail closed; doubles are injected only by tests. T-073/T-067/T-038 qualify real SQL/deployment joins before M2 acceptance. Live-provider smoke and additional wrappers remain T-085's deferred scope.

## Deliverables
Bind T-093's delivery `EffectReconciler` and the delivery `DeploymentCleanup` adapter into the core execution dependencies (replacing the unbound-port note in `internal/app/core.go`), and schedule T-092's deployment-bound cleanup through the assembled definition. Manual run/profile → agent candidate/evidence → human gate → delivery/cleanup wiring, aggregate/client updates and unit failure suites. Preserve M1 behavior. T-039 joins the native trigger; T-038 runs the actual Podman checkpoint.

## Acceptance
`agent_delivery_wiring`, `gate_wiring`, and `delivery_worker_lifecycle` verify constructor/authority selection, deterministic agent output, gate progression without compute allocation, worker cancellation and delivery routing using injected adapters. Original real-path checks `agent_delivery_assembly`, `gate_consumption_atomicity`, and `delivery_assembly_replay` transfer to T-038, retaining AR-06/AR-14, GM-12, DQ-03/DQ-04/DQ-07–DQ-10/DQ-14, PX-13 and SD-11 obligations.

## Out of scope
Waiving T-036/O-03, full M2 acceptance, hosted authority or production qualification.
