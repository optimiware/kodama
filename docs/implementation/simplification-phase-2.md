# Simplification Phase 2 handoff

**Branch/worktree:** `task/simplification-v2`, `.work/simplification-v2`; retained for all phases at the owner's request.

**Baseline:** Phase 1–2 work was rebased onto `main` at `d4240782` (T-115 run resources, T-121 profile qualification, ADR 0012 and the container-SDLC slice). The original worktree base was `74096e2623168db4502c3f2eff64721377be578e`. The rebase kept this branch's rewritten specifications, kept ADR 0012 as superseded history with a banner, preserved O-34, and marked the ADR-0012-derived spec, contract and handoff as historical rather than current direction. The schema cutover remains the intentional prerelease reset described below; it is not a backwards-compatible migration. Review/merge remains pending.

## Delivered

- Two-file PostgreSQL baseline: 14 domain tables and required audit/outbox (15 total), scoped keys, RLS, null-safe uniqueness, service scopes, immutable run snapshots and evidence, encrypted-secret envelopes and metadata-only artifacts.
- Application migration manifest uses only that baseline (`001_schema.sql`, `002_grants.sql`); T-167 later appends the single Git hosting contribution `003_git_hosting.sql`, so the manifest is now three files. Existing database checksums are refused without modifying old data; no deployed database has been reset.
- Shared ID and optional owner values; project-defined task workflow/entity with optimistic transitions, parking, explicit decisions and guarded resumption.
- Task transaction port with PostgreSQL and memdb adapters; one shared contract tests rollback, concurrent updates and tenant/project isolation.
- `memdb.NewClonedTable` isolates mutable JSON/slices/pointers so editing a caller value cannot mutate committed state or escape rollback.
- A concrete startup guard prevents the existing server's old SQL adapters from starting workers against the new schema.

## Contract refinements

The Phase 1 inventory counted 14 while listing 15; the audit/outbox is required and the count is corrected. Pipeline definitions have one authoritative JSON document, with no separate raw-YAML or triggers mirror. Runs snapshot definitions/inputs rather than reading mutable pipeline rows. Step rows have no agent-specific kind. Repository paths are derived from owner keys and relative names.

Parking retains the project's business state with explicit parking fields. This permits any project vocabulary. Resolution includes an outcome: closing a rejected promotion request cannot authorize deployment. State triggers are stored once on the workflow, including initial entry and same-state resumption.

These refinements update [schema](database-schema.md), [task behavior](../specs/work-management.md) and the ADR. They preserve the owner's two-layer direction.

## Phase 3 boundaries

The new schema and task stores are executable; the existing HTTP/CLI/MCP, scheduler,
Git indexer, secrets/gateway adapters and NATS publisher/consumers still use the old
model. Phase 3 ports these consumers and wires the new task port, exact run snapshot
admission, role-authorized resolutions and trigger deduplication. The memory task
adapter requires same-transaction repository/user reference peers when those optional
fields are used; absent peers reject them. No generic allow-all peer is installed.

The schema represents current-only secret values: admitted revision references fail
if rotation makes that revision unavailable. Service lifecycle, run resource recovery,
artifact destination verification and safe external-effect retry remain explicit
implementation work. A smaller schema does not provide these behaviors by itself.

## Verification

Task-local PostgreSQL 17.11 runs in Podman, using the image pinned by `dev/compose.yaml`.
Tests create their own databases and owner/non-owner roles and remove them afterward.

```sh
# Set KODAMA_TEST_POSTGRES_ADMIN_URL to the isolated task instance.
KODAMA_REQUIRE_POSTGRES=1 go test -race -count=1 ./internal/domain/... ./internal/memdb/... ./internal/storage/... ./internal/tasks/...
KODAMA_REQUIRE_POSTGRES=1 go test -race -count=1 ./internal/app -run 'TestMigrationBaselineIsThreeFiles|TestSimplifiedManifestMigration|TestMigrateRefusesUnsafeIdentities'
go vet ./internal/domain/... ./internal/memdb/... ./internal/storage/... ./internal/tasks/... ./internal/app/...
make build
make docs-check
git diff --check
```

**Results:** all commands above passed on this worktree. PostgreSQL checks ran
against the real task-local instance with `KODAMA_REQUIRE_POSTGRES=1`; they were
not skipped. Documentation validation passed across 148 files. The binary builds.
The temporary PostgreSQL container is removed after verification.

Legacy product-wide integration suites depend on the removed schema and are
Phase 3 porting work; passing the scoped checks above is persistence evidence,
not acceptance of an assembled new server. Independent review and merge remain
pending; the branch/worktree is retained for Phase 3.
