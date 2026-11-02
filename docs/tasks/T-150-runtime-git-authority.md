---
id: T-150
milestone: production
status: done
depends_on: [T-130, T-136, T-148, T-149]
decision_blockers: []
owns: [internal/runtimeauth/, internal/results/]
check: make check-go PKGS='./internal/runtimeauth/... ./internal/results/...'
resources: [go]
acceptance: [unit_runtime_routes_new_version, unit_attempt_git_credential_delivered_and_renewed, unit_attempt_git_credential_revoked_on_cancel, unit_result_requires_applied_push_record, unit_result_receipt_first_replay, unit_result_external_publication_unchanged]
---
# T-150 — Runtime Git authority and result finalization

## Goal
Attempts receive and renew their Git credential; results accept commit outputs only from applied push records (WS-19, GR-06).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), T-131 runtime contract, T-113 v2 authority. Migration `014_attempt_git.sql` only if needed.

## Deliverables
- Versioned runtime routes and admission delivering the attempt Git credential; renewal bounded by attempt liveness; revocation with cancellation.
- Result finalization verifying per-repository commit outputs against applied push records, receipt-first replay unchanged; external publication path unchanged.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t150 DEV_STACK=services PKGS='./internal/runtimeauth/... ./internal/results/...'`.

## Out of scope
Entrypoint (T-151), adapters (T-152).

## Completion evidence
Implemented by a subagent; independent review approved the first candidate (`d3faf2f2`) with no blocking findings, after a coordinator check that included real SQL. Coordinator checks: `make check-task TASK=T-150` PASS (6 acceptance names); `go test -race ./internal/runtimeauth/... ./internal/results/...` PASS; real SQL `make check-go-integration DEV_SCOPE=t150 DEV_STACK=services PKGS='./internal/runtimeauth/... ./internal/results/...'` PASS (13 packages). The results PostgreSQL contract manifest was repaired (it omitted contributions that the execution fixture applies).

Coordinator decisions on the reviewer's open points:
1. The integration grant is bound to the run's approved gate, not to a step (the pipeline format has no integration-step marker); blast radius is the one approved update. Accepted.
2. The Git grant routes ride `context.read` (closed action list in the database). Not an escalation (the capability comes from the pinned context and the gate). A `git.grant` action can be added in a later migration if per-step control is needed.
3. `already_integrated` is `409 publication_conflict` with `reason: already_integrated` and `observedTip` equal to the candidate; the entrypoint (T-151 follow-up, not yet implemented) treats it as the step completed with no commit output. Recorded in `docs/contracts/runtime-v3.md`.
4. Seed grants are one per provisioning attempt (gitauth keys `seed:<attempt>`); a second repository gets 403. Follow-up for `internal/gitauth` if a provisioning run may create several repositories; otherwise documented as a limit.
5. A refused or missing push in a result is 422 `output_invalid` with no durable failure; the honest entrypoint submits a `failed` result. A fabricated push ID holds the writer until the run deadline, as a hung workload already can. If a durable failure is wanted, add a `Refused` flag to `execution.ResultCommitV3` feeding `failFinalization(ReasonOutputInvalid)` (execution).

For T-132 (wiring, from the implementation): register `postgres.AttemptGitMigrations()` at slot 014 after 011 and 013; build `runtimeauth.Config.Git` from `gitgrants.NewIssuer`, `gitgrants.NewApprovals`, `postgres.NewIssuedGrants` and a repository adapter whose `CreatedByRun` reads the creating run and attempt that productmcp's `Repositories.Create` must record in the repository reservation's transaction (nothing records them today, so no seed can be issued until it exists; it fails closed); set `gitauth.Config{Attempts: Leases, Gates: GateDecisions}`, `gitpush.Dependencies.Attempts` to the same `Leases`, and `execution.Dependencies.GitGrants` to `gitgrants.NewGrants` over the execution store and its `Peers.GitGrants` revoker; mount `runtimehttp.NewV3Handler` (`Begins: execbridge.BeginsV3`, `Receipts: execbridge.ReceiptsV3`, `Shared`: the v1 secrets and artifacts handlers, `Direct{"submitResult"}`: `resultshttp.NewV3Handler`) and wrap the v1 and v2 handlers with their `WireGuard`; set `results.Dependencies.V3` with `adapters.NewExecutionV3` over the guarded execution store, `adapters.NewPushes` over `gitpush.Service.Reload`, `adapters.NewIssued` over the runtimeauth service; return `Provenance{SourceSet}` from the artifact authority for source-set attempts. For T-152: write the credential file from `AttemptSpec.Git`, rotate it with `Grants.RenewAttempt` (a rotation invalidates the old secret at once: rewrite the file atomically), issue the preparation grant (`IssuePreparation`, no caller yet) before the preparation workload starts. For T-133: cover the cancel/issue race over real SQL and a seed followed by a push-record-verified result; the v3 result path through the guarded store has no real-SQL test yet. Minors: `GateDecision.AttemptID` carries the requesting attempt (the gate-to-attempt binding exists only at run level); `Leases.lease` loads the full aggregate on every verify and push hook (a narrower read port can follow); the immutability trigger blocks DELETE for every role, so tenant offboarding needs the retention task to handle `runtime_git_grants`.
