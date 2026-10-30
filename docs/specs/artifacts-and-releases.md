# Artifacts, releases and publication (historical)

> **Superseded.** This document recorded the O-34/[ADR 0012](../adr/0012-container-sdlc-and-artifact-releases.md) artifact-catalogue, release-manifest, publication-receipt and generic-delivery design. [ADR 0013](../adr/0013-two-layer-architecture-and-domain-simplification.md) and its 2026-10-30 amendment ([O-38](../decisions.md#pipeline-runtime-and-four-layer-refinement--2026-10-30-o-38)) replace it.

Current direction:

- The shell records **metadata only** for external artifacts: name, kind, URI, digest, producing run/step/task, and metadata.
- Pipeline steps build, publish, fetch, deploy, roll back, and clean up with ordinary container tools and declared secrets.
- Human approval is a durable pipeline wait resolved by an authorized role. There is no release-receipt state machine, gate engine, deployment subsystem, or Kodama-hosted artifact catalogue.
- Provenance and delivery requirements are owned by [Artifacts, releases, and promotions](delivery-and-quality-gates.md).

The AF-01–AF-10 and AF-A01–AF-A08 identifiers are retired. Historical tasks, acceptance reports, and contracts that cite them remain historical evidence for the superseded model. The implementation cleanup is in the [pipeline-runtime simplification plan](../implementation/pipeline-runtime-simplification.md).
