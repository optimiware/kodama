// Package postgres holds the PostgreSQL-facing adapters of the results module.
// It adds no table and no migration: accepted results live in execution's
// attempts and publication receipts in the workspace module. What it provides
// is the join between three owners' existing transactions:
//
//   - AttemptAuthority maps execution's attempt rows into runtimeauth's
//     AttemptState (the T-019 port T-020 left to the integration), including
//     the row lock that conflicts with cancellation;
//   - ExecutionStore decorates execution's store so that a result commit
//     repeats runtimeauth.CheckSession inside the mutation, after the run lock
//     and before any peer effect (execution, then grant, then workspace);
//   - WorkspaceStore does the same for the mutation that records a new
//     publication intent, checking the session under the execution lock before
//     entering the workspace row lock.
//
// Both decorators guard only operations whose context carries a session
// (WithSession); trusted background reconciliation keeps using the owners'
// undecorated paths.
package postgres
