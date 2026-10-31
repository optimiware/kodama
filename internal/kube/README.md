# `internal/kube` — the Kubernetes runtime

Task T-021, reshaped by T-086 into `execution.Runtime` ([ADR 0008](../../docs/adr/0008-runtime-seam.md)).
It turns committed execution attempts (T-020) into restricted, observable Kubernetes Jobs that run
the injected entrypoint (T-016) and keeps the cluster consistent with those attempts. It is the
staging and production backend. It is the **standalone trusted-engineer
regular-container execution profile** selected by O-07 (containers share the node kernel; the
profile is not an untrusted-code sandbox). It makes no stronger-sandbox, hosted-authority or
source-fencing claim: publication fencing through Pod deletion and a Tekton executor are out of
scope. O-18/O-19 set the direction for hosted authority and custody, but hosted qualification is
T-061's; this package's evidence is standalone component evidence only.

Requirements: **PX-03, PX-04, PX-08, PX-09, IT-07, IT-08** (supporting SD-01–SD-11).
Acceptance: **PX-A02** (`create_ack_crash`), **PX-A06** (`cancel_create_race`), **IT-A06**
(`restricted_job`) and `foreign_job_collision` (PX-03/IT-07) through the named tests `create_ack_crash`,
`foreign_job_collision`, `cancel_create_race` and `restricted_job` in
[`test/integration/kube`](../../test/integration/kube/README.md).

## Ownership

| Concern | Owner |
| --- | --- |
| Attempt status, deadlines, reservations, cancellation, settlement, audit/outbox | `internal/execution` (T-020) |
| Admission grants, token issue, revocation | `internal/runtimeauth` (T-019) |
| Jobs, Pods, attempt admission Secrets, read-only namespace policy checks | **this package** |
| Tenant namespace, service account, quota, network policy provisioning | deployment owners |
| Application wiring, schedules, production Kubernetes authentication | T-071 / T-027 |

The package implements `execution.Planner`, `execution.Runtime` and
`runtimeauth.JobObserver`. Every runtime method re-reads the committed attempt from the durable
store (`Dependencies.Attempts`); nothing rests on memory a restart loses. `Start` returns the
verified Job UID, which execution binds to the attempt; `Observe` applies the reconciler's
assessment of the verified Job and its Pods; `Stop` is one conservative termination pass, repeated
by execution until it reports the workload stopped; `Remove` keeps the Job for
`CompletedRetention` after settlement (`ErrBusy`), then deletes it and the admission Secret by UID;
`Logs` reads the verified Pod through the optional `Dependencies.Logs`. `Capabilities` claims every
guarantee but not `ProvesNeverStarted` (O-25, T-102): absent Secret, Job and Pods cannot tell no create
request from an issued create whose outcome was lost, so a stop without concrete evidence reports
`LaunchUnknown` (still fencing a later `Start`, and `Stopped` only once no launch can arrive). The
shared runtime contract therefore requires `LaunchNotStarted` of a fresh stop only from backends that
advertise the proof; this does not affect the container guarantees admission requires. The reconciler additionally reports evidence through `execution.Service.ObserveAttempt`;
the package never writes execution or grant tables, and Kubernetes is never called inside an SQL
transaction.

## Constructors and composition order

```go
// Errors are returned by every constructor below; each must be checked.
policy, err := kube.NewExecutionPolicy(cfg)              // immutable; missing policy denies dispatch
cluster, err := kubernetes.NewClient(kubernetes.Config{Origin: api, HTTP: authenticatedClient})
attempts, err := adapters.NewAttempts(executionStore)    // scoped committed reads
authority, err := adapters.NewAuthority(execpgStore, policy) // runtimeauth.AttemptObserver + execpg.LockAttempt seam
deps := kube.Dependencies{Policy: policy, Cluster: cluster, Attempts: attempts}
observer, err := kube.NewJobObserver(deps, opts)         // runtimeauth.JobObserver
// runtimeauth.NewService(... Attempts: authority, Jobs: observer ...)
deps.Registrar = runtimeauthService
runtime, err := kube.NewRuntime(deps, opts)              // execution.Runtime
// execution.NewService(... Planner: kube.NewPlanner(policy), Runtime: runtime ...)
// The planner is required: Start refuses a fingerprint that is not the policy's recipe.
deps.Observations, deps.Tenants = executionService, tenants
reconciler, err := kube.NewReconciler(deps, opts)        // call Pass(ctx) on a schedule
```

`Reconciler.Pass` is bounded and context-aware; T-027 schedules it next to
`execution.Service.Reconcile`. Polling is the recovery mechanism. Watches may
accelerate it later but are not required.

## Job recipe

One Job per attempt, named by T-020's deterministic `execution.WorkloadName`:
`parallelism: 1`, `completions: 1`, `backoffLimit: 0`, Pod `restartPolicy: Never`, one
primary container plus a binary-copy init container. Both images are pinned by
digest. Pods run non-root with no privilege escalation, all capabilities dropped,
`RuntimeDefault` seccomp, no service-account token, no host namespaces or `hostPath`,
and bounded CPU, memory and ephemeral storage. Volumes are private: scratch,
workspace, outputs, results (disk-backed, bounded), memory-backed secrets and identity,
and read-only tools. The admission credential is mounted from the attempt's Secret
as a separate bootstrap volume. Pod and Job identity reach the entrypoint through
the Downward API and the Job controller's labels and are verified independently at
admission. Callbacks are HTTPS to the configured origin with the configured CA.

**Checkout credential and Git CA (T-079).** A source-consuming attempt (workspace access `read`
or `write`) also carries a read-only Git credential (`username:password`) and the Git CA. Composition
supplies a consumer-owned `kube.CheckoutProvider` through `Dependencies.Checkout`; the executor
asks it once per attempt, with a `CheckoutRequest` built only from committed data (scope, run, step,
attempt, the committed `AttemptRecord.WorkspaceID`, access, repository URL, pinned commit), after the
committed-intent and existing-object checks and only when no bootstrap Secret exists yet. The answer
must echo the request's scope, workspace and repository, hold a one-line credential and a PEM
certificate bundle, or the dispatch is denied (`ErrCheckoutDenied`, nothing created, nothing
registered); a provider outage is `ErrUnavailable`. The material is stored in the same immutable
bootstrap Secret as two more keys (`checkout-credential`, `git-ca.crt`; attempts without a workspace
keep exactly `admission` and `ca.crt`) and reaches the entrypoint through the read-only bootstrap
mount and the arguments `--checkout-credential-file=/kodama/bootstrap/checkout-credential` and
`--git-ca-file=/kodama/bootstrap/git-ca.crt`. A restart, redrive or lost response reads the stored
Secret; the provider is never consulted again, so a changed answer cannot change a created attempt.
The Job document, labels, annotations, environment, logs, events and returned errors never contain
the material (provider error text is not echoed), and the Secret verification accepts exactly the
allowed key sets with valid content. A writer's publisher credential stays server-side and is not
part of this seam. Providers should issue short-lived, repository-scoped read-only credentials: a
credential issued by a dispatcher that then loses the create race is discarded unused. The checkout
flags change the recipe, hence the fingerprint, of source attempts planned after this change.

`SpecFingerprint` covers the whole executor-controlled recipe (namespace, service
account, exact images, security settings, resource bounds, volumes, bootstrap
configuration, ownership metadata) and excludes UIDs, timestamps, credential
plaintext and API-server bookkeeping. Adoption compares the actual controlled spec
fields; an annotation that claims the right fingerprint is not evidence.

## External-effect states

`Workload` (`workload.go`) models create-recovery state apart from execution
statuses: `credentialed → registered → create_issued → owned → deleting → gone`. A
create whose outcome is not known stays in `create_issued` with `Uncertain` set (there is no
separate phase); only a definite refusal returns the record to `registered`. Unverified objects never
become owned; a confirmed UID never changes; a deleting or gone workload never
returns to creating. The record lives in the attempt's Secret with
resource-version-checked updates. It is external-effect bookkeeping, not execution
authority.

## Namespace preflight

`Preflight` (`preflight.go`) reads, and never changes, the tenant namespace before the first
create. It denies what it cannot read and distinguishes denial (`ErrPreflight`) from API
unavailability (`ErrUnavailable`). It checks:

* an active namespace enforcing Pod Security `restricted` and bound to the tenant;
* the configured service account: it does not automount a token, and the API server's
  SubjectAccessReview refuses the explicit list of forbidden permissions in `forbiddenAccess`
  (reading Secrets, Pod/Job access, exec/attach, token minting, quota/policy updates, role
  bindings, cluster listings). This is those checks, not proof of every possible RBAC permission;
* ResourceQuota bounds for Pods, CPU, memory and ephemeral storage. **Only unscoped quotas count**:
  a quota with `scopes` or a `scopeSelector` bounds some class of Pods and is not accepted as
  evidence that executor Pods are bounded. Bounds may be spread over several unscoped quotas, and
  any zero or unparsable bound denies;
* NetworkPolicies: a namespace-wide default deny for ingress and egress, and no policy that selects
  executor Pods and admits ingress or egress beyond the declared runtime destinations. Egress must
  name explicit IP blocks and single numbered ports (named ports, wildcards and **port ranges**
  (`endPort`) are refused). A policy selects executor Pods when its `matchLabels` use only the
  fixed `kodama.optimi.com/managed-by` label or the per-Job labels (`kodama.optimi.com/job`, the
  Job controller's `batch.kubernetes.io/job-name` / `batch.kubernetes.io/controller-uid` and the
  legacy `job-name` / `controller-uid`), whose values are unknown to a namespace-level check and
  therefore treated as possibly matching. A selector with any other label key, or a different
  value of the fixed label, is demonstrably about other Pods. Selector expressions are not
  evaluated: they are refused unless `matchLabels` already excludes the workload.

The restrictions (no ranges, unscoped quotas only) are intentionally conservative; deployment
owners provision explicit ports and an unscoped quota. Enforcement by the network plugin cannot
be proven from the API; the kind fixtures probe it. A success is cached for `PreflightTTL`.

## Failure semantics

* **Absence, collision, throttling, forbidden, timeout and unknown mutation outcome
  are distinct errors** (`kubernetes/errors.go`). Only an authoritative not-found is
  absence; a database or API outage is never orphan evidence.
* **Create response lost.** The executor reads back, verifies ownership, the actual spec
  and any recorded UID, and adopts the same Job. An uncertain create followed by
  absence is not permission to create another Job: the uncertainty is kept
  and execution's deadlines and trusted reconciliation resolve it.
* **Credential.** The admission credential is generated once, stored in the attempt Secret,
  then registered. Registration is retried with the same credential and never
  extends the expiry; the credential is never regenerated for an existing registration.
* **Cancellation race.** Cancellation is rechecked after the intent is recorded and
  again after create/adoption. A late workload is deleted and is never granted
  authority (the runtime denies its admission because committed state is cancelled).
* **Start of a stopped or removed workload** is `execution.ErrConflict`, as the shared runtime
  contract requires of every runtime (T-095, `kube_start_stopped_conflict`). A cancelled, revoked,
  terminal or unknown attempt, a record that is deleting or gone, and a confirmed Job that was
  deleted all refuse the Start, create nothing, and return an error that satisfies both
  `errors.Is(err, execution.ErrConflict)` and `errors.Is(err, kube.ErrNotDispatchable)` (the cause).
  Outages and other failures keep their own classes and are never turned into conflicts.
* **Foreign objects** with the deterministic name are neither adopted, mutated nor
  deleted, and their Pods are not admitted.
* **Termination** deletes only verified owned objects, with UID preconditions and
  foreground propagation, and reports `Stopped` only when the Job and its controlled
  Pods are absent. Launch evidence merges what execution committed (`Launch`, `StartedAt`),
  what the recovery record holds and what the Pods show, and only ever becomes more
  informative: a started workload stays `started` after its Secret or Pods disappear. The
  record follows the same rule (`Workload.NoteLaunch`): `started` outranks `not_started`, which
  outranks `unknown`; the stronger value is saved before any Pod is deleted and is never
  downgraded, including across a restarted executor.
  `not_started` needs affirmative evidence (a recovery record that never issued a create, a
  failed injector, or execution's own committed value); a missing Secret or Job alone is
  not such evidence and yields `unknown`, which execution reconciles. The committed Job UID
  (the record's, or execution's observed UID) identifies surviving owned Pods and rejects a
  same-name replacement Job. Create-recovery metadata is kept while a late create is possible.
* **Orphan cleanup** (`Reconciler`) treats labels, deterministic names, annotations, a claimed
  fingerprint, owner references and age as copyable claims. A Pod whose Job is gone is deleted
  only when committed data names its controlling Job UID: execution's observed UID, or the
  confirmed UID in the attempt's validated recovery Secret (matching tenant, namespace, attempt,
  deterministic name and UID), and never for a live attempt. A Job of an absent attempt is
  deleted only when the recovery record confirmed that UID. For a terminal attempt a Job is
  deletable when its UID is one committed data names (the record's or execution's observed UID,
  either surviving the loss of the other): a same-name, identical-spec Job with another UID is a
  replacement and is never deleted, even after the recovery Secret is gone. Only when no UID was
  ever committed is a Job that verifies against the committed recipe a delayed initial create; its
  UID is recorded in the validated recovery record (when one exists) before the UID-preconditioned
  delete so its Pods stay provable. A recovery Secret is deleted only once it validates as the
  attempt's own record (name, labels, annotations, fingerprint, content and record) and the Job is
  verified absent with no Pod controlled by a committed UID; a malformed, foreign or forged
  candidate is left untouched. Anything left untouched is counted in `Report.Skipped` with a
  warning. A failed database or API lookup is an error, never orphan evidence.
* **Never infer success.** Job completion and entrypoint exit zero are not results; only
  an accepted result is. Completed Jobs are retained (`Options.CompletedRetention`)
  until the attempt is settled; no TTL controller is enabled.
* **Deadlines.** Startup and execution deadlines belong to execution (T-020) and the step
  timeout to T-016. Primary-container launch is conservative evidence that work
  may have started; Pod absence never proves safe replay.

## Required Kubernetes permissions

For the controller's identity, per tenant namespace: `get/list/create/delete` on
`jobs.batch`; `get/list/delete` on `pods`; `get/list/create/update/delete` on
`secrets` (attempt-scoped Secrets only, selected by label); `get` on the namespace and
service account; `list` on `resourcequotas` and `networkpolicies`;
`create subjectaccessreviews` to check the tenant service account's effective permissions.
No cluster-wide write access is needed.

## Assumptions

See [review.md](review.md) (baseline, review checklist and the assumptions the integration owners confirm).

## Run resources and `run_shared` attempts (T-114)

T-114 implements P4 of the [run workspace plan](../../docs/implementation/run-workspace-services-builders.md)
against the seams S2-S6 and S8 frozen by T-122. It is component evidence on a fake cluster
(`kubetest.Fake`): real CSI release, CNI traffic and builder profiles are qualified by T-121/T-117, and
nothing here is wired into the application (T-116).

`kube.NewRunResources(RunResourceDependencies, RunResourceOptions)` implements `execution.RunResources`:

| Resource | Objects | Notes |
| --- | --- | --- |
| `workspace` | one `ReadWriteOncePod` PVC of the pinned policy's storage class and size | `ready` when the claim is Bound **or** is Pending with no volume while its class (the policy's, read from the cluster) has `volumeBindingMode: WaitForFirstConsumer`: execution gates the preparation and a source-free run's first attempt on the workspace being ready, and that first consumer is what binds the claim. Any other Pending claim (an Immediate class still provisioning, a missing class) is `pending`, a class whose `reclaimPolicy` is not `Delete` is refused at Ensure and `failed` (`capability_unsupported`) at Observe, and an unreadable class is `unknown`. `Released` only when no Pod of the namespace references the claim (any phase) and no `VolumeAttachment` of its volume remains; `Remove` deletes the claim by UID and confirms the volume is gone (delete reclaim); for a claim that never bound, no volume of its UID (the recorded one or `pvc-<claimUID>`) may exist, and one that appears later keeps cleanup pending |
| `preparation` | one Job (`backoffLimit: 0`, `restartPolicy: Never`) of the policy's digest-pinned image running `kodama prepare` | claim at `/workspace`, S4 environment, read-only checkout credential in the record Secret |
| `service` | one Pod (`restartPolicy: Never`) + ClusterIP Service named by the endpoint host + an ingress policy | readiness probe translated to the Pod, bounded ephemeral storage, memory `/dev/shm`, the pinned `service` role; with secret bindings it runs `kodama service-run -- argv` through the injected entrypoint |
| `network` | per-run NetworkPolicies selected by run labels | clients (`-cli`), service DNS (`-svc`), declared edges (`-eoN`/`-eiN`, run model), public egress (`-pub`), named profiles (`-egN`) |

Every resource has a **record Secret** (`<name>-rec`) created before anything else. It holds the canonical
spec (never mounted into a workload), the external-effect record and, for a workload with a runtime
identity, the admission credential (persisted **before** it is registered through the consumer-owned
`ResourceRegistrar` port, seam S5). The record fences a stopped resource (Ensure then returns
`ErrResourceFenced`), proves ownership of objects that outlive their parent, and resolves a lost create:
Stop and Remove wait until a create of unknown outcome can no longer land (`Options.CreateQuiescence`):
every create, a companion's (a service's Service and ingress policy, each network policy) included, is announced in
the record and the fence rechecked before it is sent. Cleanup deletes only what is provably ours: a bound UID, a
companion UID recorded when it was verified, or (nothing recorded) an object that carries our marks **and** equals the
rendering of the committed spec; a lookalike that copies every label and annotation is left alone. Outstanding creates are tracked **per operation** (an id and a timestamp in the record): an Ensure
removes only its own entry, after reading its object back, so no concurrent Ensure settles another's uncertainty; Stop and
Remove wait until every entry is older than the quiescence. A primary object verified by its spec with no committed UID
(a late create) has its UID persisted in the record (`seen`) before it is deleted, so repeated cleanup still finds its
volumes. A record that is lost for a bound workload is lost bookkeeping, not "never issued": Stop rebinds a fence to the
bound UID (the workload is deleted by it), reports `LaunchUnknown`, and Remove succeeds only once the bound workload is
verifiably gone. A companion whose recorded UID differs from the object found is classified by the committed spec, not assumed foreign (a create that was outstanding while the recorded instance was deleted yields a new instance of ours): an exact rendering is ours and is deleted, anything else is not; a recorded UID found absent is forgotten durably. Companion objects of a resource whose record is lost carry only copyable marks, so they are **never
deleted**: Remove stays unconfirmed (`ErrCleanupUnverified`) until an operator resolves them. Ensure re-reads the removal
marker after its record create and before any effect, so a removal that overtakes a planning Ensure is not resurrected.

**Removal tombstone.** `Remove` writes `<name>-tomb` (identity, fingerprint, bound UID; no credential, no spec) before it
deletes the record, and nothing in the adapter ever deletes it: a removed resource is refused (`ErrResourceFenced`) by
any later Ensure, from any controller instance, because execution recorded the cleanup and released the capacity. It is
one small Secret per removed resource and goes away with the tenant namespace at offboarding.
Ownership is verified by scope, deterministic name, fingerprint annotation **and** the bound UID, and the
live object's controlled spec is compared with the rendering of the committed spec; a same-name object that
differs is `ErrResourceCollision` and is never adopted, stopped or deleted. Observe never reports absence
unless every read said not-found; an outage, an unreadable policy or an unverifiable object is
`PhaseUnknown`.

The pinned policy body (role shapes, storage class, network model, egress profile destinations) comes from
a consumer-owned `PolicyCatalog` (seam S9's retained, append-only policy list); the adapter verifies the
body's id and digest against the run's pin and the spec's role-shape digest, and a profile's destinations
against the digest the run pinned. Hostname destinations are resolved once through an optional
`HostResolver` and pinned in the record; without one they are refused.

**Capabilities** advertise exactly what is rendered for every run (`run_workspace`, `workspace_fence`,
`workspace_preparation`, `resource_recovery`, `run_services`, `service_isolation`, `run_network_isolation`,
`declared_edges`, `named_egress`, `egress_default_deny`). `builder_service` and `builder_isolated_worker` are
never advertised; `builder_rootless` only if the installation vouches for it (`RunResourceOptions.Qualified`)
after qualification.

**`run_shared` attempt Jobs.** `AttemptRecord.Shared` is the committed launch (`adapters.Attempts` derives it
from the run's pins and the bound workspace). A `run_shared` Job selects the v2 wire, mounts the bound
claim at `/workspace` (its UID is verified equal to the bound one before anything is created or adopted, the
claim must not be terminating and its record must not be fenced), keeps outputs, secrets, identity,
scratch, `HOME=/kodama/home`, `XDG_RUNTIME_DIR=/kodama/run` attempt-private and bounded, receives no checkout
credential and takes its security shape exactly from the pinned step role (`hostUsers: false`, non-root with
the installation's UID/GID, all capabilities dropped then `capabilitiesAdd`, escalation iff additions,
seccomp/AppArmor `RuntimeDefault` or `Localhost`, optional unmasked `/proc`). Adoption, observation and
termination rebuild the expectation from the committed launch and compare every field. The attempt_private
Job is unchanged.

**Namespace baseline.** `Preflight.CheckRun` requires the policy's Pod Security level (and version) and accepts
exactly the owned per-run policies: by shape and, since policies are additive, only when each one equals what the
committed spec of its resource renders (`RunResources.VerifyOwnedPolicy`); an unrecognized extra also makes the
resource's observation a collision. The cache key separates the attempt_private and run_shared checks; foreign broad allows selecting run Pods are still refused.
`(*RunResources).VerifyResourcePod` confirms a claimed Pod is controlled by the preparation Job bound by
execution (seam S6).

What T-116 wires: `NewRunResources` (+ `RunResourceDependencies`: execution policy, `*kubernetes.Client`, a
`PolicyCatalog` over the chart-rendered policy list, a `ResourceRegistrar` bridging
`runtimeauth.RegisterResource`, the existing `CheckoutProvider`, `PodLogs`, optionally a `HostResolver`), passing the `RunResources` also as `Dependencies.OwnedPolicies` of the attempt
`Runtime` (a `run_shared` attempt's preflight then verifies the run policies against their records; without it a namespace
holding run policies fails that preflight),
`VerifyResourcePod` bridged to runtimeauth's `ResourceWorkloads`, `adapters.Attempts` (populates
`AttemptRecord.Shared`), and the cluster RBAC for claims, Services, NetworkPolicies, Pods and the
cluster-scoped reads of PersistentVolumes, VolumeAttachments and StorageClasses (`get` and `list` on
`persistentvolumes`, `list` on `volumeattachments`, `get` on `storageclasses`; volumes are found by their claimRef).
A preparation checkout request is `CheckoutRequest{Scope, RunID, StepID: "preparation", Access: read,
RepositoryURL, Commit}` with no attempt and no workspace binding.

## Kernel runtime adapter (`kernelrt/`, T-178)

`kernelrt/` implements the `internal/kernel/runtime` `Runtime` and `ResourceManager` ports for Kubernetes
(Job per step attempt, run workspace claim, run services) without importing the legacy execution,
runtime-admission, gate or workspace packages or this package's executor. It is the replacement path for
the retained behavior above (stable identity and adoption, fence records, UID-preconditioned deletes,
restricted workload shape, orphan reconciliation, workspace release evidence); this package stays in
place until the P3 cutover. Its package documentation lists what it enforces, what it refuses (source
preparation, service secrets, non-run service lifetimes) and the limits of its fake-cluster evidence: the unit
tests prove the mapping to API objects, not CSI, CNI, quota or kubelet behavior. Composition wiring and
the kind-backed conformance profile belong to later tasks.
