---
id: T-084
milestone: M2
status: done
depends_on: [T-033, T-034, T-062, T-086, T-087]
decision_blockers: []
owns: [internal/gates/, internal/mcpproxy/]
check: make check-go PKGS="./internal/gates/... ./internal/mcpproxy/..."
resources: [go]
acceptance: [unit_gate_subject, unit_gate_replay, unit_tool_authority, unit_gates_mcp_contract]
---
# T-084 — Gate and tool core unit qualification

## Goal
Finish human-decision and tool-authorization policy without external services.

## Contract
Use existing gate/MCP interfaces, in-memory gate and MCP-proxy repositories over `memdb` with an exported `Contract` shared with PostgreSQL (`gatestest` needs PostgreSQL today), fake current authority and deterministic tool responses. Gate waiting consumes no executor capacity. Keep SQL locking/RLS proof at T-073 and real delivery at T-038.

## Deliverables
Focused tests/fixes for stale subjects, repeated decisions, revocation, tenant/tool allow-lists and failed tool responses. Small consumer-owned seams only; no generic policy framework or fake authority in production wiring.

## Acceptance
`unit_gate_subject` rejects changed source/spec/configuration/evidence and revoked reviewers. `unit_gate_replay` converges repeated decisions and preserves explicit waiting/progression. `unit_tool_authority` denies foreign/disallowed tools and accounts for errors/limits. `unit_gates_mcp_contract` runs those contracts on the memory adapters with no service (SD-A11). Cover DQ-03/DQ-04/DQ-14, GM-12 and PX-13.

## Out of scope
Database concurrency certification, real provider adapters, wrapper images and root wiring.
