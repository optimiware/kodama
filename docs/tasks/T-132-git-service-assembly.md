---
id: T-132
milestone: production
status: deferred
depends_on: [T-116, T-134, T-128, T-138, T-160, T-161, T-162, T-163, T-164, T-150, T-152, T-153, T-154, T-155, T-156, T-158]
decision_blockers: []
owns: [internal/app/, cmd/kodama/, api/openapi.yaml, api/client/, go.mod, go.sum, docs/implementation/assembly-status.md]
check: make check-go PKGS='./internal/app/... ./cmd/kodama/... ./api/client/...' && make api-check && make test-m1 test-m2 test-m3 test-chart
resources: [go]
acceptance: [unit_app_git_service_composition, unit_app_git_route_forwards_with_assertion, unit_app_git_worker_command, unit_app_git_migrations_ordered, unit_app_production_refuses_git_fixtures, unit_app_git_workers_registered_once]
---
# T-132 — Internal Git service assembly

> **Deferred by [O-36](../decisions.md#finishing-the-git-service-on-the-kernel--2026-10-30-o-36).** The remaining Git service work now targets the ADR 0013 kernel as [T-166](T-166-kernel-git-contracts.md)–[T-170](T-170-kernel-git-closeout.md). This record stays for its merged code contribution and open findings; its wiring, installation or acceptance scope is not scheduled here.


## Goal
Wire every Git capability into the server and the `kodama git serve` role (sole shared-composition owner after T-116).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md). Business rules stay in capability packages.

## Deliverables
- Git hosting per environment with the same `internal/gitservice`: in-process per-tenant instances with temporary data roots under `development`, the Podman containers of T-139 under `test`, the T-129 Kubernetes workers under `production` ([environments](../implementation/internal-git-service.md#one-git-service-in-every-environment)); steps on every runtime reach Git only through `/git/`.
- Wire `execution.Dependencies.AgentPins = chain.Pins` beside the agents chain (T-163); shared agent steps fail closed without it.
- Constructors per environment, `/git/` forwarding route with assertion issuance, `/internal/git/v1` worker routes, public routes, product MCP, consumers and reconcilers (each registered once), readiness and shutdown.
- Migrations `007`–`015` registered in numeric order after the existing contributions.
- `kodama git serve` command; `production` refuses temporary-directory Git and fixture adapters; final API/client regeneration; assembly status.

## Acceptance
Declared check.

## Out of scope
Installation scripts (T-139, T-157), stage acceptance (T-133).
