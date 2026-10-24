// Package httpapi holds the shared transport behavior of Kodama's public HTTP
// interface on net/http (task T-004): deny-by-default routing with injected
// authentication, authorization and rate limiting, the JSON and error
// envelopes, request IDs, ETag/If-Match handling, scope-bound authenticated
// cursors, and the transport values of idempotency.
//
// It owns transport value objects only. Product entities, business transitions
// and persistence ports stay in their owning modules; the PostgreSQL adapter for
// idempotent execution lives in the postgres subpackage so this package never
// imports a database driver. See README.md for conventions and integration.
package httpapi
