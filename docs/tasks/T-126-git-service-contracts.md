---
id: T-126
milestone: production
status: done
depends_on: [T-135]
decision_blockers: []
owns: [contracts/git/, contracts/events/, docs/contracts/git-service.md, docs/contracts/event-catalog.md, docs/specs/git-repositories.md]
check: make check-go PKGS='./contracts/git/... ./contracts/events/...'
resources: [go]
acceptance: [unit_git_contract_repository_fixtures, unit_git_contract_grant_fixtures, unit_git_contract_seed_grant, unit_git_contract_forwarding_assertion, unit_git_contract_worker_rpc, unit_git_contract_push_authorization, unit_git_contract_commit_intent, unit_git_contract_ref_journal, unit_git_contract_source_set, unit_git_contract_trigger_config, unit_events_git_types_schema]
---
# T-126 — Internal Git service: contract checkpoint

## Goal
Freeze the executable wire shapes and the consumer-owned port signatures that T-127–T-157 implement in parallel.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S1–S10 of the plan. Additive only: existing event envelopes, fixtures and digests stay unchanged.

## Deliverables
- `contracts/git/v1`: JSON Schemas, Go DTOs, canonical fixtures and validation for the repository resource and policy (omitted `directPushToDefault` → `false`), Git grant claims (human, seed, preparation, attempt, integration), seed request/refusals, forwarding assertion, worker identity, worker RPC in both directions (push authorization, commit intent, outcome, initialized, assignments; receive liveness, ref and journal reads, initialize, retained refs, maintenance), push record and per-ref entry states, ref journal entry (`observed`), stable reason codes, source set (ordering, unique safe relative paths, per-repository access) and trigger configuration (repository, ref patterns, origin filter, definition).
- `contracts/git/README.md`: the Go port signatures each consumer will own, with method-level failure semantics: `repository.Hosts` (S2), `gitauth` grant issuance/verification (S5), `gitpush` authorization/intent/outcome (S4), `gitservice` control-plane client (S4), worker discovery (S3), source-set admission (S6), trigger admission (S7), retained refs/backup (S9), product MCP delegation (S10). Branch namespace for runs without a work item: `refs/heads/kodama/runs/<run-id>/<label>`.
- `contracts/events/v1`: additive `repository.created`, `repository.state_changed`, `git.refs_updated` with fixtures.
- Update the Markdown contract to point at the executable schemas.

## Acceptance
Declared check with every named test passing, including refusals (unknown fields, foreign scope, malformed refs/commits/paths, path traversal, duplicate source paths, seed for a non-empty repository).

## Out of scope
Persistence, processes, routes.

## Completion evidence
Implemented by a subagent; independent review requested changes twice (settlement guards accepting outcomes from a regressed journal, no worker branch creation for task branches, no exact-commit input for push-originated admission, seed eligibility ignoring non-seed unsettled pushes, tenant-wide human grants; then missing persisted per-ref kind after `git.refs_updated` became reference-only) and approved `5916793a`. The coordinator updated the event-catalog row and the trigger sentence for the reference-only event. Coordinator checks: `make check-task TASK=T-126` PASS (11 acceptance names), `make docs-check` and `make validate-tasks` PASS.
