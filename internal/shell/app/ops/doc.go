// Package opsapp binds the operation registry to the real Shell
// capability services.
//
// The package is deliberately named opsapp rather than ops: the transport
// package internal/shell/transport/ops owns the router, its Codec/Handler
// interfaces and the AuthorizedCall value, and the composition root imports
// both. A second package named ops in the same import graph would force every
// caller to alias one of them.
//
// # What a binding does
//
// Bindings returns exactly one ops.Binding per registry operation, in
// declaration order, with the input and result codecs taken from
// api.OperationCodecs. One handler decodes the typed public request DTO,
// maps the envelope (principal, scope, route targets, idempotency key, expected
// version) onto the capability service's complete command, calls the real
// service, and encodes the typed result DTO. A handler never performs an effect
// itself, never caches authority and never returns a placeholder: a missing
// capability service is a construction error, not a runtime success.
//
// # Scope, route parameters and identity
//
// The router resolves the ownership scope and the REST route targets through
// the injected ScopeResolver before it authorizes. This package implements that
// resolver with this rule:
//
//   - a project-scoped credential's scope is its own scope; a route that names
//     another project (or another tenant) is uniformly not_found;
//   - a tenant-wide credential must name the project the route table defines;
//     a tenant-scoped operation resolves to the credential's tenant;
//   - an id-only route has no project of its own, so a tenant-wide credential
//     is refused with invalid_input instead of being widened to the tenant.
//
// REST route parameters are returned as targets so a handler can fill an
// omitted request field; a body value that disagrees with the route is refused
// by the router before the handler runs. MCP carries the same fields in the
// input, so the handler's decoder is the MCP decoder when no route targets
// exist.
//
// # Execution authority
//
// ops.ExecutionAuthority.Admit receives only the principal, while the registry
// operation is known only from the call. The ScopeResolver therefore records
// the operation for the request-local context value the router passes to both
// ports, and the authority adapter consumes that record, reads the current
// execution facts through the injected reader and replaces the reader's
// validity marker with the single permission the registry row requires. An
// execution principal is never admitted through a human grant.
package opsapp
