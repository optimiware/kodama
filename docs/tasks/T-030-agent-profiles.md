---
id: T-030
milestone: M2
status: done
depends_on: [T-012, T-020]
decision_blockers: []
owns: [internal/agents/, internal/execution/agent-resolver.go, api/agents.yaml]
---
# T-030 — Agent profile expansion

## Goal
Map an agent step to the existing container runtime.

## Contract
Use [agent spec](../specs/agent-runtime.md): immutable image/wrapper/instruction/model/tool defaults, pinned content, `needs_input` as failed with question. Export the resolver seam consumed by execution.

## Deliverables
Profile table/handlers, registration validation, container expansion, scoped context and limits, attempt transcript/result metadata linkage.

## Acceptance
`profile_pinning`, `unknown_wrapper`, `needs_input_failure`, and `agent_no_human_role` verify AR-01–AR-04/AR-10/AR-12/AR-13.

## Out of scope
Session/handoff tables, continuation negotiation, or another scheduler.
