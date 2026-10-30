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
Confirm current primary documentation, pinned CLI/provider protocol, noninteractive behavior, structured output and selected tool routing. [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) permits direct container/secret use independently; this deferred task qualifies a maintained recipe/optional managed profile. A missing mediated protocol blocks that claimed capability, not generic execution, and direct use must be identified accurately.

## Deliverables
Wrapper/image, safe credential/config injection, result/transcript adapter, cancellation and compatibility fixtures.

## Acceptance
`codex_compatibility`, `codex_result`, `codex_cancel`, and `codex_scope` verify AR-02/AR-05/AR-06/AR-09/AR-11 and GM-12.

## Out of scope
Claiming mediated controls for a direct credential path or adding a separate execution engine.
