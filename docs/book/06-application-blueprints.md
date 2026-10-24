# 6. Application blueprints

## Markdown chassis strategy

**Confirmed (O-17):** the application chassis is Markdown-only engineering guidance. It can describe microservice architecture, approved stacks/libraries and cloud deployment methods. Tenant/project guidance lives in a scoped Git repository, normally one repository per selected scope, and agent context pins the immutable commit and selected paths. Applications own their runtime and deployment configuration; chassis does not become a shared runtime SDK.

A blueprint is a versioned package of conventions. The first packages describe [Go + PostgreSQL](../examples/blueprints/go-postgres.md) and [Supabase + Hono + Vite](../examples/blueprints/supabase-hono-vite.md).

## Blueprint contents

| Section | Required guidance |
| --- | --- |
| Purpose | Suitable problems, deployment assumptions, and relevant trade-offs |
| Layout | Application modules, migrations, tests, deployment files, and agent entry points |
| Identity | Application users, application tenants, membership, and authorization conventions |
| Data | Tenant ownership, schema evolution, seed data, backup, and legacy integration |
| Interfaces | Error conventions, pagination, compatibility, and integration contracts |
| Delivery | Build outputs, preview setup, stage promotion, smoke tests, and cleanup |
| Quality | Acceptance evidence, testing strategy, review expectations, and operational checks |
| Agent guidance | Expected workflow, contextual reading, allowed tools, and completion evidence |
| Evolution | Version, change history, compatibility notes, and adoption procedure |

These are guidance requirements for blueprint authors. Blueprints may supply executable application templates separately; that does not turn the Markdown chassis into an application runtime dependency or authority source.

## Tenant conventions

A factory tenant owns the development project. An application tenant belongs to the application's own domain. A blueprint explains how authenticated application identity determines application-tenant access and how tenant-owned data is isolated.

Applications may federate to an external identity provider. They should document their runtime dependencies explicitly. The initial chassis does not require application requests to pass through Kodama.

## Legacy modernization pattern

Use the first example application to describe an incremental modernization approach:

1. Identify one gap and the existing system of record.
2. Specify the smallest useful workflow and its acceptance criteria.
3. Define an adapter to the legacy system, including data ownership and write permissions.
4. Model synchronization, stale reads, duplicate requests, and reconciliation.
5. Build and validate the new application with realistic but appropriately scoped test data.
6. Roll out to a bounded group and observe actual usage.
7. Decide whether to expand, replace another gap, or retire the application.

Blueprints should make data ownership explicit so a gap-filling app does not silently become a competing source of truth.

## Version adoption

A work item records the blueprint identity and version used when its workspace is initialized. Its specification and agent instructions reference that version. Updating guidance is an ordinary reviewed work-item change.

Template updates may be applied to existing applications as ordinary work items with diffs, migration plans and tests. Guidance updates likewise produce an explicit reviewed revision; moving the guidance repository's branch cannot silently change an admitted run. Shared application chassis-library extraction is outside the selected O-17 scope.

## Agent skill packaging

Skills should provide a focused workflow with task-specific context pointers and observable completion criteria. Shared conventions have one authoritative location inside the blueprint; skill instructions refer to that location rather than duplicating the full rules.

The repository contains blueprint design examples, with [backlog](../tasks/README.md) template tasks for both stacks. T-059 supplies the Markdown guidance-repository conventions and pinned-context examples; it replaces the former library-extraction assessment. Existing small Kodama infrastructure helpers are independent of this application-chassis scope.
