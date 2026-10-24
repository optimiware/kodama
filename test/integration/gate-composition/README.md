# Gate/execution transaction qualification (T-073)

This suite is the prerequisite checkpoint that qualifies the merged gate owner's
(T-034) contribution against the execution/results composition (T-062) before
T-072 activates it in the application. It adds no route, event type, migration,
command, dependency or root wiring and edits no production code. It consumes the
actual constructors:

```text
execpg.NewRuntimeStore(db, appender, peers, grants, opts)            // T-062, grant revocation joined
gatespg.NewStore(Config{DB, Events, Execution, Authority, Deployments})
gatespg.NewKeyed(httpIdempotency, gateStore)
gatespg.NewExecutionStore(db, runtimeStore /*execution.Store*/, runtimeStore /*ExecutionTx*/, gateStore)
gates.NewService(Dependencies{...}, Options{GateTTL})
```

The same `*execpg.Store` is the `Execution` binding of the gate store and the
decorated store of the gate-aware execution store. A composition that selects
`execpg.NewStore` (no grant revoker) or hands the gate store a different
execution store does not get this join.

- **Requirements:** SD-04, SD-10, SD-11 ([software design](../../../docs/specs/software-design.md)); IT-03 ([identity and tenancy](../../../docs/specs/identity-tenancy-and-rbac.md)); DQ-03, DQ-04, DQ-11, DQ-14 ([delivery and gates](../../../docs/specs/delivery-and-quality-gates.md)); PX-13 ([pipeline execution](../../../docs/specs/pipeline-execution.md)).
- **Contracts consumed:** [execution composition](../../../docs/implementation/execution-composition.md) (lock order, bindings), `internal/gates/README.md` (lock order, integration owner checklist), `internal/gates/postgres/store.go` package comment.
- **Not covered:** hosted authority (T-061); a real join to the deployment module (see [Finding](#finding-no-real-deployment-join-exists)); the binder's data source is a SQL fixture table, not the delivery module's; the real agent planner (a double expands agent steps and pins the baseline); Git/S3/NATS adapters (workspace history and run source verification use the execution fixture's fake sources, evidence bytes an in-memory object store, and the decision wakeup is driven through the consumer binding's `Handle` in the consumer's tenant transaction; the inbox transaction belongs to the event module). T-072 repeats the selected executable path.

## Fixture

`fixture_test.go` runs real PostgreSQL under separate owner and non-owner runtime
roles (non-superuser, non-BYPASSRLS, forced RLS) with the migration order base,
events, httpapi, identity, work, secrets, pipeline, workspace, artifacts, execution,
gates and runtime authority (the manifest rejects an applied contribution ordered
after an unapplied one, so runtime authority is appended after gates here; the two
do not depend on each other and the root may list either first). Fault injection
(`faults_test.go`) wraps only existing seams: the transaction commit, the
execution peers (workspace, secrets), the grant revoker, the event appenders, the
gate store's transaction-bound authority binder and its deployment binder. Every
seam hit records `txid_current()` read through the very `pgx.Tx` the seam received,
so equal identifiers prove the participant joined the caller's transaction rather
than opening its own. Resource prefix `t073`.

## Transaction participation record

Verified by `Test_gate_join_rollback/the_participants_join_one_transaction` (one
transaction identifier per operation) and by the rollback cases below. Every
rollback case compares a snapshot taken immediately after the injected failure,
before any retry, with the snapshot before it. The snapshot covers run, step and
gate state, run history, **all execution audit rows** (run, step and attempt rows,
attempts found by the metadata's run id, not by a surviving attempt row), **all
execution events** (run and attempt events, attempts found by the payload's run
id), the gate's audit rows and events found by the stable `GateIDFor` identity
(so an orphan request record is visible although no gate row exists), evidence
references, step bindings, attempts, ordinals, reservations and writer slots. A
positive control proves the audit and event queries see a committed claim.

| Operation | One tenant transaction (all roll back together) | Outside the transaction / durable intent |
| --- | --- | --- |
| **Request** (`Service.RequestForStep`) | work item `FOR SHARE`, run `FOR UPDATE`, recomputed gate inputs compared with the prepared ones, gate row, evidence references, step binding (`gate_ref`), audit, `gate.requested` outbox | evidence/candidate/deployment/conformance verification and artifact pins (Git, S3, deployment reads). Pins are idempotent and keyed by the stable `GateIDFor`; a failed request leaves pins and no gate, and the retry converges on the same gate and pins. |
| **Decision** (`Store.Decide`, keyed decision) | gate `FOR UPDATE`, reviewer authority through the transaction (authorization revision `FOR SHARE`), current facts (work item revision, artifact digests/usability, deployment configuration via the binder), the immutable decision, audit, `gate.decided` outbox, and for the keyed path the stored HTTP response | Progression is **not** part of the decision. The approved gate is the durable intent. |
| **Consumption** (`CompleteStep`, `RejectStep`, wakeup consumer, `Reconcile`) | work item `FOR SHARE`, run `FOR UPDATE`, gate `FOR SHARE`, current facts, the original reviewer's authority (revision `FOR SHARE`), final expiry sample, the explicit `pending → succeeded`/`failed` step transition, history, run events | Applied from the durable decision by the reconciliation scan or the `gate.decided` consumer (`DecisionBinding`); events only wake and a missed event is covered by the scan. |
| **Protected claim** (`ExecutionStore.Update` with `Quota`) | quota scopes, work item, run, **all bound gates `FOR SHARE` before the mutation reads authorization**, the claim mutation (attempt, ordinal), the gate re-check (subject, facts, reviewer, expiry), then the effects (secret pins, workspace writer slot, capacity reservation), history and run events | none; a refusal happens inside the mutation, before any effect is applied, and rolls the transaction back (no attempt, ordinal, reservation or writer slot). |
| **Invalidation** (`Reconcile`, refused decision) | gate `FOR UPDATE`, the recorded staleness, audit, `gate.invalidated` outbox, once | definitive changes only; an outage records nothing |
| **Cancellation / result** | execution's own path (runtime store revokes grants in the same transaction); takes no gate lock and is never blocked by a stale approval | — |

Where two independent transactions are involved, atomicity is not assumed: the
decision and its progression are two transactions (durable approved gate, then
reconciliation); the evidence pins and the gate row are two (idempotent pins,
then the request); the consumer inbox record and the wakeup handler are the event
module's single transaction.

### Lock order observed

`quota rows → work item → run (+ steps, attempts) → gates → authorization
revision (reviewer, requester) and secrets → workspace → event backlog`. A
decision takes only gate → authorization → events and never the run row.

- `Test_gate_join_revocation` holds each lock (the gate row; the reviewer's
  authorization revision) in a separate transaction, observes the waiter in
  `pg_locks`, commits a revocation or a change meanwhile and shows the waiter
  re-reads current authority and facts after the wait: decision, consumption and
  protected claim.
- The remaining order evidence is `internal/gates/postgres.TestGateClaimLockOrder`
  (racing claims, decision and revocation without a deadlock), which T-034's
  acceptance keeps; this suite's racing duplicate decisions, consumptions and
  claims complete without a deadlock over the composed runtime store.

## Acceptance

| Acceptance | Test | Requirements | What it proves |
| --- | --- | --- | --- |
| `gate_join_rollback` | `Test_gate_join_rollback` | SD-04, SD-10, SD-11, IT-03, DQ-03, DQ-04, DQ-11, PX-13 | Participants share one transaction; constructors fail closed; failures injected after the authority check, after the event append, after the deployment read, after the workspace reservation, at commit and with a lost commit acknowledgment roll back gate/step/run state, history, audit and outbox for request, decision, completion, rejection and the protected claim; approvals and rejections survive a failed progression; retries produce one transition. |
| `gate_join_revocation` | `Test_gate_join_revocation` | SD-04, SD-10, SD-11, IT-03, DQ-03, DQ-04, DQ-11, DQ-14, PX-13 | Concurrent reviewer revocation (during a gate lock wait and an authorization revision wait) at decision, consumption and claim; changed specification, source, evidence, deployment configuration and conformance evidence at decision, consumption and protected claim (one invalidation, decision intact, no compute); cancellation versus approval; duplicate decisions (identical, conflicting, keyed), consumptions and claims; pooled tenant reuse over a one-connection pool with RLS and role checks; a deployment gate fails closed without the transaction-bound binder. |
| `gate_join_restart` | `Test_gate_join_restart` | PX-05, PX-13, SD-04, SD-10, SD-11, IT-03, DQ-04 | Restart of every service before and after a decision, after failed and lost-acknowledgment progression commits, after an outage, with missed/duplicated/reordered/forged wakeups, with a change or a revocation while down, and with an approval expired while down; recovery moves each step once and allocates no attempt, ordinal, reservation, writer slot or workload for a gate. |

T-034's original acceptance remains required (`make check-go` over
`./internal/gates/...`); this suite does not replace it.

## Finding: no real deployment join exists

The gate store takes a `DeploymentBinder` (`func(pgx.Tx) gates.Deployments`) and
the gate service a `gates.Deployments` port. Nothing in the merged repository
implements either over the delivery module (T-035): `internal/delivery` exposes
`GetDeployment`/`FindByAction`, each opening its own tenant transaction, and
`internal/gates/adapters` has no deployment adapter. Consequently:

- With no binder (the merged state), a deployment-bearing gate cannot be decided
  or consumed: the decision and the consumption fail with `ErrNotConfigured`, the
  step stays pending and nothing is allocated (tested). Source-only gates are
  unaffected.
- This suite exercises the deployment path through a **tenant-scoped SQL fixture
  table** (`t073_deployments`, forced RLS, non-owner runtime role; `SQLDeployments`)
  and the gate store's binder. The binder reads the row through the `pgx.Tx` it is
  handed, with one statement that also returns `txid_current()`; equal identifiers
  with the authority, event and workspace participants prove the read ran in the
  decision, consumption or claim transaction, an uncommitted row written by that
  transaction is visible to the binder and to nobody else, other projects and
  tenants never see it, and a one-connection pool completes the whole gate flow
  (a read that opened its own transaction would wait for the connection its caller
  holds). What this qualifies is the **gate store's binder contract**. It does
  **not** qualify a join to the delivery module (T-035), whose `deployments` table
  and store are not read by anything here, and is not evidence that one works. Its
  outage case is an injected failure after a successful read.

**Activation of deployment-bearing gates is therefore parked.** Bounded repair
request: an adapter implementing `gates.Deployments` and `gatespg.DeploymentBinder`
over `internal/delivery/postgres` (read the scope's `deployments` row, its
`config_digest` and `source_commit`, through the supplied `pgx.Tx`, with plain
reads that take no lock ordered before the run and gate locks, no network call
and no second transaction), owned by the T-035/T-034 owners (the adapter lives in
`internal/gates/adapters` or `internal/delivery/adapters`), ahead of T-067's
delivery qualification and T-072's wiring. T-067 then qualifies it against real
deployment rows; this suite's binder-contract cases are what it must meet.

## Known limits

- The authority binder is the standalone local identity adapter
  (`adapters.BindAuthority`); hosted authority is qualified by T-061.
- Gate expiry uses the injected clock shared with execution, as the gate owner
  documents; a database-clock source is not introduced here.
- Cancellation versus the gate (a gate step of a cancelled run) is asserted by
  outcome (no progression, no compute); the run/step terminal state is the
  execution module's.
