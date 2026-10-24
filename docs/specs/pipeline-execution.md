# Pipeline execution

**Scope:** M1 containers; M2 agent profiles and human gates. A runtime runs workloads, Kubernetes in staging and production; PostgreSQL owns desired state. JetStream remains a durable event bus.

## Model and states

A run pins definition ID/digest, work item/content revision, source commit, variables, secret-version references, and run inputs. A successful writer supplies a new immutable source output; it does not rewrite the run's original inputs.

| Record | States | Additional fields |
| --- | --- | --- |
| Run | `queued`, `running`, `succeeded`, `failed`, `cancelled` | `cancel_requested_at`, failure intent/reason, timestamps |
| Step run | `pending`, `running`, `succeeded`, `failed`, `skipped`, `cancelled` | `reason`, `next_attempt_at`, gate reference |
| Attempt | `running`, `succeeded`, `failed`, `cancelled` | `reason`, expected workload name/spec fingerprint, bound instance UID, actual start/finish, reconciliation result, workload removal |

An attempt row is dispatch intent before its workload exists; `reason=starting` explains that interval. Waiting for a gate, quota, or backoff is derived from pending steps. Cancellation is a requested action; the run stays nonterminal until admitted work is settled. A failed attempt with `reason=outcome_unknown` cannot be retried until reconciled. No lost/paused/starting status enums are needed.

`purpose=work` requires an actionable work item. M2 internal `purpose=cleanup` runs are project-scoped, use no workspace, and are authorized only for a recorded deployment's cleanup. They may run after the originating work item is done/cancelled and do not consume business WIP.

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
| PX-09 | Execution/startup deadlines and gate deadlines MUST be distinct; gate waits consume no Job. |
| PX-10 | Success MUST validate required outputs and source publication in addition to process exit. Failure diagnostics SHOULD be retained even when success outputs are unavailable. |
| PX-11 | Tenant/project concurrency reservations MUST be transactional and idempotent. |
| PX-12 | Terminal runs/attempts MUST retain their history; rerun creates a new run. |
| PX-13 | Gate steps MUST wait in durable records and revalidate their subject before progression. |
| PX-14 | Workspace sources MUST be the run commit or an explicit successful ancestor's commit. |
| PX-15 | Execution MUST reach workloads only through the backend-neutral runtime port. Admission MUST refuse a run whose steps require guarantees the runtime lacks and pin its backend and requirements; another backend MUST NOT start, stop or remove its workloads. |
| PX-16 | Dispatch MUST bind the instance the runtime reports. A dispatch-confirmed workload reported absent MUST fail as lost with its known launch evidence. Removal after settlement MUST be durably recorded and retried until confirmed. |

## Scheduling and runtime

One server process hosts API, publisher, consumers, and periodic reconciliation. Database transactions claim runnable work using row locks/`SKIP LOCKED`, then persist attempt intent before releasing locks. External calls happen outside database transactions. A recovery scan handles claims whose controller crashed. Multiple replicas can share this protocol; the initial chart uses one replica.

Execution calls one runtime port: `Start`, `Observe`, `Logs`, `Stop`, `Remove` and `Capabilities` ([ADR 0008](../adr/0008-runtime-seam.md)). Each method is idempotent and acts only on a verified instance. `Stop` reports a workload stopped only once no launch can still arrive. Backends declare the guarantees they enforce: `oci_image`, `isolation`, `resource_limits`, `network_policy`, `resource_quotas` and `crash_recovery`. The installation's requirement policy decides what a step needs. By default, an executable step requires its image, isolation and resource limits. A run admitted while no runtime is configured stays unbound and is not claimed until one is configured.

| Backend | Environment ([ADR 0009](../adr/0009-environments.md)) | Use | Guarantees |
| --- | --- | --- | --- |
| Kubernetes | `production` | Staging and production | Image, isolation, resource limits, network policy, resource quotas, crash recovery |
| Podman | `test` | Local end-to-end | Image, isolation, resource limits, crash recovery |
| Process | `development` | Tests only, explicit host programs | None |

O-25 adds an explicit, optional launch-history proof capability ([ADR 0008](../adr/0008-runtime-seam.md#launch-evidence--o-25), implementation T-102). For a fresh workload, the shared runtime contract requires affirmative `LaunchNotStarted` only when the backend advertises that capability. Kubernetes cannot infer it from absent Secret/Job/Pod records and reports `LaunchUnknown` without affirmative evidence. Every backend still fences a subsequent start and must prove no launch can arrive before reporting `Stopped`; unknown side effects retain reconciliation requirements before retry or writer-barrier release. Keep this optional proof separate from the execution guarantees required at admission.

The Kubernetes runtime injects the static `kodama step` binary through an init container, and Podman bind-mounts it. The primary container runs that entrypoint around the declared command. The entrypoint clones source, collects results, and calls scoped interfaces. Outside Kubernetes, one instance stands for both the Job and the Pod in the entrypoint's admission identity. See [entrypoint](../implementation/step-entrypoint.md) for finalization and cancellation races.

Dependencies must succeed. False conditions and skipped ancestors propagate skipping; a mandatory gate cannot be skipped through a false author-supplied condition. The baseline is fail-fast: record failure intent, stop dispatch, terminate siblings, then mark the run failed after settlement. Timeouts are `failed` with a timeout reason.

`retry.on` is exactly `infrastructure` or `transient`. Explicit command exits and validation/policy failures are not automatically retried. An error classification alone does not establish safe replay of an external effect. Defaults are one attempt and no retries.

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
| PX-A08 | Human gate survives restart without compute and stale approval cannot progress | PX-09, PX-13 |
| PX-A09 | Rerun preserves prior inputs/history; cleanup run has narrowly scoped admission | PX-02, PX-06, PX-12 |
| PX-A10 | A run its runtime cannot satisfy is refused; another backend never claims, stops or removes a bound run | PX-15 |
| PX-A11 | The real entrypoint round-trips on Process and Podman; lost workloads fail; settled workloads are removed once | PX-15, PX-16 |
| PX-A12 | Runtime launch-history capability controls affirmative never-started proof; lost records preserve uncertainty, stopping fences subsequent launch, and uncertain effects cannot bypass reconciliation | PX-07, PX-08, PX-15, PX-16 |
