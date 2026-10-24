// Package eventsv1 is the frozen v1 wire contract of Kodama domain events: the
// envelope, the payload of each initial event type, strict bounded decoding,
// validation, and canonical content digests.
//
// The authoritative semantics live in docs/contracts/event-catalog.md. This
// package contains no storage or broker code. It is deliberately separate from
// the runtime request contract (contracts/runtime/v1): runtime requests demand
// every field and explicit nulls; events follow the catalog instead (omitted
// tenant-wide projectId, nullable causationId, tolerated optional additions).
package eventsv1
