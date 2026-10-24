---
id: T-078
milestone: M1
status: done
depends_on: [T-016, T-063]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS=./internal/entrypoint/...
resources: [go]
---
# T-078 — Entrypoint container-log mirror

## Goal
Make ordinary command output observable through the Pod's Kubernetes log stream so the controller fallback can retain diagnostics when the wrapper cannot upload its retained log.

## Contract
Mirror the same bounded, redacted bytes the entrypoint would retain for `command.log` to the container's log stream (stderr), without changing the retained artifact, result finalization, process cancellation, or runtime envelopes. Known injected secrets, checkout credentials and issued tokens must be removed before any byte reaches the container log. Preserve bounded capture and explicit partial/completeness semantics; never imply that the live mirror is a complete retained log. Do not add runtime routes, wire fields, root wiring, or a second logging service.

## Deliverables
A bounded mirror at the actual command call site, redaction shared with the retained capture, clean shutdown/flush on normal exit, timeout and cancellation, and tests covering ordinary output, truncation, split secrets, failure paths and the no-duplicate-suppression boundary. Export the contribution for T-064's real Pod/controller fallback qualification.

## Acceptance
`container_log_mirror`, `container_log_redaction`, and `container_log_bounds` cover PX-10, AC-06 and VS-11. Prove that ordinary stdout/stderr bytes reach the container log, that known secret values never do, that the mirror stays bounded, and that interruption/timeout still terminates the process tree. Re-run the existing entrypoint source/result/process tests. T-064 remains the real Pod/node/controller join.

## Out of scope
Production root wiring, log service routes, controller fallback implementation, guaranteed completeness after node loss, changing the retained-log sidecar contract, or unrelated entrypoint refactoring.
