---
id: T-155
milestone: production
status: done
depends_on: [T-127, T-136, T-148]
decision_blockers: []
owns: [internal/productmcp/]
check: make check-go PKGS='./internal/productmcp/...'
resources: [go]
acceptance: [unit_productmcp_tools_listed, unit_productmcp_create_project_repository_task, unit_productmcp_caller_authority_only, unit_productmcp_idempotent_create, unit_productmcp_provisioning_delegation_bounded]
---
# T-155 — Product MCP tools

## Goal
MCP tools for projects, repositories and work items over the same authorized use cases (AC-15).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seam S10, existing `modelcontextprotocol/go-sdk` usage in `internal/mcpproxy/protocol`. Distinct from the MCP proxy.

## Deliverables
- Authenticated MCP server exposing create/list/get for projects, repositories (including seed request) and work items; idempotent creates; no authority beyond the caller's; a provisioning attempt acts only within its admitted project.

## Acceptance
Declared check.

## Out of scope
Route registration (T-132).

## Completion evidence
Implemented by a subagent; independent review requested changes twice (an unclear atomic provisioning-budget contract and replay semantics; workload callers not tied to a provisioning origin; creating run not recorded; missing no-store headers; workload idempotency keyed by attempt; unbounded text; then caller validation not covering run/attempt IDs, a caller-only budget, an inconsistent idempotency conflict code, another creator's RunID semantics, and test gaps) and approved `b1d333f0`. Coordinator checks: `make check-task TASK=T-155` PASS (5 acceptance names); `go test -race -count=2 ./internal/productmcp/...` PASS. No tables were added, so no real SQL. The coordinator corrected the S10 sketch in `contracts/git/README.md` (seeds for a provisioning run come from the runtime route).

For T-132: mount `NewHandler`; implement the `Authenticator` (verify person and attempt tokens; set `RunOrigin` to `provision` only for a live attempt of a provisioning run; map OIDC subjects to domain person IDs); write port adapters that authorize with the passed `Caller` and key idempotency by `IdempotencyPrincipal(caller)` exactly once; back `Results` with a durable store retained at least 24 hours; `Provisioning.Start` must follow the documented contract (replay first on the whole declaration, project-wide atomic count-and-admit, `ErrNotFound` for another creator's RunID); `Repositories.Create` must record the creating run and attempt; bind list cursors to scope and filter; work-item and project creates are not idempotent in HTTP today. For T-154/T-158: the tools serve PX-A21 and AC-A12.
