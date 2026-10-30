# Repositories: what the memory adapter does not model

`MemStore` (`repository.Store`) runs `Contract` over `memdb` with no database (SD-13); `MemEnv` pairs it with the in-memory event bus. It is flagged development-only (`DevelopmentOnly() bool`; SD-12). `MemStore.InsertInTx` and `UpdateInTx` take the caller's `memdb.Tx` as the PostgreSQL adapter's take their `pgx.Tx`.

Deliberately not modeled:

- **Project ownership.** Memory holds no project table, so a repository for an unknown project is accepted; the composite foreign key to `projects` is PostgreSQL's (the real-SQL isolation test proves it).
- **Row locks and isolation.** Writers are serialized by `memdb`; there is no row lock, lock order or `FOR UPDATE`. The contract asserts outcomes (one repository for concurrent creators), not how they were serialized.
- **Schema constraints and the guard trigger.** The status, reason, external-shape and version checks of `007_git_repositories.sql`, the immutable columns, the observation that may advance without a version bump, and the runtime role's column grants are SQL. Memory enforces the entity's own `Validate` on every write.
- **Audit detail.** Entries keep the action, actor, reason, request ID and version; the metadata map and `occurred_at` are not stored, and the contract reads the sorted actions.
- **Restart persistence.** The store vanishes with the process.
- **The worker.** `Hosts` here is a fake with scripted failures; real initialization, refs and the tenant volume belong to T-128/T-129.
