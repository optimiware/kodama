---
id: T-130
milestone: production
status: done
depends_on: [T-127, T-128, T-136]
decision_blockers: []
owns: [internal/gitpush/, api/git-pushes.yaml, contracts/events/, docs/contracts/event-catalog.md, docs/contracts/git-service.md, contracts/git/README.md]
check: make check-go PKGS='./internal/gitpush/...'
resources: [go]
acceptance: [unit_gitpush_authorize_records_before_update, unit_gitpush_ref_policy_matrix, unit_gitpush_default_branch_denied_by_default, unit_gitpush_seed_consumed_once, unit_gitpush_seed_single_ref_only, unit_gitpush_concurrent_seeds_one_wins, unit_gitpush_unknown_seed_blocks_new_seed, unit_gitpush_current_authority, unit_gitpush_commit_intent_refused_after_settlement, unit_gitpush_identical_tuples_attributed_by_receive, unit_gitpush_per_ref_settlement, unit_gitpush_reconcile_from_ref_journal, unit_gitpush_aba_settles_refused, unit_gitpush_outcome_emits_one_event, unit_gitpush_unknown_blocks_writer, unit_gitpush_memory_contract]
---
# T-130 — Push authorization, journal and events

## Goal
Authorize every ref update, journal every receive and publish applied pushes (GR-07, GR-09–GR-11, GR-18, GR-19).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), [push events](../specs/git-repositories.md#push-events), seams S4. Migration `009_git_pushes.sql`. No network call while a SQL lock is held: Git facts (fast-forward, emptiness, ancestry) come from the worker's request.

## Deliverables
- Push-record entity with per-ref entries; the ref-policy matrix including default-deny, seed, integration and run-scoped agent branches; authorization against grants and current authority (through T-136 ports).
- Commit intent, outcome confirmation committing audit and one `git.refs_updated` outbox event, and the reconciler applying the evidence rules (never refs, reflog or Pod deletion alone).
- Writer-reassignment blocking port for `unknown` entries; push-record reads and `api/git-pushes.yaml`.
- PostgreSQL + memory adapters with a shared contract suite.

## Acceptance
Declared check; real SQL and Git under `make check-go-integration DEV_SCOPE=t130 DEV_STACK=services PKGS='./internal/gitpush/...'` (GR-A04–A06, GR-A13, GR-A14/A15 policy side).

## Out of scope
Pipeline triggers (T-137), attempt grants (T-150).

## Completion evidence
Implemented by a subagent. The first coordinator real-SQL run failed in the test fixture (a parameter used with two types). Independent review requested changes four times (cancellation not fenced at commit intent; inconsistent journal/ref snapshots and unchecked regressions; transport failures settling `unknown`; stale repository facts at authorization; event resource identity/version mismatch; missing real-Git coverage; unvalidated journal heads; deadline checked before lock waits; regression latch not covering earlier entries) and approved `c3e583a5`. Coordinator decision: `git.refs_updated` names the push record (`git_push`, push ID, record version) as its aggregate. The real-Git tests found a worker defect (journaling `committed` for refs Git rejected), contained here and fixed in T-159. Coordinator checks: `make check-task TASK=T-130` PASS (16 acceptance names); real SQL `make check-go-integration DEV_SCOPE=t130 DEV_STACK=services PKGS='./internal/gitpush/...'` PASS including `internal/gitpush/integration`. For T-150: revoke attempt grants in the cancelling transaction. For T-132: register migration `009`, adapters for Repositories/Attempts/Reservations/ReadAuthorizer, map only the worker's `journal_unreadable` to `ErrJournalUnreadable`, pass `UnsettledPush` to gitauth. For T-138: an operator action to resolve `unknown` entries.
