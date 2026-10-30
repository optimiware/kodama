---
id: T-131
milestone: production
status: done
depends_on: [T-126]
decision_blockers: []
owns: [contracts/pipeline/, contracts/runtime/, internal/pipeline/, docs/contracts/pipeline-format-v1alpha3.md, docs/contracts/runtime-v3.md, docs/contracts/README.md]
check: make check-go PKGS='./contracts/pipeline/... ./contracts/runtime/... ./internal/pipeline/...'
resources: [go]
acceptance: [unit_pipeline_sources_declaration, unit_pipeline_old_formats_unchanged, unit_runtime_context_source_set, unit_runtime_result_per_repository_outputs, unit_runtime_old_hashes_unchanged, unit_runtime_git_grant_delivery_shape]
---
# T-131 — Source-set pipeline and runtime contracts

## Goal
Version the pipeline and runtime contracts to carry run origins, source sets, per-repository outputs and Git grant delivery (PX-23, GR-06, S6). Rescoped from the former single T-131 umbrella, whose other parts are T-147–T-153.

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Additive version: `kodama/v1alpha1` and `kodama/v1alpha2` canonical bytes, digests, runtime v1/v2 contexts and result hashes are unchanged; a controller refuses a version it does not implement.

## Deliverables
- Pipeline format addition: `spec.sources` (repository name, checkout path, access) with validation (unique safe paths, known repositories at admission).
- Runtime context/result addition: origin, source set, per-repository `commit` outputs and their push-record references, Git credential delivery path.
- Parser/validator in `internal/pipeline` and fixtures; OpenAPI fragment edits for `api/pipeline.yaml`/`api/runtime.yaml` are T-158's (owned by T-116 until it merges).

## Acceptance
Declared check with old fixtures byte-identical.

## Out of scope
Execution, entrypoint, adapters.

## Completion evidence
Implemented by a subagent: pipeline format `kodama/v1alpha3` (off by default) and runtime context/result v3 with origins, source sets, per-repository commit outputs, Git delivery, v3 preparation/resource messages, runtime seed and integration grant routes; old formats pinned unchanged. Independent review requested changes once (no valid seed-grant flow for provisioning; no way to report an approved integration push) and approved `f7c9d137`. Coordinator checks: `make check-task TASK=T-131` PASS (6 acceptance names), `go test ./contracts/... ./internal/pipeline/...` PASS, `make docs-check` and `make validate-tasks` PASS. For T-150/T-151: the credential for runtime-issued grants is returned in the route response and written by the entrypoint to the advertised file; the control plane decides integration eligibility from the gate.
