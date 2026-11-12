// Package kube implements the internal/shell/runtime/kube.Cluster seam
// over the Kubernetes REST API using only the standard library.
//
// The driver is deliberately thin. One seam operation maps to one API request,
// or to the small documented sequence of requests that an immutable create-once
// snapshot or a create-or-update needs. It never imports client-go, never
// retries a create or a conditional write on its own and never starts a
// process.
//
// The configuration is either the JSON document produced by
//
//	kubectl config view --raw --minify -o json
//
// so an in-cluster or per-shard kubeconfig can be exported without parsing YAML,
// or an explicitly selected in-cluster service-account configuration. A
// kubeconfig never falls back to the conventional service-account paths, and
// the two selections are mutually exclusive. An in-cluster selection requires
// an https endpoint and a usable CA bundle; it never skips TLS verification and
// never uses an environment proxy. A bearer token file is re-read for every
// request, so a rotated execution credential takes effect without rebuilding
// the client. Redirects are refused, every non-log response is bounded by
// MaxResponseBytes and every call is bounded by the configured timeout. Close
// releases the transport the driver owns.
//
// Uncertainty stays visible. A 404 is the seam's ErrNotFound, a create conflict
// is ErrAlreadyExists, an update/patch/delete conflict is ErrConflict and every
// other 4xx is a dependency failure. A 3xx, 5xx, transport failure, timeout,
// cancellation, TLS trust failure or oversized response is ErrAmbiguous, never
// a fabricated success or a fabricated absence. No error renders a bearer
// token, a client key, a request body or a response body.
package kube
