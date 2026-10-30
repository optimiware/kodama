---
id: T-169
milestone: kernel
status: done
depends_on: [T-168]
decision_blockers: []
owns: [internal/kernel/runner/, internal/kernel/serve/, internal/kernel/store/, internal/kernel/api/, internal/storage/migrations/003_git_hosting.sql]
check: make check-go PKGS='./internal/kernel/...'
resources: [go]
acceptance: [unit_kernel_run_checkout_pinned_commit, unit_kernel_run_checkout_context_repos, unit_kernel_run_git_credential_outside_workspace, unit_kernel_run_git_credential_revoked]
---
# T-169 — Kernel run Git sources and credentials

## Goal
A kernel run starts from its pinned sources and its steps can use ordinary Git with a run-scoped credential.

## Contract
[Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md#source-set), [workspaces](../specs/workspaces.md), O-36.

## Deliverables
- Workspace preparation checks out each pinned source at its recorded commit, honoring `path` and `access`; a single source at `.` keeps today's layout.
- Optional tenant and project `context` repositories under `/workspace/.context/`, read with the run's own read-only credential (never writable, even when pinned); a context that exists and cannot be read fails the run with `source_unavailable`.
- Run Git credential materialized outside `/workspace`, exported to steps as the documented environment and Git credential helper configuration.
- Credential lifetime tied to the run: rewritten on rotation and removed when the run ends; a cancelled or finished run's credential is refused.
- Pinned commit is never re-resolved from a moved branch; a missing commit fails the run with a stable reason.

## Acceptance
Declared check.

## Out of scope
Multi-writer source sets, service containers, Kubernetes preparation, managed-agent checkout.

## Completion evidence
Implemented by a subagent; independent review requested changes on transient-error revocation, cancellation latency, context symlink handling and non-ready context repositories; all were fixed. Commits `7c68bb10`, `3c0b7c22`, `3b38906d`. Coordinator checks on `3b38906d`: `make check-task TASK=T-169` PASS (4 acceptance names); `make docs-check` PASS; `make validate-tasks` PASS. Real SQL and the tenant-level context credential were verified against a throwaway PostgreSQL 17.11 instance. Merged into `integration/native-git` as `f7e0f6f1`; nothing pushed.

Decision: an archived context repository is still checked out (archive refuses pushes, not history). WS-03 records plainly that a step receives a usable credential and can copy it; the guarantee is that it is outside `/workspace`, absent from `.git/config`, remote URLs and the step environment, and removed when the run ends.
