---
id: T-187
milestone: kernel
status: planned
depends_on: [T-183]
decision_blockers: []
owns: [internal/shell/git/, internal/shell/api/gitpolicy.go, internal/shell/api/git.go, internal/shell/api/gitcontrol.go, internal/shell/api/git_policy_test.go, internal/shell/store/git.go, internal/shell/store/gitmemory.go, internal/shell/store/git_postgres_test.go, internal/shell/README.md, contracts/git/]
check: make check-go PKGS='./internal/shell/git/... ./internal/shell/api/... ./internal/shell/store/... ./contracts/git/...'
resources: [go, integration]
acceptance: [unit_git_integration_authority, unit_git_authority_refusals]
---
# T-187 — Git integration authority from the run, not the task

## Goal
Replace task-state approval with authority derived from the run, the eligible integration step and satisfied waits, as C4 and O-38 require.

## Contract
C4 in the [simplification plan](../implementation/pipeline-runtime-simplification.md), [Git repositories](../specs/git-repositories.md) (GR-21/GR-22, GR-A16/GR-A17), and the [Git service contract](../contracts/git-service.md).

## Deliverables
- Remove `TaskApproved` and every check that derives integration authority from a task field, resolution row or bare event.
- Issue authority only when all hold:
  - the run's pipeline is the repository's configured integration pipeline;
  - the step is the eligible integration step;
  - every required wait for the exact candidate has resolved with that wait's `approving` outcome and a `candidate` correlation equal to the pushed commit;
  - the ref is the protected default branch;
  - the expected old commit matches the worker's current ref and the new candidate is a fast-forward.
- Keep the native worker's current-authority and expected-old-value checks; the authority decision is made by the shell and rechecked at use.
- A stale candidate, an earlier non-integration step, a task status string, a rejection outcome or a bare NATS event cannot authorize a push.
- Authority is scoped to the exact ref and commit pair; it is not reusable for another candidate.

## Acceptance
`make check-go` with `Test_unit_git_integration_authority` and `Test_unit_git_authority_refusals` passing. The refusals test must cover each bullet above as its own case.

## Out of scope
Git transport, the worker journal, repository lifecycle, and pipeline indexing.
