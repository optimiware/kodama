---
id: T-046
milestone: M3
status: done
depends_on: [T-040, T-041, T-042, T-043, T-045, T-075]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md]
check: make check-go PKGS="./internal/app/... ./api/client/..."
resources: [go]
acceptance: [m3_composition_smoke, m3_contract_compatibility]
---
# T-046 — M3 composition

## Goal
Assemble business-loop features and templates into a tested release candidate.

## Contract
Register QA/profile, ChatOps, dependency admission, SQL summaries, migrations, and command/route contributions through the central owner. Preserve M1/M2 contracts.

Continue T-075's mapping/notification/flow wiring. Browser-agent QA (T-069), Supabase blueprint (T-047) and live wrapper qualification (T-085) are deferred; they do not block local business-loop code.

## Deliverables
Integrated settings/handlers, complete API schema, example/template fixtures, CI targets and verified documentation for stage/QA/human completion.

Regenerate the public client and verify authorization/forwarded gate links using controlled adapters. Actual gate/notification joins transfer to T-044. Blueprint factory delivery is qualified when deferred blueprint/QA work is scheduled, not during unit implementation.

## Acceptance
`m3_composition_smoke` and `m3_contract_compatibility` verify AC-10/AC-12 and ensure every newly exposed operation uses the real authorization module before T-044.

`chatops_gate_integration` transfers to T-044. `blueprint_factory_integration` remains deferred with T-069/T-047, covering AC-08/AC-09 and DQ-07/DQ-10 when scheduled.

## Out of scope
Treating task completion reports as a substitute for integration tests.
