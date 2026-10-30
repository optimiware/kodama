# internal/storage

PostgreSQL infrastructure: pgx pools, `WithTenant`, principal-filtered discovery,
and an ordered migration runner. Capability packages own their queries and
entities; this package does not provide a universal repository.

## ADR 0013 baseline

`migrations/001_schema.sql` contains 14 domain tables plus `audit_events` (15
product tables). `002_grants.sql` grants the non-owner runtime role only the
required table/column operations. See [schema documentation](../../docs/implementation/database-schema.md).

- Tenant/project composite keys reject cross-owner links.
- Forced RLS covers every product table; global users are visible only through
  membership in the transaction's tenant.
- Run snapshots, artifact metadata and audit payloads are immutable to runtime.
- `kodama_discover_memberships` filters by a verified issuer/subject;
  `kodama_list_tenant_ids` returns only IDs to trusted workers. Both definer
  functions revoke PUBLIC execution in the same migration as their definition.
- `schema_migrations` is migration-runner bookkeeping, with no runtime grants.

## Transactions and migrations

`WithTenant` validates the tenant ID, begins a pgx transaction, sets
`kodama.tenant_id` transaction-locally and commits only when the callback succeeds.
Errors, panics and cancellation roll back. Pooled connections retain no tenant
context. Callbacks pass their transaction to peers and perform no network I/O.

`BaseContribution()` embeds the two baseline SQL files. `NewManifest` validates
explicit contribution order, IDs and uniqueness. `Migrate` serializes runners
with a database advisory lock and verifies every existing checksum before
applying a pending migration atomically with its ledger entry. Role placeholders
are quoted identifiers, never SQL interpolation from request data.

The ADR 0013 rewrite intentionally changes the prerelease baseline checksum.
An old database returns `ErrChecksumMismatch` (or `ErrUnknownMigration` for old
module entries); it is **never automatically dropped or upgraded**. Use a fresh
database for this branch. The server's legacy adapters require the Phase 3 port.
New contributions after this reset append migrations; they do not change an
already deployed checksum.

## Verification

```sh
# Infrastructure-free checks; database checks are excluded by the unit tag.
go test -race -count=1 -tags unit ./internal/storage/...

# PostgreSQL: fixture creates isolated database + owner/runtime roles per test.
# Set KODAMA_TEST_POSTGRES_ADMIN_URL to the task-local PostgreSQL instance.
KODAMA_REQUIRE_POSTGRES=1 go test -race -count=1 ./internal/storage/...
```

Coverage includes pooled context/rollback, RLS, runtime privileges, scoped FKs,
null-safe uniqueness, service scope checks, immutable run snapshots, trigger and
request deduplication, append-only audit payloads, replayable migration application,
and refusal of an old baseline without data loss.
