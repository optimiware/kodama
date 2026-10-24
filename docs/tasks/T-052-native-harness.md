---
id: T-052
milestone: M4
status: deferred
depends_on: []
decision_blockers: []
owns: [internal/agents/native/, images/harness-native/, test/integration/harness-native/]
---
# T-052 — Native harness adapter

## Goal
Add a native agent loop behind the established container/profile contract.

## Contract
Implement model turns, approved tools, explicit budgets, and structured completion using the gateway/MCP/runtime interfaces. It must run as an ordinary harness image, not a second control plane.

## Deliverables
Bounded loop, native tool dispatch, prompt/context construction, result/transcript handling, reusable compatibility fixture shared by adapter tests.

## Acceptance
`native_tool_scope`, `native_budget`, `native_cancel`, and `native_commit_result` verify AR-01–AR-06/AR-09–AR-13 and relevant GM limits.

## Out of scope
Agent session continuation, dynamic pipeline graphs, or autonomous human approval.
