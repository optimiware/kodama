---
id: T-138
milestone: production
status: done
depends_on: [T-128, T-129, T-130, T-160]
decision_blockers: []
owns: [internal/gitops/, internal/gitpush/]
check: make check-go PKGS='./internal/gitops/... ./internal/gitpush/...'
resources: [go]
acceptance: [unit_gitops_maintenance_scheduled, unit_gitops_prune_keeps_pinned, unit_gitops_backup_bundles, unit_gitops_restore_closed_until_consistent, unit_gitops_restore_reconciles_journal, unit_gitops_offboarding_after_retention, unit_gitpush_operator_resolves_unknown, unit_gitpush_unsettled_on_ref, unit_gitpush_applied_on_ref, unit_gitpush_authorize_rechecks_writer_after_commit]
---
# T-138 — Git maintenance, backup, restore and offboarding

## Goal
Executable GR-13–GR-15 and tenant offboarding for Git storage.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S2/S9. Migrations `015_git_operations.sql` (gitpush: the operator actions, the per-ref entry index) and `016_git_recovery.sql` (gitops: backup runs and leases, recovery state, discrepancy resolutions, prune intents). Git storage is never deleted by run cleanup. The worker operations a restore and a backup need (restore fence, bundle replay and creation, ancestry) are T-160's contract; gitops depends on their semantics through ports.

## Deliverables
- Scheduled maintenance through the worker; retained-pin-aware pruning of merged task/agent/run branches.
- Per-repository bundle backups to object storage coordinated with volume snapshots; restore that keeps Git access closed until refs, push records and pins reconcile (restored-older volume → `unknown`).
- Retention-gated tenant offboarding: revoke grants, settle pushes, delete storage only after retention.
- Operator resolution of `unknown` push entries (left by T-130): an audited administrative action recording `applied` or `refused` with a reason after the operator inspected the repository, emitting the event for a resolved `applied` record exactly once, unblocking the writer and seeds; a new migration (`015`) relaxing T-130's terminal-`unknown` trigger only for this action.
- Per-ref read queries of the push records for the workspace module (T-147's `RefPushes`): `gitpush.Service.UnsettledOnRef` and `AppliedOnRef`, on the memory and PostgreSQL stores through the shared Contract suite.
- Writer reservation rechecked after the authorized record commits (found by T-147's review): `Authorize` reads `Reservations.HoldsWriter` again once the record is durable and before the verdicts are returned, and settles the record refused when it was lost; `CommitIntent` reads it once more for new intent on task-branch updates.
- Backups, pruning and maintenance never run while the tenant has an active recovery or closed Git access; pruners are serialized across instances by owner-held prune intents; a backup's lease is renewed while it streams and its unknown `Complete` outcome never deletes a complete run's objects.
- Durable recovery state in PostgreSQL (`internal/gitops/postgres`, forced RLS, composite keys, guards): backup leases that serialize backups and expiry across instances, the selected recovery source with its completed steps and audited operator resolutions of discrepancies between the recovery point and the volume, and the prune intents that make a hold on a branch and its pruning mutually exclusive.

## Acceptance
Declared check with real `git` and the memory/PostgreSQL stores (GR-A09, GR-A10).

## Out of scope
Product-wide recovery qualification (T-029).

## Completion evidence
Implemented by a subagent; independent review requested changes three times (replay bypassing the worker's hook environment; recovery not durable across restart; discrepancies and expected refs not part of reopening; backup expiry racing concurrent backups; empty repositories skipped on restore; prune not fenced against new holds; a caller-writable transaction id in the migration 015 guard; then backup/prune/maintenance running during a recovery, fail-open prune intents, apply order, lease renewal, a lost expiry-protection test, migration backfill) and approved after round three. Coordinator checks after rebasing onto integration with T-160 and T-147: `make check-task TASK=T-138` PASS (10 acceptance names); `go test -race -count=2 ./internal/gitops/... ./internal/gitpush/... ./internal/workspace/...` PASS; real SQL `make check-go-integration DEV_SCOPE=t138 DEV_STACK=services PKGS='./internal/gitpush/... ./internal/gitops/...'` PASS.

Also delivered: gitpush rechecks `HoldsWriter` after the `authorized` record commits and at commit intent; per-ref queries `UnsettledOnRef`/`AppliedOnRef` (T-147's `RefPushes`, ordered by a persisted per-entry apply number); owner-keyed durable prune intents and backup leases (migration 016).

For T-132: register `015` (gitpush `OperationsMigrations()`) and `016` (gitops); pass the same `AccessFence` and `RecoveryStore` to `Backups` and `Scheduler`; build the gitops PostgreSQL store with `postgres.Options{}` (database clock); wire `Dependencies.Admin`, a `git_push.resolve` admin route rule and `Handler.WithResolver`; satisfy `workspace.RefPushes` with `*gitpush.Service`; `Worker`/`Objects` adapters speak T-160's worker RPCs (restore order: Initialize → enter fence → replay → reconcile → leave fence); `Offboarding` as `gitworker.Config.Retention`; `Grants` adapter calls `gitauth.Service.RevokeAll(ctx, tenant, reason, actor)` until `Active` is 0 after fencing issuance; `Catalog`, `Holds` (T-148/T-149), `ReleaseGate`, `Lifecycle`, `Auditor`, `BackupStore` (+ `List`) and `Snapshots` adapters. For T-158: list `POST …/pushes/{pushId}/resolutions` in `api/git-pushes.yaml`. Minors: expiry defers failed-run object removal while a restore fence is closed; `Leases.Renew` has no per-call timeout.
