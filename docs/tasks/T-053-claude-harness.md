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
Pin documented CLI/provider/tool behavior and check whether required mediation is supported. If the current gateway protocol is incompatible, record a bounded prerequisite task and remain blocked until it lands; do not silently bypass it.

## Deliverables
Verified image/wrapper, configuration/version notes, structured result and cancellation fixtures, declared limitations.

## Acceptance
`claude_compatibility`, `claude_result`, `claude_cancel`, and `claude_scope` verify AR-02/AR-05/AR-06/AR-09 and GM-12 with a real supported provider.

## Out of scope
Assuming OpenAI protocol compatibility or advertising controls absent from the selected harness.
