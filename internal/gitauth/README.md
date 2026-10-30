# internal/gitauth

Git grants, credentials and worker identity of the internal Git service (task T-136; [ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md), [Git repositories](../../docs/specs/git-repositories.md) GR-05, GR-06, GR-18, IT-17 to IT-19, seams S3/S5/S8 of the [implementation plan](../../docs/implementation/internal-git-service.md)). The wire shapes, lifetimes, audiences and rules are the executable contract in [`contracts/git/v1`](../../contracts/git/README.md); this package applies them and redefines none.

**Status:** implemented and tested; **not wired**. No route, migration, signer or worker is registered by this package: T-132 lists migration `008_git_grants.sql`, registers `POST /tenants/{tenantId}/git-credentials` and wires the adapters. The published API document does not include `api/git-credentials.yaml` until T-158 lists it.

## Layout

| Package | Role |
| --- | --- |
| `gitauth` | `Grant` entity, `Secret`/`Digest`, `Policy`, the use cases (`Service`), the contract's provided ports (`Credentials`, `Verifier`, `Assertions`) and the consumer-owned ports (`Store`/`GrantTx`, `Repositories`, `Pushes`, `Attempts`, `Gates`). Imports shared values, the Git contract and `identity` only. |
| `memory` | `memdb` adapter (development environment): `Store` (with `Audits`), `Bind`, `RevokeAttempt`, `RevokeAll`. |
| `postgres` | Adapter for table `git_grants` (migration `008_git_grants.sql`, `Migrations()`): tenant predicates, forced row-level security, trigger guards, column-level grants; `Bind`, `RevokeAttempt`, `RevokeAll`, `ConsumeSeed` join the caller's `pgx.Tx`. |
| `jwt` | ES256 forwarding assertions and worker identities (`go-jose` signs, `go-oidc` verifies over a local key set). **Signing and verification are separate types:** `Signer` (private `KeyRing`s, the control plane only) and `Verifier` (public keys only, `PublicKeyRing`, no signing method; what a worker holds). Rotation keeps one previous key and reaches verifiers as new `PublicKeys`. |
| `http` (`package gitauthhttp`) | `POST /tenants/{tenantId}/git-credentials`, its route `Rule`, and the types generated from [`api/git-credentials.yaml`](../../api/git-credentials.yaml). |
| `gitauthtest` | Test fixture only: stand-ins for the consumed modules, the memory and PostgreSQL environments and the shared `Contract` suite. |

## Credentials

A credential is 256 random bits (`gitc_` plus base64url), shown once; only a domain-separated SHA-256 digest is stored (`Secret` redacts itself in every formatter). A signed token never has the credential syntax, so neither a forwarding assertion nor a worker identity is accepted as a credential, and a credential is never accepted as either. The grant's wire form is `gitv1.GrantClaims`, stored verbatim beside queryable columns that constraints keep equal to it.

| Kind | Issued by | Rules |
| --- | --- | --- |
| `human` | `Service.IssueHuman` (the exchange of a verified login) | current read access to the project (`source:read`); servable repository; recorded update is `HumanUpdateCeiling()` for a person who may push now (`workspace:manage`, the engineer), `[]` otherwise; at most one hour (15 minutes by default) |
| `seed` | `Service.IssueSeed` (an engineer), or `IssueAttempt` with kind `seed` (a provisioning attempt) | engineer role; under the repository's seed lock and in the grant's transaction: `gitv1.CheckSeedEligible` over the repository and its push records; revokes the repository's earlier unused seed grants; at most 15 minutes; consumed by `ConsumeSeed`/`ConsumeSeedIn` |
| `preparation`, `attempt` | `Service.IssueAttempt` | attempt live and inside its lease, repositories in the project and servable, contract lifetime (at most 24 hours); idempotent on (kind, attempt or workload) |
| `integration` | `Service.IssueIntegration` | gate approved and current, subject names exactly the requested repository, base, candidate and attempt, ref is the default branch, attempt live; one constrained update; idempotent on (gate, attempt) |

## Use of a credential

`Verify(tenant, credential)` returns the **effective** claims: the credential must be known to the tenant, unexpired and unrevoked, then the authority that can change after issuance is read again. A grant bound to an attempt requires the attempt to be live now and inside its lease. A human grant requires the person to still read the project and keeps its update set only while the person still holds the engineer role (otherwise `[]`); it is never widened beyond what was recorded. A person's seed grant requires a current engineer. Every refusal is the same `invalid_credential`; a dependency outage is `ErrUnavailable`, never access. `Claims(tenant, grantID)` gives the same answer for the push authorizer, which receives a grant ID from the worker. `VerifyScoped` adds the project and repository comparison of the Git endpoint (`not_found`).

## Seams

| Caller | Call | Transaction |
| --- | --- | --- |
| Execution (T-148/T-150) | `IssueAttempt`, `RevokeAttempt` | `RevokeAttemptIn(ctx, tx, …)` through `postgres.Bind`/`memory.Bind` in the transaction that cancels, completes, revokes or supersedes the attempt (lock order: attempt row, then grants). A receive that already obtained commit intent completes; any later one is refused. |
| Offboarding (T-138) | `RevokeAll`, `Active` | `RevokeAllIn(ctx, tx, reason, actor, now, auditID)` through `postgres.Bind`/`memory.Bind` (or `postgres.RevokeAll`/`memory.RevokeAll`) in the offboarding owner's transaction; see "Tenant-wide revocation" |
| Gates | `IssueIntegration` | own transaction |
| Push authorizer (T-130) | `Claims`, `ConsumeSeedIn` | **T-130 must call `ConsumeSeedIn` inside its push-authorization transaction, before inserting the push record, and commit both together.** It takes the repository's seed lock (advisory lock on (tenant, repository), held to the end of that transaction), the lock `IssueSeed` takes before it checks eligibility, so issuance either runs first (and revokes the unused seed) or waits and then sees the committed unsettled push (`seed_pending`). Idempotent for the same receive, `ErrSeedConsumed` (reason `seed_consumed`) for another |
| Git endpoint (T-132) | `VerifyScoped` and `Signer.SignForwarding` | — |
| Worker (T-128) | `Verifier.VerifyForwarding` over the forwarding **public** keys; presents an issued worker identity token | the worker never holds a signing key |
| Control route (T-132) | `Verifier.VerifyWorkerIdentity`; `Signer.SignWorkerIdentity` at provisioning and `Signer.RenewWorkerIdentity` on the worker's renewal call | renewal needs a still-valid token; an expired one means re-provisioning |

A retried issuance of a workload grant returns the same grant with a **fresh** secret that replaces the earlier one (only digests are stored); a different issuance under the same key is `idempotency_conflict`; a revoked, expired or consumed grant is `forbidden`.

## Tenant-wide revocation

`Service.RevokeAll(tenant, reason, actor)` revokes every unrevoked grant of the tenant, of every kind (human, seed, preparation, attempt, integration) and every project, in one transaction, and returns how many it revoked; `RevokeAllIn` does the same inside the caller's transaction. A revoked grant fails `Verify`/`Claims` at the next call (`invalid_credential`). It is **idempotent**: a repeat revokes nothing, keeps each grant's first revocation time and appends nothing. It is **audited**: when it revoked at least one grant it appends one `git.grants.revoked_all` record (actor, reason, count; no credential, digest or grant ID) to the shared `audit` table in the same transaction in PostgreSQL (resource type `git_grants`, resource ID the tenant), so the revocation and its record commit or roll back together; the memory adapter keeps the records in its kernel (`Store.Audits`). The reason is required (at most 2048 bytes) and the actor is an audit-table actor (`user`, `workload`, `system`, `operator`) with an ID of 1 to 256 bytes.

`Service.Active(tenant)` counts the grants that are neither revoked nor expired now (expired is the contract's `now >= expiresAt`); a consumed seed counts until revoked or expired. `RevokeAll` followed by `Active == 0` is the offboarding proof (IT-12) **provided issuance is already fenced**: a credential issued after the revoking transaction commits is a new grant, which `Active` then shows. T-138 closes Git access (`FenceOffboarding`) before it revokes, and repeats until `Active` is zero.

Migration `008_git_grants.sql` already permits this: the runtime role holds `UPDATE (revoked_at)` and `SELECT`, the guard trigger allows setting `revoked_at` on any row (including a consumed seed), and the `audit` table grants the runtime role `INSERT` under its tenant policy. No new migration.

## Decisions made here

- **Permissions.** The role matrix has no Git permission. Reading a repository is `source:read` (viewer, analyst, engineer, reviewer; not admin) and updating or seeding one is `workspace:manage` (engineer only), matching the specification's "admin gains no implicit push" and "engineer pushes".
- **Seed eligibility is advisory at issuance** (repository state and unsettled pushes are read through ports); the push authorizer enforces the facts again at the receive (worker `repositoryEmpty`, consumption, one seed receive).
- **Key rings keep one previous key** so tokens signed just before a rotation verify during the overlap; `RetirePrevious` ends it at once. Forwarding and identity rings must be different rings with different keys. Workers receive public keys only (`PublicKeys`, PEM helpers), refresh them with `PublicKeyRing.Replace`, and are issued tenant-bound identity tokens renewed through the control plane.
- **Token times:** verifiers require `nbf`, `iat` and `exp`, `nbf == iat` (the contract's issuance time), both equal to the contract document's times, and enforce the contract's 30 s skew themselves (go-oidc's expiry check and tolerance are off).
- **Ports narrower than the contract's sketch:** `Pushes.UnsettledPush` answers the question instead of returning records; `Attempts.Lease` also reports the attempt's deadline.
- **`RevokeAll` signature.** T-138's `Grants.RevokeAll(ctx, tenant) error` has no reason or actor and is adapted by the composition (T-132): it calls `Service.RevokeAll` with its own reason and actor and discards the count.
- **Additions to the contract's `Verifier`:** `Claims` (by grant ID) and `VerifyScoped`.

## Checks

```sh
make check-go PKGS='./internal/gitauth/...'          # unit layer: memory adapter, tokens, HTTP
make check-go-integration DEV_SCOPE=t136 DEV_STACK=services PKGS='./internal/gitauth/...'   # real SQL
go generate ./internal/gitauth/... && git diff --exit-code -- internal/gitauth/http/gitcredentials.gen.go
```
