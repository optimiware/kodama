# PostgreSQL component support

`pgtest` starts one pinned `docker.io/library/postgres:18-alpine` container per
shard through the I02a harness, creates one `kodama` database, and exposes the
I01c pgx wrapper for every conformance read and write. Authentication is
`trust` on the shard's private network only; containers are 1 CPU / 1 GiB and
carry the harness cleanup labels.

## Bootstrap facts installation must provision

- Migration `0001_roles.sql` creates `kodama_owner` as `NOLOGIN` and
  `kodama_runtime` as the non-owner runtime login. `0001` does not grant the
  owner `CREATE` on schema `public`, and PostgreSQL 15+ makes `public` owned by
  `pg_database_owner` with `CREATE` revoked from `PUBLIC`. A deployment that
  applies migrations as the owner must therefore make `kodama_owner` the
  database owner (`ALTER DATABASE ... OWNER TO kodama_owner`) or explicitly
  grant `CREATE ON SCHEMA public`. This runner does the former once, immediately
  after `0001`; the migration set itself cannot do it because the role does not
  exist before `0001` and the connecting administrator creates the database.
- `0001` and `0009_system_roles.sql` run as the administrator because
  `CREATE ROLE` needs `CREATEROLE` and the owner is deliberately
  `NOCREATEROLE`. Every other migration runs through `SET LOCAL ROLE
  kodama_owner` inside the wrapper transaction, so `0008`'s NO FORCE/FORCE
  bracket is exercised as the non-bypass owner.
- The migration ledger lives in the test-only `kodama_test_meta` schema, not
  `public`, so `0007`'s `GRANT ... ON ALL TABLES IN SCHEMA public` does not
  require the owner to have grant authority over a table it does not own.
  Re-running an applied identical migration is a no-op; a changed body is
  refused (`ErrAppliedMigrationRewritten`).

## In-process stand-ins (not conformance subjects)

`allowAuthorizer` and `confirmAuthority` in the component package are in-process
stand-ins. They exist so the real PostgreSQL adapter and server enforcement can
run; they are **not** evidence about authorization policy or authority
evaluation. This suite does not prove either. Authorization and authority are
proven by their own package suites and the I03 composition boundary. Every
operation the stand-ins admit is still constrained by the real database role,
transaction scope, row-level security, constraints and grants, and the security
tests assert the exact SQLSTATE for each refusal.

## Deliberate policy-layer isolation

`TestSystemRoleRLSPoliciesOnPostgreSQL` drops two frozen objects inside its own
throwaway shard before the publisher case: the `events.publish_state` CHECK
constraint (which mirrors the declared value set) and the `PUBLIC events_scope`
FOR ALL policy. Both would otherwise mask the publisher's declared `WITH CHECK`:
the permissive `events_scope` policy's implicit `WITH CHECK` is true for an
in-scope row, so PostgreSQL ORs it with the publisher policy. Dropping them in
the isolated shard lets the declared publisher policy be the only refuser, so
the test can prove that predicate is not a no-op. The schema-facts test
separately proves both objects remain present and unchanged in the frozen
catalog. No committed schema or product behavior is modified.

`TestMembershipGuardWithoutUniqueIndexOnPostgreSQL` applies the same technique
to the membership absence guard: on its own throwaway shard it drops the
`memberships_scope_principal` unique constraint, so only the guard `SELECT`
stands between a first grant and a duplicate row. Production keeps the unique
index, the advisory lock and the guard together (defence in depth); with all
three in place a mutant that removes any single one is masked by the others.
The test removes the index so the guard is the sole protection, and the
advisory-lock assertions in `TestMembershipGrantReadCommittedGapsOnPostgreSQL`
(the blocked backend must hold exactly one granted advisory lock) make the lock
the sole protection in the other direction. The schema-facts and migration tests
separately prove the constraint exists in the frozen catalog.

## Evidence

`KODAMA_EVIDENCE_DIR` is a **parent**: each test process resolves one unique
child `run-<pid>-<random>/` under it, publishes that actual run path to the
harness through `KODAMA_EVIDENCE_DIR`, and writes everything for that process
there. Concurrent runs that share one parent therefore land in separate run
directories instead of overwriting each other. When unset, the parent is the
ignored `.harness/evidence/I02b` path, so no unowned source path is created.

Each run directory receives `images.jsonl` from the harness and per-suite
summaries from this package. Server evidence is per shard:
`postgres-server-<shard>-<hash>.json` records the harness shard identity next to
the image tag and digest, server version and database, so every shard keeps its
own server identity. Writes stage the document in a unique temporary file next
to the target and rename it into place, so concurrent writers never share a
staging path and readers never observe a partial document.

## Publisher effective protections (r11 observation)

Production runs the event outbox **unscoped**: `internal/shell/store/postgres/outbox.go`
uses `beginUnscoped` and never sets `kodama.tenant_id`/`project_id`. In that
shape the PUBLIC `events_scope FOR ALL` policy evaluates to NULL, so only the
publisher's declared `any` SELECT and `publish_state IN ('pending','published')`
UPDATE WITH CHECK admit rows. An out-of-set `publish_state` update is therefore
refused by RLS (SQLSTATE 42501, row-security message) on the unmodified schema.

If a caller sets the row's scope, the permissive `events_scope` policy's implicit
WITH CHECK is OR-ed with the publisher WITH CHECK, so the declared publisher
WITH CHECK no longer blocks an out-of-set value; the explicit
`events_publish_state` CHECK constraint still refuses it (SQLSTATE 23514). This
is acceptable for the production publisher because it never sets a scope, but it
is an observation for the C02 owner: the declared per-role WITH CHECK is not the
effective protection in the scoped shape.

## Known defects and fixes

No `reproducer`-tagged file remains in this card; every finding is now a
required regression test.

- Fixed (master 481ef6b, C02 attempt 5): migration 0004 now creates every
  UNIQUE before foreign keys; the frozen store vector's cross-scope seed was
  corrected. `TestMigrationsApplyOnEmptyDatabase` and
  `TestFrozenStoreVectorsOnPostgreSQL` cover both.
- Fixed (C-ADAPTER r11, migration 0014): the declared `kodama_controller`
  INSERT policies on `events` and `audit` are rendered and present;
  `TestDeclaredControllerInsertPoliciesPresent` is the required proof.
- Fixed (master fb876d7, P13): `Controller.Read` closes its rows iterator before
  committing the read-only transaction; the former `conn busy` reproducer is
  promoted to `TestControllerReadProductionShapeOnPostgreSQL`.
- Observation for the work-adapter owner (not a P1): an ask without references
  stores the JSON value `null` in the nullable jsonb column
  `tasks.question_references` (`jsonColumn(nil)` in
  `internal/shell/store/units/work/task.go`), whereas an unasked task holds SQL
  NULL. The reader decodes both to an empty list, so behavior is correct, but the
  column has two spellings of "no references". The earlier helper conflated them
  because the driver decoded a jsonb `null` to Go `nil`; the golden helper now
  reads jsonb as text and the suite pins `"null"` after an ask.
- Remaining observation for C02 (not a P1): the permissive `events_scope`
  policy makes the declared publisher `WITH CHECK` ineffective when a caller
  sets a scope; the CHECK constraint still refuses out-of-set values and
  production never sets a scope, so no exploitable path was found.

## Mutation gate (I02b punch-through)

The gate applies 53 single- or double-edit mutants to a scratch copy of the tree
and requires each to turn the suite red. It is reproducible: `mutants/gate.py`
runs it from a clean `git archive` export of the candidate commit
(`python3 gate.py run <label>...`; `definitions.py` holds the reviewer's 51
single-edit definitions, `gate.py` adds the two membership probes `C1` and `C2`
and the corrected `YAML_zero`, `RCH_zero` and `GO_matcherempty`). Every mutant
passes through three tiers and stops at the first failure:

- **tier 0**: the `pgtest` database-free unit tests;
- **tier 1**: a targeted `-run` of the component tests that should catch it;
- **tier 2**: the full unfiltered component run, including every `TestMain` gate.

A mutant whose pattern does not apply or that does not build is `INVALID`, and a
suite that times out instead of failing is `HUNG`; neither is accepted. This run
had no `INVALID` or `HUNG` result.

Result: **52 killed, 1 equivalent** (10 at tier 0, 42 at tier 1). The mutants
the earlier reviewer sweep left alive (`PRG_done`, `RES_task`, `NONCE_zero`,
`VRE1`, `ABS_nolock`, `ABS_noguard`, `OB_globalasserted`, `OB_nocount`) are now
killed, as are the unmasking probes `C1` and `C2`.

| label | where (`internal/shell/` or `test/support/…`) | change | killed by (first two) | tier | verdict |
|---|---|---|---|---|---|
| `GO_matcherempty` | `pgtest/matchers.go` `GoldenGeneratedID` | an empty value accepted | `TestGoldenGeneratedID` | 0 | killed |
| `GO_nullcmp` | `pgtest/golden.go` `goldenCompare` | NULL versus non-NULL mismatch ignored | `TestGoldenNullMismatchIsTyped` | 0 | killed |
| `GO_omitcol` | `pgtest/golden.go` `validateGoldenColumns` | an unstated column no longer fails | `TestGoldenColumnSetValidation` | 0 | killed |
| `OB_deletes` | `pgtest/observer.go` `diffUnitSnapshots` | deleted rows no longer detected | `TestDiffUnitSnapshotsDetectsEveryChangeKind` | 0 | killed |
| `OB_gapsnil` | `pgtest/observer.go` `unitChangeGaps` | every changed row accepted as asserted | `TestUnitChangeGapsRequiresTheMatchingAssertion`, `TestUnitChangeGapsRejectsUnassertedRow` +1 | 0 | killed |
| `OB_globalasserted` | `pgtest/observer.go` | every observer shares one assertion map | `TestObserverAssertionsAreNotSharedBetweenObservers`, `TestFinishWithRequiresCountsAndAssertions` | 0 | killed |
| `OB_idonly` | `pgtest/observer.go` `recordGoldenAssertion` | an assertion also marks a table-wide wildcard | `TestRecordGoldenAssertionMarksExactlyOneKeyForTheAssertingTest` | 0 | killed |
| `OB_nocount` | `pgtest/observer.go` `finishWith` | expected-count mismatch ignored | `TestFinishWithRequiresCountsAndAssertions` | 0 | killed |
| `OB_nogaps` | `pgtest/observer.go` `finishWith` | unasserted changed rows ignored | `TestObserverAssertionsAreNotSharedBetweenObservers`, `TestFinishWithRequiresCountsAndAssertions` | 0 | killed |
| `OB_unexpected` | `pgtest/observer.go` `unitChangeCountMismatch` | unexpected changed tables ignored | `TestUnitChangeCountMismatchRequiresExactCounts` | 0 | killed |
| `ABS_noguard` | `store/postgres/absence.go` | absence guard `SELECT` made always empty | `TestMembershipGuardWithoutUniqueIndexOnPostgreSQL` | 1 | killed |
| `ABS_nolock` | `store/postgres/absence.go` | advisory-lock statement replaced by a no-op | `TestMembershipGrantReadCommittedGapsOnPostgreSQL`, `TestMembershipGrantReadCommittedGapsOnPostgreSQL/competing_insert_commits` +1 | 1 | killed |
| `AU_actor` | `store/postgres/unit.go` | audit actor suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestRealCredentialAdminOnPostgreSQL` +1 | 1 | killed |
| `AU_detail` | `store/postgres/unit.go` | audit detail written as empty text instead of NULL | `TestTriggersAdapterOnPostgreSQL`, `TestRealCredentialAdminOnPostgreSQL` +1 | 1 | killed |
| `BD_audit` | `store/units/identity/binding.go` | binding audit rows dropped | `TestResourceRegisterCreateIfAbsentOnPostgreSQL` | 1 | killed |
| `C1` | `store/postgres/absence.go` + `migrations.go` | advisory lock no-op **and** the `memberships_scope_principal` unique constraint not rendered | `TestMembershipFirstGrantAbsenceRaceOnPostgreSQL`, `TestMembershipFirstGrantAbsenceRaceOnPostgreSQL/tenant-wide` +1 | 1 | killed |
| `C2` | `store/postgres/migrations.go` | `memberships_scope_principal` unique constraint not rendered | `TestMembershipGrantReadCommittedGapsOnPostgreSQL`, `TestMembershipGrantReadCommittedGapsOnPostgreSQL/competing_insert_commits` +1 | 1 | killed |
| `CLI_intent` | `store/units/execution/commit.go` `planCleanup` | cleanup-intent attempts written 1 instead of 0 | `TestEngineCommitCleanupIntentOnPostgreSQL` | 1 | killed |
| `CLN_ws` | `store/units/execution/commit.go` | cleanup update workspace id written NULL | `TestRequestResolveVersusCancelOnPostgreSQL`, `TestRealTaskLifecycleOnPostgreSQL` | 1 | killed |
| `EV_data` | `store/postgres/unit.go` | event data replaced by `{}` | `TestEngineAdapterOnPostgreSQL`, `TestEngineCommitCleanupIntentOnPostgreSQL` +1 | 1 | killed |
| `EV_src` | `store/postgres/unit.go` | event source principal suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestEngineAdapterOnPostgreSQL` +1 | 1 | killed |
| `EV_subject` | `store/postgres/unit.go` | report event subject id suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestEngineAdapterOnPostgreSQL` +1 | 1 | killed |
| `EV_type` | `store/postgres/unit.go` | event type suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestEngineAdapterOnPostgreSQL` +1 | 1 | killed |
| `EXE_ver` | `store/units/work/task.go` | task-started execution version written 2 | `TestRealTaskLifecycleOnPostgreSQL`, `TestObservedTaskCreateAndStartOnPostgreSQL` | 1 | killed |
| `EXTRA_upd` | `store/postgres/unit.go` | an extra unrelated `UPDATE tenants` inside every unit | `TestTriggersAdapterOnPostgreSQL`, `TestRealCredentialAdminOnPostgreSQL` +1 | 1 | killed |
| `FAIL_json` | `store/units/execution/codec.go` | execution failure document encoded as `{}` | `TestExecutionSettlementPersistsOutputsAndFailureOnPostgreSQL`, `TestExecutionSettlementPersistsOutputsAndFailureOnPostgreSQL/typed_failure` +1 | 1 | killed |
| `GW_at` | `gateway/types.go` | usage record recorded_at shifted one day | `TestGatewayUsageAndContentRowsOnPostgreSQL` | 1 | killed |
| `GW_model` | `gateway/units.go` | content record model suffixed | `TestGatewayUsageAndContentRowsOnPostgreSQL` | 1 | killed |
| `GW_retain` | `gateway/units.go` | content record retain_until shifted one day | `TestGatewayUsageAndContentRowsOnPostgreSQL` | 1 | killed |
| `MEM_ver` | `store/units/identity/tenancy.go` | membership version written +1 | `TestMembershipFirstGrantAbsenceRaceOnPostgreSQL`, `TestMembershipFirstGrantAbsenceRaceOnPostgreSQL/tenant-wide` +1 | 1 | killed |
| `MSG_author` | `store/units/work/task.go` | task message author written as a constant | `TestRealTaskLifecycleOnPostgreSQL` | 1 | killed |
| `NONCE_zero` | `store/units/identity/credential.go` `nullableNonce` | stored nonce replaced by zero bytes of the same length | `TestRealCredentialAdminOnPostgreSQL` | 1 | killed |
| `OB_aftereqbefore` | `pgtest/observer.go` `Finish` | the after-snapshot is the before-snapshot | `TestRealTaskLifecycleOnPostgreSQL`, `TestTaskCreateAndEditsPersistOnPostgreSQL` +1 | 1 | killed |
| `OB_skiptable` | `pgtest/observer.go` `snapshotTables` | snapshot skips the `cleanup` table | `TestEngineCommitCleanupIntentOnPostgreSQL`, `TestRequestResolveVersusCancelOnPostgreSQL` +1 | 1 | killed |
| `PRG_done` | `store/units/work/task.go` `Task.Apply` | progress write stores state `open` instead of `done` | `TestObservedTaskProgressOnPostgreSQL` | 1 | killed |
| `PRJ_ver` | `store/units/identity/tenancy.go` | project version written +1 | `TestRealTenancyAdminOnPostgreSQL` | 1 | killed |
| `RCH_zero` | `store/postgres/unit.go` | receipt content hash written as 32 zero bytes | `TestTriggersAdapterOnPostgreSQL`, `TestReceiptCompareAndSetUnderConcurrency` +1 | 1 | killed |
| `RC_caller` | `store/postgres/unit.go` | receipt caller key suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestReceiptCompareAndSetUnderConcurrency` +1 | 1 | killed |
| `RC_created` | `store/postgres/unit.go` | receipt created_at shifted one day | `TestTriggersAdapterOnPostgreSQL`, `TestRealCredentialAdminOnPostgreSQL` +1 | 1 | killed |
| `RC_op` | `store/postgres/unit.go` | receipt operation suffixed | `TestTriggersAdapterOnPostgreSQL`, `TestReceiptCompareAndSetUnderConcurrency` +1 | 1 | killed |
| `REPLAY_short` | `store/postgres/unit.go` | receipt replay short-circuit disabled | `TestTriggersAdapterOnPostgreSQL`, `TestReceiptCompareAndSetUnderConcurrency` +1 | 1 | killed |
| `RES_task` | `store/units/work/adapters.go` `withResult` | receipt result replaced by `{}` | `TestRealTaskLifecycleOnPostgreSQL`, `TestTaskCreateAndEditsPersistOnPostgreSQL` +1 | 1 | killed |
| `SNAP_exec` | `store/units/execution/start.go` | execution snapshot written as `{}` | `TestRequestActivationPersistsExactRowsOnPostgreSQL`, `TestObservedResolvePassNonLastOnPostgreSQL` | 1 | killed |
| `SNAP_task` | `store/units/work/task.go` | task-started execution snapshot written as `{}` | `TestRealTaskLifecycleOnPostgreSQL`, `TestObservedTaskCreateAndStartOnPostgreSQL` +1 | 1 | killed |
| `TA3` | `store/units/work/task.go` | task unit audit rows dropped | `TestRealTaskLifecycleOnPostgreSQL` | 1 | killed |
| `TE4` | `store/units/work/task.go` | task unit events dropped | `TestRealTaskLifecycleOnPostgreSQL` | 1 | killed |
| `TNE1` | `store/units/identity/tenancy.go` | tenant unit events dropped | `TestRealTenancyAdminOnPostgreSQL` | 1 | killed |
| `TRG_sel` | `store/units/work/trigger.go` | trigger selection receipt result replaced by `{}` | `TestTriggersAdapterOnPostgreSQL`, `TestTriggerStartedWholeRowsOnPostgreSQL` | 1 | killed |
| `VRE1` | `store/units/platform/verify.go` | verification record events dropped | `TestRealVerifierAndReporterOnPostgreSQL` | 1 | killed |
| `WS_exec` | `store/units/execution/start.go` | workspace id derived from the execution id plus a suffix | `TestRequestActivationPersistsExactRowsOnPostgreSQL`, `TestObservedResolvePassNonLastOnPostgreSQL` | 1 | killed |
| `WS_task` | `store/units/work/task.go` | workspace id derived from the execution id plus a suffix | `TestRealTaskLifecycleOnPostgreSQL`, `TestObservedTaskCreateAndStartOnPostgreSQL` | 1 | killed |
| `YAML_zero` | `store/units/work/task.go` | execution yaml_sha256 written as 32 zero bytes | `TestEngineAdapterOnPostgreSQL`, `TestEngineCommitCleanupIntentOnPostgreSQL` +1 | 1 | killed |
| `ABS_rawmap` | `store/postgres/absence.go` `mapAbsenceInsertError` | SQLSTATE 23505 mapping to `version_conflict` disabled | none: full unfiltered run passed | 2 | **equivalent** (see below) |

`ABS_rawmap` is equivalent at the real boundary, not an untested path. The I01c
pgx driver wrapper (`internal/shell/app/drivers/postgres/postgres.go`
`mapError`) already maps SQLSTATE 23505 to `version_conflict` before the store
sees the error, so disabling the store's own mapping changes nothing observable
over the real driver. The store-level mapping exists for executors that do not
pass through that wrapper, and the `internal/shell/store/postgres` unit tests
(`absence_guard_test.go`) cover it with a fake that returns a raw SQLSTATE error.

The earlier attempt-5 catalog of 34 named mutants (`TA1`…`BDE2`) is superseded
by this gate and was not re-run. The one mutant it recorded as a survivor,
`TE5b` (the cleanup-intent branch of `planCleanup`), is the same code as
`CLI_intent` and is now killed by `TestEngineCommitCleanupIntentOnPostgreSQL`,
which drives that branch through `Engine.Commit`.

## Exact expectations and the loose-matcher allowlist

`RequireGoldenRow` states every declared column and compares it exactly: strings,
integers, booleans, nanosecond timestamps, exact bytes, typed NULL and canonical
JSON (key order ignored, numbers compared as exact decimals, never through
float64). jsonb is read as text so SQL NULL and the JSON value `null` stay
distinct. `GoldenMatcher` is a closed interface; only `pgtest` can construct a
matcher, and `applyGoldenRow` refuses a non-exact matcher on any column outside
its allowlist. The only non-exact comparisons left:

| matcher | allowed columns | why it may be loose |
|---|---|---|
| `GoldenDBSequence()` | `events.publish_sequence` | bigserial; rolled-back and losing statements consume values, so the exact number is not stable. Must be a positive integer. |
| `GoldenGeneratedID(prefix, reason)` | `id`, `*_id` | an identifier minted by product randomness the test cannot inject. A reason is mandatory. Not used in the current suite. |
| `GoldenOneOf(values...)` | any | a race winner's identity is one of N known values. Every candidate must itself be exact. |
| `GoldenRandomBytes(n)` | `token_nonce`, `nonce`, `wrap_nonce`, `wrapped_key`, `ciphertext` (bytea) | product cryptographic randomness. Exactly n bytes and not all zero, always paired with a behavioral round trip (token `Authenticate`, secret `Open`). |
| `ClockWindow.At(offset)` | timestamptz | a column written by a clock the test cannot inject. The first column binds the reading inside a bounded window; every later column must equal reading plus its exact offset. Not used in the current suite: the request tests read the seeded database times back and compare them exactly. |
| `GoldenJSONOf(v)` / `GoldenJSONSatisfies(desc, check)` | jsonb or bytea | exact canonical document, or named-field assertions through the product decoder for the admitted execution snapshot. |

Receipt content hashes are exact (`ContentSHA256` reproduces
`receipt.Fingerprint`), `yaml_sha256` is the SHA-256 of the exact admitted YAML,
`workspace_id` is `snapshotdoc.DeriveWorkspaceID`, and every replay compares the
whole replayed result with the first result with only `Replayed` set.

## Whole-row golden coverage (I02b attempt 4)

`RequireGoldenRow` reads every declared column from `store.SchemaDesign()` through
the runtime role (all tables the suite asserts are runtime-SELECT-able), and
fails when an expectation omits a column or names an undeclared one. NULL is a
distinct typed sentinel, so it never equals "", 0, false or a JSON object. The
component `TestMain` runs `GoldenCoverageError(nil)` after the suite unless a
`-run` filter was supplied, so a newly declared table cannot escape coverage.

| table | driven unit | whole-row test |
|---|---|---|
| tenants | tenancy CreateTenant | TestRealTenancyAdminOnPostgreSQL |
| projects | tenancy CreateProject | TestRealTenancyAdminOnPostgreSQL |
| memberships | tenancy GrantMembership | TestRealTenancyAdminOnPostgreSQL |
| services | service Create + token Issue | TestRealCredentialAdminOnPostgreSQL |
| resource_bindings | resource register | TestResourceRegisterCreateIfAbsentOnPostgreSQL |
| pipelines | pipeline Apply | TestPipelinePushTriggersPersistOnPostgreSQL |
| executions | execution Start/Activate | TestRequestActivationPersistsExactRowsOnPostgreSQL |
| actions | execution Start/Activate | TestRequestActivationPersistsExactRowsOnPostgreSQL |
| tasks | task Create/Edit | TestTaskCreateAndEditsPersistOnPostgreSQL |
| task_messages | task EditComment | TestTaskCreateAndEditsPersistOnPostgreSQL |
| events | unit event insert | TestGoldenEventAndAuditRowsOnPostgreSQL |
| audit | unit audit insert | TestGoldenEventAndAuditRowsOnPostgreSQL |
| receipts | receipt insert/incomplete/complete | TestGoldenReceiptRowsOnPostgreSQL |
| verifications | verify record | TestRealVerifierAndReporterOnPostgreSQL |
| secrets | secret management/seed | TestSecretLookupAdapterOnPostgreSQL |
| resources | resource register | TestResourceRegisterCreateIfAbsentOnPostgreSQL |
| cleanup | execution cleanup intent/update | TestGoldenCleanupRowsOnPostgreSQL |
| model_usage | gateway UsageRecord.Unit | TestGatewayUsageAndContentRowsOnPostgreSQL |
| content_records | gateway StoreContentRecorder | TestGatewayUsageAndContentRowsOnPostgreSQL |
| event_consumptions | trigger Complete | TestTriggersAdapterOnPostgreSQL |

### Observer guarantees

`ObserveUnit` snapshots id and a row hash of every declared table. `Finish`
fails when a changed row was not whole-row asserted (or proved absent) by the
same test, when the changed-row count of any table differs from the explicit
expectation, or when an unexpected table changed. An observer never shares
assertions with another observer, an assertion made by a different test is never
attributed, an observer that is never finished fails its test at cleanup, and a
duplicate row id in a snapshot is refused. The unit-type gate maps every
package-qualified `*Unit` struct under `internal/shell` to the states that prove
it, fails on a stale or unmapped entry, and requires every mapped state to be
observed.

### Derived unit×state matrix (I02b attempt 5)

The component `TestMain` renders this table from `DerivedUnitStates()` after the
full suite and refuses a README section that differs, so a new observed state or
a changed written table set cannot leave the documentation stale.

<!-- unit-states:begin (generated from DerivedUnitStates; do not edit) -->
| unit×state | changed tables |
|---|---|
| `activation.commit` | actions, audit, events, executions, receipts |
| `engine.commit` | actions, events, executions |
| `engine.commit.cleanup-intent` | actions, cleanup, events, executions |
| `execution.cancel` | actions, audit, cleanup, events, executions, receipts |
| `execution.outputs` | actions, audit, events, executions, receipts |
| `execution.settlement.failed` | actions, executions |
| `execution.settlement.pass` | actions, executions |
| `execution.start` | actions, executions |
| `execution.transition` | actions, executions |
| `gateway.content.record` | content_records |
| `gateway.usage.record` | model_usage |
| `identity.binding.delete` | audit, events, receipts, resource_bindings |
| `identity.binding.register` | audit, events, receipts, resource_bindings, resources |
| `identity.binding.register.conflict` | audit, events, receipts, resource_bindings, resources |
| `identity.binding.update` | audit, events, receipts, resource_bindings |
| `identity.credential.issue` | audit, events, receipts, services |
| `identity.credential.revoke` | audit, events, receipts, services |
| `identity.credential.rotate` | audit, events, receipts, services |
| `identity.service.create` | audit, events, receipts, services |
| `identity.service.disable` | audit, events, receipts, services |
| `identity.service.enable` | audit, events, receipts, services |
| `identity.service.update` | audit, events, receipts, services |
| `ingress.publish` | audit, events, receipts |
| `ingress.replay` | none |
| `membership.first-grant.guard-only` | none |
| `membership.first-grant.project` | memberships, receipts |
| `membership.first-grant.read-committed-commit` | memberships |
| `membership.first-grant.read-committed-rollback` | audit, events, memberships, receipts |
| `membership.first-grant.tenant-wide` | memberships, receipts |
| `membership.grant.read-committed-version` | memberships |
| `outbox.mark_published` | events |
| `pipeline.apply` | audit, events, pipelines, receipts |
| `pipeline.delete` | audit, events, pipelines, receipts |
| `pipeline.update` | audit, events, pipelines, receipts |
| `request.resolve` | actions, audit, cleanup, events, executions, receipts |
| `request.resolve.fail` | actions, audit, cleanup, events, executions, receipts |
| `request.resolve.pass.nonlast` | actions, audit, events, executions, receipts |
| `secret.delete` | audit, events, receipts, secrets |
| `secret.write` | audit, events, receipts, secrets |
| `task.ask` | actions, audit, cleanup, events, executions, receipts, tasks |
| `task.ask.loser` | none |
| `task.ask.replay` | none |
| `task.cancel` | audit, events, receipts, tasks |
| `task.cancel.loser` | none |
| `task.cancel.replay` | none |
| `task.create` | audit, events, receipts, tasks |
| `task.create.loser` | none |
| `task.create.replay` | none |
| `task.create_and_start` | actions, audit, events, executions, receipts, tasks |
| `task.create_and_start.loser` | none |
| `task.create_and_start.replay` | none |
| `task.edit.comment` | audit, receipts, task_messages, tasks |
| `task.edit.comment.loser` | none |
| `task.edit.comment.replay` | none |
| `task.edit.content` | audit, receipts, tasks |
| `task.edit.content.loser` | none |
| `task.edit.content.replay` | none |
| `task.progress` | tasks |
| `task.progress.stale` | none |
| `task.progress.wrong_execution` | none |
| `task.reply` | actions, audit, events, executions, receipts, task_messages, tasks |
| `task.reply.loser` | none |
| `task.reply.replay` | none |
| `task.select_pipeline` | audit, receipts, tasks |
| `task.select_pipeline.loser` | none |
| `task.select_pipeline.replay` | none |
| `task.start` | actions, audit, events, executions, receipts, tasks |
| `task.start.loser` | none |
| `task.start.replay` | none |
| `tenancy.membership.grant.existing` | audit, events, memberships, receipts |
| `tenancy.membership.revoke` | audit, events, memberships, receipts |
| `tenancy.project.create` | audit, events, projects, receipts |
| `tenancy.project.update` | audit, events, projects, receipts |
| `tenancy.tenant.create` | audit, events, receipts, tenants |
| `tenancy.tenant.key.rotate` | audit, events, receipts, tenants |
| `trigger.complete` | event_consumptions |
| `trigger.select` | audit, events, receipts |
| `trigger.started` | actions, audit, events, executions, receipts |
| `trigger.started.replay` | none |
| `verification.record` | verifications |
| `verification.record.with-intent` | audit, events, receipts, verifications |
| `verification.report` | audit, events, receipts |
<!-- unit-states:end -->

### List/page units

Paged units covered with ≥3 rows, limit 2, cursor follow and no skip/repeat:
memberships, bindings, task actions, outbox, event read, scanner, controller
scan, inbox, triggers pending. There is no product list unit for tasks,
services, tenants, projects or credentials in this card's adapters (only
single-row reads), and task messages are returned as one bounded page with no
cursor, so no ordered multi-page test can be written for them; inbox covers the
paged task projection.

### Migrations

The migration suite exercises the 0008 receipt-completion backfill
(`completed_at = created_at` only when `result IS NOT NULL`), the FORCE RLS
bracket restore, the 0008 delta column additions, and the S1 verification
column drop over populated legacy rows. Constraint validation statements are
exercised by the forward migration over the populated shard.
