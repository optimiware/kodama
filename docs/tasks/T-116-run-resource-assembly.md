---
id: T-116
milestone: production
status: done
depends_on: [T-113, T-114, T-115, T-120]
decision_blockers: []
owns: [internal/app/, internal/cli/, api/pipeline.yaml, api/runtime.yaml, api/execution.yaml, api/openapi.yaml, api/client/, deploy/helm/kodama/, go.mod, go.sum, docs/operations/, docs/implementation/assembly-status.md]
check: make check-go PKGS='./internal/app/... ./internal/cli/... ./api/client/... ./deploy/helm/kodama/...' && make api-check && make test-m1 test-m2 test-m3
resources: [go]
acceptance: [unit_app_run_shared_composition, unit_app_run_shared_refuses_unsupported_backends, unit_app_execution_policy_settings, unit_app_runtime_v2_routes_served, unit_app_prepare_and_service_run_commands, unit_chart_run_shared_render, unit_cli_run_resources_view]
---
# T-116 — P6: resource assembly, chart and migration

## Goal
Implement [P6](../implementation/run-workspace-services-builders.md#p6--assembly-chart-and-migration) as the sole shared-file integration owner, bridging seams S5, S6, S7 and S9 of [T-122](T-122-o30-scope-amendment.md).

## Contract
ADR 0010 as amended by O-30. Production refuses Process/Podman/memory/keyfile fallbacks. Keep old records/hashes; `legacyFormat` stays `open` until T-117 accepts, then `drain`. An older controller refuses new-format runs. Never advertise unqualified combinations.

## Deliverables
- Wire `execution.SharedConfig` (Kubernetes `RunResources` + policy source), runtimeauth resource subjects, `/runtime/v2` routes, resource logs and the `prepare`/`service-run` commands per environment; bridge the adapter-local ports to runtimeauth.
- Chart: execution policy settings (append-only list, `admission.runShared`/`legacyFormat`), the stage policy (tenant isolation, public egress, step role with the T-120 profiles, storage class), tenant namespaces at Pod Security `baseline` with a pinned version, preparation image digest, quotas including PVC storage. Preflight rejects a missing profile installation instead of failing inside Buildah.
- Integrate the v2 API fragments and regenerate the aggregate/client; `kodama run get` shows `runModel`, deadline, order and resources; `kodama run logs --resource`.
- Operations docs: enabling `run_shared`, the stage policy, cutover/drain/rollback and uninstall.

## Acceptance
Declared check with every named test passing, plus `make test-chart`. Wiring is not deployment evidence: T-117 owns stage acceptance.

## Out of scope
Cluster changes (T-120/T-121/T-117), other deferred release work.

## Completion evidence
Implemented on `task/T-116-run-resource-assembly` (`902a88a7`…`25661769`, with main `2ff20310` merged in). Owner decision during the task: enabling `run_shared` on stage switches over immediately (`legacyFormat: drain` at enablement); one Pod Security level per namespace means `attempt_private` dispatch stops in the `baseline` tenant namespaces. Integration-owner exceptions covered a separate `run_shared` step identity in `internal/kube`, v2 context decoding/fingerprint/registration in `internal/kube`, version-aware result/artifact/secret collaborators, additive artifact workspace provenance (migration `005`) and its public exposure. Two independent reviews: chart/API/CLI (`ses_e7510f83affeo0MC6t7Zd9NYTR`) requested a log-redaction reload, single-snapshot run views and a rollback correction, then approved `ac899faf`; composition (`ses_e75112159ffeN5QVb5YJkymNeu`) found that v2 attempts could not launch on Kubernetes or use results/artifacts/secrets, missing workspace provenance and shallow policy copies, then approved `25661769`. Coordinator checks on the final candidate: `make check-go` over app/cli/client/chart/kube/results/artifacts/execution/gates/delivery exit 0 with all seven pinned acceptances plus the v2 round trips PASS; `make api-check`, `make test-m1 test-m2 test-m3`, `make test-chart`, `make docs-check` exit 0; real SQL (app, artifacts, results, execution/postgres) exit 0 including provenance, resource authority and the additive migration. Kubernetes/production only: Podman/Process wiring moved to T-134 after T-115. Agent, QA and delivery context readers still decode only v1 (fail closed for v2). Composed, not qualified: T-121/T-117. Squashed to one commit; nothing pushed.
