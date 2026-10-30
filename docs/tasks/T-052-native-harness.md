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
Implement the native Go agent loop in an ordinary OCI container packaged with ko, using the same step/secret/input/output primitives ([ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md)). An optional managed profile selects gateway/MCP/tool/budget capabilities and claims only those it enforces. It must not run tenant agent loops in the server or introduce a second control plane.

## Deliverables
Bounded loop, native tool dispatch, prompt/context construction, result/transcript handling, reusable compatibility fixture shared by adapter tests.

## Acceptance
`native_tool_scope`, `native_budget`, `native_cancel`, and `native_commit_result` verify AR-01–AR-06/AR-09–AR-13 and relevant GM limits.

## Out of scope
Agent session continuation, dynamic pipeline graphs, or autonomous human approval.
