---
id: T-087
milestone: M1
status: done
depends_on: [T-003]
decision_blockers: []
owns: [internal/memdb/]
check: make check-go PKGS=./internal/memdb/...
resources: [go]
acceptance: [unit_memdb_atomic_update, unit_memdb_tenant_scope, unit_memdb_unique_conflict, unit_memdb_joined_peer, unit_memdb_commit_fault]
---
# T-087 — In-memory transaction kernel

## Goal
Give every in-memory adapter one small, correct transaction engine, so the `development` environment runs real use-case policy with no database ([ADR 0009](../adr/0009-environments.md)).

## Contract
`memdb` has two types and five calls ([environments](../implementation/environments.md#the-in-memory-kernel)): `Store.Update` and `Store.View` run a function over a tenant-bound `Tx`; `Table[V]` offers `Get`, `Put`, `Delete` and `Scan` over a `Tx`. `Update` works on a copy-on-write snapshot and commits only when the function returns nil. A `Tx` reads one tenant. `Put` returns `domain.ErrConflict` on a unique violation. `WithCommitFault` fails a commit. Writers are serialized; the kernel offers no row locks and claims no lock-order or isolation-level semantics.

Depend only on `internal/domain`. No persistence file, no query language, no reflection-driven schema, no SQL.

## Deliverables
The package, its documentation and the unit tests below. Module adapters are built by their owning tasks (T-088, T-080, T-081, T-083, T-084), not here.

## Acceptance
`unit_memdb_atomic_update` proves a failed `Update` after several writes leaves every table unchanged and a nil return publishes all of them at once. `unit_memdb_tenant_scope` proves a `Tx` cannot read or write another tenant's rows. `unit_memdb_unique_conflict` proves a duplicate unique key returns `ErrConflict` and changes nothing. `unit_memdb_joined_peer` proves a peer given the caller's `Tx` rolls back with it, and a peer that opens its own `Update` does not. `unit_memdb_commit_fault` proves an injected commit failure discards the update. Cover SD-12–SD-15 (SD-14 in particular) and SD-A12.

## Out of scope
SQLite or any file-backed store, restart persistence, concurrency semantics beyond serialized writers, module adapters and any claim about SQL, RLS or locking.
