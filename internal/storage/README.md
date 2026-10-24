# internal/storage

Shared PostgreSQL infrastructure for Kodama (task T-002): pgx pool, the
`WithTenant` transaction boundary, narrow non-tenant read paths, an ordered
embedded-migration runner, and the base tenancy/audit schema. It is
infrastructure only. Domain entities stay free of pgx, and entity-specific
persistence ports and SQL belong to the consuming modules (SD-04); there is no
generic repository.

## Exported API

```go
// Pool
type Config struct { URL string; MaxConns, MinConns int32; MaxConnLifetime, MaxConnIdleTime time.Duration; ApplicationName string }
type Pool struct{ *pgxpool.Pool }
func Open(ctx context.Context, cfg Config) (*Pool, error)        // no global state; sessions run in UTC

// Tenant transaction
const TenantSetting = "kodama.tenant_id"
type TxBeginner interface{ BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) }
func WithTenant(ctx context.Context, db TxBeginner, tenantID domain.TenantID,
    fn func(ctx context.Context, tx pgx.Tx) error) error
func WithTenantOptions(ctx context.Context, db TxBeginner, tenantID domain.TenantID,
    opts pgx.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error

// Non-tenant paths (SECURITY DEFINER functions, runtime role may execute)
type Membership struct { UserID string; TenantID domain.TenantID; ProjectID domain.ProjectID; Role string }
func DiscoverMemberships(ctx context.Context, q Querier, issuer, subject string) ([]Membership, error)
func ListTenantIDs(ctx context.Context, q Querier) ([]domain.TenantID, error)

// Migrations
type Migration struct{ ID, SQL string }
type Contribution struct{ Module string; Migrations []Migration }
func LoadFS(fsys fs.FS, dir string) ([]Migration, error)
func NewManifest(contributions ...Contribution) (Manifest, error)
func BaseContribution() Contribution
type MigrateOptions struct{ RuntimeRole string }
type MigrateResult struct{ Applied []string; Skipped int }
func Migrate(ctx context.Context, db Acquirer, m Manifest, opts MigrateOptions) (MigrateResult, error)
```

Errors: `ErrInvalidMigrationID`, `ErrDuplicateMigration`, `ErrMigrationOrder`,
`ErrChecksumMismatch`, `ErrUnknownMigration`, `ErrRuntimeRoleRequired`;
`WithTenant` wraps `domain.ErrInvalidValue` for a bad tenant ID. `WithTenant`
returns the callback's own error unwrapped.

### WithTenant semantics

1. Validates the ID with `domain.TenantID.Validate` before touching the pool.
2. `BeginTx`, then `set_config('kodama.tenant_id', $1, true)` (transaction
   local), then the callback, then `Commit`.
3. A deferred rollback (using a cancellation-free context) covers errors,
   panics (which keep propagating), `runtime.Goexit` and cancelled contexts.
4. Because the setting is transaction-local, PostgreSQL discards it at
   commit/rollback; nothing tenant-specific is left on the pooled connection
   (IT-05). The callback must not commit/roll back, keep `tx`, or make network
   calls (database-schema.md transaction boundaries).

Every policy compares `tenant_id` with `kodama_tenant_id()`, which is `NULL`
when the setting is absent or empty, so statements outside `WithTenant` see and
write nothing (fail closed).

## Base schema (T002_001 to T002_003)

| Object | Notes |
| --- | --- |
| `kodama_id` domain, `kodama_tenant_id()` | Same syntax as `domain.IDPattern`; tenant context reader |
| `tenants` | unique `key`; runtime: SELECT |
| `projects` | PK `(tenant_id, id)`, unique `(tenant_id, key)`, `settings jsonb`, `active_wip_limit >= 1`; runtime: SELECT/INSERT/UPDATE |
| `users` | global; unique `(issuer, subject)`; runtime: SELECT, visible only for members of the context tenant |
| `memberships` | PK `(tenant_id, id)`; FK `(tenant_id, project_id)` to projects; role in admin/analyst/engineer/reviewer/viewer; unique `(tenant_id, user_id, project_id, role)` with NULL project (tenant-wide) treated as equal; runtime: SELECT/INSERT/UPDATE/DELETE |
| `audit` | PK `(tenant_id, id)`, identity `seq`; actor/action/resource/version/reason/request id/size-bounded object `metadata`; FK `(tenant_id, project_id)`; runtime: SELECT/INSERT only, no UPDATE/DELETE/TRUNCATE and no policy for them |
| `kodama_discover_memberships(issuer, subject)`, `kodama_list_tenant_ids()` | SECURITY DEFINER, pinned `search_path`, EXECUTE only for the runtime role |
| `schema_migrations` | Runner bookkeeping; no runtime access |

All tables: `ENABLE` + `FORCE ROW LEVEL SECURITY`, tenant policy with `USING`
and `WITH CHECK`, owned by the migration role. An `owner_*` policy
(`TO CURRENT_USER`) lets the migration role seed data and lets the definer
functions read across tenants; the migration role is never used to serve
requests. Times are `timestamptz` (UTC sessions); IDs are opaque text; versions
are integers 1 to 2^53-1. Foreign key checks bypass RLS, so composite keys
reject cross-owner references even when the writer cannot see the target row.

## Contributing migrations (module tasks)

1. Put SQL files in your module, for example `internal/work/migrations/`, and
   embed them: `//go:embed migrations/*.sql`.
2. Name each file `T<task>_<seq>_<name>.sql` (`T011_001_work.sql`); the runner
   rejects other names. IDs and contents are **immutable once merged**; fixes
   are new files.
3. Export `func Migrations() storage.Contribution { ms, _ := storage.LoadFS(fs, "migrations"); ... }`.
   Files inside a contribution must be ascending by ID.
4. Own tables get tenant/project columns, composite foreign keys, `ENABLE` +
   `FORCE ROW LEVEL SECURITY`, a policy on `kodama_tenant_id()`, and minimal
   `GRANT ... TO {{runtime_role}}`. The runner replaces `{{runtime_role}}` with
   the quoted `MigrateOptions.RuntimeRole`. Add an `owner_all ... TO
   CURRENT_USER` policy only if a definer function or admin seeding needs it.
5. Statements must be valid inside a transaction block (no `CREATE INDEX
   CONCURRENTLY`). Each file plus its `schema_migrations` row commits atomically.
6. The composition root (T-027) lists contributions in dependency order; base
   comes first. The runner rejects duplicate IDs, a modified applied migration,
   an applied ID missing from the manifest, and a pending migration ordered
   before an applied one. A concurrent runner waits on a database-scoped
   advisory lock (key `0x4b4f44414d41`).

## Running the checks

Pure unit tests need no database. PostgreSQL tests need an administrator URI
(superuser) because Compose does not publish PostgreSQL to the host; the
helper in `test/support/postgres` creates a `t002_<random>` database, owner and
runtime roles per test and drops them afterwards.

```sh
# Without PostgreSQL: unit tests run, database tests skip with a message.
go test -race -count=1 ./internal/storage/... ./test/support/postgres/...

# With PostgreSQL (required: unset URL or failures become test failures).
export KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable'
export KODAMA_REQUIRE_POSTGRES=1
go test -race -count=1 ./internal/storage/... ./test/support/postgres/...
```

Named acceptance checks (top-level test names, selectable by `-run`):

| Pattern | Covers |
| --- | --- |
| `go test -run pooled_tenant_context ./internal/storage/` | IT-04, IT-05, IT-A03 |
| `go test -run cross_owner_fk ./internal/storage/` | IT-02, IT-04, IT-A02 |
| `go test -run migration_reapply ./internal/storage/` | ordered idempotent apply, duplicate IDs, checksum, atomic failure |
| `go test -run 'runtime_role_privileges\|non_tenant_paths' ./internal/storage/` | IT-04 grants, discovery paths |

## Requirement mapping

| Requirement | Where |
| --- | --- |
| IT-02 ownership enforced on all relations | composite FKs; `Test_cross_owner_fk` |
| IT-03 | out of scope here (authorization per access path) |
| IT-04 tenant predicates + RLS, no owner/bypass | policies, `FORCE`, grants; `Test_runtime_role_privileges`, `Test_pooled_tenant_context` |
| IT-05 pool reuse clears context; FKs include ownership | transaction-local `set_config`; `Test_pooled_tenant_context` (pool of one connection, commit/rollback/error/panic/cancel, concurrency) |
| IT-A02, IT-A03 | `Test_cross_owner_fk`, `Test_pooled_tenant_context` |
| SD-04 persistence preserves tenant predicates, RLS, ownership, tx atomicity | `WithTenant`, schema, migration runner |
| SD-A03 PostgreSQL checks of isolation and pooled context | the tests above against real PostgreSQL |

## Open issues for T-027 composition

- **Manifest ordering:** `internal/app` must call `NewManifest(storage.BaseContribution(), ...module contributions in dependency order)`. No module exports a contribution yet.
- **Runtime role name:** the runner needs `MigrateOptions.RuntimeRole` (dev: `kodama_runtime`; the role must already exist, creation is the environment's job). It is not read from the DSN; either add a configuration setting or derive it from the runtime URL user. The runner fails if the role is missing rather than silently skipping grants.
- **Two DSNs:** per `dev/configuration.md` the server reads `database-runtime-url` (kodama_runtime) for `storage.Open` request pools and `database-migration-url` (kodama_migration, owner) only for a short-lived pool passed to `Migrate` at startup/upgrade, then closes it. The migration role must not serve requests; its owner policies bypass tenant isolation. The probe Pod never receives the migration URL.
- **Configuration names:** the scaffold configuration contract does not yet define the DSN file paths or pool sizes; T-027/T-001 owners must add them.
- **Tenant/user provisioning:** the runtime role has no INSERT on `tenants` or `users`. T-010 (user mapping, first login) and T-056/bootstrap need a definer function or operator path contributed through their own migrations.
- **Offboarding:** audit has no delete path for the runtime role; T-056 retention/deletion needs an explicit owner-side policy.
