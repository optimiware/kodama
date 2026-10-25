---
id: T-054
milestone: M4
status: deferred
depends_on: []
decision_blockers: []
owns: [images/harness-codex/, test/integration/harness-codex/]
---
# T-054 — Codex wrapper

## Goal
Add a verified Codex headless image without changing orchestration semantics.

## Contract
Confirm current primary documentation, pinned CLI/provider protocol, noninteractive behavior, structured output, and tool routing. Any missing provider protocol becomes an explicit prerequisite before acceptance.

## Deliverables
Wrapper/image, safe credential/config injection, result/transcript adapter, cancellation and compatibility fixtures.

## Acceptance
`codex_compatibility`, `codex_result`, `codex_cancel`, and `codex_scope` verify AR-02/AR-05/AR-06/AR-09/AR-11 and GM-12.

## Out of scope
Provider/token passthrough that bypasses the declared controls or a separate execution engine.
