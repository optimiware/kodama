---
id: T-161
milestone: production
status: done
depends_on: [T-136]
decision_blockers: []
owns: [internal/gitauth/]
check: make check-go PKGS='./internal/gitauth/...'
resources: [go]
acceptance: [unit_gitauth_revoke_all_for_tenant, unit_gitauth_active_grants_count]
---
# T-161 — Tenant-wide Git grant revocation

## Goal
Let offboarding (T-138) revoke every Git grant of a tenant and observe that none remains active (IT-12).

## Deliverables
- `RevokeAll(tenant)` (idempotent, audited, joinable into the caller's transaction like the existing revocation) and `Active(tenant)`, on memory and PostgreSQL through the shared contract suite.

## Acceptance
Declared check with the named tests; coordinator runs the real-SQL suite.

## Out of scope
Offboarding orchestration (T-138).

## Completion evidence
Implemented by a subagent; independent review approved `d44c094c` (one minor: also test `RevokeAllIn` with an existing audit ID rolling back the revocation; deferred). Coordinator checks: `make check-task TASK=T-161` PASS (2 acceptance names); real SQL `make check-go-integration DEV_SCOPE=t161 DEV_STACK=services PKGS='./internal/gitauth/...'` PASS. For T-132/T-138: adapt `Service.RevokeAll(ctx, tenant, reason, actor)` to T-138's `Grants` port, keep calling until `Active` returns 0; issuance must be fenced first.
