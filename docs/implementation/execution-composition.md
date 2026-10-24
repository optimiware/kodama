# Execution/results transaction composition (T-062)

This record inventories the concrete constructors, transaction owners, lock participation and failure behavior that compose execution, runtime authority, secrets, workspaces, events and results. It is the prerequisite checkpoint named in [repository layout](repository-layout.md#transaction-composition-checkpoint). It describes the implemented adapter-local bindings and the evidence that qualifies them; it adds no route, event type, migration, command, dependency or root wiring.

- **Requirements:** SD-04, SD-09–SD-11 ([software design](../specs/software-design.md)); IT-03, IT-06 ([identity and tenancy](../specs/identity-tenancy-and-rbac.md)); PX-05, PX-07, PX-08, PX-10–PX-12 ([pipeline execution](../specs/pipeline-execution.md)); WS-11 ([workspaces](../specs/workspaces.md)).
- **Decisions and ADRs:** confirmed C-01/C-11/C-12/C-15/C-16 and implementation baselines P-01/P-05/P-06/P-07/P-16 in [decisions](../decisions.md); [ADR 0002](../adr/0002-postgres-state-and-events.md), [ADR 0005](../adr/0005-git-backed-workspaces.md), [ADR 0006](../adr/0006-hexagonal-domain-design.md).
- **Baseline:** the inspected baseline is the T-062 branch at `fa67f79c97fe8bad0b0fb2b5bb090bb787a90204`. `git diff 68c37ffd9839a9bbb7c05c09d96612ad506a820c HEAD` over `internal/execution`, `internal/results`, `internal/runtimeauth`, `test/integration/execution-composition` and `test/integration/results` is empty at that baseline (checked for this pass), so the production and test code is the code qualified earlier; only this record changed after it. The implementation builds on T-022 (`deff2e1`), whose receipt-first ordering and terminal `result.submit` exception are preserved unchanged. Results recorded for earlier commits (`2768927`, `49f5d4f`, `68c37ff` and conformance candidate `b8617723e7b86095d9268b10bce8a20a1a2ec563`) are prior qualification, not approval of a later candidate; the [qualification record](#qualification-record) separates them from what was run for the inspected baseline. The eventual tested candidate is the commit the coordinator qualifies.
- **Hosted authority** is qualified separately. These fixtures use the standalone local authority and a controlled hosted-identity double; they do not establish hosted acceptance.

## Boundaries

| Concern | Owner | Participation in the composition |
| --- | --- | --- |
| Run, step, attempt transitions, receipts, reservations | `internal/execution` | Ordinary mutations run through `Store.UpdateInTx`; the entities never see pgx. |
| Runtime grants, sessions, signing generation | `internal/runtimeauth` | Revoked and checked through `runtimeauth/postgres` entry points that join the caller's transaction. |
| Secret pins | `internal/secrets` | `execpg.SecretPins` registers and releases pins in the caller's transaction. |
| Writer slot, publication, retained references | `internal/workspace` | `execpg.WorkspaceTx.UpdateInTx` joins the caller's transaction; Git and S3 effects stay outside SQL. |
| Inbox, outbox, observation | `internal/events` | `Append` joins the caller's transaction; the consumer owns the inbox transaction that hands a cancellation to execution. |
| Orchestration of a runtime result | `internal/results` | Owns no durable state; decorates execution and workspace stores with the session check. |

A callback that opens its own `storage.WithTenant` transaction is not a join and is never bound. Every binding below receives the caller's `pgx.Tx`.

## Constructors and bindings

| Binding | Constructor / entry point | Transaction behavior |
| --- | --- | --- |
| Runtime-enabled execution store | `execpg.NewRuntimeStore(db, appender, peers, grants, opts)` | Requires `grants`; a nil revoker returns `execution.ErrNotConfigured`. Every transition that ends an attempt's authority without an accepted result revokes its grants in the same transaction. |
| Component-only execution store | `execpg.NewStore(db, appender, peers, opts)` | Revokes no grants. Valid only where the runtime grant schema is absent. **A composition that serves runtime credentials must select `NewRuntimeStore`.** |
| Peer seams | `execpg.Peers{Authority, Workspaces, Secrets, Work, RetainedRefPrefix}`; `Work` defaults to `workpg.LockActionable` | Authority: `execadapters.BindAuthority(tx)`. Workspaces and secrets: the peers' `UpdateInTx`/`RegisterPins`/`ReleasePins`. Events: the appender's `Append(ctx, tx, env)`. A nil peer fails closed. |
| Grant revocation | `execadapters.RevokeAttemptGrants` bound as `execpg.AttemptGrantRevoker` → `runtimeauth/postgres.RevokeAttempt(ctx, tx, scope, attempt, now)` | Uses the supplied transaction, scoped by tenant, project and attempt, and is idempotent. It deliberately does not call `runtimeauth.Service.Revoke`, which opens its own transaction and would not roll back with the mutation. |
| Attempt view for grants | `rpg.NewAttemptAuthority(store, namespaces, actions)` over the runtime store | The lockable view of an attempt that `runtimeauth/postgres` checks grants against. |
| Session guard | `rpg.NewGuard(attempts, ring.Generation, clock)` | Repeats `runtimeauth.CheckSession` inside a caller-owned transaction: route, session, signing generation (current, not token's), action, expiry, and the receipt exception. |
| Guarded execution store | `rpg.NewExecutionStore(runtimeStore, db, guard)` | With a `Session` in the context, opens one tenant transaction, calls the owner's `UpdateInTx`, and repeats the guard after execution has locked the run and loaded the aggregate, before the mutation. Without a session (trusted background work) it takes execution's own path. |
| Guarded workspace store | `rpg.NewWorkspaceStore(workspaceStore, db, guard)` | A workspace update that loads an attempt receipt under a session runs the guard first, then enters the workspace row lock; another attempt's receipt is refused. Writer claims, reconciliation and human operations pass through. |
| Session propagation | `results/adapters.NewAuthority(...)`. `Bind` calls `rpg.WithSession`; the stores read it with `rpg.SessionFrom` | Authentication is only the first check; the in-transaction guard decides. |
| Cancellation consumer | `workpg.CancellationBinding(name, execadapters.TxCancellationRequester(store))` registered with `eventspg.NewStore(...)` | The handler runs in the event consumer's inbox transaction (in a savepoint); `RequestCancellationInTx` receives that transaction. A handler failure rolls the effects back and the inbox records only the retry. |
| Periodic cancellation | `execution.Service.ReconcileCancellations` over `execadapters.NewCancelledWork` | Covers a lost or reordered event; each item uses `Store.RequestCancellation`, which opens its own transaction around the same `RequestCancellationInTx`. |

### Integration handoff

The request chain the integration owner wires for `/runtime/v1/attempts/{attemptId}/result` and `/source` (existing routes, consumed not contributed):

1. runtime store `execpg.NewRuntimeStore(...)` (with `execadapters.RevokeAttemptGrants`);
2. attempt authority `rpg.NewAttemptAuthority(store, ...)` and guard `rpg.NewGuard(attempts, ring.Generation, clock)`;
3. decorated services `rpg.NewExecutionStore(runtimeStore, db, guard)` and `rpg.NewWorkspaceStore(workspaceStore, db, guard)`, behind the execution and workspace services;
4. the existing results adapters (`results/adapters.NewAuthority`, whose `Bind` attaches the session) and HTTP handler.

Plain-store services, identified separately and never handed to the result or source routes, serve: dispatch, observation, cancellation (event binding and periodic scan) and trusted reconciliation.

Invariants the wiring must preserve: cancellation intent revokes grants and releases secret access atomically; workload uncertainty retains writer and capacity barriers until settlement; accepted terminal receipts remain immutable and replayable; explicit revocation still denies callbacks.

Downstream: T-070/T-071 register migrations, construct the chain and select stores. T-072 unit-tests gate wiring; T-073 qualifies its actual SQL join before M2 acceptance. This record does not edit root composition or other owners' contracts.

## Lock order and participation

Two shapes of path exist and they are described separately.

**Updates of an existing, locked run** (`mutateInTx`, used by `UpdateInTx` and the cancellation batch) follow one order. Each stage is taken only if the path needs it:

1. quota scope rows (tenant, then project), only when `UpdateOptions.Quota` is set (dispatch);
2. the work-item admission lock (`FOR SHARE`), only when `UpdateOptions.LockWork` is set;
3. run row (`FOR UPDATE`), then step, attempt and bookkeeping rows; an item-scoped cancellation locks every affected run in ID order before any lower-order lock;
4. the identity authorization revision (read `FOR SHARE` by the mutation's `Authorize`, before anything is saved), then runtime grants of attempts whose authority ended (attempt ID order), then secret rows;
5. the workspace row (reservation, settlement, retained reference), including the workspace's own audit and outbox-intent writes;
6. event backlog and scope rows inside `Append`, last.

**Run creation** (`AdmitInTx`) is not an update of a locked run and **takes no quota locks**: capacity is reserved later, at dispatch. It locks the work item (`FOR SHARE`, stage 2), checks the pinned content revision, checks the requester's current authorization (stage 4's identity revision), inserts the new run and its steps (the rows did not exist, so there is no stage 3 lock to take), registers the secret pins (stage 4), records the retained-source intent through the workspace entities (stage 5), then writes transitions, audit and events (stage 6). A repeat of an existing run ID returns the stored run before taking any lock.

Stage 4's grant revocation and the later stages are scheduled by the adapter, not by the order in which entities queue effects: `UpdateInTx` revokes grants, then runs queued effects with workspace writer settlement deferred behind them (`deferWriter`; `settleWriters` runs after the secret release). A batch (`RequestCancellationInTx`) locks **every** affected run row in ID order first, completes stages 3–4 for every run, then settles queued writers for every run, and only then records history, audit and events for every run. The item-scoped intent row is written before the run locks and carries no lower-order dependency.

Per path:

| Path | Stages |
| --- | --- |
| Admission (creation, `AdmitInTx`) | 2 (work), 4 (authorization, pins), 5 (retained reference), 6; no quota lock, no existing run to lock |
| Dispatch / writer claim | 1, 2, 3, 4 (authorization), 5 (writer, reservation), 6 |
| Guarded result (new or replay) | 3 (run lock taken by execution), guard check (takes the attempt lock, a no-op under the held run lock, and the grant `FOR SHARE`), mutation, 4 (secret release), 5 (writer settlement), 6 |
| Guarded workspace publication update | guard check (run lock, grant `FOR SHARE`), then 5, then 6 |
| Direct or item-scoped cancellation | 3, 4 (authorization, grants, secret release), 5 (queued settlements only), 6 |
| Reconciliation, observation, termination | 3, 4, 5, 6 as needed; trusted background path, no session |

Distinguish three kinds of locks that are easy to conflate:

- **Workspace audit writes** are plain inserts into `audit` and the event outbox made by the workspace module inside stage 5. They are not the event-backlog locks of stage 6.
- **Event-append locks** (stage 6) are the tenant's shared replay-admission advisory lock, then the `event_backlog` row (`FOR UPDATE`) and the scope row, taken inside `Append`.
- **Inbox admission and claim locks** belong to the event consumer's enclosing inbox transaction, which is separate from stage 6: the tenant's replay-admission lock in shared mode, then the `event_inbox` row (`FOR UPDATE`), then whatever the handler locks, including (through the handler) stages 3–6 above. They are distinct from append locks and must not be presented as one stage.

### Cancellation versus later settlement

A cancellation request, by event, direct call or periodic scan, is intent plus revocation: the intent row, the runs' cancel request, the attempts' control revocation, the runtime grants, secret pins, and the audit, history and outbox rows commit together. It does **not** release the workspace writer slot or the capacity reservation of an attempt whose workload is not confirmed gone. Those are released by the later termination/settlement transaction when the executor confirms the workload gone. A pending publication or uncertain workload keeps its writer barrier until trusted reconciliation produces affirmative evidence. Reconciliation never converts a cancelled attempt into success.

### Preserved invariants

- Terminal receipts are immutable. A repeat of the accepted result hash returns the original acknowledgment, including the persisted finish time, without writing history or events. A different hash is `409 result_conflict`. A receipt decision reads no artifacts.
- An attempt whose result was accepted keeps only the receipt exception: its control is revoked, its grant is not, so a retry reaches the stored acknowledgment. Active-attempt authority ends with the terminal outcome. An explicit grant revocation, expiry or a signing rotation ends the exception.
- An attempt that ends without an accepted result has its grants revoked in the transaction that ends it.
- An acknowledgment is sent only for a durable commit. A commit whose outcome is unknown is reported unavailable; the retry observes whatever committed.

## Migration contribution order

The composition fixture (`test/integration/execution-composition/fixture_test.go`) verifies this contribution order: base, events, httpapi, identity, work, secrets, pipeline, workspace, artifacts, then execution (`execution`, `execution-workspace-links`, `execution-links`), then runtime authority (`rtpg.Migrations()`) after execution. That is the order the integration owner must register in the composition root; no executable root registers these contributions yet, so this record does not show that root composition is wired. No new migration is needed, and merged migrations are not modified.

## Gate participation boundary (T-034, T-073)

Gate decisions and current-subject/current-authority checks belong to T-034's owner. The execution join uses one tenant transaction: lock in the order above, check current subject/decider authority through the caller's transaction and append audit/events last. This record does not invent a gate implementation. T-072 unit-tests its wiring; T-073 qualifies the actual SQL join before M2 acceptance. M1 gate kinds stay disabled.

## Evidence

Fixture: `test/integration/execution-composition` runs real PostgreSQL under separate owner and non-owner runtime roles (non-superuser, non-BYPASSRLS, forced RLS), the real results HTTP handler, the real event store, and, where noted, LocalStack S3 and an HTTPS Git server. Kubernetes Job observation and the hosted identity are controlled doubles. Faults wrap the existing seams (commit, peer bindings, event append, grant revoker); no production code is altered. The resource prefix is `t062`.

| Acceptance | Test | Requirements | Real dependencies |
| --- | --- | --- | --- |
| `execution_peer_rollback` | `Test_execution_peer_rollback` | SD-04, SD-09–SD-11, PX-05, PX-11, WS-11 | PostgreSQL |
| `cancel_grant_atomicity` | `Test_cancel_grant_atomicity` | SD-10, SD-11, PX-07, PX-10, PX-12, IT-03, IT-06, WS-11 | PostgreSQL, event store |
| `guarded_result_replay` | `Test_guarded_result_replay` | SD-04, SD-10, PX-10, PX-12, IT-03, IT-06 | PostgreSQL, S3 for artifact cases |
| `composition_tenant_pool` | `Test_composition_tenant_pool` | SD-04, SD-10, IT-03, IT-06, WS-11 | PostgreSQL |

Supporting tests:

- `TestCompositionLockOrder` (SD-04, SD-10, PX-07, PX-11, WS-11): batch lock order, competing finalization, cancellation and reconciliation without inversion, an unknown effect blocked across service reconstruction.
- `TestGuardedSourceRecovery` (SD-10, WS-11, PX-12): a losing source callback cannot start publication; Git success followed by SQL failure keeps the writer barrier; trusted reconciliation settles without making cancellation successful; Git and executor calls occur outside SQL, with a positive control for the probe. Needs PostgreSQL, S3 and Git.
- `Test_runtime_store_construction`, `Test_runtime_store_revocation` (`internal/execution/postgres`), `Test_revoke_attempt_grants_bridge` (`internal/execution/adapters`): required revoker, joined rollback, scoped revocation, accepted-receipt exception.

What the tests prove, by acceptance:

- **`guarded_result_replay`**: successful, failed, needs-input and validation-failure receipts replay byte for byte and conflict on a different hash; explicit revocation, signing rotation and expiry each deny a replay on their own fixture without changing the receipt; a grant revoked or an attempt cancelled **after** successful authentication and before the guarded mutation is denied by the in-transaction check, for a new result and for a receipt replay (the request reaches execution's guarded mutation, and a removed guard makes this test fail); concurrent duplicates converge; failures before commit roll back; a lost commit acknowledgment is unavailable before the replay; receipt decisions survive an artifact outage.
- **`cancel_grant_atomicity`**: direct cancellation revokes the grant with the intent, secrets release in the same transaction, in order, once; a revoker, secret, event or commit failure rolls everything back and leaves the token usable; an item-scoped batch is one transaction and idempotent; a failure at a later run of the batch rolls back an earlier run's grants, pins and history; the caller-owned-transaction rollback; the actual consumer path (`workpg.CancellationBinding` over `adapters.TxCancellationRequester`, driven through `Store.Process`) commits the cancellation with the inbox completion (same transaction), leaves only the retry record on an injected failure, deduplicates and tolerates reordered versions; the periodic scan recovers a lost event; result-versus-cancellation races produce one outcome; grant admission versus cancellation is decided by the winning transaction. The intent does not release the writer slot or capacity; the later reconciliation does.
- **`execution_peer_rollback`**: admission pins and retention, writer claims, reservations, settlement, history, audit and outbox roll back together under peer, event and commit failures; retries consume no extra ordinal and release resources once.
- **`composition_tenant_pool`**: one pooled connection reused across tenants leaves no tenant context after success, rollback, cancellation or denial; foreign bindings fail; RLS and composite ownership hold under a non-owner, non-BYPASSRLS role.

## Qualification record

Required package scope: `./internal/execution/...`, `./internal/results/...`, `./test/integration/execution-composition/...` and `./test/integration/results/...` (the T-022 suite is part of the required pre-review scope).

### Inspected baseline (`fa67f79`, resource prefix `t062`)

Commands run in this pass:

- `make check-task TASK=T-062` (unit layer, static and fast checks; not merge evidence): passed over the required package scope.
- `make docs-check`: passed (links, anchors, JSON and shell fences; task graph, ownership and index).
- The scoped `go test -race -count=1 -json` run used the then-current Python resource wrapper with `KODAMA_RESOURCE_PREFIX=t062` and `go,services` leases (real PostgreSQL, S3 and HTTPS Git as the fixtures require): every package passed. The current rerun command is `KODAMA_RESOURCE_PREFIX=t062 bun scripts/openforge/src/cli.ts resources --resources go,services -- go test -race -count=1 -json ./internal/execution/... ./internal/results/... ./test/integration/execution-composition/... ./test/integration/results/...`; this command migration is not a new qualification result. Passing test counts from the original JSON stream: `./internal/execution` 39, `./internal/execution/adapters` 3, `./internal/execution/http` 10, `./internal/execution/integration` 4, `./internal/execution/postgres` 88, `./internal/results` 27, `./internal/results/adapters` 6, `./internal/results/http` 16, `./internal/results/postgres` 3, `./test/integration/execution-composition` 43 and the T-022 suite `./test/integration/results` 39. No test was skipped or failed; the only skipped packages (`./internal/execution/executiontest`, `./internal/execution/postgres/gen`) contain no tests. The four acceptance tests and `TestCompositionLockOrder` and `TestGuardedSourceRecovery` each appear with a pass action.

Not run in this pass: the coordinator-owned complete-layer receipt (`make check-task TASK=T-062 STAGE=integration LAYER=all`), which binds to the candidate commit and adds the affected consumers. This record does not stand in for it; the prior qualification below does not either. The coordinator must confirm from that receipt that the affected T-020/T-022 suites execute without required-test skips, and obtain fresh review.

### Prior qualification (base `68c37ff`; not evidence for a later candidate)

`make check-task TASK=T-062 STAGE=integration LAYER=all` passed against `68c37ff` plus documentation corrections, with four named acceptance tests found and run. From the retained `go test -json` logs every package passed. Passing test counts: `./internal/execution` 39, `./internal/execution/adapters` 3, `./internal/execution/http` 10, `./internal/execution/integration` 4, `./internal/execution/postgres` 88, `./internal/results` 27, `./internal/results/adapters` 6, `./internal/results/http` 16, `./internal/results/postgres` 3, `./test/integration/execution-composition` 43, and the T-022 regression suite `./test/integration/results` 39. Consumer packages (agents, delivery, gateway, logs, mcpproxy, environment pinning) were also selected. The only skipped test was `TestKubernetesOwnership` in `./internal/logs/kubernetes` (needs `KODAMA_KUBECONFIG`), outside T-062's owned paths.

Dependencies used: PostgreSQL (non-owner runtime role), the PostgreSQL event store, S3 (LocalStack) and HTTPS Git as the fixtures require; no NATS is used by the composition tests. No new Go module or migration.

The coordinator verifies the candidate independently and reruns affected consumers after rebase. Hosted authority and gate activation qualification remain with their designated owners.

## Known limits

- The composition fixture selects the runtime store; a composition that selects `NewStore` revokes no grants. The integration owner must select `NewRuntimeStore` wherever runtime credentials are served.
- The Kubernetes Job observation and the hosted identity are doubles here; their production adapters are qualified by their own tasks.
- Consumer-bound cancellation covers failures inside the handler. A commit failure of the consumer's own inbox transaction is the event module's contract (the inbox and the handler's effects commit or roll back as one transaction) and is not injected here.
