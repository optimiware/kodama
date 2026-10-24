---
id: T-038
milestone: M2
status: done
depends_on: [T-026, T-039, T-065, T-067, T-073, T-101]
decision_blockers: []
owns: [test/e2e/m2/, test/integration/assembly-agent-delivery/, docs/acceptance/m2.md, openforge.json]
check: DEV_STACK=cluster make check-go-integration PKGS="./test/e2e/m2/... ./test/integration/assembly-agent-delivery/..."
resources: [go, integration]
acceptance: [m2_agent_preview, m2_rework, m2_needs_input, m2_stale_review, m2_tool_budget, agent_delivery_assembly, gate_consumption_atomicity, delivery_assembly_replay, native_hook_roundtrip]
---
# T-038 — M2 agent-to-preview acceptance

## Goal
Prove local agent/review/preview business behavior against real persistence and execution, using deterministic model/tool responses.

## Contract
Exercise [ERP preview](../examples/workflows/erp-gap-to-preview.md) in the `test` environment with real Git and registry on Podman services, the Podman runtime and a deterministic test-only agent/model adapter; kind appears only as the preview's deployment target. This is local core acceptance, not live-model/wrapper or production qualification; T-085 owns that later evidence.

## Deliverables
Successful implementation/test/review/build/preview fixture, rejected-review rework, missing-input failure, stale approval, bounded model/tool use, expiry cleanup report.

Own `agent_delivery_assembly`, `gate_consumption_atomicity`, and `delivery_assembly_replay` transferred from T-072, including actual transaction/revocation/acknowledgment-loss behavior. `native_hook_roundtrip` joins T-036's authenticated hook to real Git and durable admission. Add bounded M2 suites to the five-merge batch alongside M1 and repository unit checks, reusing preflighted scope `batch`. Keep live-provider/image smoke outside this loop.

## Acceptance
`m2_agent_preview`, `m2_rework`, `m2_needs_input`, `m2_stale_review`, and `m2_tool_budget` cover AR-A01–AR-A07, GM-A01–GM-A08, DQ-A01–DQ-A07.

## Out of scope
Claiming live-provider, wrapper-image or release acceptance from deterministic responses. Those obligations remain explicit in T-085.

## Prerequisites landed (orchestrator update)
The production joins T-038 reported as missing are now on `main`: T-096 (runtime action ceiling), T-097 (delivery environments reader, gate `DeploymentBinder`, runtime secret route), T-098 (Podman source checkout), T-099 (artifact output naming), T-100 (Git trigger production adapters) and T-101 (their binding in `internal/app`). Rebase is done. Replace the cases of `TestM2AssemblyFailsClosedWhereJoinsAreMissing` that now fail by design with the real scenarios (`m2_agent_preview`, `m2_tool_budget`, `agent_delivery_assembly`, `delivery_assembly_replay`), keep the five checks that already pass, and keep `docs/acceptance/m2.md` truthful: claim a check only when it passes against the real stack. A join that is still missing is reported as a blocker with evidence, not stood in.
