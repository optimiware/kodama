---
id: T-043
milestone: M3
status: done
depends_on: [T-014, T-030]
decision_blockers: []
owns: [templates/go-postgres/, docs/examples/blueprints/go-postgres.md]
---
# T-043 — Executable Go/PostgreSQL blueprint

## Goal
Turn [Go guidance](../examples/blueprints/go-postgres.md) into one working inventory-gap template.

## Contract
Application tenancy is independent of factory tenancy. Supply domain-guided agent instructions and ordinary quality/build/preview/stage/E2E scripts matching the factory contracts.

Develop the template independently using the merged pipeline/profile contracts. Run its application/database and delivery-script tests against isolated real dependencies; use runtime fixtures only at the factory callback boundary. Full M2 acceptance is not a prerequisite for building an application template. T-046/T-044 run it through the assembled factory, including gates and deployment evidence.

## Deliverables
Pinned application template, migrations, tenant authorization, legacy adapter fake/contract, runtime config, agent skills, reproducible image and pipeline examples.

## Acceptance
`blueprint_go_tenant`, `erp_duplicate_resolution`, `blueprint_go_delivery`, and `runtime_config_promotion` demonstrate DQ-07/DQ-10 plus the blueprint checklist against a generated app.

## Out of scope
Shared chassis libraries, requiring factory availability for application requests, or real ERP write credentials in tests.
