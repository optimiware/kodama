---
id: T-107
milestone: production
status: deferred
depends_on: [T-104]
decision_blockers: []
owns: [internal/execution/adapters/secretroute.go, test/e2e/secret-delivery/]
---
# T-107 — Direct project-secret delivery to ordinary steps

## Goal
Make the delivery the specifications already describe work on the installed server: an ordinary container step that declares a project secret receives only its pinned version, without being pinned to a deployment environment. **This task needs explicit scheduling.**

## Context (established while qualifying T-104)
- [Variables and secrets](../specs/variables-and-secrets.md#workload-delivery), the [pipeline format](../contracts/pipeline-format.md), O-19, P-13 and VS-A10 describe `env: {NAME: {secretRef: ...}}` and `secretFiles` bindings for ordinary steps, with versions pinned at admission and no plaintext in Pod fields.
- The installed server refuses the ENV form when a definition is registered (`pipelineReferences.CheckSecret` in `internal/app/adapters.go` admits only the file form).
- A project secret declared as a file registers, but `resolveSecret` answers 403: `SecretRoute` builds the attempt's bindings only from a pinned deployment environment's retained snapshot, so an attempt with no environment resolves nothing.
- T-104 therefore qualified secret use through environment-bound delivery only (a project environment naming the secret as a file binding, plus a placeholder kubeconfig credential that is never used to connect). That proves the installed KMS decrypt path across a restart; it does not qualify VS-A10's ENV binding or direct delivery.

## Questions to settle first
1. Whether direct delivery is the intended product behavior or environment-bound delivery is, and which specifications change if so (the owner decides; do not widen the grant silently).
2. The binding set a grant may resolve (declared and admission-pinned only), tenant/project/attempt scoping, revocation, audit and redaction of known injected values.
3. Which owner changes `internal/app/adapters.go` (ordered shared-composition owner) and how the runtime API contract stays frozen.

## Provisional acceptance
A step declaring an ENV and a file project secret receives exactly its pinned version before and after a server restart; an undeclared, foreign-tenant, rotated-later or revoked binding is refused with the specified error; no plaintext appears in the Job/Pod manifests, logs or retained evidence. Qualified on the Kubernetes stage installation with the live KMS, using the T-104 readiness gate's evidence rules.

## Out of scope
Agent model/tool credentials (proxy-held), hosted custody (T-061) and KMS provider changes.
