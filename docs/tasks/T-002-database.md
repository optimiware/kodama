---
id: T-002
milestone: foundation
status: done
depends_on: [T-003]
decision_blockers: []
owns: [internal/storage/, test/support/postgres/]
---
# T-002 — Database and tenant transactions

## Goal
Create the shared persistence foundation once.

## Contract
Expose pgx pool, `WithTenant` transaction boundary, ordered embedded-migration loader, and test database helper for PostgreSQL adapters. Use the [schema sketch](../implementation/database-schema.md) and [software design](../specs/software-design.md): domain entities remain independent of pgx; entity-specific persistence ports belong to their consuming modules. No generic entity repository abstraction.

## Deliverables
Tenant/project/user/membership/audit tables, composite ownership constraints, runtime/migration roles, RLS, schema lock, immutable migration IDs.

## Acceptance
`pooled_tenant_context` and `cross_owner_fk` verify IT-02–IT-05 against real PostgreSQL. `migration_reapply` verifies ordered idempotent application and duplicate-ID rejection.

## Out of scope
OIDC behavior, module-owned tables, or network calls inside transactions.
