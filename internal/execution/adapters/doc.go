// Package adapters bridges the execution module's consumer-owned ports to the
// peer modules that satisfy them (identity, pipeline definitions, secrets and
// configuration, work, workspaces) without changing those modules. Each bridge
// translates the peer's sentinel errors into the execution module's classes: a
// refusal stays a refusal, a missing resource is indistinguishable from a
// foreign one, and any other failure is ErrUnavailable, never access.
//
// Nothing here has an allow-all default; the composition root (T-027) wires the
// concrete peers.
package adapters
