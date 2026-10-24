# `internal/execution` — durable run orchestration

Task T-020. Implements the [pipeline execution specification](../../docs/specs/pipeline-execution.md)
(PX-02, PX-05–PX-09, PX-11, PX-12, PX-14; supporting PX-03/PX-04/PX-10/PX-13 seams) with
SD-01–SD-08, IT-02–IT-06, WM-09 and WS-02/WS-03/WS-11, and the runtime port of T-086 (PX-15,
PX-16). PostgreSQL owns desired state; the runtime only acts on **committed** intent, outside any
transaction.

Status: implemented and tested against real PostgreSQL (and real NATS and HTTPS Git for the
recovery tests) and, for policy, on the in-memory store (see *Unit qualification*). Nothing registers these routes, migrations, consumers or adapters yet; that is
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
| `postgres/` | PostgreSQL store, `T020_002/003/004` and `T086_001` migrations, generated copy of `T020_001`, peer-module transaction bindings, keyed creation |
| `adapters/` | Bridges from execution ports to identity, pipeline, secrets/configuration, work and workspace |
| `http/` | The five run routes and DTOs generated from `api/runs.yaml` |
| `executiontest/` | Fixtures and the exported `Contract(t, open)` of `execution.Store`. `Env` is real PostgreSQL with every dependent schema and skips in the unit layer; `Mem` is the same module over the memory store with controlled time, a table authority and no service. Doubles only for the runtime (`runtimetest.Manual` behind `runtimetest.Decorator`, or a real runtime via `Options.Runtime`), effect reconciler and Git observation |
| `../../api/runs.yaml` | OpenAPI 3.0 fragment referencing `api/common.yaml` |
| `../workspace/migrations/T020_001_attempt_fk.sql` | Deferred workspace ownership constraints (owned by this task) |
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

Compose, in this migration order after the base, events, HTTP idempotency, identity, work,
secrets, pipeline, workspace and artifact contributions:

```go
manifest, err := storage.NewManifest(append(peers, execpg.Migrations()...)...)
```

`execpg.Migrations()` returns four contributions that **must stay in this order**: the tables
(`T020_002`), the workspace constraints (`T020_001`, authored in
`internal/workspace/migrations` and embedded through `go generate ./internal/execution/...`),
the artifact-producer and secret-pin constraints (`T020_003`, `T020_004`), then workload cleanup
bookkeeping (`T086_001`). Later schema joins as a new final contribution. `execpg.Migrations()` is
the fresh-database order (`execpg.Schema()` then `execpg.CleanupMigrations()`); a composition that
deployed before cleanup existed lists `Schema()` where it did and `CleanupMigrations()` after
everything already applied, as `internal/app` does after the runtime authority schema.

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
