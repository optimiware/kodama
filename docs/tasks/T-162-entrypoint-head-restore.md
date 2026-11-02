---
id: T-162
milestone: production
status: done
depends_on: [T-151]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS='./internal/entrypoint/...'
resources: [go]
acceptance: [unit_entrypoint_restores_head_after_non_writer_publish, unit_entrypoint_reader_commit_never_reaches_task_branch]
---
# T-162 — Entrypoint restores source HEADs after publishing

## Goal
A non-writer step's agent-branch commit must not become the base of a later writer step's task-branch push (found by T-153's review; AR-A09, GR-07).

## Contract
[Git repositories](../specs/git-repositories.md) (agent branches are run-scoped; the task branch is for the reserved writer), [runtime v3](../contracts/runtime-v3.md), `internal/entrypoint/README.md` (T-151).

## Deliverables
- After a step publishes (or declines to publish) a non-writer branch, the entrypoint leaves every source checkout detached at the commit the next step must start from: the attempt's pinned or head commit as the context states, never the commit it just published. The same holds after a failed or cancelled step.
- `verifySources` and the begin observation reject a checkout whose HEAD is not that commit unless the context says the source is modified by the current attempt.
- A real-Git two-step test: a reader step commits on an agent branch; a later writer step in the same run commits on top; the task-branch push contains only the writer's commit and its own base.

## Acceptance
Declared check with the named tests.

## Out of scope
Wrapper changes (T-153), runtime adapters (T-152).

## Completion evidence
Implemented by a subagent; independent review approved `2a4d4b93` with minor findings (documentation of the partial-push-failure case; `detach` ordering and index checks), which were fixed and tested. Coordinator checks on the rebased branch: `make check-task TASK=T-162` PASS (2 acceptance names); `go test -race ./internal/entrypoint/... ./internal/runtime/bootstrap/...` PASS.

Invariant for T-150/T-152/T-132: after the command is launched, on every outcome, each source checkout is handed over detached at the context's `headCommit` (a write source whose push applied moves to the pushed commit; never to a commit published on an agent branch), and `verifySources` refuses a checkout whose HEAD differs (`workspace_unverified`, treated as a lost workspace). The next context's `headCommit` must keep following only writes to the source's own branch of succeeded attempts. A failed attempt after an applied push ends the workspace by design. The runtime adapter must hand the same volume to the next attempt without resetting it and treat `workspace_unverified` as a run failure, not retry it on the same scratch. `advance` still writes HEAD before the index (not part of this task).
