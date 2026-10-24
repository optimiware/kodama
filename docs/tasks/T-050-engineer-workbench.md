---
id: T-050
milestone: M4
status: deferred
depends_on: [T-044, T-061]
decision_blockers: []
owns: [docs/user/workbench.md, docs/implementation/platform-workbench-handoff.md, test/e2e/platform-workbench/]
---
# T-050 — Engineer workbench

## Goal
Qualify the optional Platform-hosted engineer interface to the established factory workflow.

## Contract
O-15 selects Platform as the only intended UI host; Kodama remains primarily CLI/API/MCP/webhook driven. This task is explicitly deferred. Use existing public APIs for work, Git source links, runs/logs, evidence, gates and deployments; UI code cannot introduce alternate authorization/business state.

Before scheduling, agree a separately owned Platform task/worktree, likely in `packages/dashboard`, using its existing framework/auth shell. This task owns the Kodama-side handoff and cross-repository acceptance evidence, not the sibling checkout. Preserve keyboard-accessible review and current tenant/role checks through T-061. No standalone `web/` tree is owned or required here.

## Deliverables
Upstream UI ownership/API/auth handoff, tenant/project navigation and workflow requirements, loading/error/stream-resync requirements, accessibility criteria and real upstream UI acceptance evidence linked to the supported Kodama API revision.

## Acceptance
`workbench_tracer`, `tenant_switch`, `stale_edit_ui`, `stream_resync_ui`, and `keyboard_review` verify AC-01/AC-03/AC-06 and IT-03.

## Out of scope
A standalone Kodama dashboard, browser IDE, remote workspace sessions, or duplicating server policy in UI code.
