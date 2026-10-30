# Deploy plugin

This directory contains the reusable deployment plugin engine and its provider
drivers. It is a container image contract, not a new Kodama step kind.

## Runtime contract

The plugin is launched by an ordinary `kind: container` step. The step supplies:

- a digest-pinned image;
- declared nonsecret arguments/configuration;
- declared credential files or environment variables;
- optional verified artifact inputs;
- an attempt token file and state binding authorized by the server.

The plugin never receives a tenant or project from a command-line flag. The
state binding in the runtime URL is resolved by the server against current
attempt authority.

## Commands

```text
deploy identify
deploy apply
deploy status
deploy reconcile
deploy cleanup
deploy rollback
```

Each command accepts provider/recipe configuration as flags or a configuration
document. `apply` is the only command that may create or update provider
resources. `status` and `reconcile` are read-only with respect to providers until
they settle a recorded operation.

## State document layout

All keys are relative to the authorized binding:

```text
resources/<resource-hash>
operations/<resource-hash>/<operation-id>
releases/<resource-hash>/<release-id>
```

`resources/<hash>` is authoritative. It is updated with `If-Match`; an operation
reservation is persisted before any provider mutation. `operations/` records
contain the immutable request fingerprint, checkpoints, observation and result.
`releases/` records retain the artifacts needed by an explicitly requested
rollback.

## Driver protocol

The Go driver interface is in [`driver/driver.go`](driver/driver.go). A future
external driver must implement the same values over the versioned
`kodama.deploy-driver/v1` JSON-line protocol documented in
[the implementation plan](../../../docs/implementation/container-plugins.md#contract-d-deployment-drivers).
The engine owns state retries and checkpoint acknowledgment; a driver must not
reimplement them.

## Kubernetes driver

`drivers/kubernetes` deploys a bundle containing `manifest.yaml` (or
`manifest.yml`) to the configured namespace. It uses the ordinary, namespace-
scoped kubeconfig supplied by the step. Apply is followed by rollout
observation and an inventory read. Cleanup deletes only the named owned
Deployment in the current implementation; the full inventory/UID contract is
the next hardening step.

## Build

```sh
go build -o /tmp/deploy ./images/plugins/deploy
```

The image build must be performed separately with a digest-pinned base and the
same binary. This repository does not bake credentials or a mutable tool
download into the plugin.
