// Package memory is an ephemeral in-process development persistence adapter and
// a conformance fixture for the neutral store.Executor commit-unit seam.
//
// It exists so Shell capabilities and the adapter conformance vectors can run
// without a database in development and in unit tests. One Store owns one shared
// committed state: every Execute validates the whole unit, stages it against a
// transactional copy of that state and publishes it all-or-nothing, so a refused
// or failed unit leaves no business row, receipt, audit row or event publication
// intent visible.
//
// The same store also implements the system seams directly: Store.Controller
// is the cross-scope Kernel-state/trigger-event seam (scope derived from the
// execution row inside the transaction), Store.LookupCredential is the exact-row
// pre-authentication lookup, and Store itself implements event.Outbox against
// the committed event table. Receipt completion is a compare-and-set on the
// durable completion marker, and a DeadlineGuard compares a stored timestamp
// against the store's injected clock inside the same atomic section.
//
// Declared uniqueness is mirrored from the schema declaration, the same
// source the write order is derived from. Every unique constraint, and every
// unique index declared without a predicate, holds here. Apart from the three
// groups below, a rule is checked for the row each insert stages and each
// update of one of its columns stages, against every other row of the table in
// any scope. That is the point at which PostgreSQL checks a constraint that is
// not deferrable, so the outcome depends on the unit's write order exactly as
// it does there: a delete followed by an insert of the same key commits, while
// an insert followed by the delete, or two updates that swap a key, is refused.
// A row with NULL in a constrained column conflicts with nothing unless the
// constraint is declared NULLS NOT DISTINCT. A violation is version_conflict,
// names the constraint and never a row value, and rolls the whole unit back. A
// receipt replay is resolved before any write is staged, so a replayed unit is
// never reported as a violation.
//
// Three groups of declared rules are satisfied without that check. A rule whose
// columns include the whole primary key (the *_scope_id and *_tenant_id rules)
// is implied by the primary key, which every insert already enforces.
// receipts_identity is the receipt replay index itself, which also implies the
// partial index receipts_resource_report_dedupe. events_publish_sequence is the
// store's own publication counter.
//
// What is not mirrored: a partial unique index whose predicate is SQL text
// (actions_one_running), foreign keys, check constraints and the NOT NULL of a
// column a writer omits. A unit those rules would refuse commits here.
//
// This adapter is deliberately ephemeral. Dropping the *Store drops its state,
// and two stores never share rows. It is not PostgreSQL: it does not prove
// row-level security, isolation levels, SQL constraint behavior, real locking or
// durable concurrent behavior. The PostgreSQL adapter and the later integration
// and qualification tasks own those proofs.
package memory
