// Package outbox is the shell's publication path to the central bus
// (internal/natsbus): state and publication intent commit together in one
// transaction, a publisher drains the committed intent afterwards, and a
// reconciliation pass republishes intent that was committed but never reached
// the broker. PostgreSQL is the authority; the bus is a notification path
// (ADR 0002, C5 of the pipeline-runtime simplification plan).
//
// # Guarantees
//
//   - Atomic intent. Writer.Commit runs the state mutation and records the
//     events in one Store transaction; a failure of either leaves neither.
//   - At-least-once delivery. A row leaves the outbox only after the bus
//     acknowledged it durably. A crash between the acknowledgment and the mark,
//     or a lost acknowledgment, publishes the same event again.
//   - Stable identity (EC-01). EventID derives the event id from the tenant, the
//     event type and the type's deduplication key from the event catalog, so a
//     republication, a reconciliation and a retry all carry the same id.
//   - Missed wakeups are repaired. Reconcile republishes unpublished rows that
//     are older than a grace period regardless of their retry backoff.
//
// # Cursor and commit visibility
//
// A reconciliation pass pages through a tenant's unpublished rows by a cursor
// (Row.Cursor, the PostgreSQL adapter's (created_at, id)). In PostgreSQL
// created_at is fixed when the row is inserted (the transaction's start), not
// when the transaction commits, so a long transaction can commit a row whose
// position lies before a cursor that an earlier page has already passed. Within
// the pass that read the page, the row is invisible to the cursor.
//
// The choice is a bounded, documented grace window with a reconciliation
// guarantee, not a commit-time sequence. PostgreSQL assigns a sequence value at
// insert as well, so a sequence column has the same gap, and a true commit order
// needs a table written by the commit itself, a hot serialization point on
// every state change; the schema and the intent writer are owned elsewhere. The
// guarantee holds without either:
//
//   - A pass starts at the zero cursor with its own cutoff and reads every page
//     with a fresh READ COMMITTED snapshot. A row is skipped only by the pass
//     whose cursor passed its position before it committed: once. A pass that
//     starts after the commit and at least Grace after the row's creation
//     (created_at <= start - Grace) finds it wherever the cursor begins.
//     Reconcile therefore attempts every unpublished row at most one
//     ReconcileEvery after max(commit, created_at + Grace).
//   - The drain does not use the cursor. Pending orders by next_attempt_at, a
//     column that is also set when the row is inserted and is never later than
//     the first drain after the commit, so the drain delivers a late row at the
//     next poll. A row skipped by a page is normally already published by then.
//   - Neither path publishes it twice: a published row leaves both queries, and
//     a republication (a crash between acknowledgment and mark, a lost
//     acknowledgment) keeps the event id the consumers deduplicate on.
//   - Both queries stay bounded and indexed: audit_events_unpublished serves the
//     cursor page and audit_events_pending serves the drain, each ends in a
//     LIMIT, and a pass is a sequence of such pages, never a backlog scan.
//
// Grace is not a bound on transaction length: it only decides how old a row must
// be before Reconcile treats it as missed rather than still being written. A
// transaction that outlives Grace delays nothing for a row that was committed
// before the pass began and is simply read by the next pass.
//
// # Consumer rule
//
// Delivery can be duplicated, reordered and replayed, so a consumer MUST be
// idempotent: it deduplicates on the event id (Consumer with a Ledger) or, better,
// on the identity of the resource it owns (a run's trigger key, a wait id, a
// step key); it applies a version-carrying event only when the version is newer
// than the one it holds (Projection.Advance); it returns an error to leave a
// delivery for redelivery and nil only once the effect is durable; and it treats
// the event as a notification, reloading authoritative state for any decision.
package outbox
