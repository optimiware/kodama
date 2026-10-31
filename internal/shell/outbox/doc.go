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
//   - Coverage by the next eligible completed reconciliation. A pass starts at
//     the zero cursor with its own cutoff and reads every page with a fresh
//     READ COMMITTED snapshot. A row is skipped only by the pass whose cursor
//     passed its position before it committed. A pass that starts after the
//     commit and at least Grace after the row's creation
//     (created_at <= start - Grace) is eligible for it, and when that pass
//     completes it has attempted the row wherever its cursor began. The
//     guarantee is conditional: Run must keep being scheduled, and the store
//     must stay readable for the whole pass (a read error ends that tenant's walk
//     for the pass, and the row is then covered by the next eligible pass that
//     completes). It states no wall-clock bound. Run drains synchronously before
//     it checks ReconcileEvery, a drain, a page and a broker call have no
//     duration bound, and Interval may exceed ReconcileEvery, so a pass can
//     start well after ReconcileEvery. The attempt is a publication, not a
//     delivery: if the broker refuses it the row stays unpublished and the
//     following eligible passes attempt it again whatever its backoff.
//   - Drain eligibility is not guaranteed delivery. The drain does not use the
//     cursor: Pending orders by next_attempt_at, a column also set when the row
//     is inserted, so a late row is due at the first drain after its commit and
//     usually leaves with it. But Drain pages in (next_attempt_at, id) order,
//     and a page that fails stops the drain (the failed rows are deferred), so
//     earlier due rows can keep a late row from being attempted at the next
//     poll. Drain shortens the usual delay; only reconciliation is relied on.
//   - Single-publisher exactly once, at-least-once recovery. In the successful
//     case, one publisher with the store and the broker answering, a published
//     row leaves both queries and the publisher serializes its passes, so a late
//     row is published once and neither path repeats it. That is not a promise
//     of exactly-once delivery: a crash between the acknowledgment and the mark,
//     a lost acknowledgment, a failed mark and a second publisher process all
//     publish the row again under the same event id, which the consumers
//     deduplicate on. The at-least-once contract above is unchanged.
//   - Both queries are indexed keyset pages: audit_events_unpublished serves the
//     cursor page by a (tenant_id, created_at, id) range and audit_events_pending
//     serves the drain, and each returns at most Batch rows. A complete
//     reconciliation walks the whole eligible backlog page by page, and neither
//     the LIMIT nor the index promises bounded physical work or the planner's
//     choice of plan, so the bound is on rows returned per query, not on the
//     time of a pass.
//
// Grace is not a bound on transaction length: it only decides how old a row must
// be before Reconcile treats it as missed rather than still being written. A row
// that commits after a pass began is simply read by the next eligible pass.
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
