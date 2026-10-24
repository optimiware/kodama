---
id: T-074
milestone: M2
status: done
depends_on: [T-016, T-030]
decision_blockers: []
owns: [docs/implementation/opencode-wrapper-protocol.md, contracts/harness-opencode/]
check: make docs-check
---
# T-074 — Early OpenCode wrapper protocol checkpoint

## Goal
Expose wrapper/gateway/MCP compatibility requirements early while the active adapters retain their ownership.

## Contract
Read current primary OpenCode documentation and applicable configuration guidance. Record source revisions, headless/noninteractive invocation needs, structured result/transcript, cancellation, model streaming and MCP transport/tool scope requirements against the existing runtime/profile contracts. C-15/O-04 select the direction, not an invented exact version or provider protocol.

## Deliverables
Lean compatibility matrix, positive/negative boundary fixtures, actual supported-control observations and explicit unresolved mismatches for T-031/T-032/T-033. The checkpoint may recommend a version for real qualification but must distinguish documented behavior from measured behavior. Do not rewrite frozen runtime schemas or active adapter files; required changes return to their owner as ordered prerequisites. T-031 still pins/builds/tests the real image after gateway/MCP merge.

## Acceptance
**wrapper_protocol_sources** and **wrapper_protocol_fixtures** are reviewed documentation checks for AR-02/AR-05/AR-06/AR-09/AR-11 and GM-12: every assumed control has a primary source or an explicit unresolved gap, fixtures map to the runtime/profile envelopes, and unsupported behavior blocks dependent qualification. Run `make docs-check` and `python3 scripts/dev/check.py config` for documentation/fixture syntax and record the source/fixture review evidence. These are not Go acceptance test names; this docs profile cannot satisfy opencode_real_smoke or certify an untested version/provider.

## Out of scope
Production wrapper implementation, choosing a new provider, guessing flags or marking T-031 acceptance done.
