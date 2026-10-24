---
id: T-060
milestone: M4
status: deferred
depends_on: [T-051, T-052, T-053, T-054, T-056]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, test/e2e/m4/, docs/acceptance/m4.md, README.md]
---
# T-060 — Integrate selected M4 product extensions

## Goal
Qualify selected Platform UI integrations and verified harnesses without fragmenting the product contract.

## Contract
Remain deferred. Preserve existing CLI/API/MCP/webhook behavior and the qualified production profile. O-15 places any UI in Platform, with separate upstream ownership; this task owns no local UI composition. The deferred operator decision and Markdown-only chassis guidance do not block the UI/harness release.

## Deliverables
Kodama composition/route/profile registration, upstream Platform navigation/API/auth handoff, complete dependency/contracts, cross-interface e2e and supported adapter matrix, verified release instructions.

Regenerate the public client with the aggregate contract and include it in `m4_backward_client`. Agree the separately owned Platform UI task and consume its qualified evidence before scheduling this task; the provisional local `web/src/composition/` ownership is removed under O-15.

## Acceptance
`m4_cross_interface_journey`, `m4_harness_matrix`, `m4_tenant_scope`, and `m4_backward_client` verify AC-01/AC-10/AC-12, IT-03, AR-02/AR-12, and GM-12.

## Out of scope
Shipping an unverified harness or implying optional operator/chassis assessment is implementation acceptance.
