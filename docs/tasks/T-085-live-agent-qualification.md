---
id: T-085
milestone: M4
status: deferred
depends_on: []
decision_blockers: []
owns: [test/e2e/live-agent/, docs/acceptance/live-agent.md]
check: make check-go-integration PKGS=./test/e2e/live-agent/...
resources: [go, integration]
acceptance: [live_agent_preview, live_agent_rework, live_agent_needs_input, live_agent_budget]
---
# T-085 — Live agent and wrapper qualification after M3

## Goal
Qualify the actual pinned wrapper/model/provider against the integrated factory when scheduled.

## Contract
Preserve T-031's completed implementation. M2's deterministic adapter proves core orchestration, not a live model or wrapper image. This task owns the deferred live portion of original T-038 acceptance and the deployed wrapper/gateway/MCP join. Additional harness implementations remain T-052–T-054.

## Deliverables
Pinned wrapper/model versions, explicit cost limits, real implementation/review/preview, rejected-review rework, needs-input and budget/interruption evidence. Keep this expensive suite outside periodic local-core batches.

Run the Linux-only wrapper's Go checks inside its Linux fixture/image, not the developer host's core `./...` scope. Its OS-specific process-control implementation is deliberately excluded from portable core unit checks, not waived from wrapper qualification.

## Acceptance
`live_agent_preview`, `live_agent_rework`, `live_agent_needs_input`, and `live_agent_budget` exercise real model/tool interaction, bounded usage and durable candidate/evidence through actual execution. Retain AR-A01–AR-A07 and GM-A01–GM-A08 obligations formerly requiring a live provider at T-038.

## Out of scope
Claiming this evidence from canned output, adding a new provider framework or production/hosted qualification.
