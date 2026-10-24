---
id: T-047
milestone: M3
status: deferred
depends_on: [T-014, T-030]
decision_blockers: []
owns: [templates/supabase-hono-vite/, docs/examples/blueprints/supabase-hono-vite.md]
---
# T-047 — Supabase/Hono/Vite blueprint

## Goal
Make the second documented stack an independently runnable template.

## Contract
Use [stack guidance](../examples/blueprints/supabase-hono-vite.md). Pin the template runtime/UI framework locally; this does not choose the factory workbench's future stack.

Develop against the merged pipeline/profile contracts, with isolated real application dependencies for RLS, auth, browser and delivery-script tests. Runtime fixtures may stand in for factory callbacks. T-046/T-044 retain the assembled-factory promotion and evidence checks; waiting for all of M2 is unnecessary for this independent template.

## Deliverables
Tenant membership/RLS, scoped Hono operations, browser UI, migration/seed scripts, agent instructions, delivery/E2E scripts, public-runtime-config handling.

## Acceptance
`browser_rls_scope`, `service_role_path`, `tenant_switch_cache`, `erp_duplicate_request`, and `blueprint_frontend_promotion` demonstrate the blueprint checklist and DQ-07/DQ-10.

## Out of scope
Shared factory runtime identity dependence, privileged secrets in Vite assets, or untested self-hosted Supabase support claims.
