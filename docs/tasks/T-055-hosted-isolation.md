---
id: T-055
milestone: M4
status: deferred
depends_on: [T-056]
decision_blockers: []
owns: [deploy/charts/kodama/profiles/hosted/, test/e2e/hosted-isolation/, docs/operations/hosted-isolation.md]
---
# T-055 — Untrusted hosted execution qualification

## Goal
Qualify a later optional isolation/security profile beyond the trusted-engineer regular-container baseline.

## Contract
O-07 selects trusted-engineer staging on regular Kubernetes containers now and explicitly defers stronger isolation. This task remains unscheduled; it does not block staging or the current trusted-engineer release. It follows T-056 so later chart-profile changes are serialized after the baseline release owner.

When the owner schedules this extension, evaluate gVisor, Substrate or another proposed option against concrete process/node/storage/network controls. Name the actual supported boundary, runtime versions, builder limitations and tenancy/capacity limits; a candidate name is not proof of isolation. Retain the existing acceptance scenarios and obtain a bounded owner prerequisite for any execution-module change. Do not introduce a generic security-backend framework for the current staging scope.

## Deliverables
Profile-specific chart configuration/manifests, execution preflight expectations, isolation tests, operator runbook and measured overhead.

## Acceptance
`hosted_cross_tenant_process_network_storage`, `hosted_privilege_denial`, `hosted_builder`, and `hosted_capacity_isolation` verify IT-07/IT-08 and PX-11 in the selected actual runtime.

## Out of scope
Claiming namespaces/Pod Security alone isolate mutually untrusted code.

## Future shaping after O-07
Once the actual runtime/profile is selected, separate profile manifests/preflight, adversarial isolation tests and measured qualification if useful. Assign disjoint paths or ordered handoffs before scheduling; T-055 retains all current acceptance until such tasks are approved. No runtime version or sandbox is invented here, and this shaping does not establish hosted core authority.
