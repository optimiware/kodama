# Run resources and execution policy

**Status:** normative contract frozen by T-109 for the `run_shared` model ([ADR 0010](../adr/0010-run-workspace-services-builders.md), O-28); Go shapes frozen by T-112 (execution) and policy values by T-111 (qualification); **not delivered, not qualified**. This document owns the semantics execution, adapters, entrypoint and chart share: policy pin, capabilities, resource identity/evidence, the companion port, lifecycle ordering, reasons and observations. Pipeline syntax is [`kodama/v1alpha2`](pipeline-format-v1alpha2.md); the wire is [runtime v2](runtime-v2.md); behavior requirements and acceptance are in [pipeline execution](../specs/pipeline-execution.md), [workspaces](../specs/workspaces.md), [identity](../specs/identity-tenancy-and-rbac.md), [secrets](../specs/variables-and-secrets.md) and [delivery](../specs/delivery-and-quality-gates.md).

A **run resource** is execution-owned state a run needs beyond its attempts: the scratch filesystem, the one-shot checkout, support services and the run's network boundary. It is not a business aggregate, has no status enum visible as run/step/attempt state, produces no fake attempt and never creates a checkpoint.

## Execution policy

Installation-owned, versioned, **operator-installed** and never part of a pipeline document. It replaces the fixed `restricted` Pod baseline for `run_shared` runs; `attempt_private` runs keep the existing fixed baseline unchanged. Identity is `{id, digest}`: `digest` is the SHA-256 of the canonical JSON of the policy (same canonicalization as definitions). Policies are append-only; the controller persists the body of every policy a nonterminal run pins and removes it only after those runs are terminal and cleaned.

```yaml
# Placeholder values. T-120 configures and T-121 qualifies the security/storage/
# network values and the support matrix; nothing below is a qualified profile.
id: run-v1
admission:
  legacyFormat: open          # open | drain | closed (v1alpha1 admission; see cutover)
  runShared: closed           # closed until T-116 enablement
limits:
  maxRunSeconds: 21600
  maxServices: 8
  maxRunWorkspaceMiB: 8192
  defaultRunWorkspaceMiB: 2048
  maxServiceCpuMillis: 4000
  maxServiceMemoryMiB: 8192
  maxServiceEphemeralMiB: 8192
  maxServiceStorageMiB: 8192
  preparationTimeoutSeconds: 600
  preparationAttempts: 2        # checkout tries INSIDE the one preparation workload (see lifecycle 2)
  stopGraceSeconds: 30
requires: []                   # extra resource capabilities needed by every run_shared run
workspace:
  access: ReadWriteOncePod
  reclaim: Delete
  idmapped: true
  storageClass: kodama-run-scratch
roles:                         # per-workload security shapes; the verified source of truth for adapters
  step:           {user: nonroot, userNamespace: pod, capabilitiesAdd: [SETUID, SETGID], seccomp: kodama-rootless-build-v1, lsm: kodama-rootless-build-v2, devices: []}
  service:        {user: nonroot, userNamespace: pod, capabilitiesAdd: [], seccomp: RuntimeDefault, devices: []}
  builderService: {user: nonroot, userNamespace: pod, capabilitiesAdd: [], seccomp: RuntimeDefault, devices: []}   # unused until T-123
  preparation:    {user: nonroot, userNamespace: pod, capabilitiesAdd: [], seccomp: RuntimeDefault, devices: [], image: "registry.example.invalid/kodama-prepare@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
podSecurity: {level: baseline, version: pinned-at-qualification}
builders:
  rootless: [ko, buildah]
  service: {mode: none}        # none until T-123 (rootless | no_process_sandbox | privileged_isolated_worker)
network:
  isolation: tenant            # tenant | run (O-30 stage model: tenant)
  publicEgress: true           # public HTTP/HTTPS through the installation resolver
egressProfiles:
  registry: {destinations: [{host: registry.example.invalid, ports: [443]}]}
```

`network` is part of the pinned body (and so of its digest). `isolation: tenant` lets the workloads of one tenant reach the run services of that tenant; other tenants, the control plane, the Kubernetes API, node and cloud-metadata addresses stay denied. `isolation: run` additionally denies other runs of the same tenant. `publicEgress: true` permits public HTTP/HTTPS (ports 80 and 443 to non-private addresses) through the installation resolver; `false` is external default deny. Private, cluster and platform destinations are never public: they need a named egress profile (or the per-role Kodama callback/Git/artifact grants).

Contract invariants independent of the values above: a role never carries `privileged`, host PID/IPC/network, hostPath, an engine socket, service-account token automount, `SYS_ADMIN`/`NET_ADMIN`, or a device other than an explicitly listed `/dev/fuse`; `capabilitiesAdd` is limited to `SETUID`/`SETGID` (user-namespace mapping helpers); every capability is scoped to the Pod's user namespace; `ReadWriteOnce` or Pod deletion is never accepted as the workspace fence. A rootless build that fails never enables a wider shape: widening requires a new policy version. A `privileged_isolated_worker` builder mode additionally requires the `builder_isolated_worker` capability ([ADR 0010 §6](../adr/0010-run-workspace-services-builders.md#6-buildkit-rootless-first-scoped-service-when-necessary)) and is not advertised until that isolation exists.

### Pin semantics

- **Admission pins** `{id, digest}`, the runtime binding (backend plus required guarantees and resource capabilities), resolved limits and scratch size, endpoints, effective workspace access, `executionOrder` and the egress profiles' resolved destination digests. Any ceiling violation is refused at admission (`limit_exceeds_policy`), never clamped silently.
- **Dispatch and adoption verify**: the pinned `{id, digest}` must exist in the retained policy set and each observed object must match its pinned role shape and fingerprint (not the current installation default). A run whose pinned policy is unavailable creates no new resource, stops the ones it has and fails with `policy_unavailable`.
- **Changing the policy** never mutates a pin: a new policy applies to subsequent admissions. Running runs finish under their pin; a pinned policy outliving an incompatible node/profile change is an operator drain, not an implicit upgrade.
- **Cutover**: `legacyFormat: open` admits `v1alpha1` and (when `runShared` is enabled) `v1alpha2`; `drain` refuses new `v1alpha1` runs (`format_retired`) while old runs finish under `attempt_private` semantics; `closed` additionally means no `attempt_private` run remains. Historical definitions, digests, run snapshots and results of the old model stay readable and are served under their original semantics. A controller must not start, adopt, stop or remove a run whose `runModel`/`contextVersion` it does not implement. Rollback stops admission, cancels/drains `run_shared` runs and reconciles their resources **before** reverting the controller; a `run_shared` run is never resumed on an older controller.

## Capabilities

The attempt runtime keeps its six-method port and `Guarantees` unchanged except for one marker. `Guarantees` is a `uint8` with six used bits (`oci_image`, `isolation`, `resource_limits`, `network_policy`, `resource_quotas`, `crash_recovery`); the contract uses **one** spare bit for `run_resources` ("this backend also implements the companion resource port"), reserves the last bit, and does **not** place resource capabilities there. Because persisted bindings and API output are lists of names that decoders reject when unknown, a `run_shared` binding listing `run_resources` is refused by an older controller: an additional protection beside the explicit run-model check. An implementation may widen the Go integer type provided the wire names and the decoding of persisted bindings are unchanged. Optional launch-history proof (`ProvesNeverStarted`) stays a separate flag, never a guarantee or an admission requirement. `Requirements` derives per step, so the run-level marker and the resource capabilities are derived by a run-level function at admission; T-112 chooses its Go shape. The preparation image is policy-pinned by digest (the checkout workload is controller-selected, never document-supplied).

Resource capabilities are a separate named set, `ResourceCapabilities`, with capacity for at least 16 members (a 32-bit set or equivalent), declared by the companion port (`Capabilities()`), pinned beside the binding, and rechecked at every launch:

| Name | Meaning (advertised only when enforced and tested for every run) |
| --- | --- |
| `run_workspace` | One run-scoped scratch filesystem mountable by successive attempts at `/workspace` |
| `workspace_fence` | Stop/remove report a volume released only after verifying no process/mount of any node still uses it; uncertain release blocks reuse |
| `workspace_preparation` | Controller-selected checkout workload with a trusted receipt, read-only credentials and no user code |
| `resource_recovery` | Verify/adopt resources by scope, fingerprint and UID after a controller restart |
| `run_services` | Run-lifetime services with readiness and run-unique endpoints |
| `service_isolation` | A service shares no filesystem, identity or secrets with another workload or `/workspace` |
| `run_network_isolation` | A run's workloads reach that run's declared service ports; other runs (same tenant included), tenants, platform endpoints and cloud metadata are unreachable (traffic-tested) |
| `declared_edges` | Service-to-service access only along declared edges (required only under `isolation: run`; under the tenant model edges are a document rule) |
| `named_egress` | Access to each named profile's destinations after name resolution, for the workloads that name it |
| `egress_default_deny` | Every external destination outside the named profiles denied (required only under `publicEgress: false`) |
| `builder_rootless` | The step role shape qualified for ko/Buildah (user namespaces, mapping helpers, mounts, storage) |
| `builder_service` | The pinned builder-service mode of `role: builder` services |
| `builder_isolated_worker` | Exclusive disposable isolated worker per run for a host-privileged builder mode |

Requirements are derived at admission from the definition and policy, then pinned. **Every** `run_shared` run, including one with no services, no edges and no `egress`, needs `run_workspace`, `workspace_fence`, `resource_recovery`, the `run_resources` marker and the attempt guarantee `network_policy` (the tenant boundary). The pinned policy's network model adds `run_network_isolation` when `isolation: run` and `egress_default_deny` when `publicEgress: false` ([O-30](../decisions.md#revised-scope--2026-10-29-o-30); originally both were unconditional). Beyond that: source-backed adds `workspace_preparation`; any service adds `run_services` and `service_isolation`; any edge under `isolation: run` adds `declared_edges`; any `egress` adds `named_egress`; a `role: builder` service adds `builder_service` (and `builder_isolated_worker` for that mode); the policy's `requires` adds more (for example `builder_rootless`). Missing members make admission fail `capability_unsupported` with their names; another backend never substitutes. Expected, not claimed: Kubernetes advertises the set only after P4/P7 qualification; Podman only the capabilities its tests prove (it keeps network/egress members absent unless an owned filtered path is enforced and tested); Process advertises lifecycle members only (for example `run_workspace`, `workspace_preparation`, `run_services` for explicit host programs) and never isolation, network, egress or builder members, and it does not provide the `network_policy` guarantee, so **no real or new-format admission accepts a `run_shared` run on Process or on any backend missing a required member, even a service-free run**; there is no relaxed production fallback and no flag that drops a member the pinned policy requires. Lifecycle tests of Process exercise the companion port directly, or use the existing explicitly injected test-only no-requirements boundary (`runtimetest.NoRequirements`, which exists only in test composition and never in `internal/app`) with a test-injected requirement policy; neither is an admission path. Local lifecycle evidence cannot certify Kubernetes isolation.

## Resources, identity and fingerprint

| `kind` | `key` | Workload identity | Meaning |
| --- | --- | --- | --- |
| `network` | `network` | none | The run's access boundary: per-run network/policies and endpoint records (adapter-private inventory). Created first, removed last |
| `workspace` | `workspace` | none | The scratch filesystem, bounded by the resolved size |
| `preparation` | `preparation` | yes | One-shot checkout workload (source-backed runs only) |
| `service` | `service.NAME` | yes | One support service (`role: general` or `builder`) |

Isolated builder workers are a placement property of a builder service under its pinned mode, not a separate kind. Per-run objects carry deterministic names: `name = "kr-" + first 24 lowercase hex characters of SHA-256 over the length-prefixed tuple (tenantId, projectId, runId, kind, key)`, a service endpoint host is `"s-" + first 16 hex characters of SHA-256 over the length-prefixed tuple (tenantId, projectId, runId) + "-" + NAME` (at most 49 characters), and adapters add object-type suffixes without exceeding provider limits. **Length-prefixed** has one meaning throughout: each tuple element is its UTF-8 bytes preceded by the byte count as an unsigned 4-byte big-endian integer, and the hash covers the concatenation (the framing of the repository's idempotency and cursor encodings); separator joins are never used because identifier characters such as `_` make them ambiguous. The event/audit resource ID of a resource is `"rr-" + first 40 lowercase hex characters of SHA-256` over the same framing of `(tenantId, projectId, runId, key)` ([event catalog](event-catalog.md#run-resource-events)); the raw `RUN_ID/KEY` is not an identifier. A name never authorizes anything.

- **Spec and fingerprint.** `ResourceSpec` is the canonical, provider-neutral desired state (scope, `runId`, backend, kind/key/name, policy pin and role-shape digest, deadline, and per kind: `workspace {sizeMiB, initial: git|empty}`; `preparation {image digest, repositoryUrl, commit, limits}`; `service {name, role, image, command, args, nonsecret resolved env, secret pins (ref, version, name, delivery), ports, resources, storage, readiness, egress, host}`; `network {services, edges, per-workload egress destination digests, isolation, publicEgress}`, the last two copied from the pinned policy's network model). It contains no secret values, tokens, scratch contents, provider objects or raw Pod fields. `fingerprint = "sha256:" + SHA-256(canonical JSON of the spec)`; UID is excluded.
- **UID binding.** `Ensure` returns the provider's opaque instance identity. Execution binds it durably once; every later operation requires scope, name, fingerprint **and** UID to match. A same-name object with a different scope/fingerprint/UID is a collision: never adopted, modified or removed.
- **No scratch persistence.** Execution stores no scratch bytes, only the identity/evidence below. The preparation receipt is control-plane state ([runtime v2](runtime-v2.md#resource-subjects)).

## Companion port

Execution owns a small run-resource port (a sketch of semantics; T-112 freezes exact Go signatures, memory/PostgreSQL stores and the shared contract suite). It has six methods or fewer ([SD-15](../specs/software-design.md#requirements)), uses domain DTOs only, and carries no Kubernetes/Podman/process vocabulary; provider types stay in adapters.

```go
// Sketch only.
type RunResources interface {
	Capabilities(ctx context.Context) ResourceCapabilities // backend identity + named set
	Ensure(ctx context.Context, spec ResourceSpec) (ResourceObservation, error)
	Observe(ctx context.Context, h ResourceHandle) (ResourceObservation, error)
	Logs(ctx context.Context, h ResourceHandle, bound LogBound) (ResourceLogs, error) // preparation/services only
	Stop(ctx context.Context, h ResourceHandle) (ResourceStopEvidence, error)
	Remove(ctx context.Context, h ResourceHandle) (ResourceRemoveEvidence, error)
}
```

`ResourceHandle` is `{scope, runId, kind, key, name, uid}` (`uid` empty before the first successful `Ensure`). Each method is idempotent, acts only on a verified instance of the pinned backend and never on another backend's resource.

| Method | Contract |
| --- | --- |
| `Ensure` | Create if absent; adopt if scope, name, fingerprint (and UID, when bound) match; otherwise `collision`. Returns the observation with the provider UID. Refuses a handle whose object exists in stopped/removing state (`fenced`): a stopped resource is never started again |
| `Observe` | Never creates. Reports one `phase` ([below](#phases-and-evidence)) with evidence. A missing object is `absent`; inability to determine is `unknown`, never `absent` |
| `Logs` | Bounded tail for diagnostics, redacted by the same rules as attempt logs; denied for `workspace`/`network` |
| `Stop` | Revokes admission/credentials for the resource first, then terminates. `stopped=true` only when no process of the resource can still run **and no pending create can still arrive**; for `workspace`, `released=true` only when no process/mount on any node can still use the volume. `launch` ∈ `not_started`/`started`/`unknown` follows the attempt runtime's rule: a missing record never upgrades `unknown` to `not_started` |
| `Remove` | Requires prior verified `stopped`. Deletes the object and, for `workspace`, its storage allocation, then confirms absence of the verified UID. `removed=false` keeps cleanup pending |

Execution decides desired state, retries and ordering; adapters translate it. `Stop` and `Remove` of an absent resource with no durable ownership record in execution succeed idempotently and touch nothing foreign.

### Phases and evidence

| `phase` | Meaning | Evidence |
| --- | --- | --- |
| `pending` | Exists, not yet ready | UID, spec fingerprint |
| `ready` | Verified usable | `readyAt`; service: probe kind and `readyAt`; workspace: receipt digest (source-backed) matches the bound UID and no holder; preparation: `completed` with receipt |
| `failed` | Exited/failed or startup deadline passed | Safe reason, exit code if observed, bounded log reference |
| `stopped` | Verified no process can run | `ResourceStopEvidence {stopped, launch, released}` |
| `absent` | Verified no object (never created, or removed) | UID last bound |
| `unknown` | Cannot determine (API outage, ambiguity) | Safe reason; blocks dispatch and workspace handoff, never counts as loss |

Execution records `ready`, `stopped` and `removed` evidence durably with timestamps before acting on it. An observation of `ready` older than one reconciliation period is rechecked immediately before each attempt dispatch.

## Lifecycle and ordering

All effects are intent-first: PostgreSQL commits the resource record, then the runtime is called outside SQL transactions, then the observed result is committed with audit/outbox ([execution requirements](../specs/pipeline-execution.md#requirements)).

1. **Admission** (`queued` → `running`, one transaction under the existing quota/work → run/attempt → authority → workspace → event lock order): verify capabilities, policy and ceilings; compute `executionOrder`, effective access, endpoints and the pinned binding; reserve capacity atomically (all services at startup/steady state, preparation, the largest step attempt allowed to run, and the scratch size, charged separately from container ephemeral storage); insert resource intents; pin `startedAt`/`deadlineAt`. Insufficient capacity leaves the run `queued` with a derived reason; it never creates a partial reservation. The reservation covers the largest concurrent set (every service, preparation until done, one attempt and the scratch), so a running run cannot deadlock waiting for quota for its next attempt, and a crash cannot leave unaccounted partial provisioning because the reservation commits before any effect. No runtime call occurs in this transaction.
2. **Provisioning.** `network` first, then `workspace`; then `preparation` (source-backed) concurrently with every `service`. The workspace is `ready` after the preparation receipt is recorded **and** the preparation workload is observed stopped (its credentials are gone). A source-free scratch is `ready` when bound and writable by step workloads (empty only at this initial provisioning; later attempts never clear it). **Preparation retries are internal to one workload.** There is exactly one preparation workload per run: one resource, one `Ensure`, one bound UID, `backoffLimit: 0`/no restart. Inside it the trusted preparation program may repeat its checkout (re-fetch, re-checkout, clean and rebuild its own partial tree; it runs no user code and its credential is the read-only checkout credential) up to `policy.preparationAttempts` tries, all bounded by `policy.preparationTimeoutSeconds` and the run deadline. The try count is **only** that internal counter; it is reported in the receipt (`attempts`) and is not a resource generation, not an `Attempt` row and never recreates, replaces or restarts the workload. Once the workload is `stopped` (success, failure or lost), it is never started again and no second preparation resource or name exists. A preparation that exhausts its tries, exits without a receipt, exceeds its deadline or is lost (`failed`/`absent`/UID mismatch) fails the run with `preparation_failed`; a rerun is a new run with a new scratch. This differs deliberately from step-attempt retry (lifecycle 6), where a new attempt workload may reuse intact scratch because the previous one provably ran no user code. After the receipt is recorded the preparation never runs again. A service that is not `ready` within its `readiness.timeoutSeconds` fails the run (`service_not_ready`).
3. **Barrier.** No attempt is claimed until workspace and every service are `ready`; each claim rechecks workspace/service observation, cancellation, deadline and the pinned policy.
4. **Holder handoff.** An executable attempt holds the workspace from its dispatch intent until it is settled. The next attempt (the next step in `executionOrder`, or a retry) is claimed only after the prior attempt's workload is verified stopped, its result/publication settled, and the workspace observed `released`. A claim records the holder in the same transaction that creates the attempt row; PostgreSQL admits at most one holder per run and one logical-workspace writer per workspace independently. Expiry of a SQL claim or deletion of a Pod is never a fence.
5. **Running.** Services are monitored. Affirmative `failed`/`absent`, a UID/fingerprint mismatch, or readiness failing continuously for `lossSeconds` is **loss**. `unknown` only blocks dispatch until the deadline. There is no transparent replacement of a service or scratch, and no reclone after any attempt began.
6. **Retry.** An attempt may be retried automatically only if **all** hold: its failure class is in `retry.on`, no `begin` was recorded for it **or any earlier attempt of the same step**, the workload is verifiably stopped, the workspace is observed `ready`/`released` with the bound UID and receipt, services are still `ready`, attempts remain and the deadline allows. Anything that may have run user code, an unknown launch, partial service mutation or an uncertain external effect is not replay-safe, regardless of the `infrastructure` label; existing uncertain-effect barriers (PX-07) still apply. The retry reuses the same scratch and services; the step keeps its slot in the order during backoff.
7. **Failure, cancellation, deadline.** Record intent and reason; block new claims and credential grants; revoke admission for resources; stop attempts (existing rules), then stop services and preparation **promptly** without waiting for uncertain source/external effects, which settle through their own reconciliation (publication reads the remote ref and bundle artifact, not scratch). The workspace is stopped only after no holder remains. `run_deadline_exceeded` fails the run, stops resources and invalidates pending gates (`gate.invalidated`, reason `run_deadline_exceeded`).
8. **Terminal completion.** A run becomes `succeeded` only after final result/publication finalization, and **any** run becomes terminal only after every resource is verified **stopped and removed** (workloads gone, volume released, storage allocation and network objects confirmed absent for the bound UIDs). The run's outcome intent (success, failure with reason, cancellation) is recorded as soon as it is decided; the status does not change until cleanup completes. Until then the run stays nonterminal with a visible safe reason (`resource_stop_unconfirmed` while stop is unverified, `cleanup_pending` while removal is unconfirmed) like an uncertain attempt, and stuck cleanup stays visible, retried with backoff and alerting, never hidden by a terminal status. No new run status enum is introduced. Reserved capacity is released only after verified removal of that resource. A terminal run therefore leaves no running service and no reusable scratch.
9. **Restart and orphans.** The reconciler resumes nonterminal runs from durable intents and also scans historical **terminal** records that still show unconfirmed stop/removal (for example left by an interrupted older controller or an earlier policy) and drives their cleanup, without changing their recorded outcome: observe → adopt verified resources → re-`Ensure` an intent that never bound a UID (idempotent: it adopts the same named object if the create actually happened, and never creates a second preparation or replacement generation) only while no attempt has begun and it was never `ready` → mark loss otherwise. Duplicate and reordered wakeups are harmless. An object carrying this installation's ownership marker whose run is terminal or unknown after a grace period is stopped/removed only after verifying scope and fingerprint; foreign or ambiguous objects are reported (`resource_collision`), never deleted. A create that raced cancellation has no admission authority (revoked) and is reaped.
10. **Rerun.** A new run gets a new ID, names, volume, services and pinned inputs. Recovery is Git commits plus retained artifacts; scratch survival is never a checkpoint or recovery guarantee.

### Reasons

Safe, stable codes recorded on resources/run failure intent; none is a new run, step or attempt status:

`preparation_failed` (exhausted tries, no receipt, deadline or lost workload), `service_not_ready`, `service_lost`, `workspace_lost`, `workspace_unverified`, `run_deadline_exceeded`, `policy_unavailable`, `capability_unsupported`, `resource_collision`, `resource_stop_unconfirmed`, `cleanup_pending`, `resource_fenced`. Messages and log references never include credentials, secret values or scratch contents. `workspace_unverified` is the entrypoint's pre-command verification failure and is treated as `workspace_lost`.

### Egress profiles

`egress` names resolve through the pinned policy to destination sets (host/CIDR and ports) authorized by installation (and, later, project) settings. Under `publicEgress: true` they are needed only for private, cluster or platform destinations (an in-cluster registry, a private mirror); public HTTP/HTTPS needs no profile. DNS goes only to the installation resolver; destination enforcement applies after resolution; Kodama callback and Git/artifact endpoints are granted per workload role (only preparation receives checkout access, and only to the repository host). A database never inherits a builder's registry profile; an external preview destination is a named profile such as `preview`, never a general outbound exception. Project-level authorization of profiles is not delivered: until it exists, an installation grants a profile to all projects explicitly or not at all. The network adapter must be traffic-tested for the model it advertises: same-run reachability, foreign-tenant/platform/metadata denial, DNS behavior, private-destination denial without a profile, and (only when advertising `run_network_isolation`/`egress_default_deny`) foreign-run and unnamed public destination denial.

### Builder services

**Deferred (O-30, [T-123](../tasks/T-123-buildkit-builder-service.md)).** No backend advertises `builder_service` or `builder_isolated_worker`, and the stage policy pins `builders.service.mode: none`, so a document with a `role: builder` service is refused at admission (`capability_unsupported`). The contract below is retained for T-123.

A `role: builder` service runs the policy's pinned builder-service mode and is run-exclusive: its own process namespace (no command runner, credential supervisor or unrelated service shares it), bounded storage, no cache or daemon shared with another run, and an endpoint reachable only by that run's workloads. Execution generates per-run client authentication material, delivers the server side to the service through its resource subject and the client side to executable steps as protected attempt-private files, and destroys both on stop; the format is frozen with the qualified profile (T-111) and never appears in a definition. Stop terminates the whole process tree and removes the cache with the volume. A host-privileged mode is a placement of the same service on an exclusive disposable isolated worker with no unrelated workloads or node/cloud credentials, destroyed with the run; it is not offered before `builder_isolated_worker` is advertised. If the selected mode fails, the run fails: nothing silently escalates.

## Observations and API projection

Resource lifecycle emits `run_resource.state_changed` ([event catalog](event-catalog.md#initial-event-types)) with safe references only (run, kind, key, phase transition from `none` or a phase to a phase, reason, fingerprint, whether a UID is bound, which must be true for `ready`); no secrets, endpoints beyond the logical name, or logs. Scratch mutation produces no `workspace.checkpoint_created` and no attempt/checkpoint event. `GET P/runs/{id}` additively exposes `runModel`, `deadlineAt`, `executionOrder`, effective workspace access and a `resources` list of `{kind, key, phase, reason, readyAt, stoppedAt, removedAt}`; logs of preparation/services are `GET P/runs/{id}/logs?resource=preparation|service.NAME`. Both honor current authorization and omit foreign data.

## Qualification required before any support claim

These are contract requirements; none is claimed by this document. Each needs real-backend evidence (P1/P4/P5/P7) naming exact kernel, Kubernetes, CRI/OCI runtime, CNI, CSI/storage, filesystem, LSM/seccomp and image digests: RWOP sharing across different images/UIDs with idmapped ownership and delete reclaim; subordinate ID ranges and mapping helpers; ko and Buildah (`RUN`, multiple ownership IDs, mounts, storage) actually building and pushing under the `step` role; PostgreSQL readiness/data round trip; nested networking under outer CNI policy; traffic tests of the advertised network model (same-tenant reachability, foreign-tenant/platform/metadata denial, public egress, private destinations only through profiles); cancellation/create races, unknown termination and restart adoption; complete resource inventory teardown including scratch. BuildKit/builder-service evidence belongs to T-123 and browser evidence is out of scope (O-30). Missing prerequisites become installation changes (T-116), not silent fallbacks. Until P7 passes, status documents report `run_shared` as designed and unqualified.
