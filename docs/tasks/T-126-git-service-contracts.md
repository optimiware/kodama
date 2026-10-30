---
id: T-126
milestone: production
status: planned
depends_on: [T-125]
decision_blockers: []
owns: [contracts/git/, contracts/events/]
check: make check-go PKGS='./contracts/git/... ./contracts/events/...'
resources: [go]
acceptance: [unit_git_contract_repository_fixtures, unit_git_contract_grant_fixtures, unit_git_contract_forwarding_assertion, unit_git_contract_push_authorization, unit_git_contract_source_set, unit_events_refs_updated_schema]
---
# T-126 — Internal Git service: contract checkpoint

## Goal
Freeze the executable shapes of the [Git service contract](../contracts/git-service.md) so that T-127–T-131 implement against one model.

## Contract
ADR 0011, GR-01–GR-17, seams S1–S7 of the [implementation plan](../implementation/internal-git-service.md#seams).

## Deliverables
- `contracts/git/v1`: JSON Schemas, Go DTOs and canonical fixtures for the repository resource, Git grant claims (human, attempt, seed, integration), forwarding assertion claims, worker identity, push authorization request/response with stable reason codes, push outcome, push record and source set.
- `contracts/events/v1`: additive `repository.created`, `repository.state_changed` and `git.refs_updated` types; existing envelopes, fixtures and digests unchanged.
- Validation tests for every fixture, including refusals (unknown fields, foreign scope, malformed refs and commits, duplicate source paths).

## Acceptance
Declared check with every named test passing.

## Out of scope
Persistence, Git processes, HTTP routes and runtime context versioning (T-131).
