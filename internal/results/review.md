# T-022 review notes

Candidate-specific evidence lives in the harness handoff; this file records what a
reviewer should check, the assumptions the integration owner confirms, and the
verification that was actually run.

## Baseline

Requirements PX-10, PX-12, WS-04, WS-06, WS-11, IT-06; supporting SD-01–SD-08 and
the tenant persistence invariants. Acceptance `conflicting_result`,
`foreign_artifact`, `missing_output`, `cancel_finalization_race`,
`published_git_db_failure` (relevant portions of PX-A06/PX-A07, WS-A03/WS-A04/
WS-A05/WS-A07, IT-A04/IT-A05). Decisions: confirmed C-01/C-03/C-11/C-12/C-15/C-16;
implementation baseline P-01/P-05/P-06/P-07/P-16. ADRs 0002, 0003, 0005, 0006.
O-11 (retention) and O-18/O-19 (hosted authority) are untouched: nothing here selects
a retention period, deletes a source or object, or grants platform authority.

## What to check

* **Single state owners.** Execution owns attempt/result state (`internal/execution`); the
  workspace owns publication receipts; this module has no table, migration or
  durable entity (`results/postgres` has none). `Validate` is a pure function.
* **Receipt first** (`service.go`): the artifact owner is not read when execution already
  has an accepted hash (`TestSubmitOrdering`, `Test_conflicting_result`,
  `TestResultAtomicityAndIsolation/an_artifact_outage_...`).
* **Evidence precedence** (`validation.go`): not_found/incomplete before output errors;
  diagnostics checked independently; untrusted output artifacts are handed to execution
  with an empty kind so *its own* check records the durable failure.
* **Lock order** (`postgres/execution.go`, `workspace.go`): execution run lock → grant
  `FOR SHARE` → workspace row → event backlog. Removing either guard makes
  `Test_cancel_finalization_race` fail (verified by mutation during development).
* **No SQL transaction spans Git, S3, registry or executor calls.** Git/S3 are called by the
  owners outside their transactions; the guard transactions only wrap owner `UpdateInTx`.

## Assumptions for the integration owner to confirm

1. **Acknowledgment timestamp.** `execution.ResultAck` omits the wire-required `acceptedAt`.
   `adapters.Execution.Commit` derives it from the immutable accepted attempt's `FinishedAt`
   after checking the persisted hash and terminal status match the acknowledgment. Confirm
   with T-020's owner that `FinishedAt` of the accepted attempt is the acceptance time
   (it is set by `succeed`/`fail` in the result transaction).
2. **Unsettled publication on `/result`.** There is no pending response or
   publication-conflict code for `/result`; an unsettled writer publication (execution's
   `ErrBusy`) and not-yet-published writer evidence (`ErrStale`) map to `503 unavailable`
   with `Retry-After: 1`. `409 publication_conflict` stays on `/source`. A reported
   `sourceCommit` that is never published therefore stays unacknowledged until the
   wrapper's bounded finalization ends. Confirm against the frozen operation table.
3. **Transaction integration.** The decorators wrap the owners' `UpdateInTx` methods; they do
   not change execution's transitions or add a ledger. T-027 must inject
   `postgres.ExecutionStore` into the `execution.Service` that serves `/result` and
   `postgres.WorkspaceStore` into the `workspace.Service` that serves `/source`.
   A context without a session takes the owner's undecorated path by design (dispatch,
   observation, cancellation, reconciliation).
4. **Operational settings.** Namespace resolution, the action policy and the signing
   generation are constructor dependencies (`postgres.NewAttemptAuthority`, `NewGuard`);
   no environment variable is invented. `postgres.DefaultActions` grants the entrypoint's
   actions and `source.propose` to writers only. No retention policy is selected; result and
   bundle pins (`attempt_finalization`) are never released by this module except a bundle's
   pin on `Bundles.Settled`.

## New assumptions made while implementing

5. **Grant revocation on cancellation (T-020 gap).** Execution's cancellation records
   `attempt_controls.revoked_at` but does not call `runtimeauth/postgres.RevokeAttempt`.
   Results derives the attempt phase from execution's rows (a running attempt whose control
   is revoked is `cancelled`), so every non-result action is denied and `/result` reaches
   execution, which records a late result. Revoking the runtime grants in the same
   transaction (`runtimeauth/README.md`) is still required of T-020/T-027 so that explicit
   revocation also denies `result.submit`.
6. **Artifact routes' principal.** `artifactshttp` takes the caller from `httpapi.Principal.ID`.
   `adapters.ArtifactAuthority` reads it as `runtime:<tenant>:<session>`
   (`adapters.RuntimePrincipal`); T-027's runtime-token authenticator must emit that.
   The authority checks the grant with the `artifact.reserve` action for both artifact
   routes, bounds upload instructions by the session maximum, and takes provenance, limits and
   declared outputs from the pinned context.
7. **Guarded workspace mutations.** Only `workspace.Store.Update` calls that load an attempt's
   receipt are guarded (recording and settling that attempt's publication). A
   publication authorized before cancellation can therefore finish on the remote while this
   request can no longer record it; the workspace reconciler (undecorated) records it
   (`Test_cancel_finalization_race`, last case) and the attempt stays cancelled.
8. **Untrusted outputs fail durably.** An output artifact with the wrong role/kind or with
   provenance other than the attempt's pinned input source is a durable validation failure
   (like a missing output), not a request error. A bad diagnostic is a request error
   (`422 output_invalid`) so no unverified diagnostic reference is retained.

## Verification (this candidate)

| Command | Result |
| --- | --- |
| `go test -race -count=1 ./internal/results/... ./test/integration/results/...` | pass |
| `go test -race -count=3 ./test/integration/results/` | pass (stability) |
| `make fmt lint` | pass |
| `make test` | pass |
| `make test-task TASK=T-022` | `PASS 5 acceptance test(s)` |

| Acceptance | Test | Dependencies actually used |
| --- | --- | --- |
| `conflicting_result` | `test/integration/results/result_test.go: Test_conflicting_result` | PostgreSQL |
| `foreign_artifact` | `artifacts_test.go: Test_foreign_artifact` | PostgreSQL, S3 |
| `missing_output` | `result_test.go: Test_missing_output` | PostgreSQL, S3 |
| `cancel_finalization_race` | `races_test.go: Test_cancel_finalization_race` | PostgreSQL, S3, Git |
| `published_git_db_failure` | `recovery_test.go: Test_published_git_db_failure` | PostgreSQL, S3, Git |

Supporting: `TestRuntimeContractFixtures` (`http/contract_test.go`), `TestEvidenceValidation`
(`validation_test.go`), `TestAuthorityDenials` and `TestReceiptAndRevocation`
(`authorization_test.go`), `TestPublicationConflictAndRecovery` (`source_test.go`),
`TestResultAtomicityAndIsolation` (`transactions_test.go`), `TestLateResultAndDiagnostics`
(`result_test.go`), `TestArtifactAuthorityBridge`, `TestModuleBoundaries`.

NATS delivery is not implemented here: the outbox is inspected directly for atomicity;
broker delivery is not claimed.
