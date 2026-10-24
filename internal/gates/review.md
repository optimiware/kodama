Current immutable baseline: documentation-only verification refresh of the existing
component at `13eea7734b35ef91aeaba694af368629b34a502e`. No production candidate, tenant or
deployment is selected, and no production code, schema, runtime contract or dependency
changed in this refresh. Earlier recorded baselines (including `9e73507`, the pre-rebase
expiry-after-wait documentation commit) are historical.

Baseline (historical): expiry-after-wait correction on the component at
`9a763b8f06e52e420f2c918eeb9e654063df9a37`, on top of the amendment of the implementation at
`e0a7510c5def50f9e1d7a260ec2bf372a828091d` (earlier history: repository
`673a910af918b6113451c65bdd0454f274ec6545`, rebased onto
`cacf2eab5a3593e5adea5f8fe36e5d40b9d2f080`). No production tenant or candidate is
selected by this maintenance task. Production records and tests carry tenant/project,
work item, run/step and candidate ownership.

Qualification ownership: T-061 qualifies hosted authority, T-073 the real gate
transaction/authorization join, and T-072 activates the selected composition. The
results below are component evidence, not executable-assembly evidence.

Requirements: DQ-01–DQ-06, DQ-11, DQ-14, PX-13, SD-08; supporting PX-05/PX-09,
IT-02–IT-05/IT-10/IT-11, SD-01–SD-07. Decisions C-01/C-02/C-05/C-11/C-13/C-16/C-17,
P-01/P-02/P-04/P-05/P-08/P-15/P-16; ADRs 0001, 0002, 0006, 0007. This task neither
resolves O-18 nor claims hosted authorization qualification, and adds no approval
policy, override resource, candidate aggregate or agent approval authority.

## Assumptions to confirm

1. **Gate deadline.** The contract defines no gate duration. `Options.GateTTL` must be
   injected positive (no default) and is snapshotted once; execution timeouts are not
   reused. Integration owner confirms the configured value.
2. **Deployment integration.** The deployment module (T-035) is implemented, but gates
   consume it only through their own small snapshot/revalidation port and transaction-bound
   `DeploymentBinder`; a deployment-bearing gate fails closed without them; source-only gates
   keep explicit null deployment fields. The component fixture is a controlled seam, not
   evidence of the real joined path. The integration owner confirms the concrete binding and
   its transaction/locking semantics (T-073) before activation.
3. **Conformance artifact identification.** `adapters.ConformanceMapping.ReportOutput`
   is injected installation configuration; baseline and required checks come from the
   producing attempt's immutable context. Missing or ambiguous identification blocks
   approval. T-031 and the integration owner confirm the output name.
4. **Gate read permission.** Reads map to `identity.PermEvidenceRead`, decisions to
   `identity.PermGateDecide` (adapter plus HTTP route rules). Review confirms.
5. **Writer evidence provenance.** Artifact provenance records the producer's input
   commit; for writer-produced evidence the producer's accepted result/publication is
   additionally checked against the reviewed candidate. Review confirms against T-015/T-020.

6. **Clock.** The final expiry sample uses the gate store's injected clock after authority
   waits, not the database commit time. Integration supplies one consistent clock to execution
   and gates and confirms the wiring.

## Requirement → test map

| Test | Location | Covers | Dependencies |
| --- | --- | --- | --- |
| `canonical_subject` | `subject_test.go` `TestCanonicalSubject` | DQ-01, DQ-02 | none |
| `concurrent_decision` | `integration/decision_test.go` `TestConcurrentDecision` | DQ-03, DQ-05, SD-04 | PostgreSQL |
| `changed_spec_source_config` | `integration/applicability_test.go` `TestChangedSpecSourceConfig`, `git_test.go` `TestChangedSpecSourceConfigGit` | DQ-04, DQ-11 | PostgreSQL, HTTPS Git, deployment fixture |
| `reviewer_revoked` | `integration/authority_test.go` `TestReviewerRevoked` | DQ-06, SD-05 | PostgreSQL |
| `gate_restart_no_job` | `integration/execution_test.go` `TestGateRestartNoJob` | PX-13, PX-09 | PostgreSQL, recording executor |
| `conformance_required` | `integration/conformance_test.go` `TestConformanceRequired` | DQ-14, SD-08 | PostgreSQL, S3 |
| `conformance_stale_or_blocked` | `integration/conformance_test.go` `TestConformanceStaleOrBlocked` | DQ-14, SD-08, DQ-A07 | PostgreSQL, S3 |
| `tenant_isolation` | `postgres/isolation_test.go` `TestTenantIsolation` | SD-01–SD-03 | PostgreSQL |
| `atomic_gate_effects` | `postgres/atomicity_test.go` `TestAtomicGateEffects` | SD-06, IT-10 | PostgreSQL |
| `gate_recovery_events` | `integration/recovery_test.go` `TestGateRecoveryEvents` | IT-11, PX-13 | PostgreSQL, NATS |
| `evidence_failure_recovery` | `integration/evidence_test.go` `TestEvidenceFailureRecovery` | DQ-14, SD-07 | PostgreSQL, S3 |
| `gate_transition_denials` | `execution_test.go` `TestGateTransitionDenials` | PX-13, DQ-04 | none |
| `gate_http_contract` | `http/contract_test.go` `TestGateHTTPContract` | HTTP contract, IT-02–IT-05 | PostgreSQL |
| `gate_expiry_after_lock_wait` | `postgres/expiry_test.go` `TestGateExpiryAfterLockWait` | DQ-04, PX-13, SD-04 | PostgreSQL |
| `gate_claim_lock_order` | `postgres/lock_order_test.go` `TestGateClaimLockOrder` | PX-05, PX-13, DQ-04, SD-04 | PostgreSQL |

## Implementation amendment (historical; retained in the current baseline)

Retained from the earlier implementation: the canonical `Subject`, explicit
`pending → approved|rejected` decisions that are immutable, separate invalidation and
derived expiry, rejection requiring a new run/review, the prohibition on agent approval,
the durable gate-step integration in `internal/execution/gate-step.go` (unchanged), the
persistence/migration and the HTTP contribution.

New in that amendment:

* `Gate.CheckCurrent` compares the current deployment's owning scope and recorded candidate
  as well as its identity and configuration digest, so a snapshot with foreign ownership or a
  wrong candidate yields the definitive `deployment_changed` (also at the transaction-bound
  decision, consumption and protected-claim checks). An outage stays unavailable and
  fabricates no invalidation.
* Conformance evidence identity: the report's and every cited supporting artifact's metadata
  ID must equal the requested ID (`conformance_evidence_invalid` otherwise); a substituted
  artifact is never bound or pinned as the requested evidence.
* Documentation: `BindAuthority` is the standalone transaction adapter, not hosted
  qualification (T-061 qualifies hosted authority, T-073 the real transaction join, and
  T-072 activates); transaction ownership, replay and
  lock-order coverage are listed in the README. No table, column or migration changed.

Failure-path coverage added in that amendment (tests only, no production change):

* `postgres/atomicity_test.go` `TestAtomicGateEffects`: an injected execution-event failure
  during `Store.CompleteStep` and `Store.RejectStep` rolls back the step/run mutations,
  history, audit and execution outbox while the immutable approval/rejection survives; no
  gate attempt, ordinal or reservation appears; after the fault is removed one retry makes
  exactly one transition and later retries change nothing. The completion case uses a gate
  that is the last step, because a non-final gate completion emits no execution event.
* `integration/recovery_test.go` `TestGateRecoveryEvents`: the decision consumer's transaction
  fails twice after the step/run mutations; each failed attempt leaves no step progression,
  history or completed inbox receipt (checked, scoped to the event and consumer, before the
  next attempt) and the decision stays available; the redelivery then completes exactly one
  transition with zero gate attempts or reservations, and reconciliation finds nothing more.

New regressions: `TestDeploymentApplicabilityOwnership` (`execution_test.go`),
`integration/applicability_test.go` ownership subtests (decision, consumption, protected
claim) and `integration/conformance_test.go` `TestConformanceEvidenceIdentitySubstitution`.

### Expiry-after-wait correction (DQ-04)

Defect: `checkConsumable` received `env.Now()` sampled before it took the gate lock and
before the blocking reviewer-authority read, so an approval could expire during either wait
and still authorize completion or a protected claim. Correction (`postgres/mutations.go`,
`postgres/execution_store.go`): `checkConsumable` no longer takes a time; it samples the
injected clock after the gate and facts are read, and `recheckExpiry` (a final
`Gate.Applicable` with a fresh sample) runs after every authority wait and immediately before
progression is accepted. A protected claim rechecks all of its protecting gates at the end.
Domain rules, lock order, error mappings, schema and `internal/execution/ports.go` are
unchanged.

Regression: `postgres/expiry_test.go` `TestGateExpiryAfterLockWait` (real PostgreSQL;
deterministic lock holds observed through `pg_locks`; injected clock). Cases: completion
expiring during a gate-lock wait and during a reviewer-authorization wait; the
reconciliation/wakeup path (`ProcessGate`); a protected claim expiring during an authority
wait; two protecting gates where the later gate's authority wait crosses the earlier gate's
deadline; and unexpired controls for each. Denials are non-definitive expiry, leave the gate
`approved` with unchanged version/audit/outbox, no step transition, attempt ordinal,
reservation or writer slot, and repeated attempts do not renew the approval. The suite fails
against the previous code (verified by restoring it) and passes with the correction.

Earlier results recorded in this file before the amendment are historical and are not
validation of this baseline.

## Verification run

Historical results (recorded at `9a763b8`; not validation of the current baseline): the
expiry regression and full gates suite passed, and the unit-layer check failed only in
`kodama/internal/kube` `Test_dispatch_create_response_loss` (`missing_pinned_fixture`), reported
for integration-owner triage and not waived. An earlier T-024 observation-suite failure
(`Test_cursor_expiry/retention_loss_midstream`) also remains an integration-owner triage item.

Current baseline `13eea77` (component evidence only; hosted authority is T-061, the real
joined transaction T-073, activation T-072). The owned gates paths at `13eea77` are
byte-identical to those at `1e93d7d` (`git diff 1e93d7d HEAD -- internal/gates
internal/execution/gate-step.go api/gates.yaml` is empty), a rebased predecessor of this
baseline.

| Command | Result |
| --- | --- |
| `python3 scripts/harness/resources.py --resources go,services -- go test -race -count=1 -v ./internal/gates/...` | all five packages passed; all seven named acceptance tests (`TestCanonicalSubject`, `TestConcurrentDecision`, `TestChangedSpecSourceConfig` with `TestChangedSpecSourceConfigGit`, `TestReviewerRevoked`, `TestGateRestartNoJob`, `TestConformanceRequired`, `TestConformanceStaleOrBlocked`) and `TestGateExpiryAfterLockWait`, `TestGateClaimLockOrder`, `TestTenantIsolation`, `TestAtomicGateEffects`, `TestGateRecoveryEvents`, `TestEvidenceFailureRecovery`, `TestConformanceEvidenceIdentitySubstitution`, `TestGateHTTPContract` passed; only `TestGenerateCanonicalFixture` skipped by design |
| `make check-task TASK=T-034` (unit layer, non-qualifying) | passed; receipt `20261021T124946-a6fdc7cb7e384288a8f55d0e24c44af9.json` |

Earlier receipts, recorded under their own candidates and not as verification of `13eea77`:

| Receipt | Layer | Candidate | Result |
| --- | --- | --- | --- |
| `20261021T122141-d250448…` | unit, non-qualifying | `9e73507` | passed |
| `20261021T122628-dd40b83…` | all, qualifying | `1e93d7d` | passed; seven named acceptance tests |

The complete-layer receipt is evidence of one passing run at its candidate, not proof that
earlier intermittent failures (the `internal/kube` observation above, and failures
recorded in earlier non-passing complete-layer receipts for other packages) were repaired.
A fresh `LAYER=all` receipt for the final candidate remains the coordinator's qualification.
