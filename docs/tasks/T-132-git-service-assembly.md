---
id: T-132
milestone: production
status: planned
depends_on: [T-116, T-127, T-128, T-129, T-130, T-131]
decision_blockers: []
owns: [internal/app/, internal/cli/, internal/productmcp/, api/git-credentials.yaml, api/openapi.yaml, api/client/, deploy/helm/kodama/, go.mod, go.sum, docs/operations/, docs/implementation/assembly-status.md]
check: make check-go PKGS='./internal/app/... ./internal/cli/... ./internal/productmcp/... ./api/client/... ./deploy/helm/kodama/...' && make api-check && make test-m1 test-m2 test-m3
resources: [go]
acceptance: [unit_app_git_service_composition, unit_app_git_route_forwards_with_assertion, unit_app_git_worker_command, unit_app_production_refuses_git_fixtures, unit_cli_repository_commands, unit_cli_git_credential_helper, unit_productmcp_project_repository_task_tools, unit_chart_git_hosting_render]
---
# T-132 — API, CLI, MCP and chart assembly

## Goal
Make the internal Git service usable end to end through public interfaces (AC-13–AC-15): repositories, credentials, developer `git clone`/`git push`, and the MCP tools agents use to create projects, repositories and tasks.

## Contract
ADR 0011, [Git service contract](../contracts/git-service.md), seams S3/S5. Sole owner of shared composition, API aggregate, client and chart after T-116.

## Deliverables
- Wiring per environment: repository and push services, `/git/` forwarding route with assertion issuance, `/internal/git/v1` worker routes, `kodama git serve` command, tenant hosting reconciler; `production` refuses temporary-directory Git and fixture adapters.
- The Git-credential fragment; the aggregate and client regenerated from every module fragment (repositories from T-127, push records from T-130, runs/gates/delivery/artifacts/workspaces from T-131).
- CLI: `kodama repo create|list|get|archive|restore|refs|pushes`, `kodama git credential` helper, clone/remote instructions.
- `internal/productmcp`: MCP tools over the same authorized use cases for project, repository and task creation and inspection (distinct from the MCP proxy).
- Chart: Git host, storage class, tenant hosting settings; operations documents for enabling hosting, backup/restore, maintenance, offboarding and cutover.

## Acceptance
Declared check with every named test passing, plus `make test-chart`. Wiring is not deployment evidence.

## Out of scope
Stage qualification (T-133).
