# `internal/gates` — canonical human gates

Task T-034. Implements [delivery and quality gates](../../docs/specs/delivery-and-quality-gates.md)
(DQ-01–DQ-06, DQ-11, DQ-14) and the gate half of [pipeline execution](../../docs/specs/pipeline-execution.md)
(PX-13, PX-09) with SD-01–SD-08, IT-02–IT-05 and IT-10/IT-11. A gate pauses progression on **one
attributable human decision about one identified subject** and consumes no compute: a ready gate
step stays `pending` in durable records with its gate reference, zero attempts and no reservation.

Status: implemented and tested against real PostgreSQL (plus real S3, NATS and HTTPS Git where the
test names say so). Nothing registers these routes, migrations, consumer, adapters or scheduler yet;
that is the integration owner's job (see *Integration owner*). The deployment module (T-035) is
implemented in `internal/delivery`, but this package consumes it only through its own injectable
`Deployments` port and transaction-bound `DeploymentBinder`: the component tests use a controlled
deployment fixture, which is not evidence about the real joined path. Hosted authority is qualified by
T-061, the real gate transaction/authorization join by T-073, and T-072 activates the selected composition.
This package does **not** implement the agent report producer (T-031), AI defect creation, quorums,
overrides or any approval authority for an agent identity.

## Layout

| Path | Purpose |
| --- | --- |
| `values.go`, `errors.go` | Identifiers, statuses, actors, permissions, sentinel errors (`StaleError`, `ConformanceError`) |
| `subject.go`, `gate.go` | Immutable `Subject` with canonical bytes/digest; `Gate` entity (`pending → approved\|rejected`), decision, separate invalidation, derived expiry |
| `applicability.go` | Pure `CheckCurrent`/`Revalidate` of a gate against current facts |
| `prepare.go` | `RequestForStep`: build and verify the subject (evidence, candidate, deployment, conformance), pin evidence, persist |
| `service.go`, `reconcile.go` | Use cases: read, list, decide, bounded restart-safe reconciliation |
| `ports.go` | Consumer-owned ports (store, authority, evidence, pins, candidates, deployments, conformance) |
| `postgres/` | `T034_001_gates.sql`, the store, keyed decision helper, gate-aware execution store, decision-wakeup consumer, scans |
| `adapters/` | Bridges to identity (reviewer authority), artifacts (metadata, bytes, pins), execution (conformance baseline/report identification) and workspace (candidate history) |
| `http/` | The three routes and DTOs generated from `api/gates.yaml` |
| `gatestest/` | Fixtures: real PostgreSQL with every dependent schema; controlled component seams only for deployment facts, workspace history and fault injection |
| `gatestest/gatescontract/` | The one exported `Contract(t, open)` suite of the gate port (SD-13): tenant isolation, one decision per gate, replay, stale subjects, revoked reviewers, expiry, waiting without executor capacity, agent-candidate conformance (missing/invalid/blocked/valid report), detached stored values |
| `gatestest/memory/` | Development-only memory adapter (T-084): `gates.Store` over `memdb`, fake current authority, evidence, candidate and deployment ports and an in-memory execution peer that applies the real gate mutations. Runs the contract and the `unit_gate_*` tests with no service. Not modelled: SQL, row locks, lock order, RLS, isolation levels, the claim-time guard behind gates (T-073 proves those). `gatestest.NewPostgresHarness` runs the same contract on PostgreSQL via `integration/contract_test.go` (skipped in the unit layer) |
| `testdata/canonical_subject.json` | Fixed canonical subject bytes and SHA-256 digests |
| `../execution/gate-step.go` | Execution's side of the seam: subject-input resolution, gate binding/completion/rejection, gate protection of later steps |
| `../../api/gates.yaml` | OpenAPI 3.0 fragment referencing `api/common.yaml` |
| `review.md` | Baseline, assumptions and verification evidence |

The domain imports only the standard library, `internal/domain`, `internal/execution` (aggregate
types), `internal/pipeline` (step kinds) and the runtime contract; never pgx, HTTP, S3 or Git.

## Model

* **Subject.** `tenantId, projectId, workItemId, contentRevision, candidateCommit, definitionId,
  evidence[{id,digest}], deploymentId, deploymentConfigDigest`, built **only by the server**. Canonical
  form is `runtimev1.Canonicalize` (UTF-8, ordered keys, evidence sorted by `(id, digest)`, explicit
  nulls and empty arrays, integers only); the digest is SHA-256. An identical duplicate evidence
  reference collapses and one artifact with two digests is refused.
* **Status.** Exactly `pending`, `approved`, `rejected`. Expiry is derived from the snapshotted
  `deadline` (`!now.Before(deadline)`); invalidation is a separate timestamp/reason (with the
  superseding commit/revision when known) that never rewrites the decision.
* **Decision.** One reviewer: a *human* with the fixed `reviewer` role (`gate:decide`). Admin, engineer,
  viewer, workload, service and operator identities cannot decide; an author who is also a reviewer can.
  The actor comes from the verified principal, never the body. The decision, its reason and time are
  immutable (database guard); a rejected gate can never become approved; rework is a new run.
* **Required evidence.** The step's evidence references resolve only from pinned run inputs or successful
  ancestor outputs. Each artifact must share the gate's tenant/project, be usable (not removed), come from
  the named producer step of this run, and have a provenance commit consistent with the candidate (the
  candidate producer's own artifacts are measured against the commit it was given). Unverifiable evidence
  means no gate. The artifacts are pinned for the gate (`artifact_pins`, owner kind `gate`) and bound
  into the subject by ID and digest; the snapshot (`gates.required`, `gate_evidence`) is immutable.
* **Candidate.** The last successful writer ancestor's published commit, otherwise the run's original
  commit; unordered writers or a writer without a commit are refused. It must lie in the work item's
  workspace history (verified through the workspace adapter, outside any transaction).
* **Agent conformance (DQ-14, SD-08).** When the candidate was produced by an *agent* writer, the gate
  validates the producer's declared report artifact with T-003's strict decoder and `CheckConformance`
  against the baseline pinned in the **producing attempt's immutable context** (never the report's own
  claims, never the latest profile). Report, baseline and candidate are immutable, so the outcome is
  recorded once in the snapshot: `verified` or `blocked` with a denial code. A blocked report is preserved
  for review (approval refused with `422 <code>`, rejection allowed). Verified evidence is **not** an
  approval: the gate waits for a human.
* **Progression (PX-13, DQ-04).** `CompleteStep` consumes an approval under the run lock: applicable gate
  record, current content revision, candidate, definition, evidence digests/usability and the deployment
  (identity, configuration digest, owning project and recorded candidate) all still match the subject, and the **original reviewer still holds `gate:decide`**
  (read through the transaction, so a revocation committed during a lock wait is seen). **Expiry is
  judged after the waits:** the gate lock, the fact reads and the reviewer-authority read can block, so
  the injected clock is sampled after the facts are read and again, for every consumed gate, after the
  last authority wait and immediately before progression is accepted (a protected claim rechecks all of
  its protecting gates at the end, so an earlier gate cannot expire unnoticed while a later one is
  checked). An approval that reached its deadline during any wait never authorizes progression; it stays
  `approved` (no invalidation, no new record) and a retry or redelivery cannot renew it. The step moves
  `pending → succeeded` explicitly with no attempt. Rejection fails the step (`gate_rejected`) and the
  run fails fast. The same check guards every *new attempt* behind a gate (`postgres.ExecutionStore`),
  including retries and writes after the approval: a step whose last writer ancestor published a commit
  no gate approved is refused until a new gate covers it. Cancellation, settlement and results are never
  blocked, so a stale approval cannot wedge a run.
* **Definitive vs unavailable.** A fact that differs from the subject is definitive: it is recorded as one
  `gate.invalidated` (decision untouched). An outage (deployment, artifact store, database) denies
  progression and records nothing.

## Lock order

Extends execution's (see `postgres/store.go`): quota rows → work item → run row (+ steps, attempts) →
**gate row** (FOR UPDATE for decision/invalidation, FOR SHARE for consumption) → identity authorization
revision and secrets (FOR SHARE) → workspace → event backlog. A decision takes only gate → authorization →
events and never the run row; claims and completions take the run first. Cancellation takes no gate lock.

Gate and binding effects are one transaction: gate creation joins the execution binding (`gatespg.Store`
with `NewExecutionStore`), consumption and protected claims join the execution mutation, and keyed decisions
also commit the stored HTTP response (idempotency locks first). Git/S3 calls and the preparatory artifact
pins stay outside any transaction; pins are idempotent and keyed by the stable `GateIDFor` identity, so a
retry after a crash converges on the same gate and pins. `postgres/lock_order_test.go`
(`TestGateClaimLockOrder`) covers concurrent claims, decisions and revocation.

Failure and recovery: a decision is durable before any progression. If the step transition fails (for
example an execution event cannot be appended) only the transition rolls back; the approval or rejection
stays and the next delivery or reconciliation pass applies it once. `TestAtomicGateEffects` proves this
through `Store.CompleteStep`/`RejectStep`, and `TestGateRecoveryEvents` through the real decision consumer
and NATS redelivery. A gate step that is not the last step emits no execution event when it completes
(only run and attempt state changes do), so these tests use a gate as the final step: completing it settles
the run, which is the event that fails after the step and run mutations were written.

## Integration owner

1. **Migrations.** Append `gatespg.Migrations()` after the execution contributions (see `postgres/migrations.go`).
2. **Services.** Construct `gates.NewService` with an injected positive `Options.GateTTL` (no default),
   `adapters.NewAuthority`, `adapters.NewEvidence`, `adapters.NewPins`, `adapters.NewCandidates(executionSources)`,
   `adapters.ConformanceMapping{ReportOutput: …}` and an adapter over the T-035 deployment module. The
   deployment adapter must also bind a transaction-bound `postgres.DeploymentBinder`; without bindings
   deployment-bearing gates fail closed and source-only gates keep working. The integration owner confirms
   the concrete binding and its transaction/locking semantics (T-073) before activation.
3. **Execution store.** Install `gatespg.NewExecutionStore(db, execStore, execStore, gateStore)` as the
   `execution.Store` of the execution service, and enable the agent step kind only with the agent wiring.
4. **Routes.** `gateshttp.NewHandler` + `RegisterRoutes`; map route actions `gate.list`/`gate.read` to
   `evidence:read` and `gate.decide` to `gate:decide` in the identity route rules.
5. **Scheduling and wakeups.** Run `Service.ReconcileAll` periodically; register `Store.DecisionBinding` as
   an inbox consumer for faster wakeups (events only wake; a missing event is covered by the scan).
6. **Authority.** `adapters.BindAuthority` is the standalone transaction adapter over the local identity
   store. It is not proof of hosted qualification: hosted authority is qualified by T-061 and the real gate/execution
   transaction join by T-073 before T-072 activates the application. This task selects neither.
7. **Keyed decisions.** `gatespg.NewKeyed(httpIdempotency, gateStore)` joins decisions to the HTTP idempotency transaction.

See `review.md` for assumptions to confirm.
