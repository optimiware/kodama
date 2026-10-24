---
name: inventory-migrations
description: Add or change database migrations, row-level security and tenant-owned tables.
---

Use for anything under `migrations/` or SQL in adapters.

1. Migrations are forward-only files `T<number>_<name>.sql`. Never edit an applied file: the checksum ledger refuses it.
   Fix forward with a new file.
2. Every tenant-owned table has `tenant_id`, a composite primary key starting with it, composite foreign keys
   that include it, `ENABLE` and `FORCE ROW LEVEL SECURITY`, and the `tenant_isolation` policy using `app_tenant()`.
3. The runtime role gets least privilege through `{{runtime_role}}`; it never owns objects and never has
   `BYPASSRLS`. Audit history is append-only.
4. Statements are tenant-qualified (`WHERE tenant_id = $1`) in addition to RLS, and run through `dbtx.InTenant`.
5. Keep old and new application versions compatible during a rollout: add before you use, stop using before you
   drop (see [operations](../../../docs/operations.md), "Migrations and compatibility").
6. Evidence: `go test ./migrations/... ./internal/exceptions/postgres/...` against a real PostgreSQL, including
   the tenant, rollback and concurrency cases for the new table.
   Name the candidate commit and report unresolved deviations in the conformance report (`AGENTS.md`).
