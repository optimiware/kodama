# Enabling `run_shared` (T-116)

**Status: composed and rendered, off by default, not qualified.** The product wiring, the chart and the commands below exist and are unit-tested offline (T-116). Nothing has been installed on a cluster and no run has executed under this model on one. The node profiles are installed and qualified by [T-121](../tasks/T-121-run-profile-requalification.md) and the integrated product is accepted on stage by [T-117](../tasks/T-117-run-resource-acceptance.md); until then this runbook is the procedure to follow **on a cluster you intend to qualify**, not support evidence. Contract: [ADR 0010 (Amendment O-30)](../adr/0010-run-workspace-services-builders.md#amendment-o-30-2026-10-29), [run resources and execution policy](../contracts/run-resources.md), [runtime v2](../contracts/runtime-v2.md), [pipeline `kodama/v1alpha2`](../contracts/pipeline-format-v1alpha2.md). Implementation: [assembly status](../implementation/assembly-status.md#run_shared-assembly-t-116).

`run_shared` gives a run one disposable `/workspace` (one PVC) that every executable step mounts, run-scoped support services (for example PostgreSQL) and ko/Buildah as ordinary steps under a builder-capable step role. The default `attempt_private` model and the fixed `restricted` baseline stay exactly as they are for an installation that does not enable it.

## What enabling renders

`runShared.enabled=true` in the chart adds, per installation:

| Piece | Rendered as |
| --- | --- |
| Settings section | `runShared:` in the server's `config.yaml`: the run_shared workload identity, cluster CIDRs, the `builder_rootless` vouch and the append-only execution-policy list. Its presence is what composes the model: the server then builds the Kubernetes run-resource adapter, the resource authority, the `/runtime/v2` routes, the prior-disclosure scan/redaction, the resource logs and the additive run view, enables `kodama/v1alpha2` registration and binds the execution policy source |
| Execution policy | The stage policy of O-30 (below), digest-pinned by content. `admission.runShared: closed` renders the section but admits no new run |
| Tenant namespaces | Pod Security `baseline` (enforce, audit, warn) at the pinned `runShared.podSecurity.version` instead of `restricted`. The server's namespace preflight checks level and version against the policy's pin |
| ResourceQuota | `persistentvolumeclaims`, `requests.storage` and `services` beside the existing quota |
| Controller RBAC | Namespaced: create/get/list/delete `persistentvolumeclaims` and `services`, create `pods`, create/delete `networkpolicies` (service Pods, per-run policies), Secret management (record Secrets, `execution.rbac.manageSecrets=true` is required and enforced). Cluster-scoped, read only: `get`/`list` `persistentvolumes`, `list` `volumeattachments`, `get` `storageclasses` (a scratch claim is released only when no Pod uses it and no VolumeAttachment of its volume remains) |
| Image | The preparation role's image is the digest-pinned release image (it contains `git` and runs `kodama prepare`); services with secret bindings run `kodama service-run` through the injected binary |

Disabled (the default) the chart renders none of this, the namespaces stay `restricted`, and the server refuses `kodama/v1alpha2` registration and admission exactly as before.

## Prerequisites

1. **Node profiles (T-120).** The seccomp and AppArmor profiles named by the step role (`kodama-rootless-build-v1`) are installed and ready on **every worker node**: [run builder node profile](run-builder-profile.md). Kodama cannot read node files. A missing profile is refused by the kubelet before anything runs (container creation fails for a missing seccomp file, the Pod is rejected for a missing AppArmor profile), never inside Buildah and never with a weaker profile, but the installation should not depend on that signal: the chart **refuses to render** `run_shared` with a named profile until the operator sets `runShared.nodeProfilesInstalled=true` after checking one Ready installer Pod per node.
2. **Builder image.** Buildah's subordinate ID ranges exist only for UID 1000 (the image's `build` user). The run_shared workload identity (`runShared.identity`, default 1000/1000/1000) is separate from the legacy identity (`execution.job.runAsUser`, 65532): legacy Jobs are unchanged.
3. **Storage.** A CSI storage class that supports `ReadWriteOncePod`, delete reclaim and the Pod user-namespace/idmapped mounts. Stage: `sbs-default` (`WaitForFirstConsumer`, `Delete`). The adapter refuses a class whose reclaim policy is not `Delete`; a `WaitForFirstConsumer` claim is ready as soon as it is Pending without a volume (the first consumer binds it).
4. **Network.** A CNI that enforces NetworkPolicy (Cilium on stage). The chart keeps the namespace default deny, DNS and callback rules; the per-run policies the controller creates add the tenant model: other tenants, the control plane, the Kubernetes API, node and cloud-metadata addresses stay denied, and public egress is TCP 80/443 to non-private addresses except `runShared.clusterCIDRs` (list the Pod, Service and node ranges that are not private; they are never reachable). Private destinations need a named egress profile.
5. **Secrets.** `execution.rbac.manageSecrets=true`: every run resource keeps its record and admission credential in a Secret of the tenant namespace.
6. **Quota and capacity.** Pods, CPU, memory and ephemeral storage stay under `execution.quota`; set `runShared.quota` for claims and Services. Services and scratch are reserved for the whole run, gate waits included, so size the run-deadline ceiling and PVC capacity for your concurrency.
7. **Migrations.** `kodama migrate` applies the additive `005_artifact_workspace_provenance.sql` (the last contribution, `artifacts-workspace-provenance`) before the controller serves run_shared: artifacts of a run_shared step record the workspace state the server saw when the step's command began (clean or modified) and a reference to the lineage. A server over a database without it refuses to reserve such an artifact instead of dropping the record. An artifact built in a **modified** workspace is a usable artifact but never stands for the approved commit: gates and deployments see no repository or commit for it.

## The stage values

Everything else keeps its default. Replace the placeholders.

```yaml
runShared:
  enabled: true
  nodeProfilesInstalled: true     # only after one Ready installer Pod per worker node
  admission:
    runShared: closed             # open it in the cutover below
    legacyFormat: drain           # the namespaces are baseline once enabled: see the cutover
  identity: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
  podSecurity: {version: v1.31}   # the stage cluster's tested version; never `latest`
  clusterCIDRs: ["10.32.0.0/12"]  # PLACEHOLDER: Pod, Service and node ranges that are not private
  builderRootless: false          # true only after T-121 qualified a real rootless build
  policy:
    id: run-v1
    workspace: {storageClass: sbs-default, idmapped: true}
    network: {isolation: tenant, publicEgress: true}
    roles:
      step:
        capabilitiesAdd: [SETUID, SETGID]
        seccomp: kodama-rootless-build-v1
        lsm: kodama-rootless-build-v1
    builders: {rootless: [ko, buildah]}   # builder service mode is `none` until T-123
    requires: []                          # add builder_rootless once qualified and vouched
execution:
  rbac: {manageSecrets: true}
```

Limits (`runShared.policy.limits`) default to the contract's example numbers: 6 h run ceiling, 8 services, 2 GiB default and 8 GiB maximum scratch, preparation 10 min with 2 attempts, step allowance 2 CPU / 2 GiB memory / 8 GiB ephemeral. Registry access for ko/Buildah pushes goes through a named egress profile (`runShared.policy.egressProfiles`), or public egress when the registry is public.

Enabling `builder_rootless` (`runShared.builderRootless: true` **and** `policy.requires: [builder_rootless]`) is a **new policy id**, because it changes the policy body; follow the append-only procedure below.

## Order of operations (cutover)

**A tenant namespace has one Pod Security level, so a namespace is either `restricted` (the fixed `attempt_private` baseline) or `baseline` (the `run_shared` step role).** The server's namespace preflight for an `attempt_private` attempt requires `restricted` and refuses to dispatch in a `baseline` namespace (visibly, fail closed), while a `run_shared` attempt requires the policy's level. Enabling `run_shared` therefore flips the namespaces and ends `attempt_private` dispatch in them: the cutover is a drained switch, not a coexistence. (This is the finding behind step 2; whether `attempt_private` dispatch should also be allowed in `baseline` namespaces during the acceptance window is an owner decision for T-117, not something this chart assumes.)

1. **Profiles and prerequisites** above. Install the node profiles first ([runbook](run-builder-profile.md)); confirm one Ready Pod per node.
2. **Drain `attempt_private` runs.** Stop starting `kodama/v1alpha1` runs and wait until none is active in the tenant namespaces (`kodama run list`, including their cleanup). Historical definitions, digests, snapshots and results of finished runs stay readable throughout.
3. **Render with admission closed.** `helm upgrade` with the values above: `enabled: true`, `admission.runShared: closed` and `admission.legacyFormat: drain`, so a new `kodama/v1alpha1` run is refused at admission with the stable reason `format_retired` instead of failing at dispatch. The server restarts composed: `kodama/v1alpha2` definitions can be registered, nothing is admitted yet, and the namespaces are `baseline` at the pinned version. Watch the server log for the composition and `GET /readyz`.
4. **Open admission on the trusted-engineer stage.** Change `admission.runShared` to `open`. That changes the policy body, hence its digest: give the policy a **new `id`** (`run-v2`) and move the previous body, exactly as rendered, into `runShared.retainedPolicies`. Run the T-117 matrix (ko, Buildah, PostgreSQL service, cancel/restart, cleanup) with `kodama run get` / `kodama run logs --resource`.
5. **Close.** `legacyFormat: closed` records that no `attempt_private` run remains. Deployment cleanup runs are admitted from the project's `kodama-cleanup` definition: register a `kodama/v1alpha2` version of it before draining, or cleanup of expired deployments is refused with `format_retired`.

`legacyFormat: open` with `enabled: true` admits `kodama/v1alpha1` runs that the preflight then refuses to dispatch in the `baseline` namespaces; the chart prints a notice for that combination and the values default (`open`) exists for the period before the switch.

### The policy list is append-only

A run pins `{id, digest}` and the controller verifies every resource against that pinned body, never against the current policy. Therefore:

* never edit a rendered body in place; give a changed policy a new `runShared.policy.id` and append the previous complete body (as it was rendered) to `runShared.retainedPolicies`, oldest first. The chart always renders the current policy last, and the server treats the newest as current;
* the server refuses a list with a repeated `id` or digest and any policy whose body does not validate (an unknown member, a capability beyond `SETUID`/`SETGID`, an unpinned preparation image, a non-`ReadWriteOncePod` workspace);
* a pin that is no longer in the list stops that run's resources and fails it with `policy_unavailable`. Remove a retained body only after every run that pinned it is terminal and cleaned up.

## Observe

* `kodama run get <run>` shows `runModel`, `deadlineAt`, the frozen `executionOrder` with each step's effective workspace access and the safe `resources` (`workspace`, `network`, `preparation`, `service.NAME`: phase and reason, no UID/spec/endpoint).
* `kodama run logs <run> --resource preparation` and `--resource service.NAME` read the bounded log of a preparation or service (redacted for every secret the run disclosed to any step or service).
* Events are the catalog's `run_resource.state_changed`; stuck cleanup is reported as `resource_stop_unconfirmed` / `cleanup_pending` and retried, never hidden.

## Roll back and uninstall

* **Stop admission first**: set `admission.runShared: closed` (a new policy id). Then cancel or drain `run_shared` runs, wait until every such run is terminal, every resource it listed carries removal evidence (`removedAt` set; removed resources stay listed in `kodama run get`, they are not deleted from the view) and `cleanupReason` is `null` (no `cleanup_pending` or `stop_unconfirmed`), and only then revert the chart or the controller. **A `run_shared` run is never resumed on an older controller**: an older controller refuses to start, adopt, stop or remove a run whose model it does not implement, and a `run_shared` binding names a guarantee it rejects.
* **Disabling the section** (`runShared.enabled=false`) removes the composition. It does not delete anything: run scratch (PVCs), Services, per-run policies, preparation Jobs, service Pods and record Secrets of runs that are still nonterminal stay in the tenant namespaces until those runs are cleaned. Reconcile first.
* **Uninstall and offboarding.** Run scratch, services and per-run policies are disposable: cancel runs, wait for verified removal. Scratch contents are deleted with the volume and are never restored; Git refs, artifacts and registry content follow their own retention. The tenant namespaces and the removal tombstones (`<name>-tomb`, one small Secret per removed resource) are `keep` resources and go with the namespace at offboarding. The node profiles are removed with their own [uninstall](run-builder-profile.md#roll-back-and-uninstall) after no Pod references them.
* The tenant namespaces return to `restricted` only when the section is disabled **and** no run_shared Pod remains: a `restricted` namespace refuses the step shape. `attempt_private` dispatch resumes then (and only then): `legacyFormat` has no effect once the section is disabled, and the server admits `kodama/v1alpha1` again exactly as it did before enablement.

## What is not claimed

Process and Podman do not compose `run_shared` yet (they refuse the configuration; production composes Kubernetes only). No combination is qualified: T-121 qualifies the profiles, ko/Buildah, RWOP/idmapped storage, PostgreSQL and the tenant network model on the stage cluster, and T-117 accepts the assembled product. BuildKit, builder services and the isolated-worker mode are deferred to T-123; browser support is removed (O-30). See [assembly status](../implementation/assembly-status.md#run_shared-assembly-t-116) for exactly what the server composes and what it does not.
