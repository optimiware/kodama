# Examples

These examples instantiate the proposed design; `kodama/v1alpha1` pipelines are the current executable format and `kodama/v1alpha2` ones are normative design fixtures. CLI commands, pipeline images, profile names, and delivery scripts are illustrative interfaces for future implementation. Reserved example domains and repeated-character digests are placeholders.

## Workflows

1. [ERP gap to preview](workflows/erp-gap-to-preview.md): analyst intent, planning, implementation, source-quality review, build, and preview.
2. [Staging and acceptance](workflows/staging-and-acceptance.md): reuse the image, test the stage deployment, perform agent-led exploratory QA, and obtain human acceptance.
3. [Recovery and rework](workflows/recovery-and-rework.md): controller failure, competing writers, rejected reviews, unknown external effects, and restore.
4. [Shared workspace, services and builders](workflows/run-shared-build-and-services.md): the `kodama/v1alpha2` design fixtures for ordering, service endpoints and edges, ko/Buildah/BuildKit builds, source-free cleanup and egress policy. Design fixtures only: no image, mode or combination is qualified.
5. [Container build, release, publish, deploy and rollback](workflows/container-release-and-deploy.md): ADR 0012's tool-neutral flow, direct harness secret binding, explicit artifact retention and isolated production storage. New release/helper operations remain planned.

## API and CLI

[CLI and HTTP walkthrough](api/cli-and-http.md) demonstrates the proposed command syntax, idempotency, version checks, and gate decisions.

## Blueprints

- [Go + PostgreSQL](blueprints/go-postgres.md)
- [Supabase + Hono + Vite](blueprints/supabase-hono-vite.md)

The blueprints are application-authoring guidance. Their illustrative file layouts and scripts belong to future tenant application repositories, not to the current factory implementation.

## How to review an example

Check tenant/project ownership, pinned source/specification/definition versions, workspace write ownership, evidence provenance, gate applicability, and external-effect recovery. Then verify the syntax against the [contracts](../contracts/README.md) and the outcomes against the [specifications](../README.md#contributors).
