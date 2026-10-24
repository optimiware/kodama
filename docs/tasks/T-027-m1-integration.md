---
id: T-027
milestone: M1
status: done
depends_on: [T-010, T-011, T-012, T-013, T-014, T-015, T-016, T-018, T-019, T-020, T-021, T-022, T-023, T-024, T-025, T-064, T-071, T-088]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, deploy/helm/kodama/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [m1_composition_smoke, m1_contract_consistency, m1_generated_client]
---
# T-027 — M1 composition and contract integration

## Goal
Wire independently built modules without concurrent edits to shared files.

## Contract
Consume each module's constructor/routes/migrations/command contribution. Use [ownership rules](README.md#ownership-and-integration); reject divergent runtime/OpenAPI shapes before e2e.

Continue T-070/T-071's reviewed core/execution wiring, including T-062's transaction bindings and T-064's log qualification; finish all CLI/chart/public-client/CI joins here. Those incremental checkpoints do not replace any deliverable or acceptance below. Ownership of central files transfers in dependency order, never concurrently.

Chart join (ownership of `deploy/helm/kodama/` transfers here from T-018, which is done): select the real assembly and environment explicitly, consume the rendered settings, supply the secret keys the assembly requires, and prove it with a no-cluster render test. The join is bounded: production key providers are wired by T-029 and installed-chart qualification belongs to T-089/T-056. A chart profile that cannot yet start the core assembly because of that must fail closed and say so in the chart README and render test; never weaken the `production` environment guards to make a profile start.

## Deliverables
Server/step/CLI composition, ordered migrations, aggregate OpenAPI, event consumers, deny-by-default auth wiring, CI/Make targets and accurate development README. Qualify with injected adapters and httptest. Run the composition smoke in the `development` environment with memory adapters and no container. Real service checks belong to T-026 and the installed chart to T-089; public-module migration and release packaging remain later work.

Use the selected public repository/target module `github.com/optimiware/kodama` in publication planning and client documentation. The existing local `module kodama` is not already migrated: coordinate a separately scoped mechanical module/import prerequisite before publishing an importable client, rather than rewriting unowned module files implicitly. Keep Go 1.27 (current pin 1.27.2).

Generate a public Go HTTP client in `api/client/` with pinned `oapi-codegen` configuration and a reproducible generation command. Generate from the assembled public API; keep runtime-only routes separate. The client has configurable server URL/HTTP transport and request authentication hooks, without imports of Kodama's internal implementation. Platform can generate its own client from the same versioned OpenAPI document; neither repository needs to embed the other's server. Publish the spec with release artifacts and check regeneration drift in CI.

## Acceptance
`m1_composition_smoke` verifies route/CLI/migration registration using injected adapters, including the whole slice composed under `KODAMA_ENV=development` with no service; `m1_contract_consistency` validates examples and runtime schemas. Covers AC-12, PX-05, IT-03. T-026 verifies actual migration/server behavior.

`m1_generated_client` exercises run submission/status and stable error decoding through the generated client, including an idempotent replay and a conflicting key/body. It also verifies reproducible generation. Covers AC-01/AC-02/AC-10/AC-12; hosted Platform credentials remain T-061's contract.

## Out of scope
Silently changing module contracts or declaring M1 accepted before T-026.
