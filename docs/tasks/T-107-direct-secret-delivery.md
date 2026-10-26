---
id: T-107
milestone: production
status: planned
depends_on: [T-104]
decision_blockers: []
owns: [internal/execution/adapters/secretroute.go]
check: deploy/stage/scripts/gate.sh --gate readiness --cloud scw --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
resources: [go, integration]
acceptance: [kube_secrets_kms]
---
# T-107 — Direct project-secret delivery to ordinary steps

## Goal
Make the delivery the specifications already describe work on the installed server: an ordinary container step that declares a project secret receives only its pinned version, without being pinned to a deployment environment. Scheduled by the owner on 2026-10-26.

## Context (established while qualifying T-104)
- [Variables and secrets](../specs/variables-and-secrets.md#workload-delivery), the [pipeline format](../contracts/pipeline-format.md), O-19, P-13 and VS-A10 describe `env: {NAME: {secretRef: ...}}` and `secretFiles` bindings for ordinary steps, with versions pinned at admission and no plaintext in Pod fields.
- The installed server refuses the ENV form when a definition is registered (`pipelineReferences.CheckSecret` in `internal/app/adapters.go` admits only the file form).
- A project secret declared as a file registers, but `resolveSecret` answers 403: `SecretRoute` builds the attempt's bindings only from a pinned deployment environment's retained snapshot, so an attempt with no environment resolves nothing.
- T-104 therefore qualified secret use through environment-bound delivery only (a project environment naming the secret as a file binding, plus a placeholder kubeconfig credential that is never used to connect). That proves the installed KMS decrypt path across a restart; it does not qualify VS-A10's ENV binding or direct delivery.

## Resolution (2026-10-26)
The direction was already confirmed (O-19, P-13: an ordinary script step may bind declared secret versions to child-process ENV, and files are preferred), so no new policy was needed. The change is small and keeps every guard:
- `pipelineReferences.CheckSecret` (`internal/app/adapters.go`) admits both declared forms. Registration still requires the actor to read credentials in the project and a current, non-revoked version; an absent secret is refused with `secret_denied`.
- `SecretRoute` resolves, for an attempt that pins no environment, exactly the bindings its step declared. They were validated at registration, version-pinned at admission and sit in the grant's immutable, digest-checked context. Anything else (an undeclared reference, another version, a revoked or finished attempt) is refused by the resolver or the authority check, as before.
- An attempt that pins an environment keeps the earlier rule: it resolves that environment's own credentials.

Qualified live at `8448adc623b5` (readiness receipt, `direct_secret_delivery` passed). Live coverage is narrower than the unit coverage: the live case proves ENV and file delivery, absence from the Job and Pod objects, and refusal of an absent secret at registration; the undeclared, wrong-version, foreign-tenant, rotated-later and revoked refusals are covered by unit tests (undeclared and wrong-version at the route) and the secrets module's own contract suites, not by a live case. Live qualification is a third child case of `kube_secrets_kms`, `direct_secret_delivery`, run by the T-104 readiness gate: ENV and file delivery before and after a server restart, the values absent from the Job and Pod objects and from retained logs, and an absent secret refused at registration.

## Limitations kept (follow-ups, not regressions)
- A **writer** step that declares a secret cannot publish source: `attemptNeedles` (the source-proposal scan of injected values) still answers unavailable for any attempt that declares a secret. Fail-closed, and now reachable; enumerating the needles needs the secret source wired into the workspace composition.
- A step that pins an environment **and** declares its own secrets receives only the environment's credentials.
- Redaction of known injected values in live (pre-retention) log reads is not claimed; a script that prints its own secret discloses it.

## Questions that were settled first
1. Whether direct delivery is the intended product behavior or environment-bound delivery is, and which specifications change if so (the owner decides; do not widen the grant silently).
2. The binding set a grant may resolve (declared and admission-pinned only), tenant/project/attempt scoping, revocation, audit and redaction of known injected values.
3. Which owner changes `internal/app/adapters.go` (ordered shared-composition owner) and how the runtime API contract stays frozen.

## Provisional acceptance
A step declaring an ENV and a file project secret receives exactly its pinned version before and after a server restart; an undeclared, foreign-tenant, rotated-later or revoked binding is refused with the specified error; no plaintext appears in the Job/Pod manifests, logs or retained evidence. Qualified on the Kubernetes stage installation with the live KMS, using the T-104 readiness gate's evidence rules.

## Out of scope
Agent model/tool credentials (proxy-held), hosted custody (T-061) and KMS provider changes.
