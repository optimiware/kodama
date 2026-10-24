---
id: T-083
milestone: M2
status: done
depends_on: [T-030, T-032, T-086, T-087]
decision_blockers: []
owns: [internal/agents/, internal/gateway/]
check: make check-go PKGS="./internal/agents/... ./internal/gateway/..."
resources: [go]
acceptance: [unit_agent_profile_pin, unit_agent_outcomes, unit_model_budget, unit_agents_gateway_contract]
---
# T-083 — Agent and model core unit qualification

## Goal
Test agentic behavior deterministically before connecting a live provider.

## Contract
Use existing profile/model interfaces, scripted model responses, controlled time and in-memory persistence over `memdb` (agents already has `MemRepo`; add the gateway's). Keep provider SDKs and HTTP transport behind adapters; reuse working implementations and avoid a universal agent framework.

## Deliverables
Bounded tests/fixes for immutable profiles, success/failure/needs-input outcomes, interruption and model-budget accounting. Publish constructor/fixture examples for T-072. Real provider/wrapper smoke is explicitly T-085, not this merge gate.

## Acceptance
`unit_agent_profile_pin` preserves admitted profile/instructions despite later edits. `unit_agent_outcomes` maps success, interruption and needs-input without inventing completion. `unit_model_budget` verifies limits, tenant attribution and failure accounting using deterministic responses. `unit_agents_gateway_contract` runs the agents and gateway repository contracts on the memory adapters with no service (SD-A11). Cover AR-06/AR-14 and GM-12.

## Out of scope
Live credentials, model spending, image builds, additional wrappers and hosted adapters.
