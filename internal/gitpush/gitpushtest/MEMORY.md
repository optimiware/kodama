# Push records: what the memory adapter does not model

`MemStore` (`gitpush.Store`) runs `Contract` over `memdb` with no database (SD-13); `MemEnv` pairs it with the in-memory event bus and the gitauth memory store on one kernel, so the seed consumption joins the push transaction (`gitauth/memory.Bind`). It is flagged development-only (`DevelopmentOnly() bool`; SD-12).

Deliberately not modeled:

- **Row locks and isolation.** Writers are serialized by `memdb`; there is no `FOR UPDATE`, advisory seed lock or lock order. The contract asserts outcomes (one seed receive of several concurrent ones, one event), not how they were serialized; the SQL proof is the PostgreSQL run.
- **The repository row hold.** `Tx.RepositoryState` answers from the stand-in repository facts and is not held against concurrent change; the real `FOR SHARE` serialization is proven only by `Test_postgres_authorization_serializes_with_repository_change`. The grant-row lock of commit intent is likewise memdb's writer serialization here and a row lock in `Test_postgres_commit_intent_serializes_with_cancellation`.
- **Schema constraints and guard triggers.** The foreign keys to repositories, the check constraints, the entry transition trigger, the immutable columns, the version step, the monotone journal head and the runtime role's column grants are SQL (`Test_tenant_isolation_and_guards`). Memory enforces the entity's own transition table.
- **Row level security.** Tenant separation is the kernel's per-tenant tables.
- **Audit detail.** Rows keep action, actor, reason, request ID, version and metadata; `occurred_at` is not stored.
- **Restart persistence.** The store vanishes with the process.
- **The worker.** `Worker` is a scripted fake: tests append the journal entries the hooks would write; real hooks, locks, liveness and the volume belong to T-128/T-129.
