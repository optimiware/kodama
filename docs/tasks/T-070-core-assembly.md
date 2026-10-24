---
id: T-070
milestone: M1
status: done
depends_on: [T-010, T-011, T-012, T-013, T-014, T-015, T-017, T-019, T-020, T-024, T-062]
decision_blockers: []
owns: [internal/app/, api/openapi.yaml, api/client/, go.mod, go.sum, Makefile, .github/workflows/, README.md, test/integration/assembly-core/]
check: make check-go-integration PKGS="./internal/app/... ./api/client/... ./test/integration/assembly-core/..."
resources: [go, integration]
---
# T-070 — Incremental core application assembly

## Goal
Turn merged core contributions into an executable slice before waiting for the full M1 workflow.

## Contract
Explicitly construct standalone authority, tenant stores/transaction adapters, migrations, HTTP routes and event workers. Consume T-062's transaction record/corrections. Missing executor/gate/cleanup bindings fail closed; readiness must reflect configured dependencies. Use no production fakes or implicit hosted fallback.

## Deliverables
Core CRUD/source/configuration/artifact/definition/run-admission/SSE wiring and CLI foundation, subset aggregate API/client generation, actual worker start/stop and integration instructions. Exercise real PostgreSQL, OIDC, Git, S3 and JetStream where used; source retention and cancellation repair cannot rely on a fake wakeup. Register only the supported subset and document its usable commands.

Central ownership transfers to T-071 then T-027. T-027 still owns complete M1 route/step/CLI/chart/client/CI obligations; this task does not mark a container run usable before T-071.

## Acceptance
`core_assembly_boot`, `core_assembly_authorization`, and `core_assembly_replay` verify SD-09–SD-11, IT-03, PX-05 and AC-10/AC-12. Start the actual composition, migrate in declared order, use public authorized/foreign-tenant requests, restart event workers, repeat admission keys and prove protected rollback/RLS through selected adapters. An unwired handler or permissive test authority cannot pass.

## Out of scope
Kubernetes execution qualification, enabling agent/gate kinds or complete M1 acceptance.
