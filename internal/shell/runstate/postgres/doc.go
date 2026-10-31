// Package postgres is the PostgreSQL adapter of the kernel run-state port,
// runtime.StateStore (internal/kernel/runtime). It stores the authoritative
// run record in the durable columns and tables of migrations 004 to 006
// (internal/storage) and passes the same shared contract suite as the memory
// adapter (test/support/kernelcontracts), unchanged.
//
// # Where the record lives
//
//	Run, Plan   runs (status, reason, version, cleanup_*, retained_resources; the plan is definition_snapshot)
//	Claim       runs (lease_owner, lease_purpose, lease_fence, lease_expires_at)
//	Step, Wait  run_steps (one row per step; the wait is the wait_* columns of its step, recorded at admission)
//	Outputs     run_steps.outputs, written in the commit that settles the step as succeeded
//	Command     run_commands
//	Workload    run_workload_observations
//	Resource    run_resource_observations
//	Material    run_materializations
//	Signal      signal_records (append-only, non-expiring)
//	Outbox      audit_events (resource_type run; the event is the payload)
//
// The store keeps no state of its own. Every operation reads the record from
// the rows, applies the kernel's pure rules to it and writes the difference;
// the guard triggers and checks of the migrations are a second fence that
// refuses what the Go rules did not see (a fence that goes backwards, a settled
// step that moves, rewritten outputs, a decided wait that changes).
//
// # Transactions, locks and recovery
//
// Every operation is one storage.WithTenant transaction: the tenant is set for
// row level security (forced, non-owner role) and every statement also names
// the tenant and project. A state change and the outbox events that describe it
// are written by the same transaction; if it does not commit, neither exists,
// and a retry of the same transition (same event ids) writes each event once.
//
// Writers of one run take its row lock first (SELECT ... FOR UPDATE), so claim,
// renewal, release, commit, cancellation and signal decisions are serialized per
// run and a loser re-reads what the winner committed: concurrent claimants have
// one winner, a signal and a cancellation decide a wait once, and a superseded
// engine is refused by fence before anything is written. DecideSignal also takes
// an advisory lock on the signal identity, so duplicates replay the first
// record, and locks the runs it can reach in run order. A deadlock or
// serialization abort is retried from a fresh read. Nothing waits for a lease.
//
// A restarted or replacement process needs only the database: the lease, the
// fence, commands, observations, outputs and materialization identities are all
// rows, and Runnable lists the work from them.
//
// # Times
//
// PostgreSQL keeps microseconds. The adapter rounds every time it receives down
// to the microsecond (UTC) before comparing or storing it, and returns what it
// read back, so a record equals what a later Load returns.
//
// # Known gap
//
// signal_records has no column for the reason of a cancelled decision
// (run_cancelled or run_failed). A refusal that lost to a cancellation therefore
// recovers the reason from the wait the signal targeted, and fails with
// domain.ErrInvalidValue rather than guess once that wait no longer exists.
package postgres
