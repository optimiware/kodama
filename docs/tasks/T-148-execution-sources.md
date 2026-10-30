---
id: T-148
milestone: production
status: done
depends_on: [T-131, T-147]
decision_blockers: []
owns: [internal/execution/, api/runs.yaml]
check: make check-go PKGS='./internal/execution/...'
resources: [go]
acceptance: [unit_admission_pins_source_set, unit_admission_push_origin_without_work_item, unit_admission_provisioning_run, unit_admission_manual_origin, unit_admission_cleanup_purpose_unchanged, unit_context_carries_source_set, unit_attempt_grant_requested_and_revoked, unit_old_run_models_unchanged]
---
# T-148 — Run origins and source sets in execution

## Goal
Runs pin origins and source sets and request attempt Git grants (PX-23–PX-25).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), S5/S6, the T-131 contracts. Migration `011_run_sources.sql`. One executable attempt per shared run at a time; deployment-cleanup purpose and authorization unchanged.

## Deliverables
- Admission for `work_item`, `push`, `manual`, `provision` origins without fabricated work items; source-set persistence and validation.
- Context construction for the new version; attempt grant request/revocation through a T-136 port, transactional with cancellation.
- `api/runs.yaml` fragment updates.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t148 DEV_STACK=services PKGS='./internal/execution/...'` (PX-A19, PX-A21).

## Out of scope
Runtime adapters (T-152), triggers (T-137).

## Completion evidence
Implemented by a subagent; independent review requested changes once (Git-grant revocation failing open without a revoker; the origin path also accepting `kodama/v1alpha1`/`v1alpha2`; same-`RunID` admissions with different declarations not conflicting; migration 011 loosening constraints for old runs; redrive rotating a running workload's credential; an issuance/cancellation race; missing real-SQL revocation proof) and approved `d963ee19`. Coordinator checks on the rebased branch: `make check-task TASK=T-148` PASS (8 acceptance names); `go test -race ./internal/execution/...` PASS; real SQL `make check-go-integration DEV_SCOPE=t148 DEV_STACK=services PKGS='./internal/execution/...'` PASS. `make api-check` fails until T-158 regenerates the aggregate (also stale since T-147).

For T-132: register `RunSourceMigrations()` after 007 and 010, set `execpg.Options.RunSources`, bind `Peers.GitGrants` (the store refuses without it), compose `SourceSets`, `InternalWorkspaces`, `GitGrants`; wrapper stores must forward `RevokesGitGrants()`. For T-137: `AdmitSourceRun` with `Origin{push}` and a `RunID` derived from trigger, push and ref. For T-149: gate steps in source-set runs are refused until `GateInputs` uses `Run.Sources.Digest` and per-source heads. For T-150: implement `GitGrants` over `gitauth.Service.IssueAttempt` with a lease deadline at least as long as the run deadline, `RevokeAttempt`, deliver `AttemptSpec.Git`, call the `*V3` use cases with the verified `GrantClaim`, pass runtime-issued grants as `ResultCommitV3.Issued`; the post-issue re-read narrows but does not fully close the issuance/cancellation race (use a locking read via `Store.Update`; pushes are still refused by the authorizer's liveness check). For T-152: read `DecodeContextV3`, `PreparationSpec.Sources`, `AttemptSpec.Git`; `Start` must overwrite a partially created credential Secret; `PreparationGrantFor` has no driver yet; two concurrent drives of one attempt can swap credentials after a lapsed claim lease.
