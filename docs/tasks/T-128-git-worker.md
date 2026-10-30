---
id: T-128
milestone: production
status: planned
depends_on: [T-126]
decision_blockers: []
owns: [internal/gitservice/]
check: make check-go PKGS='./internal/gitservice/...'
resources: [go]
acceptance: [unit_gitservice_smart_http_only, unit_gitservice_forwarding_assertion_required, unit_gitservice_path_from_id_only, unit_gitservice_streamed_size_limits, unit_gitservice_hooks_not_overridable, unit_gitservice_ref_journal_attributes_receive, unit_gitservice_prepared_hook_aborts_without_intent, unit_gitservice_receive_deadline_enforced, unit_gitservice_initialize_idempotent, unit_gitservice_refs_and_journal_reads, unit_gitservice_maintenance_serialized]
---
# T-128 — Native Git worker

## Goal
Implement the tenant Git worker around native Git (GR-03, GR-05, GR-08, GR-16): the package behind the proposed `kodama git serve` role.

## Contract
ADR 0011 §2–§4, [Git service contract](../contracts/git-service.md#git-endpoint), seams S2/S3, T-126 DTOs. Start from `dev/gitserver/main.go`.

## Deliverables
- Smart-HTTP handler around `git http-backend`: default-deny endpoints, protocol v2, forwarding-assertion verification per tenant audience, repository paths resolved from IDs only, sanitized environment, streamed bodies with bounded size and duration.
- Server-owned hooks implemented by the Kodama binary (`pre-receive` authorize; `reference-transaction` writing the fsynced, receive-attributed ref journal with the ref's `observed` value under lock and requesting commit intent at `prepared`, aborting on failure; `post-receive` confirm) behind a small client port to the control plane; receive deadlines enforced by terminating overrunning receives; a receive-liveness and journal read for reconciliation; system configuration that ignores repository-local hooks, filters and credential helpers.
- `Hosts` operations: initialize, read refs, read the ref journal and receive liveness, manage retained refs, run maintenance, serialized per repository with pushes.

## Acceptance
Declared check with every named test passing against real `git` in temporary directories (GR-A05 server side, GR-A11).

## Out of scope
Push records and events (T-130), Kubernetes objects (T-129), command registration (T-132).
