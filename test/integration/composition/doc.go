//go:build integration

// Package composition holds the composition test suite.
//
// Most tests start the real Shell composition (app.New) in the development
// profile over an injected loopback listener, then talk to it only through the
// public interfaces: the public client/ REST transport, raw HTTP requests and
// MCP JSON-RPC over HTTP. For those tests state is the in-memory store and the
// runtime is an injected fake, so no PostgreSQL, NATS or Podman process is
// involved.
//
// The exceptions use real infrastructure:
//
//   - live_log_test.go composes the real NATS adapter over a JetStream broker
//     that the infrastructure harness starts as a Podman container;
//   - registry_credentials_test.go builds the real kodama binary and drives
//     its production entry;
//   - the three files behind the component build tag (the two kodama migrate
//     proofs and the installable test profile) use real PostgreSQL and, for
//     the test profile, JetStream NATS and a private Podman API service. They
//     compile only with -tags=component and run in the component-stack suite,
//     never together with the integration-tagged files.
package composition
