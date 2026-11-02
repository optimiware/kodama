---
id: T-136
milestone: production
status: done
depends_on: [T-126]
decision_blockers: []
owns: [internal/gitauth/, api/git-credentials.yaml]
check: make check-go PKGS='./internal/gitauth/...'
resources: [go]
acceptance: [unit_gitauth_human_credential_exchange, unit_gitauth_current_role_on_use, unit_gitauth_seed_grant_single_use, unit_gitauth_seed_refused_non_empty, unit_gitauth_attempt_grant_scope, unit_gitauth_integration_grant_binding, unit_gitauth_revocation_transactional, unit_gitauth_forwarding_assertion_sign_verify, unit_gitauth_worker_identity_tenant_bound, unit_gitauth_audiences_disjoint, unit_gitauth_memory_contract]
---
# T-136 — Git authority: grants, credentials and worker identity

## Goal
Persistent, scoped Git grants and the credentials that carry them (GR-05, GR-06, GR-18, IT-17–IT-19).

## Contract
[ADR 0011](../adr/0011-internal-tenant-git-service.md), [Git repositories](../specs/git-repositories.md), [Git service contract](../contracts/git-service.md), the [implementation plan](../implementation/internal-git-service.md), seams S3/S5. Migration `008_git_grants.sql`. Opaque random credentials stored hashed; forwarding assertions and worker identities use the existing signing/JWT libraries of `internal/runtimeauth`/`internal/identity` patterns, not a hand-rolled verifier.

## Deliverables
- Grant entity: actor, tenant, project, repositories, read, ref patterns, seed, integration constraint (ref, from, to, gate), expiry, revocation; issuance for humans (exchange of a verified access token, current roles checked on every use), seed grants (engineer, empty repository, no unsettled seed), preparation/attempt grants (issued by execution through a port, revoked with the attempt), integration grants (bound to a gate decision through a port).
- Verification used by the Git endpoint and by push authorization; disjoint audiences (`kodama-git`, `kodama-git-worker:<tenant>`, `kodama-git-control`).
- Forwarding-assertion signer/verifier with key rotation; tenant-bound worker identity issuance and verification.
- PostgreSQL + memory adapters with a shared contract suite; `api/git-credentials.yaml` and its handler.

## Acceptance
Declared check; real SQL under `make check-go-integration DEV_SCOPE=t136 DEV_STACK=services PKGS='./internal/gitauth/...'`.

## Out of scope
Ref-policy decisions (T-130), attempt admission wiring (T-150), CLI helper (T-154).

## Completion evidence
Implemented by a subagent; independent review requested changes once (workers would hold signing keys; seed issuance not serialized with consumption; `nbf` not enforced), then a minor deterministic-race-test fix, and approved `618647cf`/`196dcabe`. Coordinator checks: `make check-task TASK=T-136` PASS (11 acceptance names); real SQL `make check-go-integration DEV_SCOPE=t136 DEV_STACK=services PKGS='./internal/gitauth/...'` PASS. For T-130: call `ConsumeSeedIn` inside the push-authorization transaction before inserting the push record. For T-132: register migration `008`, the credential route, and the signer/public-key distribution.
