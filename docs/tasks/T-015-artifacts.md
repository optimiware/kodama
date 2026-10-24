---
id: T-015
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/artifacts/, api/artifacts.yaml, test/support/s3/]
---
# T-015 — Artifact storage and scoped transfer

## Goal
Persist reports/logs/bundle transfers and immutable provenance metadata.

## Contract
S3-compatible upload reservation/completion/download must implement the [runtime envelopes](../implementation/step-entrypoint.md). References become usable only after ownership/digest/size checks.

## Deliverables
Artifact table, LocalStack-tested S3 adapter, scoped short-lived transfer URLs, OCI registration verification, orphan/retention hooks, public metadata fragment.

## Acceptance
`foreign_object`, `digest_mismatch`, `incomplete_upload`, `expired_url`, and `orphan_cleanup` verify IT-03/IT-11, PX-10, and DQ-01.

## Out of scope
Source checkpoint manifests or unrestricted caller-supplied storage keys/URLs.
