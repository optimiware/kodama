// Package runstate holds the shell's adapters for the kernel run-state port,
// runtime.StateStore (internal/kernel/runtime).
//
// Memory is the first adapter. It is built on internal/memdb and exists for
// tests and for the `development` environment's executable-contract evidence:
// it proves the port's policy (claims and fences, optimistic versions, the
// outbox committed with state, wait decisions, signal identity routing,
// cancellation and cleanup) over retained state. It is never a production
// fallback: it persists nothing, shares no state between processes, and says
// nothing about PostgreSQL row security, locking or restart recovery, which the
// persistence task qualifies against the same shared suite
// (test/support/kernelcontracts).
//
// Every operation is scoped by tenant and project. A run, wait, signal or
// outbox record that belongs to another tenant or project is reported exactly
// as one that does not exist: a *domain.Error with domain.CodeNotFound for a
// run, the zero lookup for a signal identity, and unknown_wait for a signal
// that addresses a wait outside the caller's scope.
package runstate
