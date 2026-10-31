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

## Pipeline-runtime expand (T-179)

`migrations/004_pipeline_runtime_expand.sql` expands and validates; it drops nothing.
It adds the run lease/fence, resource identity and cleanup state, the step kind,
workload identity, command intent, workspace exclusion and durable wait columns, the
task pipeline/current-run/projection fields (nullable), and `signal_records`: the
append-only, non-expiring, self-contained signal identity removed only with its
project. Validation refuses duplicate `(tenant, project, name)` pipelines and
ambiguous task-to-pipeline mappings before any change, never inventing a pipeline;
`tasks.current_run_id` is bound to its own task and project by a composite key. See
[schema documentation](../../docs/implementation/database-schema.md#pipeline-runtime-expand-and-validate-004).

## Wait decisions and materializations (T-189)

`migrations/006_wait_decision_and_materializations.sql` is expand-only. It lets a complete
wait decision be stored while its step has not settled (the kernel decides a wait
independently of its step) and keeps a decided wait immutable and closed; it adds
`run_materializations`, the tenant-isolated durable identity, issue time and
revocation of a step attempt's secret material, with `revoked` final; and it guards
`run_steps.outputs` so recorded outputs cannot change after the settling commit, and
adds the partial index `audit_events_unpublished` for bounded outbox pages. See
[schema documentation](../../docs/implementation/database-schema.md#wait-decisions-and-materializations-006).

## Signal decision reason (T-192)

`migrations/007_signal_decision_reason.sql` is expand-only. It adds the nullable
`signal_records.decision_reason` (`run_cancelled` or `run_failed`) so a refused signal
that lost to a cancellation replays on the record alone, after ordinary wait and run
retention has removed the wait row. A new cancelled record requires its reason and no
other record carries one; the constraint is `NOT VALID`, so an existing database
upgrades without rewriting history. Retention, project-scoped removal, grants and row
security are unchanged. See
[schema documentation](../../docs/implementation/database-schema.md#signal-records).

## Transactions and migrations

`WithTenant` validates the tenant ID, begins a pgx transaction, sets
`kodama.tenant_id` transaction-locally and commits only when the callback succeeds.
Errors, panics and cancellation roll back. Pooled connections retain no tenant
context. Callbacks pass their transaction to peers and perform no network I/O.

`BaseContribution()` embeds the baseline SQL files (schema, grants, the Git hosting contribution and the pipeline-runtime expand contribution `004`). `NewManifest` validates
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

The `Test_unit_storage_pipeline_*` tests are infrastructure-free checks of the
expand migration's text. Their PostgreSQL counterparts, `Test_pipeline_runtime_expand_postgres`
and `Test_pipeline_name_unique_postgres`, execute the constraints, refusals, forced
RLS under the non-owner role and rollback, and skip in the unit layer.

Coverage includes pooled context/rollback, RLS, runtime privileges, scoped FKs,
null-safe uniqueness, service scope checks, immutable run snapshots, trigger and
request deduplication, append-only audit payloads, replayable migration application,
and refusal of an old baseline without data loss.
