---
id: T-061
milestone: production
status: deferred
depends_on: []
decision_blockers: []
owns: [internal/platformintegration/, internal/app/platform-integration.go, test/e2e/platform-integration/, docs/operations/platform-integration.md]
---
# T-061 — Optimi Platform integration and hosted acceptance

## Goal
Qualify Kodama as the agent/SDLC layer for an authoritative Optimi Platform tenant, rather than treating a standalone factory demo as hosted integration.

## Contract
Implement the selected O-18/O-19 directions in [platform integration](../specs/platform-integration.md) through consumer-owned identity, credential and tool ports. Optimi's OAuth server authenticates actors and can issue UUID `tenant_id` and role claims; validate the actual issuer/audience/claim contract and use the verified tenant UUID for tenant-owned PostgreSQL/RLS context. Freeze role mapping, current revocation/entitlement checks and outage behavior before hosted acceptance. Do not infer production compatibility from the development fixture or a same-issuer token.

Reuse the existing secrets module and KMS-encrypted PostgreSQL envelope. Qualify hosted immutable-version pins and scoped audited read-for-use with the upstream owner. Prefer AI/MCP proxies that retain provider credentials server-side; allow explicit, authorized secret-to-ENV binding only for ordinary script/build children, with versions pinned and no plaintext in Pod manifests or retained output. A proxy is not a promise that an explicitly disclosed script secret cannot leak.

Record supported upstream interfaces and any delivered prerequisites before enabling hosted routes. A missing upstream capability requires a bounded, separately owned prerequisite; it is not permission to invent an API/grant, query Platform tables or import private packages. Changes needed to consumer ports return to their owning tasks.

## Deliverables
Hosted OAuth/UUID-tenant/role adapters and composition contribution, explicit authority-profile selection, migration contribution for installation-bound tenant identity constraints as needed, platform-version compatibility fixtures, audited correlation, tenant-data projection/egress policy, and a hosted onboarding/disable/restore runbook. Platform owns subscription billing, commercial usage/access and entitlements; Kodama reports operational usage/outcomes and enforces runtime bounds without duplicating that business logic. Coordinate dependency additions, migration registration and activation through T-056's integration ownership. Record the exact supported platform/OAuth revision and first vertical slice.

Target the first real integration at Kodama's separate namespace in Platform stage. Record the installation endpoint and API/client versions; exercise Platform-originated run submission/status using the accepted hosted caller contract and the generated public API client. Preserve the same run on a lost-response replay. Keep Platform scheduling intent distinct from Kodama's attempt lifecycle. Coordinate the Platform caller/dashboard changes in a separately owned upstream task; this task cannot edit the sibling checkout. Provider deployments to edge/cloud targets remain outside this slice until Platform exposes an accepted mutation interface.

## Acceptance
`core_tenant_authority`, `core_unavailable_no_fallback`, `core_pinned_credentials`, `core_tool_audience_and_entitlement`, `core_unknown_outcome`, `core_data_egress`, `core_agent_delivery_slice`, and `core_disable_restore` cover PI-01–PI-10 through PI-A01–PI-A08. Run against the declared real platform/test-tenant profile, including core revocation during execution, wrong-audience credentials, core audit failure, and restored stale local mappings. The first slice follows tenant intent → authorized tool → candidate/conformance evidence → human decision. Fakes support module work but do not satisfy hosted acceptance.

`core_tenant_authority` also covers IT-13/IT-A09: reject missing/malformed/foreign UUID tenant claims and untrusted role/tenant headers. `core_pinned_credentials` also covers VS-13/VS-A10: proxies operate without disclosing provider secrets to agents, and an explicitly declared script ENV binding receives only its authorized pinned value in the child environment; revocation blocks new delivery and no plaintext appears in Pod manifests, management responses or captured fixtures.

## Prerequisite recorded by T-104
The installed standalone server delivers a project secret to a step only through a pinned deployment environment (ENV bindings are refused at registration; a file binding gets 403 without an environment pin), so the ENV binding described above is not implemented there. [T-107](T-107-direct-secret-delivery.md) holds that prerequisite; T-104's environment-bound KMS evidence does not satisfy `core_pinned_credentials`.

## Out of scope
Implementing upstream platform capabilities without their owner contract, copying the core directory/secret store, broad machine credentials, or claiming untrusted-execution isolation from OAuth/RLS qualification. The current slice runs with trusted-engineer regular containers under O-07; T-055's later security extension is independent. No live customer or production mutation is required for the test slice.
