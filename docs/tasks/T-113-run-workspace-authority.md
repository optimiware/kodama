---
id: T-113
milestone: production
status: planned
depends_on: [T-112, T-122]
decision_blockers: []
owns: [internal/entrypoint/, internal/runtime/bootstrap/, internal/runtimeauth/, internal/secrets/, internal/workspace/, internal/results/, internal/logs/, internal/agents/, internal/app/migrate.go, internal/app/migrate_test.go, internal/app/m3_test.go]
check: make check-go PKGS='./internal/entrypoint/... ./internal/runtime/bootstrap/... ./internal/runtimeauth/... ./internal/secrets/... ./internal/workspace/... ./internal/results/... ./internal/logs/... ./internal/agents/... ./internal/app/...'
resources: [go]
acceptance: [unit_entrypoint_v2_wire_selection, unit_entrypoint_v2_verifies_workspace_and_begins_before_command, unit_entrypoint_v2_never_checks_out_or_resets, unit_prepare_checks_out_once_and_records_receipt, unit_prepare_retries_inside_one_workload, unit_prepare_hardening, unit_service_run_resolves_only_pinned_secrets, unit_runtimeauth_resource_admission_and_replay, unit_runtimeauth_resource_audiences_are_disjoint, unit_runtimeauth_resource_revocation, unit_runtime_v2_routes_contract, unit_cumulative_writer_publication, unit_publication_git_hardening, unit_prior_disclosure_redaction]
---
# T-113 — P3: one preparation, scoped authority and cumulative publication

## Goal
Implement [P3](../implementation/run-workspace-services-builders.md#p3--entrypoint-scoped-authority-and-publication) against the frozen seams S1, S2, S4–S7 of [T-122](T-122-o30-scope-amendment.md).

## Contract
ADR 0010 as amended by O-30, [runtime v2](../contracts/runtime-v2.md), [run resources](../contracts/run-resources.md), T-109/T-110/T-112/T-122 checkpoints. Reuse publisher-owned expected-tip CAS and the v1 result hash. Preparation/service identities are not attempts and never acquire result/source/model/tool authority. Execution's committed state (`RecordBegin`, `RecordPreparationReceipt`, `BuildContextV2`, the bound resource UID) is the authority; HTTP claims are evidence.

## Deliverables
- `kodama step` with `KODAMA_RUNTIME_API=v2`: v2 admit/context/token, verify the mounted `/workspace` (source-backed: head, tracked/untracked/ignored state; source-free: writable and present), call `begin` and start the command only after its acknowledgment; no clone, reset or clean; no checkout credential.
- `PrepareCommand()` (`kodama prepare`): resource admission, read-only checkout of the pinned commit with hooks, submodules, filters and credential helpers disabled, bounded internal retries (`maxAttempts`, `timeoutSeconds`), credential removal and `preparation/complete` receipt. `ServiceRunCommand()` (`kodama service-run -- argv`): resource admission, pinned secret resolution, then exec of the service argv with signal forwarding.
- runtimeauth resource subjects: `RegisterResource` (seam S5), resource admission/replay/renewal bound to `(tenant, project, run, key, workloadUID)`, the `ResourceWorkloads` port (S6), revocation on stop/cancel/deadline, and the `kodama-runtime-resource` audience. Persistence in `runtimeauth/postgres` with an append-only module migration contribution joined after the existing ones in `internal/app/migrate.go` (bounded integration-owner exception, as T-112's).
- `/runtime/v2` HTTP routes in `internal/runtimeauth/http` (S7), reusing v1 handlers for source/result/artifacts/secrets; strict contract tests against `contracts/runtime/v2` fixtures, including cross-version and cross-audience refusals.
- Cumulative writers: later writers publish on top of earlier in-run publications with expected-tip CAS; human-push conflict and lost-response reconciliation unchanged. Publication Git ignores workspace config, hooks, filters, helpers and unsafe symlinks/ownership. Source/output scanning includes values disclosed to earlier steps and services of the run.

## Acceptance
Declared check with every named test passing; real-SQL coverage of resource grants under `make check-go-integration DEV_SCOPE=t113 DEV_STACK=services PKGS='./internal/runtimeauth/...'` where services are available. Unit fakes do not qualify Kubernetes.

## Out of scope
Kubernetes/Podman resources, `internal/app` composition beyond the migration join, chart, CLI and API aggregate (T-116).
