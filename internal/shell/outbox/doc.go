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
