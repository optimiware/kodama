---
id: T-131
milestone: production
status: planned
depends_on: [T-113, T-130]
decision_blockers: []
owns: [internal/execution/, internal/workspace/, internal/runtimeauth/, internal/entrypoint/, internal/results/, internal/gates/, internal/artifacts/, internal/delivery/, internal/pipeline/, contracts/pipeline/, contracts/runtime/, internal/storage/migrations/, internal/storage/manifest.go, internal/storage/manifest_test.go, api/runs.yaml, api/gates.yaml, api/delivery.yaml, api/artifacts.yaml, api/workspace.yaml]
check: make check-go PKGS='./internal/execution/... ./internal/workspace/... ./internal/runtimeauth/... ./internal/entrypoint/... ./internal/results/... ./internal/gates/... ./internal/artifacts/... ./internal/delivery/... ./internal/pipeline/... ./contracts/...'
resources: [go]
acceptance: [unit_admission_pins_source_set, unit_admission_push_origin_without_work_item, unit_admission_provisioning_run, unit_attempt_git_grant_scoped_and_revoked, unit_attempt_grant_agent_namespace_only_without_writer, unit_entrypoint_checks_out_source_set, unit_entrypoint_pushes_agent_branch, unit_result_requires_confirmed_push, unit_integration_grant_bound_to_candidate, unit_evidence_identifies_source_set, unit_deployment_provenance_source_set, unit_external_repositories_unchanged]
---
# T-131 — Task, agent and source integration

## Goal
Let runs pin several repositories, let attempts push their own branches to internal repositories, and integrate approved candidates (WS-17–WS-19, PX-23–PX-25, IT-17–IT-19, DQ-17, AR-15, GR-06, GR-12).

## Contract
ADR 0011 §6–§8, [source set](../contracts/git-service.md#source-set), seams S5/S6, T-113's merged runtime v2 authority. External repositories keep the existing publisher, bundle transport and contexts.

## Deliverables
- Run origin (`work_item`, `push`, `manual`, `provision`) and source-set pinning at admission; provisioning runs before a repository exists; push-originated runs without a work item.
- A versioned pipeline-format and runtime-context addition carrying the source set; existing documents, contexts, digests and result hashes unchanged.
- Attempt Git grants issued at admission from declared access, delivered like the attempt token, revoked with the attempt; entrypoint checkout of every source and explicit pushes; result finalization requiring the confirmed push record for each declared commit output.
- Attempt grants limited to the run's agent-branch namespace, with the task branch only for the reserved writer.
- Integration grants bound to an approved gate subject naming candidate and base; gate subjects, artifact provenance and deployment records identify the source set, and delivery rejects a deployment whose provenance mismatches any of its repositories.
- Module OpenAPI fragments for runs, gates, delivery, artifacts and workspaces carry the new optional fields; T-132 regenerates the aggregate and client.

## Acceptance
Declared check with every named test passing; local checkpoint with real PostgreSQL, Git worker and Podman for two agents integrating one task (GR-A08 local side).

## Out of scope
Routes, CLI, MCP and chart (T-132); stage acceptance (T-133).
