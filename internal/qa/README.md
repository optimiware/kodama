# `internal/qa` — exploratory QA evidence to defect work items

Task T-040. Maps the deployment-bound observations of an AI exploratory QA attempt to
**ordinary defect work items** with durable artifact links. Implements the mapping and
authority half of [delivery and quality gates](../../docs/specs/delivery-and-quality-gates.md)
(DQ-06, DQ-10), [agent runtime](../../docs/specs/agent-runtime.md) (AR-05, AR-12) and
[work management](../../docs/specs/work-management.md) (WM-01).

Status: implemented and tested against real PostgreSQL and S3 (real execution, artifact,
delivery and work stores and the real adapters). Only the browser/model (the test writes the
report and the screenshots a QA attempt would upload) and fault injection at owner boundaries
are controlled. Nothing registers this service yet (T-072). **Component completion does not
qualify exploratory QA:** T-069 owns the real browser image, profile and qualification and
repeats the integrated evidence checks; T-046 consumes the mapping from its journeys.

There is no separate findings service, table, route, migration or event: a finding is a work
item of kind `defect`, an artifact link is an `evidence_links` entry, retention protection is an
`artifact_pins` row, and the defects are attributed in work history and audit to the configured
mapping principal (an identity-module service principal), with the QA attempt named in the
specification's provenance.

## Layout

| Path | Purpose |
| --- | --- |
| `values.go`, `errors.go` | Bounds, severities, evidence kinds; failure classes (`ErrWrongDeployment`, `ErrEvidence`, `ErrContext`, …) |
| `report.go`, `schema.go`, `schema.json` | The structured report contract: strict `Decode`, canonical form and digest, `Observation.Fingerprint`, embedded JSON Schema |
| `key.go` | Deterministic `DefectKey` and `DefectIDFor` |
| `ports.go` | Consumer-owned ports: `Attempts`, `Deployments`, `Artifacts`, `Defects` |
| `service.go`, `mapping.go` | `Service.Record`, the only operation: verify, then pin and file |
| `adapters/` | Execution, delivery, artifact and work adapters; `WorkGuards` for the QA work store |
| `qatest/` | Component fixture: real services, a QA attempt with uploads, fault injection |
| `testdata/` | `report.json`, `report_clean.json`, `cases.json` (contract cases for T-069/T-046) |
| `integration/` | Component tests over real PostgreSQL and S3 (resource prefix `t040`) |

The root package imports only the standard library, `internal/domain` and the runtime
contract; peer modules appear only in `adapters/` and the test fixture. `boundaries_test.go`
enforces it, and that nothing in the module imports the gate or identity decision machinery.

## Model

A QA step is an ordinary run step that declares an `environment` (so the immutable
environment/configuration pin is in its context), a declared artifact output for the report
(`Config.ReportOutput`), and artifact outputs or diagnostics for screenshots. When the attempt
has an accepted completed result, a control-plane caller issues `Record(scope, attempt)`:

1. **Authorize the caller** before any read: only the trusted control plane (`system`).
   Mapping is a post-completion control-plane operation, not a runtime tool action: a finished
   attempt holds no runtime authority and the runtime action set has no mapping action, so a
   `workload` (the attempt's own included), a person (even a reviewer) and an operator are
   denied: people use the work API, and humans decide at the gate. Missing attribution is denied.
2. **Trusted facts**: the attempt's owner, its pinned context (verified against the stored
   digest, scope and identity) and its accepted result come from the execution store. A foreign
   attempt is `not found`. The attempt must have been pinned to a work item **and** an
   environment, and have an accepted `completed` result (`ErrNotReady`, `ErrContext`).
3. **Report**: the declared output's artifact must be a usable file produced by this attempt;
   it is read through the artifact module (size and digest re-verified) and strictly decoded
   (unknown, misspelled, duplicate and omitted fields, floats and trailing data are errors, so a
   report cannot smuggle an `approved` field).
4. **Pinned acceptance context** (AR-05): the report must judge the pinned work item at the
   pinned content revision and cite only criteria the pinned context holds (`ErrContext`); the
   work item under test must exist in the scope and not be cancelled.
5. **Deployment** (DQ-10): the report states the deployment, environment, configuration digest,
   candidate and image. The deployment module's record in **this scope** must be a *succeeded*
   deployment of exactly the pinned environment and configuration for exactly that candidate and
   image, **and it must be the deployment the attempt's pinned input names**
   (`Config.DeploymentInput`, read from the immutable pinned context, a string): a report cannot
   choose its deployment, so another succeeded deployment of the same environment and
   configuration (for example of another candidate) cannot be substituted. An unknown, foreign (other project or tenant), unsettled, failed or mismatching
   deployment is one refusal, `ErrWrongDeployment`, indistinguishable from each other.
6. **Evidence** (DQ-01): every cited artifact must be a usable file of the QA attempt itself
   (tenant, project, run, attempt), finalized by its accepted result (a declared artifact output
   or a listed diagnostic), with exactly the stated digest; a `screenshot` must be an image
   (`ErrEvidence`; absent, foreign and unusable are one answer).
7. **Only then write**: for each observation, pin the report and its evidence to the defect
   identity, then create-or-observe the defect, then add any link the existing defect lacks. A
   refused report leaves no defect and no pin.

### Deterministic deduplication

`DefectKey` = SHA-256 over the canonical `{tenant, project, work item under test, candidate
commit, finding fingerprint}`; the defect's work item ID is derived from it (`wi_qa_<32 hex>`)
and is the primary key, so one finding on one candidate is at most one row, whoever calls and
however often. The fingerprint is the violated criterion, the normalized route (no trailing
slash) and the normalized title (case and whitespace folded); severity, steps, wording of
expected/actual and evidence do not change it. The key does not depend on the attempt, the
report artifact or the clock. A **different candidate** is a different key, so a regression is
never hidden behind an old closed defect.

**Immutable payload.** The key says which finding this is; the *payload digest*
(`Observation.PayloadDigest`: severity, normalized route, criterion, normalized title, steps,
expected, actual; evidence excluded) says what was claimed. It is written as the stable line
``- Finding payload digest: `<hex>` `` in the defect specification at creation, i.e. into the
immutable creation record of the work history. Before anything is pinned and again in `Open`, an
existing defect's creation entry (not the editable current specification) is compared with the
new report's digest: a different payload under the same key is `ErrConflict` and writes nothing
(no defect, evidence link, pin or event), while a retry of the same payload, even with new
evidence, is observed and merged. A defect without the marker is a conflict too.

`Open` is create-or-observe: any creation failure is followed by a reread, so a concurrent
creator and a commit whose acknowledgment was lost end as "exists" (`Created=false`), while a
rolled-back write leaves nothing (`ErrUnavailable`) and the retry creates it. Observing a
defect again (a second pass with new screenshots) appends the links it lacks as one ordinary
evidence-link edit attributed to the new attempt (bounded by the work item's 50-link limit,
optimistic retry on version conflict); a `done`/`cancelled` defect is observed and never edited.
Every write is one work-module transaction: state, history, audit and `work_item.created`/
`work_item.updated` outbox event commit together or not at all (WM-08).

### Provenance and evidence links

The defect specification names the QA attempt/run/step, the work item and content revision
judged, the deployment, environment, configuration digest, candidate commit and image digest
inspected, the report artifact and its digests, and the deduplication key. Its links are the
report (`QA report`) followed by each cited artifact (`screenshot: …`, `log: …`). Each linked
artifact is pinned (`artifact_pins`, owner kind `qa_defect`, owner = defect ID) before the defect
exists, so retention cannot delete evidence a defect refers to; pins are idempotent and a crash
between pin and create leaves only a pin the retry reuses.

### Authority (DQ-06, AR-05, AR-12, WM-07)

* `Service` has one method, `Record`; the `Defects` port is `Subject`, `Verify`, `Open`,
  `Attach`. There is no approve, complete, transition, assign or block operation to call.
* Who may ask: only a `system` caller (see step 1). Who files: the **mapping principal** given
  to `adapters.NewWork` (a verified identity-module principal bound in the project, for example
  as analyst), whose authority is the work module's own current decision through the injected
  `work.Authority`, narrowed by the adapter to `work_item:read`, `create` and `edit_spec`
  (evidence links). The adapter checks it before reading or writing anything (`Subject`,
  `Verify`, `Open`, `Attach`), so a missing or revoked authority is `ErrDenied` before the first
  pin. Nothing builds a permission from the attempt or the report.
* Defects are written by a work store built with `adapters.WorkGuards(artifacts, txPermissions)`:
  its transaction-bound permission check refuses a workload actor and every permission outside
  read/create/edit-spec, then asks the injected owner checker (identity's `TxPermissions`)
  inside the mutating transaction, so a binding revoked while the mutation waited is seen;
  without a checker every mutation fails closed. Its writer and dependency guards are unset
  (every transition fails closed); its evidence guard accepts only usable artifacts of the
  scope and refuses commits and review subjects. The tests run all-permissive upstream
  authorities against this store and still cannot move any item.
* The standalone identity authority and the gate/work bridges give a workload no
  `gate:decide`/`work_item:complete` and resolve no actor for an attempt ID.
* The report is a claim, never authority: tenant, project, attempt and environment come from the
  attempt's records; the report cannot select them.

## Tests

| Test | Covers |
| --- | --- |
| `Test_qa_wrong_deployment` | DQ-06, DQ-10, AR-05, WM-01: a report naming another succeeded deployment (another candidate) than the pinned input, unknown, other-project, other-tenant, other-environment, edited-configuration, candidate/image mismatch, failed and unsettled deployments |
| `Test_qa_defect_retry` | DQ-10, WM-01, WM-08: changed payload under one key conflicts without mutation, replay, lost acknowledgment, rolled-back commit, failed event append, partial failure, concurrency, second pass evidence merge, closed defect |
| `Test_qa_evidence_links` | DQ-01, DQ-10: links, provenance, pins and retention, foreign/other-attempt/unfinalized/wrong-digest/non-image evidence, store guard |
| `Test_qa_mapping_authority` | AR-05: workload/unattributed/person callers denied before any pin or write, runtimeauth gives the finished attempt no mapping action, revoked or read-only mapping authority files nothing, tx-level recheck, store without a permission guard fails closed |
| `Test_qa_cannot_approve` | DQ-06, AR-12, WM-07: identity and bridge denial, forged decision fields, caller types, all-permissive authority, untouched subject and events |
| `Test_qa_tenant_isolation` | WM-01: RLS, scoped lookups, foreign-scope mapping |
| `TestReportContractFixtures`, `TestRecord*`, `TestModuleBoundaries` | contract, use-case state machine with in-memory ports, dependency direction (no services) |

```sh
source .harness/test-env.sh   # real PostgreSQL, S3, … (required: missing services fail, never skip)
go test -race -count=1 ./internal/qa/...
go test -tags unit -race -count=1 ./internal/qa/...   # infrastructure-free layer
```

## For T-069, T-046 and the integration owner (T-072)

1. **Construct** (after the artifact, execution, delivery and work contributions):

   ```go
   txPerms := func(tx pgx.Tx) work.PermissionChecker { return identityconsumer.NewTxPermissions(tx) }
   qaWork, _ := workpg.NewStore(runtimePool, eventStore, adapters.WorkGuards(artifactStore, txPerms)) // a second work.Store instance
   attempts, _ := adapters.NewAttempts(execStore)
   deploys, _ := adapters.NewDeployments(deliveryStore)
   arts, _ := adapters.NewArtifacts(artifactStore, objectStore, clock)
   defects, _ := adapters.NewWork(qaWork, workAuthority, "<mapping principal id>", clock)
   svc, _ := qa.NewService(qa.Config{Attempts: attempts, Deployments: deploys, Artifacts: arts, Defects: defects,
       ReportOutput: "<declared output>", DeploymentInput: "<pinned input naming the deployment>"})
   ```

   `workAuthority` is the identity-backed `work.Authority` (`identityconsumer.NewWorkAuthority`).
   The mapping principal is a user-typed identity principal bound in each project with a role that
   holds read, create and edit-spec work (the fixture binds `qatest.Mapper` as analyst; the
   adapter narrows it further); revoking its binding stops all mapping. The QA work store is a
   *second* work store: only the QA store carries the narrowing guard. The pipeline that admits
   the QA step must pin the deployment ID as a string input named `DeploymentInput`. No
   migration, route or permission is added.
2. **Trigger**: call `svc.Record` with `qa.Caller{Actor: {Type: qa.ActorSystem, …}}` after the QA
   step's attempt is accepted (an event consumer plus a periodic scan of completed QA attempts is
   the integration owner's choice; the call is idempotent and may be repeated at any time).
   Which steps are QA steps, and the declared output name, are T-069's profile/pipeline contract.
3. **Fixtures**: `schema.json` (embedded as `qa.Schema`), `testdata/` (valid reports and negative
   cases with expected Go/schema verdicts) and `qatest` (real-service fixture with
   `Deploy`, `StartQA`, `Upload`, `Complete`, `Command`, fault injectors) are the shared evidence.
4. T-069 repeats these checks with the real browser image/profile; the `qa_*` names here remain
   the mapping/authority acceptance.

## Assumptions to confirm

* QA findings are mapped **after** the attempt's accepted completed result by the control plane,
  not by a runtime callback of the running agent (runtime actions are a closed set owned by
  T-019; no `qa.*` action exists). A `workload` caller is therefore refused; admitting one later
  would need an owner-verified runtimeauth mapping grant, which does not exist.
* The mapping principal is a service principal modelled as an identity user binding (the
  standalone authority grants roles to users only), so defects show a `user` actor in history;
  the QA attempt is named in the defect's provenance. A dedicated service-principal type in the
  identity module would be a cross-module change (not made here).
* The deployment under inspection is named by a pinned run input (`Config.DeploymentInput`);
  the runtime v1 envelope has no QA-specific field, so T-069/T-046 must pin it.
* Screenshots and other evidence are declared artifact outputs or listed diagnostics of the QA
  attempt; the report is the declared output named by `Config.ReportOutput` (installation
  configuration, like the conformance report mapping).
* The dedup key binds the candidate commit; deployment identity, configuration and attempt are
  provenance, not identity.
* A defect's relationship to the work item under test is recorded in its specification (the work
  module has no typed relation besides epic parents); a typed link would be a work-module change.
* Evidence verification inside the work transaction reads the artifact store through its own
  connection (a separate pool slot); the runtime pool must have headroom for one extra
  connection per in-flight defect write.
* Pins taken for a report whose defect creation later fails remain until the retry (or are
  never released if no retry happens): there is no automatic unpin, as for gate pins.

## Not here

The real browser image, profile and qualification (T-069), journeys (T-046), any approval or
completion of work, quorums or overrides, a findings service or resource, HTTP routes, a
scheduler, and composition (T-072).
