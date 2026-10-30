---
id: T-130
milestone: production
status: planned
depends_on: [T-127, T-128]
decision_blockers: []
owns: [internal/gitpush/, api/git-pushes.yaml, internal/integrations/gitwebhook/, internal/storage/migrations/, internal/storage/manifest.go, internal/storage/manifest_test.go]
check: make check-go PKGS='./internal/gitpush/... ./internal/integrations/gitwebhook/... ./internal/storage/...'
resources: [go]
acceptance: [unit_gitpush_authorize_records_before_update, unit_gitpush_commit_intent_refused_after_settlement, unit_gitpush_identical_tuples_attributed_by_receive, unit_gitpush_per_ref_settlement, unit_gitpush_ref_policy_matrix, unit_gitpush_current_authority, unit_gitpush_outcome_emits_one_event, unit_gitpush_reconcile_from_ref_journal, unit_gitpush_unknown_blocks_writer, unit_trigger_dedup_by_push_and_ref, unit_trigger_excludes_origin_run, unit_trigger_exact_pushed_commit]
---
# T-130 — Push journal, events and triggers

## Goal
Authorize every ref update, journal every receive operation and turn applied pushes into pipeline runs (GR-07, GR-09, GR-10, GR-11).

## Contract
ADR 0011 §4–§5, [push journal](../specs/git-repositories.md#push-journal-and-events), [worker interface](../contracts/git-service.md#worker-interface), seams S4/S7.

## Deliverables
- `internal/gitpush`: push-record entity, the ref-policy matrix, authorization against grants and current authority, outcome confirmation committing audit and one `git.refs_updated` outbox event, commit-intent handling, and a reconciler settling unconfirmed entries from the ref journal and receive liveness under the specification's evidence rules (never from refs, reflog or Pod deletion alone).
- One appended migration contribution for push records.
- Trigger admission from `git.refs_updated` reusing gitwebhook's claim/lease/refusal ledger, deduplicated by `(trigger, push, ref)`, pinning the pushed commit, filtering by repository, ref pattern and origin. The external HMAC endpoint is unchanged.

## Acceptance
Declared check with every named test passing; real SQL and Git under `make check-go-integration DEV_SCOPE=t130 DEV_STACK=services PKGS='./internal/gitpush/...'` (GR-A06, GR-A13, GR-A07 trigger side).

## Out of scope
Attempt grant issuance (T-131), routes (T-132).
