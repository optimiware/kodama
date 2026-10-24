// Package adapters binds the gate module's consumer-owned ports to the merged
// peer modules: identity (reviewer authority), artifacts (evidence metadata,
// bytes and pins), execution (conformance baseline and report identification
// from immutable run data) and workspace (candidate history). It holds no
// business rule and no SQL; the composition root wires it.
package adapters
