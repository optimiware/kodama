---
id: T-101
milestone: M2
status: done
depends_on: [T-075, T-096, T-097, T-098, T-099, T-100]
decision_blockers: []
owns: [internal/app/]
check: make check-go PKGS="./internal/app/..."
resources: [go]
acceptance: [app_action_ceiling_bound, app_delivery_joins_bound, app_podman_checkout_bound, app_registry_verifier_bound, app_git_trigger_mounted]
---
# T-101 — M2 production assembly joins

## Goal
Bind the M2 prerequisites into `NewCore` so the production composition runs the agent, delivery and Git-trigger scenarios that T-038 could not run.

## Contract
Bind, in `internal/app` only: (1) T-096's action-ceiling policy from operator configuration; (2) T-097's transaction-bound delivery environments reader, the gate store's `DeploymentBinder` and the runtime secret route, enabling the `deployment-gates` and `runtime-secret-resolution` capabilities and the cleanup environment; (3) T-098's Podman checkout provider using the existing `checkoutProvider` material; (4) a production registry verifier for `ociArtifact` outputs in place of `deniedRegistry` (T-067's tests use the real `oci.Verifier` over the scope registry), failing closed when no registry is configured; (5) T-100's Git trigger adapters mounted behind the existing `AssembleTriggers` so `git-webhooks` is no longer disabled; and (6) enabling `agent-candidate-gates` once the conformance report output name agreed with the wrapper in T-031 is bound. Every binding fails closed when its configuration is absent and records the capability as disabled with a reason, exactly as the composition already does. The `development` composition keeps its memory adapters and must not regress. If any binding needs a change outside `internal/app/`, stop and report it as a blocker with the evidence.

## Deliverables
The bindings, their configuration structs and chart/README configuration notes, removal of the corresponding entries from the disabled-capability list, and unit tests with injected adapters and httptest.

## Acceptance
`app_action_ceiling_bound` proves the core issues an agent attempt the ceiling's actions and a plain step only the defaults. `app_delivery_joins_bound` proves an environment-declaring step is admitted, a deployment-bearing gate is decidable and the secret route is registered, and that each fails closed without its configuration. `app_podman_checkout_bound` proves a source-consuming Podman attempt receives checkout material for its pinned repository only. `app_registry_verifier_bound` proves an `ociArtifact` output verifies against the configured registry and is refused without one. `app_git_trigger_mounted` proves `git-webhooks` is registered when configured and reported disabled otherwise. Keep the M1 and T-075 acceptance passing.

## Out of scope
Real service and Podman qualification (T-038), Kubernetes forms (T-089), and production KMS wiring (T-029).
