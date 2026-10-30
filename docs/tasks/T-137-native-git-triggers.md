---
id: T-137
milestone: production
status: done
depends_on: [T-130, T-148]
decision_blockers: []
owns: [internal/integrations/gitwebhook/, api/git-triggers.yaml]
check: make check-go PKGS='./internal/integrations/gitwebhook/...'
resources: [go]
acceptance: [unit_trigger_dedup_by_push_and_ref, unit_trigger_excludes_origin_run, unit_trigger_exact_pushed_commit, unit_trigger_without_work_item, unit_trigger_repository_and_ref_filters, unit_trigger_configuration_authorized, unit_trigger_task_ready_admission, unit_trigger_external_hmac_unchanged]
---
# T-137 — Native Git triggers and task routing

## Goal
Start pipelines from applied pushes and ready tasks (GR-10, GR-11, PX-24).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seam S7. Reuse the existing claim/lease/refusal ledger; keep the external HMAC endpoint unchanged. Migration `012_git_triggers.sql` if the trigger table changes.

## Deliverables
- Consumer of `git.refs_updated` with repository, ref-pattern and origin filters, deduplicated by `(trigger, push, ref)`, admitting exact-commit runs through T-148's push origin.
- Task routing: a project's configured pipeline admits a `work_item` run when an item becomes ready, subject to current authority, WIP, quotas and idempotency.
- Trigger configuration use cases and `api/git-triggers.yaml`.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t137 DEV_STACK=services PKGS='./internal/integrations/gitwebhook/...'` (GR-A07, PX-A20).

## Out of scope
Provider webhooks (O-32).

## Completion evidence
Implemented by a subagent; independent review approved the first candidate with minor findings, which were fixed and re-approved (`173d3b56`): PATCH 409 in the API fragment; a lease-takeover conflict classified as retryable; non-`started` outcomes recorded only by the lease holder; stable refusal codes in the inbox with status reads; `enabledAt` so late redelivery cannot start runs for earlier pushes; retention documented; memory/PostgreSQL finish validation parity. Coordinator checks after rebasing onto integration: `make check-task TASK=T-137` PASS (8 acceptance names); `go test -race ./internal/integrations/gitwebhook/...` PASS; real SQL `make check-go-integration DEV_SCOPE=t137 DEV_STACK=services PKGS='./internal/integrations/gitwebhook/...'` PASS.

For T-132: register `postgres.NativeMigrations()` after 007 and 011 (migration `012`); subscribe durable consumers (`git.refs_updated` to `PushConsumer.HandleRefsUpdated`, `work_item.transitioned`/`work_item.created` to `TaskRouter.HandleWorkItemEvent`; ack on nil/`ErrInvalid`/`ErrNotFound`, redeliver with backoff on `ErrUnavailable`/`ErrAdmissionBusy`); unique `Holder` per process; implement the `Authorizer` (`git_trigger.manage`, `git_trigger.view`); wire `execution.Service`, `gitpush.Service`, `repository.Store` and the work repository into the adapters; schedule inbox cleanup (owner-role maintenance, horizon longer than the redelivery window). For T-158: list `api/git-triggers.yaml` (including the two status reads and `enabledAt`) in the aggregate and regenerate the client; error mapping: denied 403, not found 404, stale 412, hosting mismatch 422, archived 409. Open minors: `refusalCode` should extract the code only when the sentinel prefix was actually trimmed (add a wrapped-refusal test); `enabled_at` could be made non-decreasing in the migration guard.
