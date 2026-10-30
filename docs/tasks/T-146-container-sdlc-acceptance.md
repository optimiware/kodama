---
id: T-146
milestone: production
status: deferred
depends_on: [T-145, T-117]
decision_blockers: []
plan: true
owns: [test/integration/container-sdlc/, test/integration/artifact-publication/, docs/acceptance/container-sdlc.md, docs/status.md, docs/traceability.md]
check: make check-go-integration PKGS='./test/integration/container-sdlc/... ./test/integration/artifact-publication/...'
resources: [go, integration]
acceptance: [sdlc_release_roundtrip, sdlc_isolated_target_publication, sdlc_release_rollback, sdlc_retention_recovery, sdlc_legacy_cutover]
---
# T-146 — Real container SDLC, release and rollback acceptance

## Goal
Qualify the complete ADR 0012 path with real PostgreSQL, S3, two OCI registries and containers, using the accepted runtime profile. Native-Git end-to-end claims additionally require the dependency reconciliation in the [handoff](../implementation/container-sdlc-refinement.md#coordination-with-native-git-and-active-run-resource-work).

## Deliverables and acceptance
- Retain binary/archive, chart and JSON/YAML/HTML reports; build/register an OCI image, create an immutable release and fetch exact artifacts from a later run. Exercise AF-A01–AF-A04 and DQ-A13.
- Publish image/chart/file outputs to target-accessible private storage; verify full OCI content, then deploy to a target denied access to Kodama storage. Include a Kubernetes recipe and a non-Kubernetes upload/deploy recipe, proving generic records rather than just schema representability (AF-A05/AF-A08, DQ-A14).
- New rollback run selects prior artifacts/configuration without invoking a build. Credential rotation/revocation, missing layers/objects and mutated labels do not replace identity or resurrect authority (DQ-A16).
- Real pin/delete concurrency, response loss, restart, cross-tenant denial, partial publication and unknown apply outcomes preserve references and avoid duplicate effects (AF-A06/AF-A07, DQ-A15).
- Prove legacy drain/read compatibility and removal of obsolete prerequisites. Retain candidate/profile/resource receipts and clean only this task's resources; mark capability status only for actual passing evidence.
- Required acceptance fixtures fail or report an explicit infrastructure blocker when unavailable; skips, stand-in stores or a tool's `--version` do not satisfy any real-path row.

## Evidence limits
Use a local deterministic harness command to prove the generic container/secret primitive; it is not live Claude/OpenCode/native-model qualification. Wrappers remain T-052–T-054/T-085. No cloud account, new stage mutation, hosted integration, air-gap claim or optional registry packaging is implied by this task plan; name real providers/targets when scheduling the relevant acceptance run.
