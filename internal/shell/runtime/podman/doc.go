// Package podman implements the test Podman (libpod) container runtime adapter.
//
// The adapter talks to the container engine only through the small Libpod seam
// in api.go and to the host filesystem only through the Files seam. It is
// deliberately independent of the containers/podman Go client: unit tests drive
// deterministic in-memory libpod, file and clock fakes, and no socket, process,
// container or daemon call happens here.
//
// The adapter implements the declared runtime.Client contract exactly. It
// consumes the persisted engine.EffectIdentity for every operation, so a crash
// before or after an effect cannot derive a different workload, and a retry
// converges on one stable container.
//
// Preparation materializes the activation snapshot once, confined to
// <root>/<workload>/kodama and bind-mounted read-only at /kodama, and attaches
// the execution's pre-existing retained workspace volume at /workspace. The
// credential is the sole mutable mounted file and is replaced atomically by
// RefreshCredential without restarting the container.
//
// Observation and stop evidence stay in the declared Kernel vocabulary:
//
//   - an absent container, an unreachable daemon, a lost create or start
//     response and a delayed start never prove that a command stopped or that it
//     never ran;
//   - never-started requires a created container with no accepted start and no
//     pending start;
//   - only affirmatively observed whole-container termination confirms the stop
//     barrier; a stop acknowledgment never does;
//   - Remove removes only the identity's container and materialization, writes a
//     durable removal fence outside the removed root and never touches the
//     retained workspace volume.
package podman
