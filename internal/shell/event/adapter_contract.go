// Package event declares the durable-event publication seam.
//
// Event rows are committed as publication intent inside the same transaction as
// the business change. A Publisher consumes those already-committed rows; it
// never creates, reorders or replaces publication intent. A failed publish is
// retried with the same stable event ID and is never treated as proof the event
// did or did not commit.
package event

import "context"

// Publisher publishes one already-committed event envelope. Implementations
// must retain the envelope ID across retries and must not publish an envelope
// that was not committed by a domain transaction.
type Publisher interface {
	Publish(context.Context, Envelope) error
}
