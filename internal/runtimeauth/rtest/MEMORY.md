# Runtime authority: what the memory adapter does not model

`MemStore` and `MemAttempts` run `Contract` over `memdb` with no database (SD-13) and are flagged development-only (`DevelopmentOnly() bool`; SD-12). `Store.Do` is one `memdb.Update`; `LockAttempt` reads the authoritative attempt row through the same `Tx`; `MemAttempts.Cancel` revokes the attempt's grants in the owner's own `Update` through `RevokeAttemptMem`, as `postgres.RevokeAttempt` joins the owner's `pgx.Tx`. `MemStack` runs the real `runtimeauth.Service` and the real JWT signer and verifier on them.

Deliberately not modeled:

- **Locks.** There is no `FOR SHARE`/`FOR UPDATE` and no lock order. Serialized writers make a cancellation commit before or after a whole admission or refresh, which is the only ordering claim; the PostgreSQL checkpoint (T-026) owns the interleavings.
- **Constraints.** The schema's triggers (immutable registration, write-once consumption, irreversible revocation), column grants, check constraints and the project foreign key are SQL. Memory enforces the observable outcomes (`ErrConflict` on a reused credential or a second consumption, a kept first revocation) in the adapter.
- **Global session uniqueness.** `runtime_grants_session_unique` spans tenants; memory enforces it per tenant (a collision of 128-bit sessions is not a scenario). The credential digest is unique across tenants in both.
- **Locate as a definer function.** PostgreSQL resolves a digest through a `SECURITY DEFINER` function outside a tenant context; memory keeps a process-local digest index and confirms the grant in its tenant, so a rolled-back registration is unknown.
- **Unknown commit outcome** comes only from `memdb.WithCommitFault`; the contract does not inject it (the fault wrappers of `rtest.Faults` still apply to the service).
- **Restart persistence.** Nothing survives the process, so a restored database resurrecting old grants cannot be exercised.
