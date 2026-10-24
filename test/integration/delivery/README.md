# test/integration/delivery — deployment apply/reconcile/cleanup qualification (T-067)

Resource prefix `t067`. Acceptance checks `apply_ack_loss`, `deployment_cleanup_join`,
`deployment_provenance_join`, `duplicate_deployment_key`, `wrong_image_source`,
`cleanup_after_work_done`, `cleanup_scope` and `cleanup_duplicate_claim`
(DQ-01, DQ-07–DQ-10, DQ-15/DQ-A11, PX-06/PX-07), run by the same-named `Test_…` functions. The
environment/credential pin checks transferred from T-077 are in
[`../environment-pinning-runtime`](../environment-pinning-runtime/README.md).

```bash
make check-go-integration PKGS="./test/integration/delivery/... ./test/integration/environment-pinning-runtime/..."
# or, against an existing scope (host-env.json loaded):
go test -race -count=1 ./test/integration/delivery/...
```

Nothing skips: a missing cluster (`dev/.state/environments/<scope>/kubeconfig` or
`KODAMA_TEST_KUBECONFIG`), container provider, scope registry (`KODAMA_REGISTRY_URL`) or failed
image build **fails** the test. `KODAMA_TOOLS_IMAGE` names a prebuilt local image and skips the build.
The `cluster` stack is required; `services` has no kind cluster. `git` with `git-http-backend` is required on the host.

## The joined path

An **admitted** step runs. Execution dispatches the persisted step — its image digest, command, environment
and pinned context — through the **real Podman runtime** (`internal/runtime/podman`) as a container
whose entrypoint is the real `kodama step`. The step entrypoint is admitted by runtime admission against
the verified container, reads its pinned context, **checks out the admitted source commit** from a real Git
repository, **resolves the environment's credential through the real secrets resolver** (the pinned version,
audited as `secret.resolve`) into `$KODAMA_SECRETS_DIR/KUBECONFIG`, runs the declared script from the tools
image (`apply.sh` / `delete.sh`) against the real kind cluster under the ordinary-step restrictions, and
submits the script's result manifest, which execution accepts against the step's declared outputs. The tests
assert on what the PostgreSQL record, the cluster and the accepted results say; nothing is a runner-side
copy of an input.

| Real | Notes |
| --- | --- |
| PostgreSQL | a database per stack migrated with the whole delivery manifest; every store is used through the non-owner, non-`BYPASSRLS` runtime role under forced row-level security. The owner role only inspects |
| The assembled modules | `stack/`: execution (store **with the transaction-bound environment binder**), secrets (encrypted store, resolver, runtime adapter), pipeline definitions, artifacts, runtime authority (attempt JWTs, grants), delivery store and service, T-077's adapters, T-066's cleanup bindings and expiry scheduler |
| Dispatch | `execution.Service.DispatchNext` / `ReconcileTenant` with the Podman runtime as `Dependencies.Runtime`: dispatch, observation, termination, settlement, retry and removal are execution's |
| The entrypoint | `go build ./test/integration/runtime/stepbin` for the Podman machine's Linux, mounted by the runtime's own `Entrypoint` program |
| The runtime API | runtime admission and context (`internal/runtimeauth/http`), the delivery routes (`internal/delivery/http` on the real router, in front of the real `delivery.Service`) and secret resolution (the real `secrets.Resolver`) behind TLS on the host |
| The tools image and scripts | `images/deployment-tools` built by `build.sh`, pushed to the scope registry, pulled by the Podman runtime and by the kubelet by the same digest, run as uid 65532, all capabilities dropped, no-new-privileges, read-only root |
| The image identity | registered through the real `oci.Verifier` fetching the digest-addressed manifest from the scope registry |
| Source | a real bare Git repository served over TLS by `git-http-backend`; each flow commits its `deploy/` directory and the workspace points at it, so the verified candidate is what the container checks out |
| Kubernetes | the task's kind cluster; each test owns a namespace under restricted Pod Security and a namespace-scoped ServiceAccount whose token is the project's kubeconfig secret |
| Time | the fixtures' controllable clock: a preview "expires" by advancing it past the environment's TTL |

### Controlled (and why)

| Controlled | Why |
| --- | --- |
| The result route is the T-086 stand-in (runtime authorization, then `execution.CommitResult`) | T-022 owns the production route with its source and artifact handling; the manifest, declared outputs and acceptance are real |
| The secret route is a thin handler over the real resolver | `internal/app` marks `runtime-secret-resolution` disabled; the resolver, adapter, store, pins and audit are real |
| The Podman runtime's `Entrypoint` program is wrapped | it refuses an attempt that consumes the workspace because no checkout credential exists outside Kubernetes: the repository the tests serve is anonymous, so the refusal is lifted for the checkout; files mounted into the container are made readable by the image's non-root user (rootless Podman); `/workspace` is `tmpfs:U` (the Pod recipe's `fsGroup`; the inherited M-4 of T-016/T-021: Git refuses a directory another user owns) |
| Host networking | the kind API server and the runtime API both listen on loopback |
| `Pins` edits (`stack.EditPins`) | explicitly identified fault injection of an absent/foreign/mismatched pin at the pinned-context reader boundary |
| `DeploymentEffects` (`stack/effects.go`) | a thin recording/withholding decorator around the delivery module's own reconciler (`delivery/adapters.NewEffects`, built over the real deployment store and the stack's real cleanup bindings). Every affirmative verdict is the adapter's; the decorator only records queries and can answer "still unknown" to model unreachable evidence |
| API-level attempts (`stack.DirectAttempt`) | the registration-concurrency, changed-request, schema and promotion cases call the delivery service as an attempt that was claimed but **never launched**; no command ran, so their retry is truthfully a not-started one |
| The record's outcome marked unknown (`markUnknown`) | the script reports `unknown` itself only for an apply it cannot vouch for; the two tests that need it set it as the system actor, as fault injection of the record's state |
| `FakeSources` (workspace history/tip) and the build step | the build "ran" elsewhere: its attempt is claimed and its result committed with the registered image, the source is the served repository |

### Recovery is driven by the runtime's own evidence

Faults sit in front of every runtime route: a request is lost before it is processed, or its response after the
state change committed, and either may kill the calling container. Two kinds of death matter:

* **vanish** — the container is killed *and removed* before the control plane ever observed it running.
  The runtime cannot say whether the command ran (launch *unknown*): execution fails the attempt
  `outcome_unknown`, holds it **unsettled** and asks the effect adapter. The tests assert that, with no
  affirmative evidence, no number of passes or hours starts another attempt; then the adapter (`delivery/adapters.Effects`) derives a
  *replay-safe* verdict from the deployment record, execution records that evidence, settles the attempt,
  schedules the step's retry, and the next container runs the same admitted step. Nothing is marked
  `not_started` that already executed.
* **crash** — the control plane saw the command start, then the container died. The runtime's evidence is
  *started*; execution never retries a started attempt (nothing the record says can show the effect did
  not happen), so the step and the run fail and the record stays `running/applying`.

## Acceptance map

| Test | What it proves |
| --- | --- |
| `Test_apply_ack_loss` | (a) the intent's response is lost and the container vanishes: nothing was applied, the record is durable `running/applying` with the intended objects; uncertain-effect blocking (above); after the record's evidence the retry settles the **same** deployment once; (b) the success report's *request* is lost: the retry settles by annotation and the live uids are unchanged; (c) the success *response* is lost after commit: the retry only verifies, posts no outcome and re-applies nothing; (d) one lost response is absorbed by the script's own idempotent retry; (e) an unknown outcome is settled by observing the recorded objects — a replica count a re-apply would reset survives; (f) unknown with nothing present fails `apply_not_observed` and applies nothing; (g) a crash after the control plane saw the start is never retried and leaves the record visible (and a sweep reports it `not_settled`); (h) kubectl prints `created` for a workload that can never be scheduled: the step fails `rollout_timeout`, the record is `failed`, nothing is recorded ready |
| `Test_deployment_provenance_join` | admission → persisted snapshot → runtime context → delivery → apply. After admission the environment (namespace, configuration) is edited and the credential **rotated to a value that opens no cluster**: the deployment still succeeds because the entrypoint resolved the *pinned* version 1 (audited; v2 never), and the record, receipt, live annotations, outputs and accepted result carry the admitted configuration; outputs equal the record; the row joins producer attempt, tool image, artifact, repository and commit; the live workload runs the registered digest. Absent, unretained, environment-less, wrong-version, binding-less and foreign-run pins each fail closed with no record and no object. A revoked pinned credential fails the step before the cluster (`secret_unavailable`, no record call, no object). Cleanup after the environment moved on and the credential was replaced pins the *recorded* snapshot and credential and deletes in the recorded namespace; a revoked retained credential refuses cleanup admission and changes nothing |
| `Test_duplicate_deployment_key` | eight concurrent registrations create one deployment (one history receipt, one event); a restart, a not-started retry attempt and the human route observe it; changed digest or source is refused; the schema's unique action key refuses a second row for the application role; a promotion of the same image is its own action. Replay of a *settled* action by the real script is `Test_apply_ack_loss` (c) |
| `Test_wrong_image_source` | an image built from another repository/commit, a claimed source that is not the admitted candidate, an image that is registered but not an accepted output, the image of a failed build, another digest, and a manifest running another image are each refused by the admitted step's real script with **no** deployment row and no live object; the verified inputs then deploy |
| `Test_deployment_cleanup_join` | a preview expires by the controlled clock; the scheduler requests and admits the derived cleanup run; the admitted cleanup step runs in Podman and its real `delete.sh` deletes exactly the recorded objects through the retained credential (audited resolution of the pinned version); the record completes `deletedAt` keeping its outcome, objects and history; a lost completion acknowledgment (vanished container) blocks, then converges without another deleter |
| `Test_cleanup_after_work_done` | work completed or cancelled: cleanup is still admitted and executed; the run has no work item, workspace or retained source, and the work item, its project's active count and retained references are unchanged |
| `Test_cleanup_scope` | another tenant's, an unrequested and a missing deployment cannot be admitted for cleanup; a run **bound to one deployment whose admitted step names another** (same project, another tenant, same names, same cluster) is refused and the scheduler then retries with the right definition; bystander objects and objects claimed by another deployment survive; row-level security hides the record from the other tenant; each scope cleans its own on expiry |
| `Test_cleanup_duplicate_claim` | concurrent schedulers claiming one round create one run, one pending dispatch, one credential pin; every claimant converges on its next pass; a repeat claim returns the stored run; a cleanup left `unknown` is never retried by a pass (any time, any restart) and two simultaneous explicit reconciliations admit exactly one further round that completes |

### Teardown ownership

Containers are removed only by the exact workload names the stack's recording runtime was asked to start
(recorded before each start, so a failed start is included) — never by the image they run, which another scope
or invocation may share. `Test_teardown_removes_only_the_containers_it_started` runs an unrelated container
from the same tools image and shows the teardown leaves it alone.

## Cleanup input: the project definition and the T-092 handoff

There is ONE cleanup definition per project (`stack.CleanupDefinition`). It declares the string input
`deployment_id` and its step names no target; execution's `cleanupInputs` (T-092) fills the input with the
deployment the run was admitted for, the entrypoint exports it as `KODAMA_INPUT_DEPLOYMENT_ID`, and `delete.sh`
consumes it. The tests verify, for every cleanup run they execute, that the persisted run input and the
attempt's persisted pinned context carry exactly the run's bound deployment, that the step's environment
carries no target, and that `delete.sh`'s accepted `deployment_id` output equals it. `Test_cleanup_scope` makes
two deployments of a project and one of another tenant due at once under the same definition and checks each
run's own input; a caller-supplied `deployment_id` naming another deployment is refused at admission. The only
definition that names a target is `RetargetedCleanupDefinition`, the negative case (a step overriding the target
with `DEPLOY_DEPLOYMENT_ID`) showing that a run bound to one deployment cannot read or clean another.

## Dependency gaps this qualification found (owners named)

Nothing below is papered over by the runner; each is either asserted visibly or listed.

1. **Work runs cannot have a step without a workspace (T-020).** Dispatch passes the run's workspace for an
   access-`none` step of a *work* run, which `step_attempts_workspace_shape` refuses (`claim: … invalid
   transition`). A deploy step therefore consumes the workspace, which the Podman runtime refuses
   (`bootstrap.ErrNoCheckout`: no checkout credential outside Kubernetes). The wrapper lifts the refusal.
2. **A *crashed-after-start* attempt is never retried**, so its `running` deployment has no automatic
   recovery (asserted in `Test_apply_ack_loss` (g)): the delivery reconciler cannot show the effect did not
   happen.
3. **No production `TxDelivery` and no assembled secret route (T-072).** Execution's store needs
   `Peers.Environments` for any environment-declaring admission, and `internal/app` keeps
   `runtime-secret-resolution` disabled; `stack/txdelivery.go` and the secret handler in `stack/server.go` are the test-local
   equivalents. `deliverytest.New` wires no binder, so T-035's component tests that admit an environment fail
   with `transactional environment binding is not available` (outside this task's paths).
4. **The loser of a simultaneous cleanup claim sees a raw `23505` on `runs_pkey`** (`OutcomeError`,
   retried by its next pass) instead of `created=false` (`execution.Service.AdmitCleanupRound` checks, then
   inserts). The invariant holds — one run, one pin, nothing half-written, asserted — but convergence is
   only eventual. Owner: T-020/T-066.
5. `lookup.sh` is no longer driven here: its action key must be the attempt's own (`act:…`, which the
   entrypoint does not export), so a lookup is only possible as the same step. Its qualification is T-065's
   `Test_deployment_annotation_lookup`.

## Limits

This is the joined deployment path, not the M2 acceptance: registration of the processes (T-072), the whole
agent tracer (T-038) and a production cluster are out of scope.
