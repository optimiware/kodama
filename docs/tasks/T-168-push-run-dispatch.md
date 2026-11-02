---
id: T-168
milestone: production
status: done
depends_on: [T-167]
decision_blockers: []
owns: [internal/kernel/store/, internal/kernel/api/, internal/kernel/serve/, internal/kernel/bus/, internal/kernel/dispatch/, internal/gitservice/, internal/integrations/gitwebhook/, internal/gitpush/, internal/app/kernel.go, internal/app/kernel_config_test.go, contracts/git/, contracts/events/, docs/contracts/, api/git-pushes.yaml, api/git-triggers.yaml]
check: make check-go PKGS='./internal/kernel/... ./internal/gitservice/... ./internal/integrations/gitwebhook/... ./contracts/git/... ./contracts/events/...'
resources: [go]
acceptance: [unit_kernel_git_outbox_publishes_once, unit_kernel_git_pipeline_index_exact_commit, unit_kernel_git_pipeline_index_rejects_invalid, unit_kernel_git_trigger_pins_commit, unit_kernel_git_trigger_deduplicates, unit_kernel_git_trigger_excludes_origin_run, unit_kernel_git_trigger_stale_follows_push_order, unit_kernel_git_trigger_archive_during_read]
---
# T-168 — Push to pipeline and run dispatch

## Goal
Turn an accepted default-branch push into indexed pipelines and one run pinned to the pushed commit.

## Contract
[Git repositories](../specs/git-repositories.md), [event catalog](../contracts/event-catalog.md), [plan](../implementation/internal-git-service.md), O-36.

## Deliverables
- Outbox publisher over `audit_events` (`published_at`, `retry_count`, `next_attempt_at`) delivering `git.refs_updated` to NATS JetStream with an in-process implementation for `development`; stable event identity, at-least-once delivery, consumer deduplication.
- `.kodama/<processname>.yaml` indexing at the pushed commit: strict parse and validate, upsert into `pipelines` with `source: git` and `source_commit`, disable on removal, and never displace a valid definition with an invalid one.
- Trigger matching from the pipeline document's `triggers.git` (branches, tags, events) and run admission through `runs.trigger_key = (pipeline, event, ref)`.
- Pinning: a push-originated run stores the pushed commit in `runs.sources`; a redelivered event admits nothing new; a run's own push is excluded by `originRunId`.
- Scheduler wake on admission so the kernel runner picks the run up without polling delay.

## Acceptance
Declared check. T-170 smokes the assembled push-to-run path against real PostgreSQL and Git.

## Out of scope
Checkout of pinned sources (T-169), task-transition triggers, ChatOps, external webhooks.

## Completion evidence
Implemented by a subagent; independent review requested changes on stale-push ordering and archive-during-read, and both were fixed. Commits `ef3ee1a2`, `f6d4bdfb`, `d159d8c8`, `0e5cd049`, `897c0797`, `4f17a0a8`. Coordinator checks on `4f17a0a8`: `make check-task TASK=T-168` PASS (8 acceptance names); `make docs-check` PASS; `make validate-tasks` PASS. Merged into `integration/native-git` as `72097cec`; nothing pushed.

Decision: staleness between default-branch pushes follows the receive's journal position, not report time. An event without a position is indexed as it arrives (only possible when the worker could not read its journal).
