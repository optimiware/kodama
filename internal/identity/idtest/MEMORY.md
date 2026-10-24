# Identity: what the memory adapter does not model

`Mem` is the in-memory `identity.Repository`, `Directory`, `PrincipalMapper` and `AccessReader`, held to `Contract`. It is flagged development-only (`DevelopmentOnly() bool`; SD-12).

Modeled over `memdb`: every tenant record (projects, bindings, authorization revisions, audit rows, recorded changes) lives in the kernel. `Transact` is one `memdb.Update`, so the state, the revisions, the audit rows and the `membership.changed` / `project.settings_changed` events appended through the event bus (`NewMemOn(kernel, eventstest.Mem)`) commit or roll back together, as `postgres.Record` writes them in its tenant transaction. `TransactTx(ctx, tx, fn)` joins an identity mutation to a caller's own `memdb.Tx` and `AuthorityIn(tx)` is the twin of `postgres.NewTxAuthority`. The contract asserts the committed events and audit rows and the rollback of state, revisions, audit and events when an event append fails; a memory test covers the joined transaction.

Deliberately not modeled:

- **The global registry.** Users and the tenant list are global in PostgreSQL (`SECURITY DEFINER` functions); `Mem` keeps them in a small registry outside the kernel. `MapPrincipal`, `AddUser` and `AddTenant` are never rolled back with a tenant unit.
- **`Changes`.** The `Changes` field (kept for the HTTP tests) is appended only by a top-level `Transact` after its commit; `TransactTx` and `ChangesOf` read the authoritative kernel table.
- **Foreign records.** `InsertBinding` and `InsertProject` refuse a record of another tenant as `ErrInvalid`, and updates change only what PostgreSQL's `UPDATE` changes (role and version; display name, settings, WIP limit, update time and version), so ownership, scope, key and creation time are immutable. The contract asserts both, with state, audit and events unchanged in both tenants.
- **Lock order.** `LockProject` and `LockAuthorization` only validate existence; the PostgreSQL adapter's ordering errors (`errOrder`), row locks and `FOR SHARE` bindings are SQL obligations.
- **Bootstrap** (`postgres.Store.Bootstrap`) has no memory twin.
- **Directory visibility by function.** PostgreSQL reads users and tenants through definer functions filtered by the principal; `Mem` filters in code.
- **Restart persistence.** Nothing survives the process.
