---
id: T-082
milestone: M1
status: done
depends_on: [T-016, T-063, T-078, T-079, T-086]
decision_blockers: []
owns: [internal/entrypoint/]
check: make check-go PKGS=./internal/entrypoint/...
resources: [go]
acceptance: [unit_step_lifecycle, unit_step_retry, unit_step_secret_scope]
---
# T-082 — Entrypoint unit qualification

## Goal
Finish command/result/log coordination behind the existing entrypoint interface.

## Contract
Use a fake process runner, callback transport and controlled filesystem/time where needed. Preserve existing credential-delivery, cancellation and callback contracts. Local temp directories and httptest are allowed; container provisioning is not needed.

## Deliverables
Focused tests and bounded fixes for cancellation/exit/result ordering, log upload retry, callback replay and credential exclusion from outputs. Keep actual image execution at T-026.

## Acceptance
`unit_step_lifecycle` verifies exit/cancellation/result ordering. `unit_step_retry` verifies retry/replay preserves attempt identity and does not rerun commands. `unit_step_secret_scope` verifies checkout credentials stay scoped and excluded from retained output. Cover PX-05/PX-07 and VS-03/VS-07.

## Out of scope
Wrapper images, live agents, new runtime frameworks and production qualification.
