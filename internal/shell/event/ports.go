package event

import "context"

// ReadPort reads committed events for the authorized event.read operation.
//
// Read is scope-qualified and cursor based: it returns only committed rows in
// durable sequence order, never another scope's events, and reports the next
// cursor and whether more committed rows exist. It is a read seam, never a
// publication or mutation split.
//
// Event rows themselves remain publication intent written inside each domain
// commit unit (BindingUnit, TransitionUnit, MutationUnit, ...), never through a
// separate publish call that could commit before or after the business change.
// The retrying publisher, NATS subjects and live subscription mechanics belong
// to the adapters. event.subscribe setup returns the validated SubscribeGrant
// value; it does not add a second event store here.
type ReadPort interface {
	Read(context.Context, ReadQuery) (ReadPage, error)
}
