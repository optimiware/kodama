// Package memory is the in-memory execution.Store: the run, step, attempt,
// bookkeeping, history, audit, evidence, reservation and cancellation tables
// over the memdb transaction kernel, with the same transaction semantics as the
// PostgreSQL store (internal/execution/postgres): state, history, audit and
// outbox rows of one call commit together or not at all, and the peer joins
// (work admission, workspace writer and retained-source, secret pins, events,
// runtime grants) receive the caller's memdb.Tx, exactly as the PostgreSQL
// store passes its pgx.Tx.
//
// The package also holds small in-memory peers (World) so the execution use
// cases run end to end with no service: a work catalog, workspaces with a
// single writer slot and retained-source pins, a secret catalog with exact
// version pins, a table authority and an event outbox. They are fakes of the
// consumer-owned seams, not implementations of the peer modules.
//
// What the memory store deliberately does not model: row locks, SKIP LOCKED
// (ErrBusy for a contended run never happens), lock order, isolation levels,
// row-level security, database triggers beyond the terminal-immutability and
// pinned-column guards written in Go, restart persistence and database
// constraint error classes. Those obligations stay with PostgreSQL (SD-14) and
// are exercised by the explicit `test` environment run of the same contract.
// It must never be selected in production.
package memory
