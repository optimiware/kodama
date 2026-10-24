---
id: T-031
milestone: M2
status: done
depends_on: [T-016, T-030, T-032, T-033, T-074]
decision_blockers: []
owns: [images/harness-opencode/, test/integration/harness-opencode/]
check: make check-go-integration PKGS=./test/integration/harness-opencode/...
resources: [go, integration]
---
# T-031 — Verified OpenCode headless wrapper

## Goal
Make the first real agent image work through the common entrypoint.

## Contract
Consult the OpenCode skill/current primary docs during this task; pin an actual version, headless invocation, model protocol, and MCP configuration. Reconcile any protocol mismatch with T-032/T-033 before merge.

T-074 supplies an early protocol/fixture checkpoint without waiting for gateway/MCP implementation. Consume that checkpoint, then qualify exact versions and supported controls against the merged real adapters here. The checkpoint is not real-image acceptance and does not select a new provider. If a mismatch needs changes outside these paths, request an ordered prerequisite from that owner before activation.

## Deliverables
Reproducible image/wrapper, structured output adapter, no-input failure behavior, transcript handling, gateway routing/tool scopes, smoke fixture generating a commit proposal.

Consume T-003's pinned design context/report schema, include blueprint/upstream references where applicable, and return candidate-bound conformance evidence under [AR-14](../specs/agent-runtime.md#requirements).

O-17 guidance is Markdown in a scoped Git repository: preserve its pinned commit/path references in instruction context rather than loading a moving branch during execution. Under O-19, model/tool provider credentials remain in their proxies; the wrapper uses scoped runtime authority and does not inherit another build step's explicit secret ENV bindings.

## Acceptance
`opencode_real_smoke`, `structured_result_required`, `cancel_wrapper`, and `tool_scope_denied` verify AR-02/AR-05/AR-06/AR-09/AR-11 and GM-12. Record any unavailable controls honestly.

`design_baseline_pinned` and `design_conflict_reported` verify AR-14/SD-08: moving an upstream branch cannot alter a running instruction context, and a material unresolved deviation produces blocked conformance evidence.

## Out of scope
Guessing CLI flags, claiming unsupported native resume, or bypassing the gateway to make the demo pass.
