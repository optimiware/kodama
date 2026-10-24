# Examples

These examples instantiate the proposed design. CLI commands, pipeline images, profile names, and delivery scripts are illustrative interfaces for future implementation. Reserved example domains and repeated-character digests are placeholders.

## Workflows

1. [ERP gap to preview](workflows/erp-gap-to-preview.md): analyst intent, planning, implementation, source-quality review, build, and preview.
2. [Staging and acceptance](workflows/staging-and-acceptance.md): reuse the image, test the stage deployment, perform agent-led exploratory QA, and obtain human acceptance.
3. [Recovery and rework](workflows/recovery-and-rework.md): controller failure, competing writers, rejected reviews, unknown external effects, and restore.

## API and CLI

[CLI and HTTP walkthrough](api/cli-and-http.md) demonstrates the proposed command syntax, idempotency, version checks, and gate decisions.

## Blueprints

- [Go + PostgreSQL](blueprints/go-postgres.md)
- [Supabase + Hono + Vite](blueprints/supabase-hono-vite.md)

The blueprints are application-authoring guidance. Their illustrative file layouts and scripts belong to future tenant application repositories, not to the current factory implementation.

## How to review an example

Check tenant/project ownership, pinned source/specification/definition versions, workspace write ownership, evidence provenance, gate applicability, and external-effect recovery. Then verify the syntax against the [contracts](../contracts/README.md) and the outcomes against the [specifications](../README.md#contributors).
