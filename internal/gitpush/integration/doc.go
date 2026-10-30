// Package integration composes the real tenant Git worker (internal/gitservice:
// smart HTTP, native Git, the hooks and the ref journal) with the push
// authorizer of internal/gitpush through the worker's own control-plane client
// and the authorizer's worker-facing HTTP handlers. It holds tests only: they
// prove, with real Git, that a refusal changes no ref, that a lost confirmation
// is settled from the real journal, that a partial receive applies exactly its
// applied refs, that an applied record emits exactly one event and that a seed
// is a single-ref transaction (GR-07, GR-09, GR-10, GR-18, GR-A06, GR-A13,
// GR-A14).
package integration
