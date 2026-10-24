# Artifacts: what the memory adapters do not model

`MemStore` (`artifacts.Repository` and `CleanupStore`) runs `RepoContract` over `memdb`, and `MemObjects` (`artifacts.ObjectStore`) runs `ObjectContract`, both with no service (SD-13). Both are flagged development-only (`DevelopmentOnly() bool`; SD-12). `MemEnv` wires the real `artifacts.Service` over them. `MemObjects` extends the minimal `Open/Put/Delete` store of the gates tests (`gatestest.MemObjects`) with the presigned transfers the service issues, served by a loopback endpoint in the process (`Serve`); the gates fixture keeps its own copy.

Deliberately not modeled:

- **Locks.** There is no per-attempt advisory lock, `FOR UPDATE`, `FOR SHARE` or `SKIP LOCKED`. Serialized writers make the existence check, the aggregate limit check and the insert one decision, and order a pin against a retention decision, but nothing is claimed about concurrent PostgreSQL transactions or about two workers racing for a deletion lease.
- **Schema constraints.** Composite foreign keys (an unknown project fails in PostgreSQL, not in memory), RLS and column types are SQL. Memory keeps uniqueness of `(attempt, name)` and of an object key within an artifact.
- **Identity collisions.** Artifacts, objects, pins and audit rows are keyed by (project, ID) within the tenant, as PostgreSQL's `(tenant_id, project_id, id)`: the same ID reserved in two projects coexists, and `Reserve` refuses a reused ID inside one project without changing anything. `RetentionCandidates` pages by ID alone, as PostgreSQL does, so a same-ID artifact of a sibling project sorts by project only to break the tie.
- **The tenant list.** `Tenants` returns tenants that reserved an artifact through this store; PostgreSQL enumerates every tenant.
- **Audit rows** are written for parity and read back only through `AuditActions`; their request and metadata columns are not stored.
- **S3 behavior.** `MemObjects` does not sign: its instructions are opaque tokens served by the loopback endpoint, which binds the media type, the length and the content digest and refuses an expired or reused-for-another-purpose token with 403. It models no SigV4, no checksum header negotiation, no bucket prefix, no `MaxTransfer` cap and no conditional-write race (an existing key is refused, as `If-None-Match: *` does).
- **Restart persistence.** Bytes, grants and records vanish with the process.
