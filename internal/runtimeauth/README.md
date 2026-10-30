# internal/runtimeauth

Attempt admission and runtime tokens (task T-019). Gives each Kubernetes Job
bounded authority that is independent of human tokens: one-time admission
credentials, short-lived audience-bound tokens, current-attempt and cancellation
checks, signing-key rotation, explicit action scopes and safe logging. It
implements the admission/refresh half of the [step entrypoint
contract](../../docs/implementation/step-entrypoint.md) against the frozen
[runtime wire contract](../../contracts/runtime/README.md).

It grants **no** human review authority, no tenant administration and no
platform authority (O-18/O-19 remain hosted-integration gates). A runtime token
authorizes later use of Kodama's product interfaces; it does not fence an
external effect that already started, and revoking it is not evidence that such
effects ended (PX-08 settlement belongs to execution).

## Baseline

| | |
| --- | --- |
| Requirements | IT-06, IT-08, PX-08; contributes the runtime-auth part of IT-A05, IT-A06, PX-A06. Applies SD-01 to SD-07. |
| Decisions | C-01, C-02, C-03, C-16, C-17; ADRs 0001, 0003, 0006. |
| Not claimed | Kubernetes/kind acceptance, restore qualification (T-029), external-effect settlement, hosted support. |

## Layout (ports and adapters)

| Package | Role |
| --- | --- |
| `runtimeauth` | Entities (`AdmissionGrant`, `ActionSet`, `Policy`, `Credential`), use cases (`Service`), consumer-owned ports (`ports.go`). Imports only shared values, the runtime contract DTOs and the standard library. |
| `jwt` | ES256 session-reference tokens: `go-jose` signs, `go-oidc` verifies over a local key set. `KeyRing` owns the current key and signing generation. |
| `postgres` | `Store`, the grant tables in the baseline schema (`internal/storage/migrations/001_schema.sql`), the restricted digest lookup, and the transaction seam (`Bind`, `RevokeAttempt`). |
| `http` (`package runtimehttp`) | `POST /admit`, `POST /token`, `GET /context` over `net/http`. |
| `rtest` | Test fixture only: migrated database, transaction-aware execution fixture, controlled Job observer, fault injection. |

## State machine

```text
            register                first admission              replay (same nonce, same Pod)
 (none) ------------> registered ----------------------> consumed ----------------------+
                         |  expires after AdmissionTTL      |  ^                          |
                         |  (never revives)                 |  +--------------------------+
                         v                                  v
                  revoke / cancel  <-------------------- revoke / cancel
                         |
                         v
                      revoked (terminal, irreversible)
```

- `register`: idempotent. Ownership, the Job binding, the pinned context digest
  and the authority revision come from the attempt's locked execution state; the
  caller supplies the credential (digest stored, never the credential) and the
  action ceiling, which must be a subset of what the attempt allows. A retry with
  the same content returns the original registration without extending its
  expiry; any contradiction is `ErrConflict` and replaces nothing.
- first admission: fixes the session ID, the Pod binding (Job UID, Pod name, Pod
  UID), the consumed nonce digest and the **maximum session expiry**. Nothing
  changes them afterwards, in the entity or, through triggers and column
  grants, in the database.
- replay: the same credential and nonce with the same Pod identity returns a
  fresh token under the same maximum, rechecking cancellation, ownership and
  wrapper compatibility, until the maximum. Another nonce is
  `admission_consumed`; another Pod with the same nonce is `forbidden`.
- revoke: written once, never cleared; denies admission, replay, renewal and every
  action. Expiry is a separate, monotonic condition: it never revives anything.
- Effective authority at each use = grant ceiling ∩ the attempt's current allowed
  actions. It can narrow while the attempt runs and can never widen.

## Admission sequence

1. Decode the strict `AdmitRequest`; look the credential digest up through
   `kodama_runtime_locate_grant` (a `SECURITY DEFINER` function that returns only
   tenant, project and attempt for the one matching digest). The route attempt
   must match, otherwise `invalid_credential`.
2. Advisory read of the attempt; a cancelled or finished attempt is
   `attempt_revoked` before Kubernetes is queried.
3. Kubernetes observation **outside any SQL transaction** (`JobObserver`). The
   claimed Job/Pod are evidence: namespace and Job name come from execution
   state; the Pod must exist in that namespace and be controlled by that Job
   (immutable UIDs).
4. One tenant transaction: re-read and lock the attempt (`GrantTx.LockAttempt`),
   lock the grant `FOR UPDATE`, then check cancellation, authority revision,
   Job binding and context digest against the grant and the earlier observation
   (a changed observation is `unavailable`, retry), signing generation, wrapper
   compatibility, then consume and sign. Cancellation or drift seen under the
   lock durably revokes the grant.
5. The token is returned only after the commit.

## Lock order

For every transaction in the system: **the attempt's execution state first, then
grant rows.** Admission and refresh take the attempt with a lock that conflicts
with the execution owner's cancellation (`FOR SHARE` against the owner's `FOR
UPDATE`), then the grant. Cancellation takes the same order and revokes in the
same transaction. A previously successful middleware check is not a durable
mutation permit: protected mutations (result, artifact, source, secret) call
`runtimeauth.CheckSession` through `postgres.Bind` inside their own transaction
and write before committing. No Kubernetes, network or signing-key call happens
while a SQL lock is held (token signing is local CPU only).

## Error mapping

| Class | HTTP | Code | Notes |
| --- | --- | --- | --- |
| `ErrInvalidCredential` / `ErrNotFound` | 401 | `invalid_credential` | unknown, expired, foreign, retired credential or token; path substitution; nothing about existence is disclosed |
| `ErrForbidden` | 403 | `forbidden` | wrong Job/Pod; ungranted action. On routes whose frozen inventory lacks `forbidden` (`/token`, `/context`) it is `invalid_credential` |
| `ErrAttemptRevoked` | 403 | `attempt_revoked` | cancelled, finished, revoked, stale revision |
| `ErrAdmissionConsumed` | 409 | `admission_consumed` | another nonce after consumption |
| `ErrProfileUnsupported` | 422 | `profile_unsupported` | wrapper policy |
| `ErrInvalidRequest` | 400 | `invalid_request` | strict decoding |
| `ErrUnavailable`, `ErrConflict`, anything unclassified | 503 | `unavailable` | fail closed, `Retry-After: 1`, nothing granted |

Error bodies and logs carry fixed text only; a cause is never rendered.

## Tokens and keys

Compact ES256 JWTs (< 1,000 characters even with maximum-length identifiers):
`iss`, fixed `aud` = `kodama-runtime`, `sub` = session reference, `exp`, `nbf`,
`iat`, `jti`, plus `tid` (tenant) and `gen` (signing generation). Project,
attempt, Job/Pod and actions are deliberately **not** in the token: they are read
from the durable grant on every request, so revocation and narrowing take effect
immediately and the 1,000-character wire bound never limits ownership data.

Rotation (`KeyRing.Rotate`) atomically replaces the key and increments the
generation. Old tokens stop verifying immediately; grants registered under an
older generation can no longer be admitted, including grants restored from an old
database. There is no overlap window in this first implementation. Rotation
cannot retract delivered secrets or stop remote calls already in progress; it is
not fencing (see [upgrades and recovery](../../docs/operations/upgrades-backup-and-recovery.md)).

## Action ceiling policy (T-096)

`DefaultActions` is what every attempt gets: context, renewal, secrets,
artifacts, result, and `source.propose` for a writer. Three further actions are
granted on their own interfaces and only through an operator-configured
ceiling, `CeilingPolicy`, chosen **by step kind** from the attempt's pinned
facts:

| Action | Kind | Granted only when |
| --- | --- | --- |
| `model.invoke` | `agent` | the pinned profile version names a model |
| `tool.invoke` | `agent` | the pinned profile version names at least one tool |
| `deployment.call` | `container`, `agent` | the step declares a deployment environment (pinned `environment`) |

`CeilingConfig{Kinds: {kind: KindCeiling{Grants}}}` is the operator's upper
bound ("may hold"); the table's condition is the attempt's own fact ("does
hold"). Nothing else can be added: a configuration naming an unknown kind,
another action (default or unknown), a duplicate, or `model.invoke`/`tool.invoke`
for a kind without a profile is invalid. `NewCeilingPolicy(source, profiles)`
takes a `CeilingSource` (`StaticCeiling` for a fixed configuration, any
reloadable source otherwise; it is read at every decision) and a
`ProfileResolver` (the agents module's lookup of a pinned profile version's
capabilities; it returns booleans, never endpoints or tool names).

`CeilingPolicy.Actions(ctx, CeilingInput)` never fails and never returns less
than `DefaultActions`. An unknown or empty kind, a missing profile pin, an
unresolvable profile, a missing resolver, an unreadable or invalid source and
the zero policy all yield exactly `DefaultActions(writer)`. A failed profile
lookup drops the whole decision to the defaults rather than keeping a partial
grant.

The ceiling is an upper bound. At every use the effective authority is still
the grant ceiling ∩ the attempt's current allowed actions
(`AttemptState.Actions`), so a narrower per-attempt grant (cancellation, AR-A06's
agent-specific revocation) removes an action while the attempt runs, and an
attempt that allows more than the ceiling never widens it.

**Binding obligations (T-101, `internal/app`).**
- The same policy must feed both halves: `bootstrap.Issuer{Ceiling: &policy}`
  (and the Kubernetes registration) for the grant, and the attempt authority's
  `ActionPolicy` for `AttemptState.Actions`. Registration refuses a ceiling that
  is not a subset of the attempt's allowed actions (`forbidden`).
- Registration is idempotent by content: re-registering the same credential
  with a different ceiling is `ErrConflict`. A source that fails on one try and
  recovers on the retry therefore conflicts with the first, default-only
  registration; the operator-visible symptom is a failed attempt, never a wider
  grant.
- `bootstrap.Issuer.StepKind` supplies the step kind, which the attempt spec
  does not carry; without it a pinned profile means `agent` and anything else
  `container`. A failing report leaves the kind unknown, i.e. the defaults.

## Configuration inputs (supplied by T-027; nothing here reads the environment)

| Input | Where |
| --- | --- |
| `CeilingConfig` / `CeilingSource` / `ProfileResolver` | `runtimeauth.NewCeilingPolicy`; see the action ceiling policy above. |
| `Policy{AdmissionTTL, TokenTTL, MaxSession}` | `runtimeauth.Config.Policy`; validated, no defaults. Ceilings: 24 h, 1 h, 8 d. |
| Signing key (P-256) and generation | `jwt.NewKeyRing(generation, key)`; the deployment persists both and never reuses a generation for another key (T-029 restore). |
| Issuer URL | `jwt.NewSigner`, `jwt.VerifierConfig.Issuer`. |
| `MaxLifetime` of a token | `jwt.VerifierConfig.MaxLifetime`; set to `Policy.MaxSession`. |
| Supported wrapper versions | `runtimeauth.ExactWrapperPolicy{Supported}`; a pinned profile must match exactly. |
| Server limits | `runtimehttp.ApplyServerLimits(*http.Server)` (8 KiB headers, 16 KiB bodies). |

## Obligations for T-020 / T-021 / T-027

- **T-020 (execution)** implements `postgres.AttemptAuthority` (`ObserveAttempt`,
  and `LockAttempt(ctx, tx, scope, attempt)`), including: a lock that conflicts
  with its cancellation; an *authority revision* that changes exactly when the
  Job binding, pinned context or allowed actions change; the context digest from
  `runtimeauth.PinContext`. Its cancellation transaction locks the attempt
  (`FOR UPDATE`), then calls `postgres.RevokeAttempt` in the same transaction.
  It may add a composite foreign key from `runtime_grants (tenant_id,
  project_id, attempt_id)` to its attempts table as a later contribution after
  the baseline.
- **T-021 (Kubernetes)** implements `runtimeauth.JobObserver`: read the Job by
  name in the tenant namespace, the Pod by name, and report immutable UIDs and
  the Pod's controlling owner UID. `ErrNotObserved` means definitely absent;
  any other error is inconclusive (503). It generates the credential with
  `runtimeauth.GenerateCredential`, registers it with `Service.Register`
  (retrying with the same credential after a lost response), and places the
  credential in the Job.
- **T-027 (composition)** lists only `storage.BaseContribution()` and wires the
  constructors, mounts `runtimehttp.NewHandler`, and provides the settings above.
  The runtime-authority tables are part of the baseline schema
  (`internal/storage/migrations/001_schema.sql`), ordered after the execution
  tables they reference.
- **Result/artifact/source/secret owners** call `Service.Authorize` for routing
  and `runtimeauth.CheckSession` inside their own transaction for mutations.

## Run-resource subjects and the run_shared wire (task T-113)

The preparation workload and the support services of a `run_shared` run have their own
runtime subject ([runtime v2](../../docs/contracts/runtime-v2.md#resource-subjects)); they
are not attempts. Not wired by `internal/app` (T-116), except that the migration is joined
after the existing ones.

| Piece | Where |
| --- | --- |
| Entity `ResourceGrant`: one-time credential (digest only), one session, bound to `(tenant, project, run, resource key)` and, once admitted, to the committed workload name/UID (+ the Pod pair of a Job-owned Pod); fingerprint and deadline fixed at registration; revocation irreversible | `resource.go` |
| Consumer-owned ports: `ResourceObserver`/`ResourceTx.LockResource` (execution's committed state, locked against stop and cancellation), `ResourceStore`, `ResourceWorkloads` (S6: confirms a claimed Pod belongs to the committed UID), `ResourceTokenIssuer/Verifier` | `resource_ports.go` |
| Use cases `RegisterResource` (S5; idempotent for the same credential, `ErrConflict` otherwise, only for the committed fingerprint and a deadline not beyond the committed one), `AdmitResource` (committed `(name, UID)` compared with the claim, Pod verified outside the transaction, replay with the same nonce, `admission_consumed`, `profile_unsupported`, `resource_unbound`), `RenewResource`, `ResourceContext`, `AuthorizeResource`, `RevokeResource`, `RevokeRunResources` | `resource_service.go` |
| Authority: preparation = renew, own context, `preparation.complete`; service = renew, own context, `secret.resolve` for its pinned bindings; nothing else exists | `OpAllowed` |
| Tokens: audience `kodama-runtime-resource`, same key and generation; an attempt token on a resource route and the reverse are `forbidden` (the service tries the other audience before answering) | `jwt/`, `verifyResource`/`verifyAttempt` |
| Revocation: on every call the committed phase (stop intent, stopped/removed, cancelled/failed/terminal run, deadline), the fingerprint and the bound UID are re-read under lock; explicit revocation through `RevokeResource`/`postgres.RevokeResource` in execution's own transaction | `checkResourceSession` |
| PostgreSQL: `runtime_resource_grants` (append-only contribution `004_runtime_resource_grants.sql`, RLS like `runtime_grants`, write-once triggers, no DELETE, restricted digest lookup) and `postgres.ResourceStore`/`ResourceAuthority`; `postgres.ResourceMigrations()` is joined last in `internal/app.Contributions()` | `postgres/` |
| Wire: attempts of a `run_shared` run are registered with `RegisterV2` (ContextV2), admitted with `AdmitV2`, renewed with `RenewV2` and read with `ContextV2`; the grant's wire is its context's `contextVersion`, an attempt of the other wire is `context_version_mismatch` with nothing consumed (`Authorize` takes `ContextVersion`) | `admission.go`, `authorization.go` |
| HTTP: `/runtime/v2/attempts/{id}/…` and `/runtime/v2/runs/{run}/resources/{key}/…` (`http.NewV2Handler`), `begin` and `preparation/complete` through `BeginRecorder`/`ReceiptRecorder`, service secrets through `ResourceSecretResolver`, the shared bodies (secrets, artifacts, source, result) through the v1 owners' handlers behind a `WireGuard` (T-116 also wraps the v1 handlers with a `WireV1` guard) | `http/v2.go` |
| Bridge to execution: `execbridge.StateOf` (committed record and spec → `ResourceState`), `Begins`, `Receipts`, `Classify` | `execbridge/` |

`begin` is authorized with `context.read`: the frozen action ceiling has no action of its
own for it and it must never ride the result receipt's replay exemption; execution's
`RecordBegin` linearizes the command authorization against cancellation.

## Checks

```sh
export KODAMA_TEST_POSTGRES_ADMIN_URL='postgres://postgres:<password>@127.0.0.1:<port>/postgres?sslmode=disable'
export KODAMA_REQUIRE_POSTGRES=1
go test -race -count=1 ./internal/runtimeauth/...
go test -race -count=1 ./contracts/... ./internal/domain/... ./internal/identity/... ./internal/entrypoint/...
make test-task TASK=T-019
```

| Test | File | Covers |
| --- | --- | --- |
| `Test_cancelled_admission` | `integration/acceptance_test.go` | IT-06, PX-08 |
| `Test_wrong_job` | `integration/acceptance_test.go` | IT-06, IT-08 |
| `Test_wrong_audience` | `integration/acceptance_test.go` | IT-06 |
| `Test_refresh_after_revoke` | `integration/acceptance_test.go` | IT-06, PX-08 |
| `Test_rotated_key` | `integration/acceptance_test.go` | IT-06, IT-A05 |
| `Test_incompatible_wrapper_profile` | `integration/acceptance_test.go` | AR-02 |
| `Test_admission_replay_same_nonce_then_new_nonce`, `Test_admission_failure_recovery` | `integration/replay_test.go` | IT-06 recovery |
| `Test_action_scope_and_path_binding` | `integration/authorization_test.go` | IT-06, IT-08, AR-05 |
| `Test_admission_concurrency`, `Test_cancel_admit_refresh_race` | `postgres/concurrency_test.go` | one-time consumption, PX-08 ordering |
| `Test_runtimeauth_tenant_isolation`, `Test_migration_reapply` | `postgres/` | IT-02, IT-04, IT-05 |
| `Test_grant_transitions` and friends | `grant_test.go` | entity invariants |
| `Test_token_validation_and_bounds`, `Test_key_rotation`, `Test_rotation_races` | `jwt/verifier_test.go` | token rules, rotation |
| `Test_http_contract_and_safe_logging` | `http/contract_test.go` | strict decoding, errors, logging |

The execution authority and the Job observer in these tests are controlled
fixtures (`rtest`). They prove this module's behavior against real PostgreSQL;
they are not Kubernetes or execution-module qualification, which T-020, T-021
and the kind tests supply.

## Rechecking before durable mutations

An `Authorization` (or `ResourceAuthorization`) is a decision made before the
request body is read. The v2 `begin` and preparation-receipt handlers therefore
call `Service.Reauthorize` / `ReauthorizeResource` after decoding the body and
immediately before recording; `execbridge.Begins` and `Receipts` repeat it (an
early refusal) and refuse (unavailable) when no `Recheck` is wired.

That is not atomic: a standalone revocation (`Service.Revoke`, `RevokeResource`,
`RevokeRunResources`) is a single `UPDATE` of the grant row that changes no
execution state, so it can commit after the recheck and before execution records.
Execution therefore checks the grant itself, in the transaction that records:
the bridge passes the verified session and signing generation as an
`execution.GrantClaim`, and `RecordBegin` / `RecordPreparationReceipt` call the
store's `execution.GrantFence` after the run row is locked. The PostgreSQL fence
(`execution/postgres.SQLGrantFence`, installed by `NewRuntimeStore`) reads
`runtime_grants` / `runtime_resource_grants` with `FOR SHARE` and refuses a
revoked, missing or foreign-session grant (`ErrAttemptRevoked` /
`ErrResourceRevoked`). Either the revocation commits first and is seen, or the
record commits first and the revocation waits for its share lock and lands after
it. Lock order is unchanged (quota/work -> run/attempt -> grants -> secrets ->
workspace -> events): the revocation locks only the grant row, so it cannot
deadlock with a record, which takes the run first. A store without the fence (a
component store, or a composition that forgot it) refuses these two records with
`ErrNotConfigured`; nothing skips the check.

## Source-set wire and Git authority (task T-150, runtime v3)

The runtime wire of a run that pins a source set of internally hosted repositories ([runtime v3](../../docs/contracts/runtime-v3.md),
[ADR 0011](../../docs/adr/0011-internal-tenant-git-service.md), GR-06, WS-19, PX-23–PX-25). **Not wired by `internal/app`
(T-132).** The v1 and v2 behavior, tokens, tables and routes are unchanged (the earlier tests pass untouched); a run pins its
context version at admission and is served only on the matching wire, in both directions (`context_version_mismatch`, nothing
consumed, issued or changed).

| Piece | Where |
| --- | --- |
| `WireV3`, `WireOf` (`contextVersion` 3), `PinContextV3`, `RegisterV3`, `AdmitV3`, `RenewV3`, `ContextV3` (same attempt token, audience and grant rules as v2) | `admission.go`, `authorization.go` |
| Resource subjects on the v3 wire: `ResourceState.ContextV3`, `AdmitResourceV3`, `RenewResourceV3`, `ResourceContextV3`, `ResourceAuthorizeRequest.ContextVersion` (a resource of the other wire is a mismatch before anything is done) | `resource_ports.go`, `resource_service.go` |
| Runtime-issued Git grants: `Service.SeedGitGrant`, `Service.IntegrationGitGrant`, `Service.IssuedGitGrants`; consumer ports `GitIssuer`, `GitRepositories`, `IntegrationApprovals`, `IssuedGrants`; `Config.Git` | `git_ports.go`, `git_service.go` |
| Records of the grants issued to an attempt: migration `014_attempt_git.sql` (`postgres.AttemptGitMigrations()`), `postgres.IssuedGrants`, shared `rtest.IssuedGrantsContract` (memory and PostgreSQL) | `postgres/` |
| `/runtime/v3/...` routes: `http.NewV3Handler` (`V3Config`), `WireGuard` for WireV3, the v3 error envelope with `publication_conflict` details | `http/v3.go`, `http/v2.go`, `http/errors.go` |
| Bridge to execution: the v3 resource state (`StateOf` serves a source-set run on the v3 wire only), `BeginsV3`, `ReceiptsV3`, `LeaseOf`, `PreparationGrantRef` | `execbridge/v3.go` |
| Adapters over the Git authority, execution and the gates module (no business rule): `Grants` (`execution.GitGrants`, renewal, preparation), `Leases` (`gitauth.Attempts` and `gitpush.Attempts`), `Issuer` (`GitIssuer`), `Approvals` (`IntegrationApprovals` over `gates.Service.IntegrationApproval`), `GateDecisions` (`gitauth.Gates`) | `gitgrants/` |

### Two different secrets

The route authority of every v3 attempt route is the **runtime token** (the unchanged `kodama-runtime` token and its grant). The
**Git credential** is a different secret: a gitauth grant (`gitc_…`, audience `kodama-git`) whose authority is re-read at every
Git use. The first never buys the second except through the routes below, and the second never reaches anything the runtime
authority stores or logs: `GitCredential` (here) and `execution.GitSecret` print `[redacted]` in every formatter and encoder, the
context carries file names and a deterministic grant *reference* only, the records of issued grants hold claims (no digest, no
credential), and a credential appears in exactly two places: the body of the response that issues it (`Cache-Control: no-store`)
and, for the attempt's own, `AttemptSpec.Git` once. A repeat of an issuance returns the same grant with a **fresh** credential
that replaces the earlier one (gitauth stores digests only); that, and `Grants.RenewAttempt`, are the only times a credential is
returned twice.

### The attempt's own credential

`execution` requests it after the attempt is committed and before its workload starts (`GitGrants.IssueAttempt` with
`AttemptGrantFor`: the pinned context's repositories and ref patterns, expiry at most the run's deadline and 24 h) and revokes it
in the transaction that ends the attempt's authority (the stores' `GitGrants` peer; `gitauth/{postgres,memory}.RevokeAttempt`).
`gitgrants.Grants` is that port over `*gitauth.Service`. **The issuance/cancellation race is closed here, not merely narrowed:**
the Git authority reads the attempt's lease outside its own transaction, so a cancellation can land between its read and its
insert; `Grants.IssueAttempt` therefore, after the grant is committed, reads the attempt's liveness under the run's row lock
(`execution.Store.Update` with a mutation that only reads: the lock the cancelling transaction takes before it revokes) and
revokes the grant of an attempt that ended (`execution.ErrAttemptRevoked`, nothing delivered). A live answer under the lock means
any later cancellation finds the committed grant and revokes it. The runtime Git grants do the same under `CheckSession`'s lock
(`closeIssuanceRace`). Execution's own re-read after issuing stays and is now redundant.

`Leases` answers what the Git services ask of an attempt: `gitauth.Attempts.Lease` (live, deadline) and `gitpush.Attempts.Live`.
**Live** is the committed record's: the attempt is running, its control is not revoked (cancellation, failure drain, timeout,
supersession and an accepted result all end it), its run is neither draining, terminal nor past its deadline. The **deadline** is
the run's pinned deadline, which is what `AttemptGrantFor` caps a grant at, so the lease lasts as long as the grant (execution's
`GitGrants` contract). Each Git service re-reads it at every credential use and every commit intent, so a cancelled attempt is
refused at its next push *before* its grants are revoked; revocation is the cleanup, not the authority.

**Renewal.** The v3 contract has no route for renewing the Git credential and the workload never needs one inside the
24-hour cap, so renewal is a controller-side use case, `Grants.RenewAttempt(scope, attempt)`, for the runtime adapter that wrote the
credential file (T-152). It rebuilds the request from the attempt's committed context (never from the caller), reads the lease
before and after and refuses an attempt that is not running, was revoked, whose run is draining, terminal or past its deadline
(`execution.ErrAttemptRevoked`); a revoked, consumed or expired grant is refused by gitauth. It returns the same grant with a fresh
credential and **never extends the grant**: a replay keeps the first issuance's expiry, so it cannot reach past the run's deadline
or the contract's workload lifetime. An attempt that ends while its credential is rotated has its grants revoked and gets nothing.

### The runtime-issued grants

`POST /runtime/v3/attempts/{attemptId}/git/seed` and `…/git/integration` (the routes of `runtimev3.Operations`). Their action is
`context.read`: the action ceiling is a closed list in the database (a migration would be needed to extend it, and execution's
`AttemptState.Actions` must follow) and the begin fence rides it for the same reason. What makes the routes safe is not the action:
the use case reads the capability from the attempt's **own pinned context in the durable grant** (never the request), re-verifies the
session in one tenant transaction (the module-wide lock order: attempt, then grant) and refuses on the first failure, and the
grant's claims are built by the control plane from its records:

- **Seed** (PX-25). The context must advertise `runtimeGrants.seed` (only a provisioning run) otherwise 403; the repository must be
  one **this run created** in its project (`GitRepositories.CreatedByRun`, otherwise 404 `not_found`, indistinguishable from an
  unknown one, and nothing else is asked); the Git contract's seed eligibility with its 409/422 codes; then `GitIssuer.IssueSeed`
  (kind `seed`, exactly `refs/heads/<defaultBranch>`, one repository, `seed: true`, at most 15 minutes and the run's deadline, issued
  to this attempt), `runtimev3.CheckSeedGrant` on the answer and the record. Idempotent until consumed (a repeat is the same grant
  with a fresh credential). gitauth keys a seed by its attempt, so a second repository by the same attempt is 403 (a limit of the Git
  authority, recorded below).
- **Integration.** The context must advertise `runtimeGrants.integration` and the repository must be a write source. The approval is
  `gates.Service.IntegrationApproval` (mapped by `gitgrants.Approvals`), never the request: no approval, an absent or foreign gate is
  403 (no grant exists); a default branch that moved is **409 `publication_conflict`** with `details {reason: base_moved,
  observedTip}`; a branch **already at the candidate** is the same 409 with `reason: already_integrated` and the candidate as
  `observedTip` and **no grant** (the integration was applied). The runtime v3 contract has no success shape without a grant, so the
  wire cannot carry "success"; Go callers read `errors.Is(err, ErrAlreadyIntegrated)`. Then `GitIssuer.IssueIntegration` (kind
  `integration`, one repository, update exactly the integration ref, constraint `{ref, from, to, gateId}` taken from the approval),
  `runtimev3.CheckIntegrationGrant` and the record. gitauth reads the gate's decision again when it issues (`GateDecisions`), so a
  reviewer who loses the authority between the route's check and the issuance gets no grant.
- **Recorded, then reported.** Every issued grant is recorded (`runtime_git_grants`: claims only) before the credential is returned,
  idempotently by grant ID. That record is how the result module recognizes a commit output on the default branch (the
  `Issued` claims of `execution.ResultCommitV3`): from the control plane's own records, never the caller's word. A crash between
  issuing and recording leaves a grant nobody received; the repeat returns it and records it.
- **Errors.** Business refusals are `ErrRefused` carrying the contract code (`not_found`, `forbidden`, `hosting_mismatch`,
  `repository_archived`, `repository_not_ready`, `repository_not_empty`, `seed_pending`); the HTTP layer renders only a code the
  operation's v3 inventory lists, otherwise `unavailable`. An attempt that is no longer live is `attempt_revoked`, whichever side saw it.

### What T-132 (and T-152) must wire

1. Migrations: register `postgres.AttemptGitMigrations()` at slot `014` (after `011` run sources and `013` source provenance, before
   `015`); nothing else changes in this package's earlier contributions (`ResourceMigrations()` still loads exactly `004`).
2. `runtimeauth.Config.Git = &GitConfig{Issuer: gitgrants.NewIssuer(gitauthSvc, clock), Repositories: <adapter>, Approvals: gitgrants.NewApprovals(gatesSvc), Issued: postgres.NewIssuedGrants(db, clock)}`.
   `Repositories` is the repository module's read plus **the record of which run created which repository**, which the repository
   module does not keep today (`repository.Repository.CreatedBy` is an actor; productmcp's `Repositories.Create` must record the
   creating run and attempt and the adapter's `CreatedByRun` must read it). Without it no seed is ever issued (fail closed).
3. `gitauth.Config{Attempts: gitgrants.NewLeases(executionStore, clock), Gates: gitgrants.NewGateDecisions(approvals)}`;
   `gitpush.Dependencies.Attempts` is the same `Leases`; `execution.Dependencies.GitGrants = gitgrants.NewGrants(gitauthSvc,
   executionStore, clock)` over a store that revokes through the `Peers.GitGrants` peer (`gitauth` `RevokeAttemptIn`).
4. Mount `runtimehttp.NewV3Handler` (`Begins: execbridge.BeginsV3{Exec, Recheck}`, `Receipts: execbridge.ReceiptsV3`, `ResourceSecrets`,
   `Shared`: the v1 handlers of secrets and artifacts, `Direct{"submitResult"}`: `resultshttp.NewV3Handler`) next to the v1 and v2
   handlers; wrap the v1 and v2 handlers with the matching `WireGuard`. The resource states of the v3 wire come from `execbridge.StateOf`
   (it serves a source-set run on the v3 wire only). Register attempts of context version 3 with `RegisterV3`.
5. T-152: write the attempt credential file from `AttemptSpec.Git` before the entrypoint starts, rotate it with
   `Grants.RenewAttempt` when needed, issue the preparation grant with `Grants.IssuePreparation` and write the files the context names.

### Decisions and limits recorded here

- The two runtime Git routes ride `context.read` (above); the pinned context's capability, the creating-run record and the gate
  approval are the authority.
- `already_integrated` is a 409 with a reason, not a success: the frozen contract has no success without a grant.
- A provisioning attempt can seed **one** repository: gitauth keys a seed grant by `(kind, attempt)`. Seeding several repositories
  needs per-repository keys in gitauth (not this task's paths).
- A renewal rotates the secret and never extends the expiry (gitauth's replay keeps it): a run longer than the 24-hour workload
  grant lifetime cannot be served by one attempt grant. Extending needs a gitauth change.
- A preparation grant is read-only and bounded by the preparation's deadline, but gitauth cannot revoke a grant by workload (its
  revocation is by attempt), so it ends at its expiry, not with the preparation.
- Commit outputs are verified by the result module (`internal/results`), not here.

### Source-set tests

| Test | File |
| --- | --- |
| `Test_unit_runtime_routes_new_version`, `Test_unit_runtime_routes_git_grants`, `Test_unit_runtime_routes_integration_conflict`, `Test_unit_runtime_routes_resources` | `http/v3_contract_test.go` |
| `Test_unit_attempt_git_credential_delivered_and_renewed`, `…_renewal_refusals`, `Test_unit_attempt_git_credential_revoked_on_cancel`, `Test_unit_attempt_preparation_grant_is_read_only` (real gitauth) | `gitgrants/gitgrants_test.go` |
| `Test_unit_runtime_seed_grant_over_gitauth`, `…_integration_grant_over_gitauth`, `…_grant_issuance_race_over_gitauth` | `gitgrants/runtime_grants_test.go` |
| `Test_unit_runtimeauth_v3_wire_admission_and_fence`, `…_v3_resources`, `…_git_seed_grant`, `…_git_seed_needs_the_capability`, `…_git_integration_grant`, `…_git_grant_issuance_race`, `…_git_grants_refused_after_authority_ended`, `…_git_not_configured_fails_closed` | `rtest/v3_unit_test.go` |
| `TestStateOfServesASourceSetRunOnTheV3WireOnly`, `TestLeaseOfIsTheAttemptsOwnState`, `TestV3RecordersServeExecutionsAnswers` | `execbridge/v3_test.go` |
| `Test_unit_runtimeauth_issued_grants_memory_contract`, `TestPostgresIssuedGrantsContract`, `Test_attempt_git_migration_reapply`, `Test_attempt_git_isolation_and_guards` (real SQL) | `rtest/`, `postgres/` |

```sh
make check-go PKGS='./internal/runtimeauth/... ./internal/results/...'
make check-go-integration DEV_SCOPE=t150 DEV_STACK=services PKGS='./internal/runtimeauth/... ./internal/results/...'
```
