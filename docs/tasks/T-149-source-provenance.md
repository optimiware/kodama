---
id: T-149
milestone: production
status: done
depends_on: [T-136, T-148]
decision_blockers: []
owns: [internal/artifacts/, internal/gates/, internal/delivery/, internal/deliverycleanup/, api/artifacts.yaml, api/gates.yaml, api/delivery.yaml]
check: make check-go PKGS='./internal/artifacts/... ./internal/gates/... ./internal/delivery/... ./internal/deliverycleanup/...'
resources: [go]
acceptance: [unit_artifact_provenance_source_set, unit_gate_subject_source_set_and_base, unit_gate_moved_base_not_applicable, unit_integration_grant_requires_approved_subject, unit_deployment_provenance_source_set, unit_deployment_mismatch_rejected, unit_cleanup_preserves_git_storage, unit_single_source_subjects_unchanged]
---
# T-149 — Source-set provenance and integration approval

## Goal
Evidence, gates and deployments identify the whole source set; integration is authorized only for an approved, applicable candidate/base (DQ-17, GR-12).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), [delivery](../specs/delivery-and-quality-gates.md). Migration `013_source_provenance.sql`. Existing single-source subjects and digests stay valid.

## Deliverables
- Source-set provenance for artifacts and OCI registrations; gate subjects with candidate set and integration base; applicability rules.
- Integration-grant issuance port to T-136 bound to an approved subject; deployment provenance and mismatch rejection; cleanup regression tests.
- API fragment updates.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t149 DEV_STACK=services PKGS='./internal/gates/... ./internal/delivery/...'` (DQ-A13).

## Out of scope
Runtime routes (T-150).

## Completion evidence
Implemented by a subagent; independent review requested changes once (an approval invalidated after a successful integration push; no current-authority check of the deciding reviewer in `IntegrationApproval`; a fail-open single-source delivery check for v3 attempts; `candidateCommit` made nullable, a breaking API change; missing database ownership checks and cleanup round test) and approved after the fixes (`82785484`). Coordinator decision: `candidateCommit` and delivery `source` stay required and non-null, presenting the primary source's commit for a set, with `sourceSet` additive. Coordinator checks on the rebased branch: `make check-task TASK=T-149` PASS (8 acceptance names); `go test -race` on the four packages PASS; real SQL `make check-go-integration DEV_SCOPE=t149 DEV_STACK=services PKGS='./internal/artifacts/... ./internal/gates/... ./internal/delivery/... ./internal/deliverycleanup/...'` PASS. The coordinator applied the delivery specification wording (set digest, per-source base, primary commit, consumed approval).

For T-132: register `artifactspg.SourceProvenanceMigrations()` after 007 and 011 and before 014; set `SourceProvenance: true` on the artifacts, delivery and gates stores; set `Dependencies.Bases`, `delivery.Config.SourceSets` (`adapters.Execution`) and `BindDeploymentsWithSourceSets` together (a store opted in with a plain deployment reader should be refused at construction: add that guard); return `Provenance{SourceSet}` from `internal/app/registry.go`'s attempt-provenance resolver and the results artifact authority for source-set runs; give the gate and delivery stores a v3-aware run reader. For T-150: consume `IntegrationApproval` (map to `gitauth.GateDecision` and runtime v3's `IntegrationApproval`); treat `ErrAlreadyIntegrated` as success without a grant and `*StaleError` as `409 publication_conflict` with `SupersedingCommit`; provide a `gates.Bases` adapter over the worker's `ReadRefValues`. For T-158: regenerate `api/openapi.yaml` and `api/client` for the additive `sourceSet` members (artifacts, gates `subject.sourceSet`, delivery source-set bodies). Limitations: a gate for a run without a work item is out of scope; an agent candidate spanning several writable repositories is blocked at approval (`conformance_source_set_unsupported`); a tip that is a descendant of the candidate invalidates as `base_moved` (conservative; check ancestry Git-side in T-150). Minors: pin `search_path` and qualify `public.` in migration 013's `t149_*` trigger functions (convention of 015/014).
