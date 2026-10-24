# Work: what the memory adapter does not model

`MemStore` runs `Contract` over `memdb` with no database (SD-13) and is flagged development-only (`DevelopmentOnly() bool`; SD-12). One `Insert` or `Update` is one `memdb.Update`: item, history entry, audit row and the event appended through `eventstest.Mem.Append(tx, …)` commit or roll back together, and `MemAuthority.Guards` reads grants through the same `Tx`. `LockActionable(tx, …)` takes the caller's `Tx` as `workpg.LockActionable` takes its `pgx.Tx`.

Deliberately not modeled:

- **Aliasing.** `Insert` and `Update` return a deep copy (criteria, evidence links, review subject, completion), so mutating a result never changes the committed row; the contract mutates returned values and rereads.
- **Locks.** Writers are serialized by `memdb`, so there are no project or item row locks, no `FOR SHARE` admission lock and no lock order. A `LockActionable` check followed by a reservation in the same `Update` is atomic; nothing is claimed about concurrent PostgreSQL transactions.
- **Constraints the schema owns.** Foreign keys, RLS policies, the `work_item_history_version_key` unique index and column types are SQL. Memory enforces the project check and the history-version uniqueness by explicit tests in the adapter, not by schema.
- **Memberships and projects.** `SeedProject` and `AddMember` stand in for identity's tables; the contract does not assert assignee eligibility because the PostgreSQL fixture has no membership seeding (a memory-only test covers it).
- **Audit queries.** Audit rows are written for parity and never read back.
- **Unknown commit outcome** is produced only by `memdb.WithCommitFault`; there is no connection to lose.
- **Restart persistence.** Nothing survives the process.
