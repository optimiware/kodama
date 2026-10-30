---
id: T-147
milestone: production
status: done
depends_on: [T-127, T-130, T-131, T-136]
decision_blockers: []
owns: [internal/workspace/, api/workspace.yaml]
check: make check-go PKGS='./internal/workspace/...'
resources: [go]
acceptance: [unit_workspace_repository_associations, unit_workspace_task_branch_provisioned_idempotent, unit_workspace_single_writer_per_item, unit_workspace_internal_checkpoint_requires_applied_push, unit_workspace_unknown_push_blocks_reassignment, unit_workspace_retained_pins_internal, unit_workspace_prior_disclosure_scan_on_push, unit_workspace_external_publisher_unchanged]
---
# T-147 — Workspace repository associations and internal checkpoints

## Goal
Workspaces hold task branches per repository and accept internal checkpoints from confirmed pushes (WS-17–WS-19).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), [workspaces](../specs/workspaces.md#internally-hosted-repositories). Migration `010_workspace_repositories.sql`. The one-workspace-per-work-item rule and writer reservation stay; external repositories keep the publisher.

## Deliverables
- Per-repository associations (task branch, accepted commit); idempotent task-branch creation through the worker; writer reservation unchanged.
- Internal checkpoint acceptance from applied push records; `unknown` blocks reassignment; retained pins on the internal host.
- Prior-disclosure scanning (existing `disclosures.go`) applied to internal pushes before a commit output is accepted.
- API fragment updates.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t147 DEV_STACK=services PKGS='./internal/workspace/...'` (WS-A13–A15).

## Out of scope
Execution admission (T-148).

## Completion evidence
Implemented by a subagent; independent review requested changes twice (synchronization adopting unproven fast-forwards, archive and fence not closing writer authority, stale gate results at admission, concurrent initial provisioning losing repositories, non-atomic internal sync; then a wrong gitpush obligation in the documentation and minors) and approved `b9d7eb3b`. Coordinator checks: `make check-task TASK=T-147` PASS (8 acceptance names); real SQL `make check-go-integration DEV_SCOPE=t147 DEV_STACK=services PKGS='./internal/workspace/...'` PASS. `make api-check` is expected to fail until T-158 regenerates the aggregate (`api/workspace.yaml` changed).

For T-132: `*gitpush.Service` must satisfy `workspace.RefPushes` (`UnsettledOnRef`, `AppliedOnRef`; added by T-138) and `HoldsWriter` (gitpush rechecks it after its `authorized` record commits, T-138); register migration `010` after `009`; compose the five optional ports (`Hosts`, `InternalRepositories`, `PushRecords`, `WriterGate`, `PushInspector`, `RefPushes`); a missing port fails closed with `ErrNotConfigured`. For T-148: use `ReserveInternalWriter` (or `CheckWriterGateAt` plus `ReserveInternalWriterMutation(in, observed, now)`); accept checkpoints before `SettleWriter`; expect `ErrCheckpointRequired`, `ErrPushPending`, `ErrWriterBlocked`. For T-150: a cancelled writer's unsettled pushes hold reassignment and archive until settled or operator-resolved. Open minors: `finishArchive` retry exhaustion returns `ErrStale` instead of `ErrArchiveUncertain`; a gitpush `not_found` from `RefPushes` surfaces as 404 on admission (503/500 is more apt).
