// Package testkit provides deterministic, in-process fakes for the Kernel
// ports.
//
// The fakes exist so Kernel unit tests can drive every K-RECOVER, K-STOP and
// K-WORKSPACE schedule without wall-clock sleeps, real processes, containers,
// databases, brokers or network access. They model the contracts rather than
// hiding uncertainty: an unknown start outcome stays unknown, a stop
// acknowledgment is never promoted to confirmation, absence is never proof of
// nonexecution, and an unretained or lost workspace is never recreated.
//
// The fakes also detect incorrect callers. Replayed starts, stops without a
// prepared identity, removals before a confirmed stop, commits with stale
// expected versions, invalid state transitions and malformed log reads return
// typed errors instead of silently succeeding. State is persisted by the
// Store, so a new State handle simulates a controller restart while the fake
// runtime and workspace records stay stable.
package testkit
