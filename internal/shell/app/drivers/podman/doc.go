// Package podman implements the Podman runtime seams
// (podman.Libpod and podman.Files) over the libpod REST API and the operating
// system file API.
//
// Libpod talks only to a Podman API Unix socket with net/http. It never imports
// a Podman/containers SDK, opens a shell or starts a container by itself. The
// test support code starts the per-test `podman system service`; this package
// only dials the socket it is given.
//
// Uncertainty stays visible. A 404 is the seam's ErrNotFound; a create conflict
// is ErrAlreadyExists; a transport failure, timeout or cancellation is
// ErrAmbiguous, never a fabricated success or a fabricated absence. The adapter
// above this seam turns every non-sentinel failure into an uncertain outcome.
//
// Files confines every operation under one adapter root, refuses a symlink in
// any component instead of following it and replaces files atomically. Real
// Process and Podman conformance belongs to the component suite.
package podman
