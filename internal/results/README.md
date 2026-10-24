# `internal/results` — runtime results and source finalization

Task T-022. Accepts valid attempt outputs once, without trusting arbitrary
workload IDs: the `POST /runtime/v1/attempts/{attemptId}/result` and
`/source` routes of the [step entrypoint contract](../../docs/implementation/step-entrypoint.md),
against the frozen [runtime wire contract](../../contracts/runtime/README.md).

Requirements: PX-10, PX-12, WS-04, WS-06, WS-11, IT-06; supporting SD-01–SD-08.
Named acceptance: `conflicting_result`, `foreign_artifact`, `missing_output`,
`cancel_finalization_race`, `published_git_db_failure`. Review notes, assumptions
and verification results: [`review.md`](review.md).

This module owns **no durable state**. Execution owns attempt and result
transitions, the accepted hash, history, audit and outbox. The workspace owns
publication receipts, Git compare-and-swap and reconciliation. The artifact
module owns bytes, provenance and pins. Runtime authority owns tokens and grants.
Results orchestrates them.

## Layout (ports and adapters)

| Package | Role |
| --- | --- |
| `results` | Application layer: `Service` (`Submit`, `ProposeSource`), pure validation (`Validate`, `ReferencedArtifacts`), classified errors, and the consumer-owned ports `Authority`, `Contexts`, `Evidence`, `Finalizer`, `Source`. Imports only the shared contract and values. |
| `results/adapters` | Port implementations over the peers: `Authority` (runtimeauth), `Execution` (`Finalizer` + `Contexts`), `Artifacts` (`Evidence`), `ArtifactAuthority` (T-015's `AttemptAuthority`), `Bundles` (`workspace.Bundles` over sealed objects), `Source` (workspace runtime translation). |
| `results/postgres` | The join of three owners' transactions: `AttemptAuthority` (T-019's `AttemptAuthority` port over execution's rows), `ExecutionStore` and `WorkspaceStore` decorators, `Guard`. No tables, no migrations. |
| `results/http` (`resultshttp`) | The two routes over `net/http`: bounded strict decoding, no-store responses, request IDs, sanitized error envelopes, status mapping within the frozen error inventory. |

## Result acceptance order (frozen)

`Service.Submit`:

1. **Authenticate**: verify the bearer and the route binding (`runtimeauth.CheckSession`, action `result.submit`). Scope, run, attempt and session come from the grant, never the request.
2. **Validate the request**: strict decoding (transport), then `ResultRequest.Validate`, which recomputes the canonical hash (`422 result_hash_mismatch`).
3. **Receipt first**: if execution already accepted a hash for the attempt, go straight to commit. The same hash returns the original acknowledgment; a different one `409 result_conflict`. No artifact is read, so an artifact outage or retention never replaces a receipt decision.
4. **New result**: load the pinned context, read artifact evidence through the artifact owner and `Validate` it (below).
5. **Protect** the consumed artifacts (pin owner `attempt_finalization`/attempt), then **commit** through `execution.Service.CommitResult`, which, inside its own transaction, repeats `CheckSession` under the run lock (the guarded `ExecutionStore`), enforces the startup and execution deadline, terminal immutability, declared outputs, the writer source rule and writer settlement (an unsettled publication blocks success).

Success is never inferred from process exit or logs.

### Evidence validation (`results.Validate`)

* Artifact IDs are collected **only** from declared artifact-valued outputs and diagnostic references; ordinary string outputs are never artifact IDs.
* Existence/ownership first, for every reference: absent, foreign-tenant, foreign-project, foreign-attempt and removed references are the same `404 not_found`; an unfinished upload is `409 artifact_incomplete`. These win over any output error.
* Diagnostics are checked independently of outputs (file kind, `diagnostic` role): an early missing-output error cannot let an unverified diagnostic reference reach execution.
* A completed result whose declared outputs are missing, mistyped, of an untrusted kind/role, or whose provenance is not the attempt's **pinned input source** (never the writer's newly proposed commit) reaches execution, which records the durable validation failure: a failed acknowledgment, readable diagnostics, no success outputs, no retry.
* Failed and `needs_input` results carry diagnostics only; any success output or source commit is `400`.

## Source proposals

`Service.ProposeSource` authenticates `source.propose` (granted to writers only),
protects the bundle artifact, and calls the workspace owner
(`workspace/runtime.Source` → `workspace.Service.ProposeSource`): the pending
publication is durable **before** Git CAS; the response is `200` accepted,
`202` pending (must settle before the attempt can succeed) or `409
publication_conflict` with `details.observedTip`. Publication credentials never
reach the workload. The recording mutation of a new publication is guarded
(`WorkspaceStore`): the session check runs under the execution lock first, so a
callback that lost the race against cancellation cannot initiate publication; a
publication authorized earlier may still finish remotely and is settled by the
workspace reconciler, which never turns a cancelled attempt into success.

## Authority and lock order

`execution (run row) → runtime grant → workspace row → event backlog`.
`ExecutionStore` repeats `CheckSession` inside `UpdateInTx` after the run lock;
`WorkspaceStore` runs it before entering the workspace `UpdateInTx`. Neither
holds a SQL transaction across Git, S3, registry or executor calls. Contexts
without a session (dispatch, observation, cancellation, reconciliation) take the
owners' own undecorated paths.

`result.submit` stays authorized after the attempt leaves `active` while the grant
is not revoked and its binding still matches (`runtimeauth.CheckSession`'s receipt
exception), so a retry reaches execution. Explicit revocation, expiry, wrong
audience, retired signing generation or a changed binding deny the callback.

## Failure and recovery

| Event | Behavior |
| --- | --- |
| Lost response after commit | Retry observes the receipt: `200` with the original `acceptedAt` (the persisted finish time). |
| Unknown commit outcome | `503 unavailable`; nothing is acknowledged; retry observes whatever committed. |
| Artifact store outage on a new result | `503`; no state changed. |
| Unsettled publication at result time | `503` retry guidance (`/result` has no pending response); the result cannot succeed until it settles. |
| Git success, database failure | Publication stays `pending`; the writer slot is retained; the workspace reconciler records the published commit from the remote; the repeated proposal and result then settle once. |
| Cancellation vs. finalization | Serialized on the run lock; the loser sees the winner's state. A late result is recorded as evidence and rejected `403 attempt_revoked`. |

## Composition (T-027)

1. `runtimeauth` store over `postgres.AttemptAuthority` (namespace resolver and optional action policy are trusted configuration).
2. `postgres.NewGuard(authority, keyRing.Generation, clock)`.
3. Wrap execution's and the workspace's PostgreSQL stores: `postgres.NewExecutionStore`, `postgres.NewWorkspaceStore`; build `execution.Service` and `workspace.Service` over the **decorated** stores for the result/source request path (reconcilers may keep the plain ones).
4. `adapters.NewAuthority`, `NewExecution`, `NewArtifacts`, `NewSource` (over `workspace/runtime.NewSource`), `results.NewService`, `resultshttp.NewHandler`.
5. Artifact routes stay with `artifactshttp`; give `artifacts.NewService` `adapters.NewArtifactAuthority` and the runtime-token authenticator must emit `adapters.RuntimePrincipal(tenant, session)` as the principal ID.
6. `workspace.Dependencies.Bundles = adapters.NewBundles(artifactsRepository, objectStore)`.
7. Execution's cancellation transaction must revoke the attempt's grants (`runtimeauth/postgres.RevokeAttempt`) in the same transaction as its run lock.

## Unit qualification (task T-081)

Requirements: **PX-05**, **PX-07**, **IT-03**, **IT-06**, **AC-06**; SD-13/SD-A11.
`resultstest` holds `Memory`, the `development` counterpart of the join the
production composition makes between runtime authority, execution, the artifact
owner and the workspace: every port of `results.Dependencies` over one `memdb`
store (one all-or-nothing `Update` per call), an expected-tip compare-and-swap
remote outside the store, controllable clock, and injectable commit, push and
acknowledgment faults. `resultstest.Contract(t, open)` is the shared suite; the
memory side runs it unconditionally (`Test_unit_results_logs_contract`) and
`postgres.TestPostgresContract` (`postgres/contract_test.go`) runs the same suite
on the real composition - PostgreSQL with the execution, workspace, artifact and
runtime-grant schemas, S3, a Git server and this module's adapters and guards -
behind `layer.SkipService`, so the unit gate compiles it and skips it and the
`test` environment runs both. The Contract also holds the memory adapter to the
real publisher's policy (the writer's reserved expected tip, a complete
bundle-role artifact of the attempt, receipt first), to the immutability of the
pinned context against caller mutation, and to the same result of a lost race.

* `Test_unit_result_replay` - a lost acknowledgment replays the persisted
  acceptance time; an unknown commit leaves no trace; concurrent duplicates and a
  rival hash leave one receipt; an unknown push, a Git success with a failed
  receipt write and a lost intent write stay pending (202) or unpublished, never
  success, and settle once from the remote (`Reconcile`).
* `Test_unit_result_revocation` - revoked, stale, foreign, expired and forged
  credentials are denied before evidence is read, pinned or committed; the guard
  repeats the check in the commit and intent transactions (a cancellation between
  the first check and the mutation wins); a late result is evidence, not the
  receipt; `result.submit` keeps its receipt exception until explicit revocation.
* `Test_unit_result_protection` - only consumed evidence is pinned and a pinned
  artifact survives retention.

Not modelled: SQL, row-level security, lock order, isolation, real tokens, Git
objects, object bytes and restart persistence (SD-14); a pass here is not
evidence for them.
