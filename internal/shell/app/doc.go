// Package app is the one Shell composition root.
//
// It assembles the completed Shell capabilities over an explicit adapter
// profile and owns nothing but wiring and lifecycle:
//
//   - ProfileDevelopment is the local composition: memory state, the Process
//     runtime, ephemeral keys, an in-process broker and an explicitly named
//     development authenticator plus a first-admin seed. Every ephemeral or
//     weaker adapter is named and logged as development-only.
//   - ProfileTest is either the same composition with every adapter supplied
//     by the caller, so a test can use deterministic fakes without a host
//     effect, or, with Config.Test, the durable single-host test environment
//     (PostgreSQL, NATS, the Podman runtime, a local keyfile and OIDC humans)
//     built by the same durable builder production uses.
//   - ProfileProduction requires the real adapters (PostgreSQL, NATS, the
//     Kubernetes runtime, a cloud KMS, OIDC and a registry) and refuses every
//     development or test adapter, a static authenticator, the first-admin seed
//     and an in-memory store.
//
// New builds the dependency graph, performs startup preflight and seeds the
// first administrator through the store seam; it starts no listener and no
// background loop. Run and Serve start the Kernel reconcile loop, the event
// outbox dispatcher, the push scanner and the optional HTTP listener. Close is
// concurrent-safe, idempotent, joins every owned goroutine, stops only the live
// workloads of the process runtime before closing the driver, and deliberately
// leaves durable Podman and Kubernetes workloads running: their stable identity
// is adopted and reconciled by the next controller. A durable workload stops
// only through persisted cancellation, deadline or terminal reconciliation.
//
// This package contains no business rule. A defect in a capability or adapter
// is reported to its owner with a unit reproducer instead of being repaired
// here.
package app
