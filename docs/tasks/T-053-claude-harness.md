---
id: T-053
milestone: M4
status: deferred
depends_on: []
decision_blockers: []
owns: [images/harness-claude/, test/integration/harness-claude/]
---
# T-053 — Claude Code wrapper

## Goal
Validate one additional headless harness against the common result contract.

## Contract
Pin documented CLI/provider/tool behavior. Under [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md), a direct Claude command with an explicitly bound API key is an ordinary container and needs no managed wrapper or gateway compatibility. This deferred task qualifies a maintained recipe/optional managed profile; if it claims mediation, prove that path or leave that capability unsupported. Incompatible mediation must not block the direct-container primitive or be silently claimed.

## Deliverables
Verified image/wrapper, configuration/version notes, structured result and cancellation fixtures, declared limitations.

## Acceptance
`claude_compatibility`, `claude_result`, `claude_cancel`, and `claude_scope` verify AR-02/AR-05/AR-06/AR-09 and GM-12 with a real supported provider.

## Out of scope
Assuming OpenAI protocol compatibility or advertising controls absent from the selected harness.
