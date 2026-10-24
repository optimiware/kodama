# Blueprint guidance: Supabase + Hono + Vite

**Status:** proposed application guidance, not a scaffold or shared runtime package. Record the adopted guidance version in the work item and workspace.

## Suitable applications

Workflow-oriented internal applications that benefit from a browser UI, PostgreSQL-backed data, identity integration, and a small API layer for business operations and legacy-system access.

Supabase supplies the selected database/auth/storage capabilities. Hono hosts application-facing server operations. Vite supplies frontend development/build tooling; the UI framework and Hono runtime are choices to pin when the template is implemented.

## Suggested application layout

```text
AGENTS.md
README.md
CONTEXT.md
apps/
  api/
  web/
packages/
  domain/
  contracts/
supabase/
  migrations/
  seed.sql
ci/
  quality.sh
  build.sh
  deploy-preview.sh
  deploy-stage.sh
  e2e.sh
docs/
  architecture.md
  tenancy.md
  operations.md
```

The template must pin package manager, runtime, dependency versions, and deployment topology. Shared packages are justified by behavior reused between the API and UI; business authorization remains enforced on the server/data path.

## Application identity and tenancy

Use the selected Supabase Auth or federated identity configuration to establish user identity. A user may have memberships in several application tenants. The selected tenant in the browser is a requested scope, verified against membership for each protected action.

Tenant-owned tables include explicit application-tenant ownership and appropriate relationship constraints. RLS policies validate the authenticated user's membership and permitted action. Browser-originated database/storage access must be covered by those policies; API checks alone do not secure a separate direct-data path.

Hono validates the expected token issuer/audience and authentication mode before authorizing business operations. Privileged server credentials remain server-side and have narrow use. A service-role path that bypasses RLS must perform explicit authorization and be covered by separate tests; it must never be exposed as a browser credential or a generic query proxy.

The factory tenant owning development is separate from the application's tenants and identity configuration.

## Server and browser responsibilities

| Layer | Responsibility |
| --- | --- |
| Vite application | User interaction, tenant selection, validated input presentation, visible progress/error state |
| Hono operations | Business authorization, integration orchestration, input contracts, idempotent commands |
| Supabase data policies | Tenant/row/storage access for every supported access path |
| Legacy adapter | ERP request mapping, credentials, retries, unknown-outcome reconciliation |

The UI displays pending or unknown integration outcomes accurately. It must not convert an HTTP timeout into a false success or automatically submit a new business action with a new key.

## Data ownership and migrations

The ERP remains authoritative for stock data in the inventory example. The application stores exception workflows, membership, audit, and durable integration intents. Read models state their synchronization/refresh semantics.

Version schema, RLS, function, and storage-policy changes together. Test migrations against representative data and test policy behavior with real authenticated roles. Seed data uses synthetic or specifically authorized business data. Preview fixtures are scoped to their application tenant and environment.

## Runtime configuration

Distinguish public frontend configuration from privileged API/database/ERP credentials. Vite build-time values are included in distributed assets; only intentionally public values belong there. Runtime secret values use the selected server/deployment secret mechanism.

To promote the same built artifact between preview and stage, prefer runtime public configuration or a documented equivalent. If the chosen frontend deployment embeds environment-specific values at build time, record that limitation and identify separate artifacts/evidence rather than claiming an unchanged digest was promoted.

Supabase may be externally operated or self-hosted according to the application's needs. The application's on-premises expectations, backup responsibilities, email/auth dependencies, and feature availability must be documented for its selected deployment.

## Quality and delivery

| Stage | Expected evidence |
| --- | --- |
| Specification | Business flows, role/tenant matrix, UI outcomes, legacy-system ownership |
| Quality | Type checking, meaningful domain/API tests, migrations, RLS/storage-policy tests |
| Build | Reproducible API/UI artifacts, dependency/toolchain versions, source commit |
| Preview | Authorized test identities, isolated data/configuration, readable URL, expiration |
| E2E | Login/tenant switching, exception resolution, duplicate submission, unauthorized access |
| Exploratory QA | Navigation, error handling, stale/pending state, evidence for acceptance criteria |
| Human review | Warehouse usability and correct business outcomes on an identified deployment |

Test both direct browser-to-Supabase access, if enabled, and Hono-mediated access. Include service-role operations in authorization coverage. Browser tests identify the deployment and runtime configuration inspected.

## Agent guidance

Generated instructions should direct an agent changing data access to the tenancy/RLS guide, an agent changing business behavior to the specification/domain model, and an agent changing UI to the acceptance flows and design guidance.

Agents should return source changes, migration/policy changes when applicable, and evidence for the affected acceptance criteria. A successful screenshot alone does not establish authorization or data consistency; use the appropriate integration evidence for those behaviors.

## Blueprint review checklist

- Browser-visible configuration contains only intentionally public values.
- Every enabled data path enforces application-tenant membership and action permission.
- Service-role use is server-side, narrowly scoped, and independently tested.
- Tenant switching cannot reuse another tenant's cached records or mutation context.
- ERP writes have idempotency/unknown-outcome handling.
- Preview/stage artifact and runtime-configuration claims match the chosen frontend deployment model.
- The application documents its own operating and backup dependencies.

## Evolution

Begin with guidance and reproducible template examples. Extract shared UI/domain/runtime packages only when repeated application work demonstrates stable common behavior. Template upgrades enter existing applications through ordinary reviewed work items with migration and acceptance evidence.
