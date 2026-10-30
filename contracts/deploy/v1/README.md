# Deployment driver contract v1

This package is the public provider-neutral contract for an image-implemented
deployment driver. It is intentionally separate from the Kodama server packages
so a future driver image can compile against it without importing the server.

## Operations

| Operation | Requirement |
| --- | --- |
| `Identify` | Validate recipe/configuration and return a normalized resource key, artifact kinds, rollback mode and observation/precondition capabilities. It must not mutate a provider |
| `Apply` | Persist any required checkpoints through the engine callback before the associated provider mutation. Return the effect certainty and provider identifiers |
| `Observe` | Read the resource and return `absent`, `progressing`, `ready`, `unhealthy`, `conflict` or `unknown` plus observed provider identity |
| `Destroy` | Delete only recorded owned resources, behind provider preconditions where available |
| `Rollback` | Activate a recorded provider release or explicitly reapply a retained artifact; return an error when the recipe does not support it |

The engine owns state reservations, operation IDs, artifact retention,
checkpoint persistence, retries and receipts. A driver must not write the
Kodama state API directly or start another pipeline run.

## External executable form

The future `kodama.deploy-driver/v1` JSON-line protocol uses the same request,
checkpoint, result and observation values. A driver receives one JSON request
line on stdin and emits bounded checkpoint frames followed by one result/error
line on stdout. Checkpoint frames must be acknowledged before the associated
external mutation. Exact framing and limits are owned by
[the implementation plan](../../../docs/implementation/container-plugins.md#72-future-executable-driver-protocol).

## Security

- Credentials arrive through the ordinary secret-file/environment delivery, not
  through request JSON, logs or state values.
- A driver validates provider identity and target namespace/project before
  mutation.
- A successful command or HTTP 200 is not readiness.
- Parent/child archive paths, symlinks, bundle expansion and unpinned image
  references are rejected before effects.
- Provider resources not recorded as owned are not deleted.
