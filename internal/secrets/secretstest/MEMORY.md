# Secrets: what the memory adapters do not model

`MemStore` (`secrets.Store`, `VariableCatalog`, `SecretCatalog`, with the pin seam) and `MemVersionSource` (`secrets.VersionSource`) run `StoreContract` over `memdb`, and `EphemeralWrapper` (`secrets.KeyWrapper`) runs `WrapperContract` together with the keyfile provider (SD-13). All are flagged development-only (`DevelopmentOnly() bool`; SD-12): the ephemeral wrapper is the development KEK that production must refuse. `Atomically` is one `memdb.Update`; `RegisterPins` and `ReleasePins` take the caller's `memdb.Tx` as `postgres.RegisterPins` takes its `pgx.Tx`, so the run admission joins pins to its own unit. Key-provider calls happen before a unit starts, exactly as with PostgreSQL.

Deliberately not modeled:

- **Locks.** There is no `FOR SHARE`/`FOR UPDATE` on the secret, version, pin or scope-key rows and no lock order. Serialized writers make pin registration, deletion, revocation, pin release and resolution finalization totally ordered, which is the only ordering the contract asserts; interleavings between concurrent PostgreSQL transactions (T-026) are not covered.
- **Constraints.** Foreign keys (an unknown project fails in PostgreSQL, not in memory; a pin's run owner is added by T-020), RLS, column types and the composite foreign key from a value to its scope key's generation are SQL. Memory keeps uniqueness of names per project (a deleted name stays reserved), of a scope-key generation per tenant and of a handling effect per consumer, and checks that a stored value names an existing scope key and generation.
- **Create-only collisions.** `CreateVariable`, `CreateSecret` and a new secret version refuse a reused primary key without replacing the row; the contract asserts the refusal and unchanged state, not PostgreSQL's raw error.
- **Variable history.** PostgreSQL keeps `variable_versions`; nothing reads it through a port, so memory does not store it.
- **The HTTP idempotency record.** `postgres.Keyed` shares the unit with the idempotency table; the memory counterpart belongs to the HTTP adapter task, not here.
- **Audit detail.** Audit rows keep the action, resource, version, actor and safe metadata; `occurred_at`, request ID and reason are not stored, and the contract reads only the actions.
- **The keyfile's custody.** `EphemeralWrapper` generates its KEKs in the process: a wrap dies with the process, no key file exists, and a restored backup cannot be unwrapped. It models the wrapper's failure classes (`KeyUnavailable`, `KeyNotFound`, `KeyCorrupt`) and the context binding, not file permissions or retained-key custody.
- **Restart persistence.** Nothing survives the process.
