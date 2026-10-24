---
id: T-094
milestone: M1
status: done
depends_on: [T-020, T-080, T-088]
decision_blockers: []
owns: [internal/execution/]
check: make check-go PKGS="./internal/execution/..."
resources: [go]
acceptance: [execution_claim_workspace_free, execution_claim_shape_memory]
---
# T-094 — Workspace-free step claim shape

## Goal
Let a work run dispatch a workspace-free container step on PostgreSQL, and make the memory adapter refuse the same shape so the two cannot diverge again.

## Contract
`claimMutation` in `internal/execution/dispatch.go` passes `Workspace: run.WorkspaceID` to `Aggregate.Claim` for every step, but the attempt row requires an all-or-nothing shape: PostgreSQL's `step_attempts_workspace_shape` refuses an attempt with `access = 'none'` that carries a workspace. Every claim of an `access: none` step of a work run therefore fails on PostgreSQL and the run never starts, which blocks M1 acceptance (T-026 finding 1). The Podman runtime runs only workspace-free attempts (ADR 0008). A step that consumes no source (`AccessNone`) is bound to no workspace; every other access keeps the run's workspace. The memory adapter, which has no such constraint, must enforce the same shape so unit tests catch the divergence.

Verified repair from T-026 (apply, then test):

```diff
-		at, err := agg.Claim(ClaimParams{
-			StepID: step.ID, AttemptID: attemptID, Plan: plan, Access: binding.Access, Workspace: run.WorkspaceID,
+		workspaceID := run.WorkspaceID
+		if binding.Access == AccessNone {
+			workspaceID = ""
+		}
+		at, err := agg.Claim(ClaimParams{
+			StepID: step.ID, AttemptID: attemptID, Plan: plan, Access: binding.Access, Workspace: workspaceID,
```

## Deliverables
The fix in `dispatch.go`, the shape rule in the memory store (and its contract suite, so the PostgreSQL adapter is held to the same rule), and the unit tests below. Change nothing outside `internal/execution/`.

## Acceptance
`execution_claim_workspace_free` proves a work run's `access: none` step is claimed with no workspace while a source-consuming step keeps the run's workspace, on the memory adapter and through the shared adapter contract. `execution_claim_shape_memory` proves the memory store refuses an attempt that pairs `access: none` with a workspace or a source-consuming access without one, exactly as `step_attempts_workspace_shape` does. Keep the existing T-020, T-080 and T-077 acceptance passing.

## Out of scope
Spool-directory creation in `NewCore` (`internal/app`), artifact output naming across `internal/entrypoint` and `internal/artifacts`, and result classification in `internal/results`: the T-026 report records these as follow-ups.
