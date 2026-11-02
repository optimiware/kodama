# `internal/execution` — durable run orchestration

Task T-020. Implements the [pipeline execution specification](../../docs/specs/pipeline-execution.md)
(PX-02, PX-05–PX-09, PX-11, PX-12, PX-14; supporting PX-03/PX-04/PX-10/PX-13 seams) with
SD-01–SD-08, IT-02–IT-06, WM-09 and WS-02/WS-03/WS-11, and the runtime port of T-086 (PX-15,
PX-16). PostgreSQL owns desired state; the runtime only acts on **committed** intent, outside any
transaction.

Status: implemented and tested against real PostgreSQL (and real NATS and HTTPS Git for the
recovery tests) and, for policy, on the in-memory store (see *Unit qualification*). Nothing registers these routes, consumers or adapters yet; that is
T-027's job (see *Integration owner*). It does **not** claim Kubernetes execution (T-021),
runtime `/result` routing or artifact verification (T-022), agent profiles (T-030), human gates
(T-034) or deployment cleanup (T-035): those are narrow seams with disabled M1 defaults.

## Layout

| Path | Purpose |
| --- | --- |
| `run.go`, `step.go`, `attempt.go`, `aggregate.go` | Entities `Run`, `StepRun`, `Attempt` (+ mutable `Control`) and their `Aggregate`; explicit transitions, no I/O |
| `progression.go`, `context.go`, `stepcodec.go` | Pure planning: skips, runnable steps, source selection, reference resolution, pinned runtime context, workload identity |
| `runtime.go`, `workload.go` | The `Runtime` port, `AttemptSpec`, `Workload`, `Capabilities`/`Guarantees`, the requirement policy and the runtime use cases: binding at admission, per-run runtime selection, observation and removal of settled workloads |
| `ports.go`, `errors.go`, `values.go` | Consumer-owned ports, sentinel errors, value objects; use-case inputs sit beside their use case (`AdmitCommand`, `ResultCommit`, `Observation`) |
| `service.go`, `admission.go`, `dispatch.go`, `observation.go`, `cancellation.go`, `result.go`, `reconcile.go`, `queries.go` | Use cases |
| `memory/` | In-memory `execution.Store` over `memdb` (T-080): the same transaction semantics as `postgres/` with the peer joins (work admission, workspace writer/retained source, secret pins, event outbox, runtime grants) passing the caller's `memdb.Tx`; `World`/`Authority` are fakes of the consumer-owned seams for the unit layer |
| `postgres/` | PostgreSQL store, the execution tables and their cross-module constraints (in the baseline schema, `internal/storage/migrations/001_schema.sql`), peer-module transaction bindings, keyed creation |
| `adapters/` | Bridges from execution ports to identity, pipeline, secrets/configuration, work and workspace |
| `http/` | The five run routes and DTOs generated from `api/runs.yaml` |
| `executiontest/` | Fixtures and the exported `Contract(t, open)` of `execution.Store`. `Env` is real PostgreSQL with every dependent schema and skips in the unit layer; `Mem` is the same module over the memory store with controlled time, a table authority and no service. Doubles only for the runtime (`runtimetest.Manual` behind `runtimetest.Decorator`, or a real runtime via `Options.Runtime`), effect reconciler and Git observation |
| `../../api/runs.yaml` | OpenAPI 3.0 fragment referencing `api/common.yaml` |
| `internal/storage/migrations/001_schema.sql` | The execution tables and the deferred workspace/artifact/secret-pin ownership constraints (this task's schema, in dependency order) |
| `review.md` | Candidate-specific verification and assumptions |

`internal/execution` imports only the standard library, `internal/domain`, `internal/pipeline`
(definition types), the runtime contract (`contracts/runtime/v1`) and nothing that touches pgx,
HTTP, Kubernetes, Git or a broker. Peer transaction calls exist only in `postgres/` and
`adapters/`.

## Unit qualification (T-080)

`memory/` runs every use case with no database, cluster or broker (`go test -tags unit
./internal/execution/memory/`). `TestUnitExecutionStoreContract` runs `executiontest.Contract`
on it; `postgres/contract_test.go` runs the same suite on PostgreSQL in the `test` environment
(it skips in the unit layer, so the PostgreSQL result is recorded at T-026, not by the unit
gate). `TestUnitDispatchReplay` (PX-06, PX-07), `TestUnitCancelFencing` (PX-08, PX-09),
`TestUnitAdmissionPins` (PX-01, PX-02, PX-15), `TestUnitTransitionOutbox` (PX-05) and
`TestUnitPeerJoins` assert the policy on the memory store. The memory store does **not** model
row locks, `SKIP LOCKED` (`ErrBusy` for a contended run never happens), lock order, isolation,
row-level security, database constraint error classes or restart persistence; those stay with
PostgreSQL. It must not be selected in `production`.

## Model

Statuses are exactly the specified enums. There are no `lost`/`paused`/`starting` statuses:
waiting for a gate, capacity or backoff is derived from pending steps.

| Record | States | Notes |
| --- | --- | --- |
| Run | `queued`, `running`, `succeeded`, `failed`, `cancelled` | `cancel_requested_at` and failure intent are durable *requests*; the run stays nonterminal until admitted work is settled |
| Step run | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | persisted `next_attempt_at` backoff; stable action key; finalized outputs only on success |
| Attempt | `running`, `succeeded`, `failed`, `cancelled` | created `running`/`starting` = dispatch intent *before* any workload; terminal rows never change |

* **Pins.** A run pins definition ID/digest, work item and content revision with its
  specification snapshot, the original source commit, resolved inputs with provenance
  (default → project variable → run input) and the exact secret version of every step's
  bindings (references and versions only, never plaintext; the same binding name in two steps is
  not flattened). The pins and the terminal history are immutable in the database (triggers
  apply to every role, the owner included). A rerun is another `POST /runs` with a new key.
* **Controller bookkeeping apart from outcomes.** `attempt_controls` holds dispatch
  confirmation/claim lease, revocation, termination request/confirmation, launch evidence,
  effect state and settlement; `attempt_evidence` (append-only) holds late observations, late
  results, mismatches and reconciliations. A late event never rewrites a terminal attempt: a
  failed `outcome_unknown` attempt stays failed whatever reconciliation says.
* **Purpose.** `work` needs an actionable work item and a workspace source; `cleanup` is
  project-scoped, has no workspace, work item, specification or retained reference, is bound to
  one recorded deployment, and consumes no business WIP. Public creation admits work runs only.
* **Business WIP stays separate from runtime concurrency.** Execution never touches the work
  item's state; a successful run never completes its work item.

## Transactions and lock order

Every transition commits with its history row, audit row and catalog event
(`run.created`, `run.state_changed`, `step_attempt.state_changed`; step transitions are history
and audit only). Order, followed by every execution transaction:

1. execution quota scope rows (`tenant`, then `project:<id>`; dispatch only);
2. the work item admission lock (`workpg.LockActionable`, `FOR SHARE`);
3. the run row (`FOR UPDATE`), then its step, attempt and bookkeeping rows;
4. the identity authorization revision and secret rows (`FOR SHARE`);
5. the workspace row (`workspacepg.UpdateInTx`: writer reservation/settlement, retained ref);
6. the event backlog and scope rows inside `Append`, last.

Mutations decide purely first and hand peer effects to `Effects.After`, which the store runs in
this order. External calls (Git, runtime) never happen inside a transaction.

## Flows

* **Admission** (`Prepare` → `Store.Admit`/`Keyed.CreateKeyed` → `ConfirmSource`). Outside the
  transaction: authorization, immutable definition, step-kind capability, work content and the
  source commit's membership in retained workspace history, configuration resolution. In one
  transaction under the work item lock: content revision recheck (an edit race is `409`, never a
  stale pin), requester authority recheck, run + steps, secret pin registration and the
  run-bound retained-source *intent* through workspace entities. The run may not dispatch until
  `ConfirmSource` creates and confirms the remote reference; a crash leaves the same run
  `pending` and reconciliation repeats it idempotently.
* **Dispatch** (`ClaimRunnable`, `DispatchNext`). Bounded oldest-first scan; the claim
  transaction (`SKIP LOCKED` on the run) rechecks work cancellation, current authorization
  through the transaction, run intent, due time, dependencies, source readiness, tenant/project
  capacity under the scope locks and the writer tip, inserts **one** attempt, **one** capacity
  reservation and the writer reservation atomically. Contention, quota and stale tips roll the
  claim back: no ordinal is consumed and no reservation leaks. A lost start is redriven with the
  same attempt and workload identity once its claim lease lapses. A run is claimed only by the
  runtime it was admitted for, and only while the runtime meets what the current policy requires
  of the step.
* **Runtime binding.** Admission computes the guarantees the run's steps require
  (`Dependencies.Requirements`, default `DefaultRequirements`: image, isolation and resource
  limits for executable steps), refuses a runtime lacking one and pins
  `Snapshot.Runtime{Backend, Requires}` in the snapshot and its digest. Without a runtime the run
  is admitted unbound and waits. No other backend ever starts, stops or removes a bound run's
  workloads.
* **Observation** (`ObserveAttempt`). Expected workload name/spec fingerprint are persisted
  before any workload exists; the start confirmation binds the instance UID the runtime reports;
  mismatching workloads, instances or specs are refused and kept as evidence. Reconciliation
  observes every dispatch-confirmed running attempt through `Runtime.Observe`; a workload the
  runtime affirms absent fails as `workload_lost` (infrastructure) with its known launch evidence.
  Startup and execution deadlines are distinct; gates have neither and allocate no workload or
  reservation.
* **Retry** is bounded (`maxAttempts`, default 1), classified (`infrastructure`/`transient`
  only), backed off by a persisted `next_attempt_at`, and requires *affirmative* replay safety:
  the runtime's `not_started` launch evidence or trusted reconciliation. Command, validation,
  policy and timeout failures are never retried. An ambiguous failure is recorded as
  `outcome_unknown`; the step, its writer slot and its capacity stay blocked (also across
  restarts) until a trusted `EffectReconciler` says `replay_safe` or `settled`.
* **Failure drain.** A nonretryable failure records failure intent, stops claims, revokes and
  cancels sibling attempts, requests termination and fails the run only after every attempt is
  settled. Reservations release exactly once, on verified settlement.
* **Cancellation.** `POST …/cancel` (`202`) and the work module's cancellation (event consumer
  plus periodic scan) record an idempotent request, deny the attempts' authority at once, end the
  run's secret-consumer access, and settle before terminal cancellation. Termination is requested
  after the commit and is recoverable: `Runtime.Stop` is repeated until it reports the workload
  stopped, which it does only once no pending launch can still start it. A workload created after
  cancellation is stopped by reconciliation and its late results are evidence only.
* **Removal.** After settlement, `RemoveSettled` (the last step of `ReconcileTenant`) asks the run's
  runtime to remove each settled attempt's workload and records the confirmed removal
  (`attempt_controls.workload_removed_at`, once). A failed or still-retained (`ErrBusy`) removal
  stays pending and is retried, for attempts of terminal runs too.
* **Result commit** (`CommitResult`, the T-022 seam). Re-checks the canonical hash and
  `runtimev1.CheckResult` against the attempt's pinned context, current authority and writer
  publication under the run lock; duplicate hash → original acknowledgment, different hash →
  conflict with no mutation; a completed result that violates its declared outputs fails the
  attempt (validation, not retried) keeping diagnostics only.

## Failure semantics

PostgreSQL commit before external effect; exactly-once *reservation release*, not exactly-once
command execution. `ErrUnavailable` means a dependency is down or an outcome is unknown and is
never replayed automatically. Missing authority, limits, workspace, secret or runtime bindings
fail closed.

## Integration owner (T-027 and later tasks)

Compose the baseline; there is no per-module contribution order to register:

```go
manifest, err := storage.NewManifest(storage.BaseContribution())
```

`internal/app.Contributions()` returns the baseline and, after it, this package's one later
contribution: the additive run_shared schema of T-112 (`003_run_resources.sql`). The execution
tables and every constraint that depends on them live in the baseline schema
(`internal/storage/migrations/001_schema.sql`) in dependency order: the tables themselves,
the deferred workspace ownership constraints, the artifact-producer and secret-pin constraints,
and the workload cleanup bookkeeping. There is no `execpg.Migrations()`, no generated copy of
another module's SQL and no per-module migration directory: that SQL is in the baseline and is
applied once, while a later schema change appends one contribution after it (the ledger refuses a
pending migration ordered before an applied one).

* **Store.** `execpg.NewStore(pool, events, execpg.Peers{Authority: adapters.BindAuthority,
  Workspaces: workspaceStore, Secrets: secretsStore, RetainedRefPrefix: <workspace option>}, opts)`.
* **Service.** `execution.NewService(Dependencies, Options)`; `Limits` is an injected, validated
  `adapters.StaticLimits` (project settings define no typed concurrency or startup-budget keys);
  `Hooks` is `execution.EnabledKinds{container}` in M1. `Runtime` is the selected backend
  (`kube.NewRuntime` with `Planner: kube.NewPlanner(policy)`, or `podman.New` locally);
  `Requirements` is the installation's admission policy.
* **Cancellation consumer.** `workpg.CancellationBinding(name, adapters.TxCancellationRequester(store))`
  registered with the event consumer; additionally call `Service.ReconcileCancellations` (or
  `work.ReconcileCancellations` with `adapters.NewCancellationRequester`) periodically.
* **Reconciler.** Run `Service.Reconcile` periodically (and on wakeup) and `Service.DispatchNext`
  per tenant in the same loop; both are safe from several replicas.
* **Workspace.** `execpg.NewAttemptAuthority(store)` satisfies
  `workspace.Attempts`; `adapters.BindWriterGuard` completes the work guard binder with
  `execpg.NewWriterGuard(tx)`. The workspace authorizer must permit the `execution` service actor
  for `workspace.sync` and `workspace.pin`.
* **HTTP.** `executionhttp.NewHandler(Config{Service, Creator: execpg.NewKeyed(idem, store), Cursors})`
  and `RegisterRoutes`. The router authorizer maps `run.list/create/read/steps/cancel`; the use
  cases enforce `run:read`/`run:manage` themselves.
* **T-019 (runtime admission).** `execpg.LockAttempt(ctx, tx, scope, attempt)` is the authoritative,
  cancel-serialized read for `GrantTx.LockAttempt`; map `AttemptSnapshot` (Job name, context digest,
  revision, live) to `AttemptState`. `Attempt.Context` is the canonical pinned context document and
  `ContextDigest` its SHA-256 (the same canonicalization as `runtimeauth.PinContext`).
* **Runtimes (T-021, T-086).** Implement `execution.Runtime`: `Start` creates or adopts after
  verifying ownership and the persisted `SpecFingerprint` and returns the instance; `Observe`
  reports `dispatched`, `started` or `failed`; `Stop` reports stopped with launch evidence;
  `Remove` releases a settled workload (`ErrBusy` while retained). Every adapter passes
  `runtimetest.Contract`. A runtime whose workload shape depends on policy supplies an
  `execution.Planner` folding it into the fingerprint.
* **T-022 (result finalization).** Verify artifacts and publication, then call
  `Service.CommitResult(ResultCommit{…, Artifacts})`; map `ErrAttemptRevoked`, `ErrResultConflict`,
  `ErrInvalid` and `ErrStale` (unsettled publication) to the runtime error codes.
* **M2.** Gate (T-034), agent profile (T-030) and deployment cleanup (T-035) plug into
  `Hooks`, `Dependencies.Cleanup` and `Service.AdmitCleanup`; none is claimed delivered here.

## `run_shared` resource lifecycle and persistence (T-112, P2)

Implements [P2](../../docs/implementation/run-workspace-services-builders.md#p2--run-resource-lifecycle-and-persistence)
under the frozen contracts (T-109 `9db322ff`, executable T-110 `6343b9e5`:
[run resources](../../docs/contracts/run-resources.md), [runtime v2](../../docs/contracts/runtime-v2.md),
[v1alpha2](../../docs/contracts/pipeline-format-v1alpha2.md)). **Not enabled, not qualified.** New-format
admission is closed unless an installation composes `Dependencies.Shared` explicitly; nothing in
`internal/app` does, no production default exists and no test-only permissive constructor ships. P1
(real profile qualification) is independent; this phase does not claim it.

| File | Purpose |
| --- | --- |
| `resources.go` | The **frozen companion port** `RunResources` (six methods: `Capabilities`, `Ensure`, `Observe`, `Logs`, `Stop`, `Remove`) and its DTOs: `ResourceKind`, `ResourceHandle`, `ResourceSpec` (+ `Fingerprint`), `ResourceObservation`, `ResourcePhase`, `ResourceStopEvidence`, `ResourceRemoveEvidence`, `ResourceCapabilities`/`ResourceCapabilitySet` (a 32-bit named set, 13 members), `ResourceReason`, `PolicyPin`, the receipt digest object and the deterministic `ResourceName`/`EndpointHost` |
| `policy.go` | `ExecutionPolicy` (installation-owned, canonical digest, role/limit/egress validation) and the `Policies` port |
| `shared.go`, `shared_aggregate.go`, `runresource.go` | `RunShared` pins, requirement derivation, `BuildResourceSpecs`, the `RunResource` entity and its state machine, `RunHolder`, `BeginRecord`, the aggregate operations (start, observe, stop/remove evidence, receipt, begin, order, `BuildContextV2`) |
| `shared_service.go`, `shared_reconcile.go` | `SharedConfig` (the opt-in), `SharedEnv`/`SharedStore` (capacity reservation, orphan scan), admission planning, `StartShared`, shared dispatch, `RecordPreparationReceipt`, `RecordBegin`, and the intent-first provisioning/teardown driver |
| `shared_view.go` | The safe `SharedView`/`ResourceView` observation projection |
| `memory/shared.go`, `postgres/shared.go`, `postgres/migrations/003_run_resources.sql` | Persistence |
| `executiontest/shared*.go` | `FakeResources` (injected by tests only), `TestPolicy`, `SharedContract` |

### Frozen identities (for P3–P6)

* `execution.RunResources` as above; adapters implement it and keep provider types private. The attempt
  `Runtime` keeps its six methods. `Guarantees` stays a `uint8`: six used bits, `RunResourcesMarker` is
  bit 6 (`"run_resources"` on the wire), bit 7 is reserved, `AllGuarantees` is unchanged (a backend that
  implements the companion declares `AllGuarantees.WithRunResources()`). Finer capabilities live only in
  `ResourceCapabilitySet`. `Capabilities.ProvesNeverStarted` stays a separate flag.
* `RuntimeBinding` additionally pins `ResourceRequires` (omitted when empty: legacy bindings, digests and
  readers are unchanged). The binding, `RunShared` pins and the retained policy body are written once.
* **Receipt digest object** (frozen, golden vector in `Test_unit_preparation_receipt_digest_object_is_frozen`):
  canonical JSON of `{"request": <PreparationReceiptRequest>, "resourceKey": "preparation", "runId": RUN, "workspaceUid": WORKSPACE_UID}`,
  `sha256:` + hex SHA-256. `Aggregate.RecordPreparationReceipt` binds it to the **committed** workspace UID, refuses
  an unbound resource (`ErrResourceUnbound` → `resource_unbound`), a stopping/stopped/past-deadline preparation
  (`ErrResourceRevoked`), a wrong commit or too many tries, and a different receipt (`ErrReceiptConflict`).
  The workspace is ready only when the receipt is recorded, still verifies against the bound UID **and** the
  preparation workload is verified stopped.
* **Names:** `kr-` + first 24 hex of SHA-256, endpoint host `s-` + 16 hex + `-NAME`, both over the tuple with each
  element framed as a 4-byte big-endian length plus its bytes (the contract text, now fixed by T-119, and the repository's
  idempotency framing). **Audit/event resource ID:** `ResourceAuditID(scope, run, key)` is `eventsv1.RunResourceID`
  (`rr-` + 40 hex of the same framing over `(tenant, project, run, key)`): the single derivation for the audit row's
  `resource_id` and the event envelope, domain-valid and not aliased across tuple boundaries (the first candidate's
  underscore flattening is gone).

### Events, audit and history (T-119)

Every resource transition is one `run_history` row (subject `resource`), one `audit` row (resource type
`run_resource`, action `run_resource.state_changed`) and one `run_resource.state_changed` outbox event with the eight
safe fields of `eventsv1.RunResourceStateChanged` (`runId, kind, key, from, to, reason, specFingerprint, uidBound`), in the
transaction that saves the state, in the memory and PostgreSQL stores alike. `from`/`to` are phases (`none` at creation;
verified removal is `absent`); a same-phase observation (a bound UID, the recorded receipt, the ensure intent, a stop
intent, a reason change) is a transition too. The event ID is stable (`run, subject, key, record version, index`). A resource-only
outbox refusal rolls back every row of the transition (`...outbox refusal rolls back the whole transition`).

### Constructor and profile handoff

* **P3** (entrypoint/authority): call `Service.RecordPreparationReceipt(scope, run, claim, req)` and
  `Service.RecordBegin(scope, attempt, claim, req)` from the verified resource/attempt routes (the use cases
  re-verify under the run lock, and check the runtime grant of the `GrantClaim` (the verified session and signing
  generation) through the transaction's `GrantFence` after the run lock: a revocation committed after the
  caller's own check records nothing (`ErrAttemptRevoked` / `ErrResourceRevoked`); a store without the fence,
  `postgres.SQLGrantFence` (installed by `NewRuntimeStore`) or `memory.Peers.GrantFence`, refuses with
  `ErrNotConfigured`; the bearer verification and `ResourceIdentity` comparison against
  `RunResource.Name`/`UID`/`Stopping()` stay in P3). `RecordBegin` rechecks live command authority (revocation,
  cancellation, failure intent, the pinned deadline) before ANY acknowledgment, an identical replay included
  (`ErrAttemptRevoked` → `attempt_revoked`), and keeps the original record. Resource subjects are revoked from
  `RunResource.Stopping()` or a passed `Run.DeadlineAt`. Attempt contexts of run_shared runs are canonical
  `runtimev2.ContextV2` (`AttemptSpec.DecodeContextV2`); `decodeContext` for result checking projects either version.
* **P4/P5** (providers): implement `execution.RunResources` per backend and declare only the capabilities
  that are enforced and tested. Compose with the same backend identity as the attempt runtime
  (`Capabilities.Backend` equality is checked at admission, dispatch, launch and reconcile).
  `Stop` must report `Stopped` only when no pending create can arrive and, for the workspace, `Released`
  only when no node uses the volume; `Observe` of a workspace reports `Released` for the holder handoff; an
  unobservable resource must be `PhaseUnknown`, never `PhaseAbsent`. A **collision reply is never evidence**: for a
  handle with a bound UID (or whose create may have happened) it means only that the object at the name cannot be
  verified, so execution keeps the stop/removal unconfirmed (`resource_collision`, retried, nonterminal). A handle with
  NO bound UID whose name holds a foreign object must be answered with a verified stop/removal that touches nothing foreign.
  `ResourceObservation.ObservedAt` is the adapter's own observation time; a missing one is taken as the start of the request.
* **P6** (assembly): construct `execution.NewService` with `Dependencies.Shared =
  &execution.SharedConfig{Resources: <companion port of the SAME backend>, Policies: <installation policy source>}`
  only when an installation enabled `runShared: open`; give `Limit.SharedTenant/SharedProject` explicit
  capacity (zero reserves nothing and nothing starts) and set `Dependencies.Cleanup` for source-free runs. The
  `RunResourceMigrations()` contribution is **already appended** to `app.Contributions()` by T-112's bounded exception
  (`internal/app/migrate.go`, regression `TestRunResourceMigrationIsAppendedLastAndPreservesTheLedgerPrefix`): the store reads
  its columns unconditionally. Nothing else is wired. The reconciler is `Service.Reconcile`/`ReconcileTenant`/`ReconcileRun`
  (they also drive resources and terminal-run cleanup via `SharedStore.UnfinishedResources`) and dispatch is
  `ClaimRunnable`/`DispatchNext` (the first claim of a queued run starts resource holding).

### Module API handoff (T-116 owns `api/execution.yaml`, the aggregate and the client; this task leaves them unchanged)

`GET /…/runs/{id}` gains, additively, `runModel`, `deadlineAt`, `executionOrder`, per-step effective `workspace.access`
and `resources[]` of `{kind, key, phase, reason, readyAt, stoppedAt, removedAt}`: source it from
`Aggregate.SharedView()` (no UID, fingerprint, spec, endpoint, log or credential in it; `CleanupReason` is the visible
`resource_stop_unconfirmed` / `cleanup_pending` of a run held nonterminal after failure, cancellation or success). Logs of
`preparation`/`service.NAME` come from `RunResources.Logs` with the caller's current authorization. Admission refusals use the
stable `Refusedf` messages prefixed `capability_unsupported`, `policy_unavailable`, `limit_exceeds_policy`,
`egress_profile_unknown`, `workspace_none_source_backed`, `workspace_access_source_free`, `agent_source_free`.

### Lifecycle summary

* **Admission** admits `kodama/v1alpha2` only through the explicit `SharedConfig`: work runs (source-backed) and internal
  deployment cleanup runs (`PrepareCleanup`/`AdmitCleanup`, source-free: no work item, source lookup, checkout credential or
  preparation; the scratch is empty only at its initial provisioning and later steps see earlier steps' files, with the
  explicit source-free lineage values) under the same complete capability, policy and ceiling gate. An unconfigured
  composition refuses both.
* **Start** (`Service.StartShared`, first claim): one transaction under quota → work → run locks: pins deadline, inserts
  the resource intents (spec + fingerprint, before any runtime call) and reserves run capacity: services, preparation, the
  largest step allowance, the scratch volume (separately from ephemeral storage) **and one tenant/project active-attempt
  slot**, which every successive attempt of the run consumes without a second charge and which other runs' admissions
  count. Insufficient capacity rolls the whole step back and leaves the run queued. Terminal completion releases the
  reservation only after every resource is verified removed.
* **Two authorities.** LAUNCH (create, adopt, redrive, dispatch) needs model, pinned backend, verified pins/policy, the
  runtime's pinned guarantees and every pinned resource capability, checked before every launch (`sharedDriveCheck`),
  redrive and adoption. CLEANUP (observe, stop, remove the committed owned UIDs) needs only that the controller implements the
  model and the companion is the pinned backend: a lost policy or capability fails the run (`policy_unavailable` /
  `capability_unsupported`) and the resources still settle; a run of an unsupported model or another backend stays untouched.
* **Provision:** network → workspace → preparation ∥ services. Every `Ensure` is preceded by a committed intent mark; an
  unbound intent that never became ready is re-`Ensure`d (idempotent) only before any command began, otherwise it is loss.
  Observation AND Ensure requests carry a server-side generation committed before the runtime call; EVERY outcome (ready, unknown,
  an outage with its backoff, collision, fenced) is applied through that fence and only if newer than every applied one, so a delayed
  older "ready" never clears a newer unknown and a delayed older outage never degrades a newer ready; an ambiguous Ensure outcome
  persists an unknown observation of its generation before its backoff. Identity is verified BEFORE anything is bound: a foreign
  UID or a non-matching fingerprint is a collision that binds nothing; an empty fingerprint ("could not verify") is unknown, binds
  nothing and is never ready or release evidence. A preparation completes only with its trusted receipt (and verified stop): one that
  is ready without a receipt fails with `preparation_failed` at its own budget, inside the total deadline.
* **Ownership guard.** Every entry that could change a persisted run (reconcile, results, begin, receipts, attempt observations)
  first requires that this controller implements the run's pinned model and, for reconciliation, owns its backend: an unsupported
  model or foreign backend is refused before any deadline, failure, revocation, stop or peer effect is recorded
  (`...cannot own the run model changes nothing, even past the deadline`).
* **Barrier and handoff:** dispatch needs network, workspace (receipt bound, preparation stopped) and every service ready
  and observed within `Options.ResourceFreshness`, no unreleased holder, the workspace observed released and the pinned order
  (a gate occupies its position and is eligible only when every earlier node is settled and no attempt holds the volume).
  The claim records the holder in the attempt's transaction; the release observation is applied and the holder released in ONE
  transaction on the current UID-bound, fresh evidence observed after the verified stop. The logical writer is separate.
* **Retry:** automatic retry only when the workload is verified `not_started` and no `begin` was recorded for the step;
  a begun attempt of a retryable class fails the run now (services stop promptly while the uncertain attempt settles).
* **Loss/unknown/deadline:** affirmative failed/absent, an unsolicited verified stop (except a preparation after its receipt),
  UID or fingerprint mismatch or continuous readiness failure is loss and fails the run; `unknown` blocks dispatch only; the
  pinned total deadline (covering gate waits) fails the run with `run_deadline_exceeded` in the reconciler, in a result
  transaction (a late success is evidence only; an accepted result stays replayable), in begin and in `CompleteGate`.
* **Teardown:** failure/cancel/deadline/completion set every resource's stop intent at once, stop services/preparation promptly,
  the workspace after no attempt can use it, the network last; stuck stop/removal stays visible
  (`resource_stop_unconfirmed`, `cleanup_pending`, `resource_collision`), retried with persisted exponential backoff,
  restart-safe. Terminal status needs every resource stopped and removed.

### Evidence

* `Test_unit_*` in `shared_test.go` (seam identity, vectors, state machine, generations, holder release, unsolicited stop,
  command authority, order/deadline); `TestUnitSharedResourceContract` (memory) and `Test_shared_resource_contract`
  (PostgreSQL) run `executiontest.SharedContract` including the review regressions; real-SQL only:
  `Test_shared_forced_rls_ownership_and_guards`, `Test_shared_start_is_atomic_with_its_effects`,
  `Test_shared_concurrent_starts_never_exceed_capacity`, `Test_shared_migration_is_additive` (real historical runs, fresh and
  upgrade, the application manifest).
* `Test_unit_pipeline_v2_step_codec_round_trip` replaces T-110's v1-only codec pin; `Test_unit_pipeline_v2_guard_unchanged`
  and the ordinary/cleanup refusal tests still pass under the default composition.

### Known limits (explicit)

* The memory store does not model locks or RLS; the claims about them rest on the PostgreSQL suites. Its guards mirror the SQL triggers.
* No provider, HTTP route, app composition or client exists; those are P3–P6.
* Real Kubernetes/CSI/CNI behavior (collision semantics of a real adapter, volume release) is the providers' and P7's evidence.

## Run origins and source sets (T-148, ADR 0011)

Implements [PX-23–PX-25](../../docs/specs/pipeline-execution.md#requirements) and seams S5/S6 of the
[internal Git service plan](../../docs/implementation/internal-git-service.md) in execution. **Not wired and not
enabled**: nothing in `internal/app` composes it, migration `011_run_sources.sql` is not yet registered (T-132), no route
creates such a run, and no runtime adapter renders runtime context v3 (T-152). Every earlier run model, context, digest,
definition version and admission path is unchanged: `Test_unit_old_run_models_unchanged` compares the attempt_private and
run_shared v2 contexts, digests and workload fingerprints byte for byte with goldens captured from the release before
this task (`executiontest/testdata/old_contexts.json`).

| File | Purpose |
| --- | --- |
| `origin.go` | `Origin` (`work_item`, `push`, `manual`, `provision`) and `OriginOf` (the implicit origin of an earlier run; a cleanup run is `manual`) |
| `sources.go` | `RunSources` (the immutable pin), `NewRunSources`, and the consumer-owned ports `SourceSets` (resolve, pin, retain) and `InternalWorkspaces` (provision, writer gate, checkpoints) |
| `source_admission.go` | `PrepareSourceRun` / `AdmitSourceRun`: admission of any origin; `ConfirmSource` retains the set |
| `shared_v3.go` | The aggregate side of context v3: `BuildContextV3`, receipt per source, begin over every source, `SourceHeads`, lineage, `CompleteAttemptV3` |
| `source_service.go` | The v3 use cases for the runtime routes: `RecordPreparationReceiptV3`, `RecordBeginV3`, `CommitResultV3` |
| `source_grants.go` | The `GitGrants` port, `AttemptGrantFor`/`PreparationGrantFor`, the redacting `GitSecret`, the grant requested at start |
| `adapters/sourcesets.go`, `adapters/internalworkspaces.go` | `execution.SourceSets` over the repository, gitpush and Git worker ports and `execution.InternalWorkspaces` over the workspace module (T-147) |
| `postgres/migrations/011_run_sources.sql`, `postgres.RunSourceMigrations()` | `runs.origin_*`/`source_*`, `run_sources`, `attempt_commits`, the relaxed run and attempt shapes, one unsettled attempt per run |

### Admission

`PrepareSourceRun` is not the public `POST /runs`: it takes a `SourceRunCommand` (origin, a `kodama/v1alpha3`
definition, optional stable `RunID`, optional default `Repository`, per-repository `Pins`) and is refused unless the
installation composed `Dependencies.Shared`, `Runtime` **and** `SourceSets`. Outside any transaction it authorizes
`run:manage`, resolves the repositories of the project (a foreign or unknown repository is the stable refusal
`source_repository_unknown`; a non-internal, not-ready or archived one `hosting_mismatch`/`repository_not_ready`/
`repository_archived`), provisions the work item's internal workspace (only a work item's run that writes a source: the
task branch must exist before its tip is read), pins **one exact commit per source** (`SourceSets.Pin`: the push origin's
source pins `gitv1.PinnedCommit` of the applied entry, a work item's writable source the task branch's accepted tip, any
other source its command pin or the default branch tip, read once) and builds the validated `gitv1.SourceSet`. A push
origin whose pushed repository is not a source, a refused/deleted/unapplied entry, a duplicate or nested path and an
unreachable commit are all refusals that create and retain nothing. No work item is fabricated: a push, manual or
provisioning run has no work item, workspace, specification or source commit, takes no work-item lock, consumes no WIP and
emits `run.state_changed` (not `run.created`, whose payload names a work item) at creation.

The run commits with its origin and sources (`runs` columns plus `run_sources`, with a composite foreign key to the
project's own repositories) and `source_retention` `pending`; `ConfirmSource` then calls `SourceSets.Retain` (every pinned
commit reachable for the run's retention, the run branch of each write source created at its commit) and records the
confirmation, so dispatch waits for it and a crash is repeated by reconciliation. A source-free run (provisioning, or manual
without sources) is `not_required`. **Replay:** (concurrent admissions of one run ID are serialized by an advisory lock; a store that finds the run with another
declaration answers `ErrConflict`: `CheckReplay`) with the same `RunID` and declaration the stored run is returned with its
pinned commits whatever the branches did since and nothing is read again; another declaration under that ID is
`ErrConflict`. Trigger admission (T-137) derives the `RunID` from `(trigger, push record, ref)`.

Write sources update `refs/heads/kodama/work/<workItemId>` (work_item origin) or `refs/heads/kodama/runs/<runId>/work`
(every other origin; `Origin.Branch`). Only the former has a workspace and a writer reservation.

### Context and attempts

A source-set run pins `RunShared.ContextVersion = 3` (`Supported` accepts 2 and 3; an older controller refuses it).
`BuildContextV3` carries the origin, the sources with per-source `headCommit` (what prior successful attempts published to
that source's own branch; `Attempt.Commits`/`attempt_commits`) and the writer's `expectedTip`, the source set digest, the
recorded per-source preparation receipt (`PreparationReceiptObjectV3`, same frozen digest object) and the Git delivery whose
`repositories` equal what `ContextV3.GitAccess` derives (the run's agent namespace; plus the write branches only for the
writer). `PreparationSpec.Sources` lists every checkout of the one preparation (omitted for a single repository, so earlier
fingerprints are unchanged). A v3 attempt reaching the v1/v2 receipt, begin or result use cases is
`ErrContextVersionMismatch` (409, no side effects).

**One executable attempt per shared run:** `Aggregate.Claim` refuses while *any* attempt of a `run_shared` run is unsettled
and a database trigger (`step_attempts_one_per_run_guard`) says the same; the existing workspace-holder uniqueness remains.

**Agent pins (T-163).** `Dependencies.AgentPins` (`execution.AgentPins`, implemented by `agents/adapters.Pinner`) is consulted by
`BuildContextV2` and `BuildContextV3` (through `sharedContextCore`, so the two versions cannot drift) with the run's tenant and
the step's frozen command; the returned `profile` and `designBaseline` are set before the context is validated, canonicalized and
digested, so they are part of the frozen context, its digest and the workload fingerprint. The port performs no I/O and reads the
frozen payload only, so rebuilding the context for the same step yields the same bytes. `ok=false` (no payload) leaves the context
exactly as before (`"profile":null`, `"designBaseline":null`; the earlier-model goldens are unchanged); an invalid payload is
`ErrInvalid`. A nil port keeps every non-agent step unchanged, and a step of kind `agent` is refused with `ErrAdmissionRefused`
(the step fails as `step_failed`, no attempt is created): it never runs without pins. The v1 path keeps the `Planner` decoration.

### Git grants

The Git authority is a consumer-owned port. `drive` calls `GitGrants.IssueAttempt` after the attempt is committed and
**before** the runtime creates the workload, never under a lock, and **only when the runtime affirms (Observe →
`ErrNotFound`) that no workload exists**: a redrive finding one adopts it with `spec.Git == nil` (issuing again would
replace the credential it holds; `Runtime.Start` must not need a new one to adopt). After issuing, the attempt is read again; one
that ended meanwhile (the authority reads its lease outside the grant's transaction) gets its grant revoked through
`GitGrants.RevokeAttempt` and nothing is started. The adapter's lease deadline must not be earlier than the run's, which caps the
grant; the request is exactly the pinned context's grant (`AttemptGrantFor`: kind `attempt`, workload actor with run and
attempt, the context's repositories and ref patterns, expiry bounded by the run deadline and 24 h). The handle reaches the
runtime only as `AttemptSpec.Git` (`GitSecret` prints and encodes `[redacted]`, is never persisted or put in the context).
A failure starts nothing; the unconfirmed attempt is redriven. **Revocation is transactional with the transition:**
`Control.AuthorityEnded` marks each attempt whose authority this mutation ended (cancellation, failure drain, timeout,
revocation, a failed result **and an accepted one**), and the stores call their `GitGrants` peer for each, in attempt ID
order, in the same transaction (lock level 4 beside the runtime grants): `postgres.Peers.GitGrants` is
`AttemptGrantRevoker` shaped (`gitauth/postgres.RevokeAttempt` is exactly that function), `memory.Peers.GitGrants`
`GrantRevoker` shaped (`gitauth/memory.RevokeAttempt`). A revocation failure rolls the cancellation back. **Fail closed:** `execpg.NewStore` with `RunSources` refuses (`ErrNotConfigured`)
without `Peers.GitGrants`; either store refuses a mutation that ends a source-set attempt's authority without a revoker;
`NewService` refuses `Dependencies.GitGrants` over a store that reports `RevokesGitGrants() == false`
(`GitRevokingStore`). The real gitauth store joined this way is proved in `Test_source_git_grants_revoked_with_the_attempt`. A receive that
already obtained commit intent completes; any later one is refused by the push authorizer.
`context.git.grantId` is the deterministic reference `GitGrantRef(scope, attempt)`, because the context is pinned before a
grant can exist; it is not gitauth's grant ID.

### Results and the workspace

`CommitResultV3` accepts a completed writer's task-branch commit outputs through `InternalWorkspaces.AcceptCheckpoint`
**before** the transaction that settles the writer, then settles it without a source commit. `ErrBusy` (the push is not
settled) leaves the attempt running and repeatable; a refused output fails the attempt as `output_invalid`; outputs on a run or agent
branch are not checkpoints (they move the source head). A writer's claim runs `InternalWorkspaces.AdmitWriter` outside the
transaction (synchronize, writer gate) and reserves the slot with `workspace.ReserveInternalWriterMutation` at the version
the gate was read at (`InternalWriterEnv`), so a reservation never commits against a stale gate answer (`ErrStale`/
`ErrBusy`: the run waits, nothing is claimed).

### Compatibility and the migration

An origin run admits only a `kodama/v1alpha3` definition (`checkSourceSetVersion`).

`execpg.Options.RunSources` declares that the composition applied migration 011 (which needs 007 repositories); without it
the store uses exactly the earlier schema, reads and writes no new column and refuses an origin run (`ErrNotConfigured`), so
no existing composition changes. `Env`/`executiontest` apply 007, 010 and 009. The relaxed shapes keep every earlier row valid and every earlier run strict (triggers on `origin_kind IS NULL`: a read/write
attempt still binds a workspace, a receipt stays within 4096 bytes):
`runs_purpose_shape` (origin runs only), `step_attempts_workspace_shape` (a read/write attempt without a workspace binds
none), `run_resources_receipt_bounds` (65536).

### Evidence

`Test_unit_admission_pins_source_set`, `…_push_origin_without_work_item`, `…_provisioning_run`, `…_manual_origin`,
`…_cleanup_purpose_unchanged`, `Test_unit_context_carries_source_set`, `Test_unit_attempt_grant_requested_and_revoked`,
`Test_unit_old_run_models_unchanged` (memory, `executiontest.Source*`), the same scenarios on PostgreSQL in
`Test_source_contract` and the database guards in `Test_source_run_sql_guards`
(`make check-go-integration DEV_SCOPE=t148 DEV_STACK=services PKGS='./internal/execution/...'`).

### Handoff and limits

* **T-132:** register `RunSourceMigrations()` after the repository (007) and workspace (010) contributions and set
  `Options.RunSources`; bind `Peers.GitGrants`; compose `SourceSets`, `InternalWorkspaces` and `GitGrants` (adapters below);
  `execpg.NewAttemptAuthority` is unchanged and answers `workspace.Attempts` for an attempt without a workspace as an empty
  workspace id.
* **T-137:** call `AdmitSourceRun`/`PrepareSourceRun` with `Origin{push}` and a `RunID` derived from the trigger, push and ref;
  `Repository` is not needed for a push (the pushed repository is the implicit source).
* **T-150:** implement `GitGrants` over `gitauth.Service.IssueAttempt` (convert `AttemptGrantRequest`/`GitGrantHandle`) and its
  `Attempts.Lease`; deliver `AttemptSpec.Git` and `PreparationGrantFor` to the runtime, call the `*V3` use cases with the
  verified `GrantClaim`; load the issued seed/integration grants for `ResultCommitV3.Issued`. The runtime-issued seed grant
  (PX-25: only for a repository this run created) is not issued here.
* **T-149:** gate steps are still refused for a source-set run (`GateInputs` needs a work run with a source commit); gate
  subjects and integration need `Run.Sources.Digest` and the per-source heads.
* **T-152:** read `AttemptSpec.DecodeContextV3`, `PreparationSpec.Sources` and `AttemptSpec.Git`; the workload fingerprint
  already binds the context digest, hence the source set.
* **Adapters delivered here, unwired:** `adapters.NewSourceSets` (repository store, clone URLs, the worker's
  `ReadRefValues`/`SetRetainedRef`/`CreateBranch`, `gitpush.Service.Reload`; the pin of an exact commit is verified by the
  worker only when `Retain` asks it to pin the commit, because the worker port has no object read yet) and
  `adapters.NewInternalWorkspaces` (`workspace.Service` and its store; the execution service actor needs
  `workspace.provision`, `workspace.sync`, `workspace.admit` and `workspace.accept_checkpoint`). T-138's `gitops.Pins` may later
  replace the numbered retained refs `Retain` creates; a checkpoint that finds a number taken moves to the next.
* **Not delivered:** the preparation grant is built (`PreparationGrantFor`) but not issued by any driver; the real workspace
  writer reservation is exercised by the workspace module's own tests (here by a fake that models the version guard and,
  for the adapter, by unit tests of its mapping).
