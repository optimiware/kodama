# Pipeline execution

**Scope:** M1 containers; M2 agent profiles and human gates. A runtime runs workloads, Kubernetes in staging and production; PostgreSQL owns desired state. JetStream remains a durable event bus.

**Run models.** Every run pins one model at admission. `attempt_private` (format `kodama/v1alpha1`, runtime context v1) is the **current executable model**: each attempt checks out its own private scratch under the fixed restricted baseline. `run_shared` (format [`kodama/v1alpha2`](../contracts/pipeline-format-v1alpha2.md), [runtime v2](../contracts/runtime-v2.md)) is the normative model of [ADR 0010](../adr/0010-run-workspace-services-builders.md): one disposable `/workspace`, declared support services, a total run deadline and a pinned [execution policy](../contracts/run-resources.md). It is **designed, not delivered or qualified**: no installation admits it until T-116 enables it and T-117 passes. Requirements below apply to both models unless labeled `[attempt_private]` or `[run_shared]`; replaced `attempt_private` rules keep their IDs and are not renumbered.

## Model and states

A run pins definition ID/digest, work item/content revision, source commit, variables, secret-version references, and run inputs. A successful writer supplies a new immutable source output; it does not rewrite the run's original inputs.

| Record | States | Additional fields |
| --- | --- | --- |
| Run | `queued`, `running`, `succeeded`, `failed`, `cancelled` | `cancel_requested_at`, failure intent/reason, timestamps |
| Step run | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | `reason`, `next_attempt_at`, gate reference |
| Attempt | `running`, `succeeded`, `failed`, `cancelled` | `reason`, expected workload name/spec fingerprint, bound instance UID, actual start/finish, reconciliation result, workload removal |

An attempt row is dispatch intent before its workload exists; `reason=starting` explains that interval. Waiting for a gate, quota, or backoff is derived from pending steps. Cancellation is a requested action; the run stays nonterminal until admitted work is settled. A failed attempt with `reason=outcome_unknown` cannot be retried until reconciled. No lost/paused/starting status enums are needed.

`purpose=work` requires an actionable work item. M2 internal `purpose=cleanup` runs are project-scoped, use no workspace, and are authorized only for a recorded deployment's cleanup. They may run after the originating work item is done/cancelled and do not consume business WIP.

**Origins and source sets (internal hosting, designed; T-131).** Under [ADR 0011](../adr/0011-internal-tenant-git-service.md) a run also records its **origin**: `work_item`, `push` (an applied push record and ref, from a trigger), `manual`, or `provision`. A push-originated run needs no work item and consumes no WIP; a `provision` run may start before its repository exists and receives seed authority only for repositories it creates. A run pins a **source set**, one exact commit and checkout path per selected repository ([contract](../contracts/git-service.md#source-set)); a single-repository run is the one-entry case. The fields carrying origin and source set are a versioned addition; existing definitions, contexts and hashes are unchanged.

## Requirements

| ID | Requirement |
| --- | --- |
| PX-01 | Definition admission MUST validate graph, references, types, and available step kinds. |
| PX-02 | Run admission MUST pin resolved inputs and reject conflicting source/specification references. |
| PX-03 | Attempt identity and Job name MUST be stable; existing Jobs MUST match ownership and expected spec before adoption. |
| PX-04 | Each executable attempt MUST use one Job, one primary workload container, and `backoffLimit: 0`; factory retries create a new attempt. |
| PX-05 | Transitions MUST commit with audit/outbox; consumers MUST deduplicate and reconciliation MUST tolerate missing/reordered wakeups. |
| PX-06 | Dispatch MUST check dependencies, cancellation, current authorization, quotas, and writer admission. |
| PX-07 | Retry MUST be bounded and classified; unknown external outcomes MUST be reconciled first. |
| PX-08 | Cancellation MUST block new dispatch claims, revoke attempt access, terminate observed workloads, and settle known side effects before terminal completion. |
| PX-09 | Execution/startup deadlines and gate deadlines MUST be distinct; gate waits consume no Job. `[attempt_private]` A waiting run holds no compute. `[run_shared]` the run's services and scratch persist through gate waits, bounded by the run deadline (PX-20). |
| PX-10 | Success MUST validate required outputs and source publication in addition to process exit. Failure diagnostics SHOULD be retained even when success outputs are unavailable. |
| PX-11 | Tenant/project concurrency reservations MUST be transactional and idempotent. |
| PX-12 | Terminal runs/attempts MUST retain their history; rerun creates a new run. |
| PX-13 | Gate steps MUST wait in durable records and revalidate their subject before progression. |
| PX-14 | `[attempt_private]` Workspace sources MUST be the run commit or an explicit successful ancestor's commit. `[run_shared]` replaced: the prepared scratch is cumulative and the source rules are WS-13–WS-16. |
| PX-15 | Execution MUST reach workloads only through the backend-neutral runtime port. Admission MUST refuse a run whose steps require guarantees the runtime lacks and pin its backend and requirements; another backend MUST NOT start, stop or remove its workloads. |
| PX-16 | Dispatch MUST bind the instance the runtime reports. A dispatch-confirmed workload reported absent MUST fail as lost with its known launch evidence. Removal after settlement MUST be durably recorded and retried until confirmed. |
| PX-17 | `[run_shared]` Declared services MUST start with the run and expose only run-unique endpoints, reachable according to the pinned network model (IT-14: the run's workloads under the run model, the tenant's under the tenant model of O-30); no user command MAY be dispatched until workspace preparation and every service are verified ready. |
| PX-18 | `[run_shared]` A service startup timeout, exit, affirmative loss or sustained readiness loss, or a lost/unverifiable workspace, MUST fail the run with an explicit reason; execution MUST NOT transparently replace a service or reconstruct scratch. Restart MUST adopt only resources verified by scope, fingerprint and UID. |
| PX-19 | `[run_shared]` Success, failure, timeout and cancellation MUST stop every run resource and record verified stop **and confirmed removal** before terminal completion (stuck cleanup keeps the run nonterminal with a visible safe reason and is retried; historical terminal records with unconfirmed cleanup are still reconciled); lost responses, late creates and restarts MUST converge without touching a foreign object. |
| PX-20 | `[run_shared]` Admission MUST reserve bounded service, active-attempt and scratch capacity atomically and pin a required total run deadline covering preparation, startup, attempts, retries and gate waits; reservations MUST be released only after verified removal. |
| PX-21 | `[run_shared]` A run MUST pin its model, runtime context version, execution policy, runtime binding and resource capabilities; a controller, runtime or policy lacking any MUST refuse it. Historical definitions, digests, histories and result hashes MUST remain intact and old-model runs MUST drain under their original semantics. |
| PX-22 | `[run_shared]` Automatic retry MUST be limited to attempts proven not to have begun their command while scratch and services remain intact; an attempt that may have run user code, an unknown launch or an uncertain effect MUST NOT be replayed automatically. |
| PX-23 | `[internal hosting]` A run MUST pin its origin and a source set of exact commits with unique checkout paths; every commit MUST belong to a repository of the run's project and stay reachable for the run's retention. |
| PX-24 | `[internal hosting]` A push-originated run MUST reference one applied push record and ref, pin that update's new commit, and be created at most once per trigger, push and ref. |
| PX-25 | `[internal hosting]` A provisioning run MUST be admitted without an existing repository and receive seed authority only for repositories it creates in its project. |

## Scheduling and runtime

One server process hosts API, publisher, consumers, and periodic reconciliation. Database transactions claim runnable work using row locks/`SKIP LOCKED`, then persist attempt intent before releasing locks. External calls happen outside database transactions. A recovery scan handles claims whose controller crashed. Multiple replicas can share this protocol; the initial chart uses one replica.

Execution calls one runtime port: `Start`, `Observe`, `Logs`, `Stop`, `Remove` and `Capabilities` ([ADR 0008](../adr/0008-runtime-seam.md)). For `run_shared` runs it also calls a small execution-owned companion port (`Capabilities`, `Ensure`, `Observe`, `Logs`, `Stop`, `Remove`) over run resources ([contract](../contracts/run-resources.md#companion-port)); the attempt port is unchanged. Each method is idempotent and acts only on a verified instance. `Stop` reports a workload stopped only once no launch can still arrive. Backends declare the guarantees they enforce: `oci_image`, `isolation`, `resource_limits`, `network_policy`, `resource_quotas` and `crash_recovery`. The installation's requirement policy decides what a step needs. By default, an executable step requires its image, isolation and resource limits. A run admitted while no runtime is configured stays unbound and is not claimed until one is configured.

| Backend | Environment ([ADR 0009](../adr/0009-environments.md)) | Use | Guarantees |
| --- | --- | --- | --- |
| Kubernetes | `production` | Staging and production | Image, isolation, resource limits, network policy, resource quotas, crash recovery |
| Podman | `test` | Local end-to-end | Image, isolation, resource limits, crash recovery |
| Process | `development` | Tests only, explicit host programs | None |

`run_shared` runs additionally require the backend's [resource capabilities](../contracts/run-resources.md#capabilities) (a separate named set; the guarantees above plus one `run_resources` marker bit in the existing `uint8`). Expected, only after qualification: Kubernetes the full set, Podman the lifecycle/volume/service members its tests prove with network/egress members absent unless enforced and tested, Process lifecycle members only.

O-25 adds an explicit, optional launch-history proof capability ([ADR 0008](../adr/0008-runtime-seam.md#launch-evidence--o-25), implementation T-102). For a fresh workload, the shared runtime contract requires affirmative `LaunchNotStarted` only when the backend advertises that capability. Kubernetes cannot infer it from absent Secret/Job/Pod records and reports `LaunchUnknown` without affirmative evidence. Every backend still fences a subsequent start and must prove no launch can arrive before reporting `Stopped`; unknown side effects retain reconciliation requirements before retry or writer-barrier release. Keep this optional proof separate from the execution guarantees required at admission.

The Kubernetes runtime injects the static `kodama step` binary through an init container, and Podman bind-mounts it. The primary container runs that entrypoint around the declared command. The entrypoint clones source (`attempt_private`; in `run_shared` it verifies the already prepared shared workspace and records the command-begin fence instead), collects results, and calls scoped interfaces. Outside Kubernetes, one instance stands for both the Job and the Pod in the entrypoint's admission identity. See [entrypoint](../implementation/step-entrypoint.md) for finalization and cancellation races.

Dependencies must succeed. False conditions and skipped ancestors propagate skipping; a mandatory gate cannot be skipped through a false author-supplied condition. The baseline is fail-fast: record failure intent, stop dispatch, terminate siblings, then mark the run failed after settlement. Timeouts are `failed` with a timeout reason.

`retry.on` is exactly `infrastructure` or `transient`. Explicit command exits and validation/policy failures are not automatically retried. An error classification alone does not establish safe replay of an external effect. Defaults are one attempt and no retries.

## `run_shared` model

Normative design; implementation is T-112–T-117 and nothing here is claimed delivered. The [run-resources contract](../contracts/run-resources.md) owns identities, ports and lifecycle detail; this section owns the observable rules.

**One workspace, serialized.** A source-backed run prepares its pinned commit once in a controller-owned workload (read-only credentials, no user code, hooks/submodule recursion disabled) and records a trusted receipt; a source-free run receives initially empty scratch (earlier steps' files persist for later steps) and no checkout credential. Every executable step, each with its own digest-pinned image, mounts the same scratch at `/workspace`. Exactly one attempt holds it at a time, including `read` steps (they can modify it). Definition admission freezes `executionOrder`: the lexicographically smallest topological order by declaration index ([algorithm and fixtures](../contracts/pipeline-format-v1alpha2.md#deterministic-execution-order)). Gates occupy their position and create no Job or mount. A node is claimed only when its predecessor is settled: workload verified stopped, result/publication settled, and the workspace observed released. `needs` still governs logical dependency, skipping, output availability and expression validity. A false condition settles a step without holding the scratch. Backoff keeps the step's slot.

**Services and readiness.** Services start with the run, concurrently with preparation, as run-owned workloads (not steps, sidecars or fake attempts). Endpoints are fixed at admission and published in immutable attempt context. Readiness (TCP, HTTP or exec, with startup budget) gates the first command; service loss fails the run (PX-18). Services keep their own bounded storage and share no mounts, identities or secrets with steps. Network reachability follows the pinned network model (IT-14): tenant-scoped under O-30's stage model, run-scoped under the run model.

**Deadlines.** The required `spec.timeoutSeconds` bounds the run from its start (the admission transaction that reserves resources) and includes gates. It is separate from per-step execution deadlines and gate deadlines; a gate's deadline cannot outlive it. Expiry fails the run (`run_deadline_exceeded`), stops resources and invalidates pending gates.

**Failure and recovery.** There is no scratch checkpoint: after any attempt began, a lost or unverifiable workspace or service fails the run (`workspace_lost`, `service_lost`); a rerun is a new run with new resources. Automatic retry requires pre-command proof from the server-recorded command-begin fence ([runtime v2](../contracts/runtime-v2.md#command-begin-fence)), not from missing runtime records. Cancellation/failure blocks claims, revokes resource and attempt grants, stops attempts then services promptly, and lets uncertain source/external effects settle independently. A run is terminal only after every resource is verified stopped **and** removed; stuck cleanup stays visible as a nonterminal safe reason (`resource_stop_unconfirmed`, `cleanup_pending`), is retried, and reserved capacity is released after verified removal.

**Admission refusals.** `format_retired`, `capability_unsupported`, `policy_unavailable`, `limit_exceeds_policy`, `egress_profile_unknown` and the source-default refusals are stable 422 outcomes ([list](../contracts/pipeline-format-v1alpha2.md#format-availability-and-admission-refusals)). A runtime that does not declare the complete required companion capabilities admits no `run_shared` run.

## Effects, cancellation, and recovery

An action key is stable per logical step across its attempts. External tools must honor it or expose outcome lookup. General commands cannot promise exactly-once execution. A timeout or lost node can leave an uncertain effect requiring manual reconciliation.

A cancellation may race with a Kubernetes create already in flight. Revoked runtime admission prevents the new Pod from starting authorized work; reconciliation deletes it. Late results cannot change a terminal/cancelled run. Required source/artifact finalization occurs before Job cleanup.

Restore rotates workload signing keys and revokes old attempts; it also fences old controllers and reconciles remote Jobs/Git/deployments before dispatch. Token rotation alone cannot stop previously authorized external requests.

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| PX-A01 | Invalid graph/inputs fail before dispatch | PX-01, PX-02 |
| PX-A02 | Crash after Job create adopts the same matching Job | PX-03, PX-04 |
| PX-A03 | Duplicate events/broker outage converge through outbox/inbox and reconciliation | PX-05, PX-12 |
| PX-A04 | Contending dispatch obeys dependencies, quotas, and source ownership | PX-06, PX-11, PX-14 |
| PX-A05 | Ambiguous side effect blocks automatic retry | PX-07 |
| PX-A06 | Cancel during create/run settles Pods and rejects late results | PX-08, PX-09 |
| PX-A07 | Missing outputs fail success finalization while diagnostics remain readable | PX-10 |
| PX-A08 | Human gate survives restart without a Job (`attempt_private`: without compute) and stale approval cannot progress | PX-09, PX-13 |
| PX-A09 | Rerun preserves prior inputs/history; cleanup run has narrowly scoped admission | PX-02, PX-06, PX-12 |
| PX-A10 | A run its runtime cannot satisfy is refused; another backend never claims, stops or removes a bound run | PX-15 |
| PX-A11 | The real entrypoint round-trips on Process and Podman; lost workloads fail; settled workloads are removed once | PX-15, PX-16 |
| PX-A12 | Runtime launch-history capability controls affirmative never-started proof; lost records preserve uncertainty, stopping fences subsequent launch, and uncertain effects cannot bypass reconciliation | PX-07, PX-08, PX-15, PX-16 |
| PX-A13 | Two runs with identical service aliases resolve only their own run-unique endpoints (and, under the run model, cannot reach each other's); readiness precedes the first command; a database round trip works from different step images | PX-17 |
| PX-A14 | Startup timeout, service exit/loss and scratch loss fail the run without empty-state replacement; restart adopts intact resources | PX-18 |
| PX-A15 | Success, failure, timeout and cancellation remove all resources; lost responses and late creates converge without deleting foreign objects, including after controller restart | PX-19 |
| PX-A16 | Capacity reservations are atomic and bounded; gate waits retain resources only until the run deadline, whose expiry settles resources and gates | PX-09, PX-20 |
| PX-A17 | Model/context/policy/capability pins survive restart; an unsupported runtime or older controller refuses a `run_shared` run; historical definition digests, histories and result hashes are unchanged | PX-15, PX-21 |
| PX-A18 | A pre-command infrastructure failure retries on the same scratch; a begun or unknown attempt is never replayed automatically and partial scratch/service effects cannot be silently repeated | PX-07, PX-22 |
| PX-A19 | A run over two repositories checks each out at its pinned commit and path; a foreign repository, duplicate path or unreachable commit fails admission | PX-02, PX-23 |
| PX-A20 | A developer push starts one run for the pushed commit; redelivery, replay and later branch movement start nothing more | PX-24, GR-10 |
| PX-A21 | A provisioning run creates and seeds a repository, then a second run uses it; the seed grant cannot write another repository | PX-25, GR-07 |
