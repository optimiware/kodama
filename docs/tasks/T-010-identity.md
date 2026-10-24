---
id: T-010
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/identity/, api/identity.yaml, test/support/oidc/]
review: human
---
# T-010 — OIDC and five-role authorization

## Goal
Authenticate people and expose one scoped authorization interface to modules.

## Contract
Implement [identity](../specs/identity-tenancy-and-rbac.md): issuer/subject mapping, API access-token validation, tenant/project membership, fixed roles, bootstrap, and current-permission checks.

Local authority is the standalone profile. Export the narrow consumer-owned authorization seam needed by [platform integration](../specs/platform-integration.md); do not duplicate core grants or assume same-issuer tokens establish hosted membership. O-18/T-061 selects and qualifies the hosted adapter.

## Deliverables
Discovery/membership/project handlers, audit/events, role map, `go-oidc` verification, verified bootstrap flow, OpenAPI fragment. Do not use the probe's `golang-jwt` parser as the product verifier.

## Acceptance
`wrong_audience`, `membership_required`, `role_revocation`, and `principal_discovery_scope` verify IT-01/IT-03/IT-09/IT-10/IT-11 using a real test issuer.

## Out of scope
Custom roles, generic policy language, workload token issuance, or treating ID tokens as API tokens.
