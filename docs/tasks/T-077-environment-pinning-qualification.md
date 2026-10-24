---
id: T-077
milestone: M2
status: done
depends_on: [T-035, T-062, T-076, T-080]
decision_blockers: []
owns: [internal/execution/admission.go, internal/execution/environment.go, internal/execution/ports.go, internal/execution/adapters/, internal/execution/postgres/]
check: make check-go PKGS=./internal/execution/...
resources: [go]
acceptance: [environment_binding, environment_binding_rollback, cleanup_retained_configuration]
---
# T-077 — Delivery environment and cleanup binding

## Goal
Implement immutable environment and deployment-bound cleanup bindings with deterministic Go tests. T-067 owns real persistence qualification.

## Contract
Consume `execution.EnvironmentResolver`, `EnvironmentRequest`, `EnvironmentSnapshot` and the existing runtime environment/secret bindings. Delivery owns immutable configuration and exact credential references. Revalidate current authority and snapshot retention/revocation through the owning admission transaction without holding SQL locks across network calls. A changed environment cannot rewrite admitted inputs; an unavailable retained snapshot fails closed. Preserve the pure planner and historical no-environment identities.

## Deliverables
Delivery-resolver adapter and transaction binding, additive migrations only when necessary, deterministic callback/revocation tests and deployment-bound cleanup admission. Replace cleanup refusal only when its binding resolves retained configuration, manages exact pins and rechecks authority. Publish constructor/fixture contracts to T-066/T-072; T-067 qualifies actual persistence/callback behavior. No root wiring is owned here.

## Acceptance
`environment_binding`, `environment_binding_rollback`, and `cleanup_retained_configuration` exercise immutable pins, foreign/revoked/missing snapshots, transaction-port failure and cleanup authorization using controlled ports. Original `environment_pinned_at_admission`, `environment_edit_preserves_pin`, `environment_pin_rollback`, and `environment_pin_restart_scope` transfer to T-067, including real non-owner-role persistence, credential edits, rollback and restart. DQ-15/PX-01/PX-05/IT-02/VS-02 semantics remain required; unit evidence does not certify SQL behavior.

## Out of scope
Rewriting T-035's domain policy, a generic transaction framework or claiming real delivery qualification. T-072 may wire this code; T-038 qualifies it before M2 acceptance.
