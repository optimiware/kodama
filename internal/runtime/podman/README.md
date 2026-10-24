# Podman runtime

`internal/runtime/podman` runs attempt workloads as Podman containers: the step's digest-pinned image on a read-only root filesystem, all capabilities dropped, under CPU, memory and process limits. It is the local end-to-end backend ([ADR 0008](../../../docs/adr/0008-runtime-seam.md)); it does not enforce tenant network policy or quotas.

## Source checkout (T-098)

Checkout is supported when a `bootstrap.CheckoutProvider` is configured on `podman.Entrypoint.Checkout`; without one, an attempt with `workspace.access: read|write` is refused with `bootstrap.ErrNoCheckout` and no container, admission credential or file is created. This is the Podman equivalent of `kube.CheckoutProvider`; the composition (T-101) supplies the provider.

For a source-consuming attempt the runtime asks the provider once per container create, with the committed attempt's scope, workspace and the repository and commit its pinned context names. The provider answers only for that repository; a refusal (`bootstrap.ErrCheckoutDenied`) or an answer that does not echo the request's scope, workspace and repository, that is not one `username:password` line, or whose Git CA is not a certificate bundle, is a definite refusal. Any other provider error is an outage: nothing is started and the dispatch is redriven.

The credential and the Git CA are written (mode 0600) as `checkout-credential` and `git-ca.crt` into the attempt's bootstrap directory, which is bind-mounted read-only at `/kodama/bootstrap` and removed with the workload. The entrypoint is pointed at them with `--checkout-credential-file` and `--git-ca-file`. They are never environment variables, arguments, labels or image content. A workspace-free attempt receives neither file and does not consult the provider. The writer's publisher credential is never requested or delivered: publication stays on the workspace service. An adopted container (a restart or redrive) never consults the provider again.

## Launch evidence (T-102)

Podman declares `Capabilities.ProvesNeverStarted`: the durable record is written before any container exists, so a `Stop` before the start is fenced (a later `Start` is `ErrConflict`) and affirmatively reports `LaunchNotStarted`. The declaration is launch-history proof only and separate from the container guarantees. The shared `runtimetest.Contract` requires it of Podman and of the Process runtime; the real-engine run stays with the integration checkpoint.
