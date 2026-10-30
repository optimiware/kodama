---
id: T-144
milestone: production
status: deferred
depends_on: [T-143]
decision_blockers: []
plan: true
owns: [internal/artifacts/, images/deployment-tools/, docs/examples/workflows/container-release-and-deploy.md, test/integration/artifact-publication/]
check: make check-go PKGS='./internal/artifacts/...' && make docs-check
resources: [go]
acceptance: [unit_publication_copy_identity, unit_publication_complete_oci_graph, unit_publication_destination_authority]
---
# T-144 — Verified publications and ordinary tool recipes

## Goal
Implement AF-06/AF-07 and practical publication recipes over standard storage tools, not a replication engine. Follow [ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md).

## Deliverables
- Verify destination file bytes/complete OCI content graphs with bounded authorized clients and media-type-aware metadata; reuse existing S3/registry adapters.
- Record idempotent copy receipts without changing artifact identity; changed digest requires a derived artifact. Missing content, misleading ETags, redirects or unapproved origins never produce verified copies.
- Ordinary container scripts for image/chart registry copy, archive/static upload and deploy/observation/cleanup. Scripts consume the generic runtime helper contract with explicit secrets, preserving stable action/receipt identities.
- Replace mandatory Kubernetes-specific deployment-tools protocol with optional examples; list obsolete consumers for T-145's drain/removal. Provide real local registry/S3 fixtures for T-146.

## Acceptance
Unit checks cover complete multi-platform image/chart graphs, missing child content, conflicting receipts, credential audience and partial-copy recovery. Real two-registry copy, private-target reachability and tool invocation are required in T-146; a mocked receipt is not publication qualification.

## Out of scope
Bundled registry packaging, continuous replication, package-manager indexes, public report hosting and disconnected runners.
