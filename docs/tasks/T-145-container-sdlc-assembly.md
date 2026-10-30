---
id: T-145
milestone: production
status: deferred
depends_on: [T-144, T-132]
decision_blockers: []
plan: true
owns: [internal/app/, internal/cli/, internal/delivery/, internal/deliverycleanup/, images/deployment-tools/, api/, deploy/charts/kodama/, go.mod, go.sum, docs/operations/, docs/implementation/assembly-status.md]
check: make check-go PKGS='./internal/app/... ./internal/cli/... ./internal/delivery/... ./internal/deliverycleanup/... ./api/client/...' && make api-check && make test-chart && make docs-check
resources: [go]
acceptance: [unit_container_sdlc_composition, unit_generic_tools_without_catalogues, unit_sdlc_cli_roundtrip, unit_legacy_delivery_drain]
---
# T-145 — Assemble the simple SDLC path and retire old surface

## Goal
Wire T-141–T-144 into the real server/CLI and complete the [removal inventory](../implementation/container-sdlc-refinement.md#reuse-reduce-and-retire). Order after native Git assembly before scheduling shared-file work.

## Deliverables
- Register scoped artifact/release/publication operations, runtime fetch/helpers, generic delivery and current authority; compose real stores/adapters and retention workers without fallback fakes.
- Add `artifact`, `release`, `publication` CLI groups and update environment/deployment commands. Bind exact release inputs before use; regenerate aggregate OpenAPI/client and compatible API baselines.
- Wire optional managed agent/gateway/proxy capabilities independently of generic container/secret admission. Remove compulsory delivery-tool catalogue loading from the new path; reduce chart/config requirements accordingly.
- Migrate/project legacy records; keep old admitted runs and cleanup obligations readable and settle/drain their mutation path. Then remove obsolete active protocol branches, `delivery-tools.json`/`ToolQualification` wiring, adapters, fixtures and mandatory tool images with documented compatibility evidence.
- Register appended migrations in order, operation/permission mappings and revised operations/status documentation. Keep optional configured registries; no registry deployment is made mandatory.

## Acceptance
Composition/CLI tests cover direct harnesses with mediation disabled, generic deploy without Kubernetes/image fields, exact release selection, absence of stale catalogue prerequisites, unknown-effect retention and historical clients/records. Run the API/chart checks above and affected M1–M3 composition checks if their contracts changed. Unit evidence is not installed-product acceptance.
