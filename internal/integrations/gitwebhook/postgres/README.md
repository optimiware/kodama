# internal/integrations/gitwebhook/postgres

PostgreSQL adapters of the native Git push trigger (task T-100; the trigger itself is T-036, see [`../README.md`](../README.md)). **Not wired**: T-101 mounts them in `internal/app`. This package is not a third-party provider adapter and changes neither the hook payload nor the signature protocol.

| Type | Port | Role |
| --- | --- | --- |
| `Store` | `gitwebhook.Store` | Triggers, the durable delivery claim with its admission lease and write-once outcome, and audit written in the transaction of the change it describes. |
| `Ledger` | `adapters.Ledger` | The admission ledger: run creation and the refusal decision for one idempotency key in one transaction. `adapters.Runs` maps `gitwebhook.Runs` onto it. |
| Baseline schema | — | `git_triggers`, `git_trigger_deliveries`, `git_trigger_admissions`, the guard triggers and `kodama_git_trigger_locate`, in `internal/storage/migrations/001_schema.sql`. |

## Wiring (for T-101)

```go
manifest, err := storage.NewManifest(storage.BaseContribution())             // trigger tables: baseline, after runs, pipeline definitions and work items
store, err := gwpg.NewStore(runtimePool, gwpg.Options{})                      // gitwebhook.Dependencies.Store
ledger, err := gwpg.NewLedger(runtimePool, executionStore)                    // executionStore: *execution/postgres.Store (AdmitInTx)
planner, err := adapters.NewExecutionPlanner(adapters.PlannerConfig{Preparer: executionService, Work: workContent, Repositories: repositories, Workspaces: workspaceStore})
runs, err := adapters.NewRuns(planner, ledger)                                // gitwebhook.Dependencies.Runs
```

The trigger tables are part of the baseline schema (`internal/storage/migrations/001_schema.sql`), placed there after `tenants`/`projects`, `pipeline_definitions`, `work_items` and `runs` because they reference them. A later schema change appends one contribution after the baseline; the ledger refuses a pending migration ordered before an applied one.

## Tenancy

Every request statement runs in `storage.WithTenant` under the non-owner runtime role, with explicit tenant and project predicates, composite foreign keys (trigger → project/definition/work item; delivery → trigger/run; ledger → project/run) and `ENABLE`+`FORCE` row level security. The hook URL carries only the trigger address, so the address is globally unique (`git_triggers_address_key`) and `kodama_git_trigger_locate(address)` — a `SECURITY DEFINER` function returning only `(tenant_id, project_id)` — finds the owner before the tenant transaction starts (same pattern as T-019's grant locator). Delivery methods that receive only a `DeliveryKey` locate through the trigger, so a delivery is never addressable outside its trigger's tenant. A claim, finish or audit naming another scope is `ErrNotFound` and changes nothing.

## Delivery claim

The primary key `(tenant, project, trigger, delivery)` (also unique on `(trigger, delivery)`) is the claim: `ClaimDelivery` inserts `ON CONFLICT DO NOTHING` and reads the row back, so concurrent identical deliveries produce one created record and every other caller gets it. The row carries the admitting mark (set once, never withdrawn), the admission lease and the single outcome. Guards (every role): a finished delivery is immutable; admission is not withdrawn; identity columns are not updatable. `FinishDelivery` evaluates the fence and the holder check under the row lock (`FOR UPDATE`): a pre-admission finish of an admitting delivery, an admission finish of one that never began, and a rejected admission finish by a non-holder are `ErrConflict`; the first finish wins and writes no second audit. The run of a `started` outcome is a foreign key to `runs`.

## Admission ledger

`git_trigger_admissions` has one row per `(tenant, project, idempotency key)` that names **either** the run admission created **or** the refusal admission decided (`CHECK ((run_id IS NULL) <> (refusal_reason IS NULL))`); rows are never updated or deleted (no grant, no policy). `Ledger.Admit` and `Ledger.Decide` take a transaction-scoped advisory lock on the key, read the row, and only for an undecided key either call the core's `AdmitInTx` and insert the run row (same transaction, so a rollback or lost commit never leaves a run without its key or a key without its run) or insert the refusal. Hence a refusal commits only if no run did, and after it commits every later `Admit` — including a holder that planned before the refusal, stalled, lost its lease and resumes, or a process after a restart — finds it under the lock and creates nothing (`ErrAdmissionClosed`). The lock wait is bounded (`lock_timeout` 10 s); expiry is an outage, never a decision.

## Not provided / decisions left open

* **Retention.** Delivery records, the ledger and audit are kept; there is no `DELETE` grant. A retention horizon (README of the module) is not chosen here.
* **Rate limiting** of unauthenticated traffic stays an edge concern of the assembly.
* The admission plan's run inputs are empty: no payload field other than the verified commit reaches admission.

## Tests

`Test_git_trigger_store_atomic_claim` (unit layer): the Store contract over the module's memory store. The same contract runs against PostgreSQL in `TestPostgresStoreContract`, with SQL-level proof in `TestPostgresStoreIsolation` (tenant isolation, RLS, locator), `TestPostgresStoreDurability` (lost acknowledgements, write-once outcomes, guards, restart), `TestPostgresMigrations`, `TestPostgresLedgerContract` and `TestPostgresLedgerAtomicity` (the refusal-ledger contract over the real execution admission, rollback and lost commit, schema exclusivity) and `TestPostgresTriggerRoundtrip` (the module's `Service` over the PostgreSQL store and the core's real run admission). PostgreSQL tests use `test/support/postgres` and skip without `KODAMA_TEST_POSTGRES_ADMIN_URL` (always in the unit layer); skipped service tests are not acceptance evidence.
