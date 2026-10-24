---
id: T-098
milestone: M2
status: done
depends_on: [T-079, T-086, T-096]
decision_blockers: []
owns: [internal/runtime/podman/, internal/runtime/process/, internal/runtime/bootstrap/]
check: make check-go PKGS="./internal/runtime/..."
resources: [go]
acceptance: [podman_checkout_issued, podman_checkout_refuses_foreign_source, podman_checkout_unconfigured_refuses]
---
# T-098 — Podman source-checkout issuance

## Goal
Let a workspace-consuming step run on the Podman runtime, as ADR 0008 anticipates, so test, build and preview steps can read source and agents can publish a commit.

## Contract
ADR 0008: the Podman runtime "does not yet issue source-checkout credentials, so it runs only workspace-free attempts until checkout support is added". Today `bootstrap.RequireNoCheckout` refuses any `workspace.access: read|write` attempt with `bootstrap.ErrNoCheckout`, and the dispatch is redriven until `startup_timeout`; as a result no step produces an artifact with source provenance and no writer agent can publish a candidate. Add the Podman equivalent of the Kubernetes `kube.CheckoutProvider` path (`internal/app/checkout.go` is the reference for its material): a `CheckoutProvider` port in the runtime package, a Podman implementation of the delivery of that material to the container (read-only credential file and Git CA mounted for the attempt only, never in the environment or the image), and the refusal when no provider is configured (keep `ErrNoCheckout`). The provider answers only for the repository the attempt's pinned context names; a mismatch is a definite refusal. The publisher credential never reaches the container.

## Deliverables
The port, the Podman delivery of the material, the unconfigured refusal, README/ADR 0008 text updated to say checkout is supported when a provider is configured, and unit tests with the Podman test double. Do not edit `internal/app`; T-101 supplies the provider.

## Acceptance
`podman_checkout_issued` proves a read and a write attempt receive the credential and CA as attempt-scoped read-only mounts and not as environment variables, and that a workspace-free attempt receives nothing. `podman_checkout_refuses_foreign_source` proves a provider refusal for a repository other than the pinned one starts no container. `podman_checkout_unconfigured_refuses` proves a runtime with no provider still refuses a source-consuming attempt with `ErrNoCheckout`. Covers AR-11 and DQ-01 on the Podman path.

## Out of scope
Binding in `internal/app`, publication of a writer's bundle (WS-A03 stays on the existing workspace service), and the Kubernetes runtime.
