// Package memdb is the single in-memory transaction kernel behind the
// `development` environment (ADR 0009, docs/implementation/environments.md).
// Module adapters are thin tables over it; none implements its own locking or
// rollback.
//
// A Store owns all state. Store.Update runs a function over a tenant-bound Tx
// on a copy-on-write snapshot and publishes every write at once when the
// function returns nil; any error, panic, cancelled context or injected commit
// fault discards all of it. Store.View reads the last published state.
//
// A Tx belongs to exactly one tenant, and a Table can only be reached through a
// Tx, so no caller can read or write another tenant's rows: this is the
// in-memory counterpart of row-level security. Cross-module joins pass the
// caller's Tx to the peer, the counterpart of passing a pgx.Tx. A peer that
// opens its own Update has escaped the caller's transaction exactly as with
// another WithTenant: it commits or fails independently, and it does not see
// the caller's uncommitted rows.
//
// Writers are serialized. That gives deterministic isolation and nothing more:
// there are no row locks, no SKIP LOCKED and no lock order, and the kernel
// claims no isolation level. It is not evidence for SQL, RLS, locking or lock
// order (SD-14); those obligations stay with PostgreSQL. State lives only in
// the Store; there is no persistence file and no restart persistence.
//
// Values are stored as given and returned as stored. Use value types, or treat
// values as immutable, because the kernel does not deep-copy them.
package memdb
