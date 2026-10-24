---
id: T-059
milestone: M4
status: deferred
depends_on: []
decision_blockers: []
owns: [docs/implementation/chassis-guidance.md, templates/chassis-guidance/]
---
# T-059 — Tenant/project Markdown chassis guidance

## Goal
Provide Git-versioned Markdown engineering guidance for a tenant or project, without a shared application runtime.

## Contract
Resolved O-17 supersedes the former library-extraction assessment. Chassis is Markdown guidance: microservice boundaries, approved stacks and libraries, and deployment methods for the target cloud. Establish a simple tenant/project-scoped guidance repository convention (normally one Git repository per selected scope), with explicit repository access, immutable commit/path references and a recorded baseline in agent context. Any tenant-to-project inheritance must be explicit and pinned, not a moving branch lookup.

Use the merged pipeline/profile instruction contracts. Do not invent a settings/API resource or edit T-030/T-031 outside ownership; identify a bounded owner prerequisite if consuming an external guidance repository needs a missing seam. Repository Markdown conveys instructions, never runtime authority or credentials.

## Deliverables
Markdown-only sample guidance repository/layout, tenant/project scope and adoption documentation, example microservice/stack/library/cloud-deployment guidance, pinned instruction-context examples and a reviewed update procedure compatible with the two blueprints.

## Acceptance
`chassis_guidance_review` verifies a Markdown-only kit, scope isolation, approved-stack/deployment examples, immutable revision/path references and reproducible instruction context. Updating a repository branch cannot silently alter an admitted run. The former chassis-extraction review is retired under O-17; it does not authorize library work.

## Out of scope
Application runtime/chassis SDKs, automatic shared-library extraction, a mandatory framework, or treating Markdown as authorization.
