# T-020 review notes

Candidate-specific evidence lives in the harness handoff; this file records what a reviewer
should check and the assumptions the integration owner confirms.

## Baseline

Requirements PX-02, PX-05–PX-09, PX-11, PX-12, PX-14 (acceptance PX-A01, PX-A03–PX-A06,
PX-A09; PX-A02/PX-A07/PX-A08 orchestration seams only). Decisions: confirmed C-01–C-04,
C-11–C-14, C-16/C-17; ADRs 0001–0003, 0005–0007; proposed P-01/P-02, P-05–P-09, P-13–P-15
preserved; O-11 (retention) and O-18/O-19 (hosted authority/custody) untouched.

## What to check

* Entities (`run.go`, `step.go`, `attempt.go`, `aggregate.go`) hold the invariants; the
  PostgreSQL adapter only persists them (`postgres/save.go`, `load.go`) and binds peer seams
  (`env.go`, `admission.go`, `cancellation.go`, `authority.go`).
* Lock order in `postgres/store.go` and `ports.go` (`Env`).
* The execution tables in the squashed baseline (`internal/storage/migrations/001_schema.sql`)
  guard immutable pins, terminal immutability, one unsettled attempt per step, column-scoped grants,
  append-only history/evidence and forced RLS.
* The unknown-effect barrier (`Aggregate.Settle`) and the replay-safety rule
  (`launch == not_started` or trusted reconciliation).

## Assumptions

1. **Execution settings.** Merged project settings define no typed concurrency or startup-budget
   fields: `execution.Limit` is an explicitly injected, validated policy (`adapters.StaticLimits`);
   missing limits deny dispatch. No environment variable or project-setting key is invented.
2. **Retry safety.** General commands have no effect-lookup contract. An ambiguous failure is
   `outcome_unknown`; only the executor's `not_started` launch evidence or a trusted
   `EffectReconciler` makes a replay safe, and missing evidence leaves the step blocked.
3. **M2 seams.** Cleanup (`Dependencies.Cleanup`), gate and agent hooks exist but are disabled in M1
   wiring; public creation accepts work runs only.
4. **Migration packaging.** The execution tables and their dependent workspace, artifact and
   secret-pin constraints are part of the squashed baseline
   (`internal/storage/migrations/001_schema.sql`); there is no per-module migration directory and no
   generated copy. The one later change this package contributes is the additive run_shared schema,
   appended as `003_run_resources.sql`.
5. **Retention confirmation.** The retained-source *intent* is persisted atomically with the run;
   confirmation uses `workspace.Service.PinCommit`, which is idempotent for the same run and commit.
6. **Event vocabulary.** Only existing catalog events are used. A cleanup run (no work item) emits
   `run.state_changed none → queued` instead of `run.created`, whose schema requires a work item;
   a retention confirmation and a cancellation request emit `run.state_changed` with equal
   `from`/`to` and reason `source_retained` / `cancel_requested`.
7. **Success settlement.** A successful attempt is settled in the result transaction (its writer
   publication verified there); Job cleanup remains the executor's responsibility.

## T-080 (unit qualification) notes

* `memory/` is a second `execution.Store`; compare `memory/save.go`, `update.go`, `admission.go`
  with their `postgres/` counterparts. The trigger guards (terminal immutability, pinned
  columns, one-step versions, unsettled-attempt insert guard, bookkeeping recorded once) are
  written in Go there and covered by `memory/guards_test.go`.
* Bounded fix: `AdmitInTx` (both stores) staged the creation transitions on the caller's plan,
  so repeating a plan after a lost commit stacked a second set of creation history rows and
  `run.created` events with different event identities. It now stages them on a copy
  (`TestUnitPeerJoins`). The PostgreSQL path of this fix is covered by the existing suites only
  when the `test` environment runs them.
* `executiontest.Contract` has not been run against PostgreSQL by this task (no service in the
  unit layer); its first PostgreSQL run belongs to T-026. Assertions avoid lock order, isolation
  and error classes that differ by backend.
* Review repair: evidence and audit payloads are deep-copied through JSON (as into jsonb, `TestUnitEvidenceIsSerialized`). Earlier: `memory/clone.go` copies every row at the adapter boundary in both directions
  (memdb does not deep-copy), and history/audit/evidence keys come from a kernel-resident,
  transactional sequence (`nextSeq`) instead of a per-adapter counter. Covered by the contract
  subtest "nested data crosses the store boundary by copy" and `TestUnitSharedKernelAppendOnly`.
