---
status: accepted
---

# ADR 0010: Run-scoped workspace, support services, and rootless builders

**Date:** 2026-10-26  
**Direction:** owner-requested and scheduled (O-28, 2026-10-26). Shared workspace, services, builder support, and retained isolation/publication invariants are selected direction.  
**Disposition:** accepted by the T-109 contract checkpoint as the normative design of the **versioned `run_shared` model**. This is not delivery or qualification: the current `attempt_private` model (`kodama/v1alpha1`, runtime v1, fixed `restricted` baseline) remains the only executable model until the T-116 cutover and the T-117 acceptance. Values that depend on real kernel/runtime/storage/CNI behavior (security shapes, mapping ranges, builder modes, the supported matrix) stay qualification-dependent: T-111 freezes them, and no combination is a supported builder until its evidence exists.  
**Repository baseline:** `github.com/optimiware/kodama` at `7a66d3493f2bc69d197406f757841e603efbb0c1`.  
**Register:** [O-28](../decisions.md#run-workspace-and-builders--2026-10-26-o-28).  
**Implementation:** [implementation plan](../implementation/run-workspace-services-builders.md).  
**Contracts:** [pipeline `v1alpha2`](../contracts/pipeline-format-v1alpha2.md), [runtime v2](../contracts/runtime-v2.md), [run resources and execution policy](../contracts/run-resources.md); requirements WS-13–WS-16, PX-17–PX-22, IT-14–IT-16, VS-14, DQ-16 in the owning specifications.

## Context

Today each attempt creates a private checkout. Kubernetes mounts an attempt-local `emptyDir` at `/workspace`; Podman supplies an attempt-local tmpfs. The entrypoint checks out source before each command. `workspace.from` selects the original run commit or an ancestor writer's published commit. Uncommitted files, generated assets, dependencies, and build context consequently do not flow between steps.

The Kubernetes template fixes non-root execution, no privilege escalation, all capabilities dropped, and `RuntimeDefault` seccomp; tenant namespaces enforce Pod Security `restricted`. These controls can prevent rootless builders from creating user namespaces, installing subordinate UID/GID maps, mounting build filesystems, and running build commands. Requiring builders to fit that policy does not deliver Buildah or BuildKit support. `ko` itself needs no container daemon or nested mount privileges, but does need writable Go/build caches, registry credentials, and permitted dependency/registry access.

A database, browser, or builder daemon has a run lifetime rather than an attempt lifetime. It needs readiness, stable run-local endpoints, bounded resources, and cancellation/recovery ownership. The logical work-item workspace and existing Git publisher already provide durable source continuity; runtime files must not become another checkpoint authority.

## Decision

### 1. One disposable workspace per run

Allocate one private scratch filesystem per run. Every executable step mounts it at `/workspace`, including steps using different digest-pinned images. Gates create no workload or filesystem mount. A source-backed run prepares its exact admitted commit once before user commands start; a source-free deployment-cleanup run receives an empty scratch filesystem and no Git authority.

Preparation is a controller-owned, digest-pinned checkout workload with read-only repository access. It verifies the commit, disables hooks and implicit submodule recursion, removes checkout credentials, and records a completion receipt bound to run, backend instance, and scratch identity. Retries are internal to the single preparation workload (bounded tries by the trusted program, no recreate after it stops): it may repair its own incomplete checkout before any user command starts. Successfully prepared scratch is never reset, recloned, or reconstructed between steps, on retry, or after controller restart. A marker writable by tenant code is not preparation/admission evidence.

The initial Kubernetes implementation uses one bounded, dynamically provisioned CSI PVC per run, with `ReadWriteOncePod` access and a delete-reclaim storage class. This is a run-lifetime scratch allocation, not a durable source PVC. It has no cross-run reuse, snapshot/restore API, or checkpoint acknowledgment. The CSI filesystem must support the selected user-namespace/idmapped-mount configuration. Podman uses a run-owned volume/directory; Process uses a run-owned temporary directory for lifecycle tests.

Services have their own bounded temporary storage. They do not mount `/workspace`, `.git`, attempt identity, or another workload's secrets. A remote builder receives build context through the client, rather than mounting the run checkout.

Outputs, result manifests, identities, and delivered credentials remain in separate per-attempt paths. Writable builder graph/run roots and runtime directories have explicit bounded mounts outside `/workspace`; shared source is not shared credential storage.

### 2. Explicit shared-filesystem scheduling and source semantics

Initially allow one executable attempt to hold a run's workspace at a time. Include nonpublishing steps: today's `read` access allows filesystem writes. Different runs remain concurrent; support services run concurrently with steps.

Freeze a deterministic topological order at admission, breaking ties by declaration order. Process executable steps and gates in that order, preserving `needs`, conditions, mandatory gates, explicit output dependencies, and fail-fast behavior. A prior step must finish result/publication settlement and relinquish its workload/mount before the next executable attempt starts. An expired SQL claim or deleted Pod object is not a storage fence; uncertain termination prevents workspace handoff.

Use a new pipeline format, `kodama/v1alpha2`, because shared mutable contents cannot preserve `v1alpha1` per-attempt snapshot semantics:

- All executable steps receive `/workspace` implicitly.
- `workspace.access: read|write` expresses publication authority, not filesystem mutability. `none` remains available to source-free runs and is rejected for source-backed runs; it never claims filesystem isolation. Omission defaults to `read` for a source-backed run and `none` for a source-free run, explicit in admitted context.
- Remove `workspace.from`: an ancestor reference cannot select or rewind a separate filesystem snapshot. Explicit writer `commit` outputs still identify published commits.
- Each container step supplies its own digest-pinned image; agents retain immutable profile/image expansion. Nonwriting steps need command dependencies, not Git merely for checkout. Writers need qualified Git publication tooling.
- Run input `sourceCommit` stays immutable. Separately record the latest successful in-run publication and source/provenance used by each attempt. Nonpublishing edits may be consumed and later committed by a writer; those edits do not create a checkpoint.

Retain workspace-wide writer reservation, active-attempt checks, ancestry validation, temporary bundle transport, and publisher-owned expected-tip compare-and-swap. Only the publisher receives canonical-branch write credentials. Human pushes still cause conflicts; uncertain pushes still block writer reassignment. Run scratch ownership and the logical workspace's publication slot are separate constraints.

Protected builds/review/deployment must not label dirty source as an unchanged approved commit. Preserve candidate-bound evidence and gate checks; record generated/dirty build inputs accurately, and require publication/new applicable review when candidate source changes. Protected successors may consume a registered approved artifact digest independently of mutable scratch.

### 3. Declarative run support services

Add `spec.services`, keyed by validated service name. Each declaration contains a digest-pinned image, optional argv/command, nonsecret environment and explicit version-pinned secret bindings, named ports, bounded resources/storage, and a bounded readiness probe. Support TCP, HTTP, or exec readiness with explicit timeouts. Service expressions may use pinned run inputs and declared service endpoints, never later step outputs. Initially start services independently and wait for all; do not introduce service-start dependency cycles.

Start services with the run, alongside workspace preparation. Dispatch no user command until preparation and every service are ready. Kubernetes uses separate run-owned Pods with `restartPolicy: Never` and internal ClusterIP Services; Podman uses containers on a run network. They are run resources, not fake pipeline steps or sidecars repeated in each attempt Job.

Publish a typed endpoint map in immutable attempt context. Whole-scalar expressions such as `${{ services.db.host }}` and `${{ services.db.ports.sql }}` resolve from it. Names are unique per run: `db` never creates a tenant-wide database endpoint. Users cannot supply Kubernetes namespaces, selectors, host ports, NodePorts, LoadBalancers, or ingress.

Each service has its own workload identity and only its declared secret-resolution authority. It cannot submit step results, publish source, call model/tool proxies, or acquire arbitrary tenant credentials. Secret bindings are pinned and audited at admission/use. Registry push credentials normally stay with the build client and are forwarded through a scoped build session when needed.

Readiness failure, unexpected service exit/loss, or sustained readiness loss fails the run and initiates settlement. Initially do not silently replace a failed database/browser with empty state. Service data and browser sessions are ephemeral.

Services remain alive during gate waits because their lifetime is the run. Gates themselves create no compute. Pin an installation-bounded run deadline covering preparation, services, execution, retries, and gate waits; reserve/account for service resources throughout that interval. This intentionally changes the blanket statement that a waiting run consumes no compute.

### 4. Run-only networking

Keep tenant namespaces, default-deny ingress/egress, per-tenant quotas, scoped service accounts, and no mounted Kubernetes API token. Add controller-owned rules matching both tenant and run identity:

- Steps reach only that run's declared service ports. Deny other runs in the same tenant as well as other tenants.
- Services accept declared run-local clients/ports. A service-to-service edge must be declared, not inferred from tenant membership.
- External access requires installation/project-authorized destinations. Builds, Git, DNS, artifact transfer, and Kodama callbacks get only the endpoints/ports required by the workload. A database does not inherit a builder's registry egress.
- Browser access to an external preview is explicitly authorized, not a general outbound exception.
- ClusterIP/DNS discovery is not authorization. Enforce ingress and egress with the actual CNI; restrict DNS to the installation resolver and enforce external destinations after name resolution.

Update namespace preflight to recognize only the new owned selector-based policies and existing qualified DNS shape. Replace overlapping broad allow policies because Kubernetes NetworkPolicies are additive. Verify labels against committed ownership; workloads cannot create Pods or change labels/policies.

Rootless nested networking remains inside the Pod/container network boundary, using user-mode networking where needed. A builder's internal `network.host` means its scoped container network namespace, never Kubernetes `hostNetwork`. It does not bypass default deny. No control-plane PostgreSQL, broker, node, cloud-metadata, or arbitrary Kubernetes API access is granted.

### 5. A versioned builder-capable tenant baseline

Replace the fixed restricted-container baseline for new runs with a versioned, operator-installed rootless-builder policy. Pin its digest/guarantees at admission and verify at dispatch/adoption. Ordinary tenant execution remains nonprivileged; pipeline documents cannot inject arbitrary security contexts.

| Area | Required behavior |
| --- | --- |
| User namespaces | Use Pod user namespaces (`hostUsers: false`) and enable nested unprivileged user namespaces on qualified nodes. Run ko/Buildah as a non-root in-container user; qualify subordinate UID/GID mappings and `newuidmap`/`newgidmap` helpers. |
| UID/GID capacity | The Pod mapping must contain the builder image's subordinate ranges. A 65,536-ID Pod range does not cover stock images mapping above 100,000; qualify larger node `idsPerPod` allocation or rebuild the pinned image with compatible mappings. |
| Escalation/capabilities | Permit set-ID mapping helpers instead of hard-coding `allowPrivilegeEscalation: false`. Start from dropped capabilities and permit qualified minimal helpers, initially `SETUID`/`SETGID`; exclude host-level `SYS_ADMIN`, `NET_ADMIN`, and privileged ordinary steps. Capabilities remain in the Pod user namespace. |
| Syscalls/LSM | Install versioned seccomp/AppArmor/SELinux configuration permitting qualified namespace/mount operations: `clone`/`clone3`, `unshare`, `setns`, `mount`, `umount2`, and selected runtime mount API needs. Do not equate `RuntimeDefault` with demonstrated support. |
| Writable storage | Writable HOME, XDG runtime/cache, `/tmp`, isolated Buildah graph/run roots. Qualify rootless overlay on compatible backing storage and support VFS. Optional FUSE overlay needs a scoped `/dev/fuse` device grant, not arbitrary host paths/devices. |
| Build networking | User-mode nested networking and authorized DNS, dependency mirrors, registries, and run services, with outer CNI enforcement. |
| Bounds | CPU, memory, PIDs, service count, volume capacity, ephemeral storage, and total run lifetime. Charge PVC storage separately from container ephemeral storage. |
| Host boundaries | No host PID/IPC/network namespaces, host paths, host engine socket, or shared host build store. No service-account token automount. |

Use Pod Security `baseline` with a tested Kubernetes policy-version pin, plus narrower Kodama template/adoption checks and installation admission constraints. `restricted` capability/escalation rules cannot remain enforced while enabling mapping helpers. Install LSM profiles/node configuration through installation management, not tenant init containers. If the provider cannot enforce this profile, provide a qualified builder-capable node pool rather than accepting runs that subsequently fail inside Buildah.

Qualification must actually build and push with ko and Buildah. Buildah coverage includes Containerfile `RUN`, multiple UID/GID ownership, mounts, and storage. `--version`, mocked security contexts, or passing ko alone are insufficient.

### 6. BuildKit: rootless first, scoped service when necessary

First qualify normal rootless BuildKit under the same baseline with writable state, a compatible snapshotter, user namespaces, and permitted mount/proc behavior. Where supported, `procMount: Unmasked` is a specific user-namespaced BuildKit profile with installation/admission qualification, not a global relaxation.

If the platform still requires `--oci-worker-no-process-sandbox`, use a separate run-exclusive builder service with its own process namespace, credentials, bounded scratch, and authenticated run-only endpoint. No command runner, broader-authority credential supervisor, or unrelated service shares its process namespace. Stop the whole workload on teardown, including leftover build subprocesses. Never share its daemon/cache across runs.

If BuildKit needs actual host privilege, a namespace or labels alone do not establish scope: allocate an isolated worker/VM exclusively to the run, with no unrelated tenant/control-plane workloads and no node/cloud credentials accessible to builds. Destroy that worker with the run. This is a builder-service backend/profile under the same execution resource lifecycle, not permission for privileged ordinary tenant Pods.

The installation selects and pins a qualified mode; a failed rootless attempt never silently escalates. Expose the mode in admission, diagnostics, and retained evidence. Each mode's support claim requires evidence for that exact mode.

### 7. Durable ownership, cancellation, and recovery

Execution owns desired run resources and reconciliation. Persist tenant/project/run, backend, deterministic resource key, spec fingerprint, observed UID, preparation/readiness evidence, deadlines, stop intent, and removal evidence. Runtime calls stay outside SQL transactions; decisions commit with audit/outbox. Adapters translate records into volumes, networks, services, and workloads.

Retain the six-method attempt `execution.Runtime` port; add a small companion execution-owned run-resource port: ensure, observe, logs, stop, remove, plus its own capability declaration (six methods). These resources are not another public business aggregate. `internal/app` wires both ports from the selected backend. Capabilities describe workspace sharing, run-service isolation, and qualified builder profiles without overstating Process/Podman guarantees.

Cancellation/failure blocks preparation/service/step claims and revokes runtime/credential grants. Stop attempts/services promptly while reconciling uncertain source/external effects; preserve evidence needed to settle those effects. Capture bounded diagnostics and remove run resources. Success uses the same teardown after result/publication finalization. Keep cleanup intent until removal is confirmed; a run is terminal only after every resource is verified stopped **and** removed, so terminal completion leaves neither running services nor reusable scratch. Stuck cleanup keeps the run nonterminal with a visible safe reason and is retried; the decided outcome is recorded meanwhile and no new run status enum appears. Cleanup/orphan reconciliation survives restart and also scans historical terminal records with unconfirmed cleanup left by an interrupted older controller.

Names alone never authorize adoption/deletion: verify scope, pinned spec, and UID. Late creates racing cancellation are revoked and reaped. Missing objects cannot upgrade `LaunchUnknown` to never-started proof. Do not transfer a writable volume while a prior process may still use it.

After controller restart, adopt the same verified workspace/services if intact. If scratch or an essential service is lost, fail with an explicit resource-loss reason; do not reclone and resume later steps as though files survived. A rerun gets a new ID, volume, services, and pinned inputs. Recovery remains Git commits plus retained artifacts. PVC writes never acknowledge a source checkpoint.

Initially allow automatic attempt retries only when launch is affirmatively known not to have reached the command and scratch remains intact. A command that may have changed scratch/services is not replay-safe just because failure is classified `infrastructure`. Preserve existing uncertain-external-effect barriers.

## Details frozen by the contract checkpoint

The sections above are the design; T-109 froze these choices where they were open, so dependent tasks implement one reading:

- **Source defaults and canonical form.** Whether a run has source is a run property. The definition's canonical form leaves `workspace` absent when absent (no source-dependent default and no installation-dependent default is filled); admission resolves and records effective access, scratch size and limits in the run snapshot. Contract-constant defaults (ports protocol, probe period, `shmMiB`) are filled. `v1alpha1` canonical bytes, digests and `ResultHash` are untouched.
- **Ordering.** Lexicographically smallest topological order by declaration index; gates hold their position; settlement means workload stopped, result/publication settled, volume released.
- **Pre-command proof.** A server-recorded command-begin fence (`POST /begin`) rather than runtime launch history proves pre-command; it works on every backend and keeps `ProvesNeverStarted` optional and separate.
- **Network is unconditional.** Every `run_shared` run requires run-network isolation and external default deny, even without services or egress; a backend lacking them (Process, today's Podman) admits no `run_shared` run.
- **Capabilities.** One spare bit of the attempt `Guarantees` (`uint8`, six bits used) marks `run_resources` and doubles as a decode-time refusal for older controllers; all finer capabilities form a separate named set. Provider types never enter domain contracts.
- **Pins.** Execution policy `{id, digest}`, runtime binding, capabilities, run model and context version are pinned at admission and survive restart/adoption; policies are append-only and retained while pinned.
- **Services.** Required resources/readiness, bounded ports/storage, declared directed edges, named egress profiles (never hosts/CIDRs in documents), at most one `role: builder` service per run; endpoints resolve at admission from typed expressions.
- **Cleanup and terminal state.** A run is terminal only after every resource is verified stopped **and** removal is confirmed (the original ADR direction: terminal completion includes stop/removal); until then it stays nonterminal with a visible safe reason (`resource_stop_unconfirmed`, `cleanup_pending`), cleanup is retried, and capacity is released after verified removal. Historical terminal records with pending cleanup are still reconciled after a crash.
- **Preparation.** Exactly one preparation workload and UID per run; checkout retries are internal to it and bounded; a lost or failed preparation fails the run, unlike step-attempt retry.
- **Cutover.** Explicit opt-in format capability (off by default), `open`/`drain`/`closed` legacy admission, old runs drain under original semantics, older controllers refuse `run_shared` runs.

## Compatibility and consequences

- Introduce new immutable pipeline/runtime context versions; keep historical definitions, digests, results, and `v1alpha1` semantics readable. Drain old-format runs under their pinned policy before disabling old-format admission. Never reinterpret old `workspace.from` or hashes.
- Amend ADR 0005's private-scratch-per-attempt requirement while retaining Git-checkpoint/CAS rationale. Amend ADR 0008 for the companion seam, profiles, and inter-run isolation semantics. Retain ADR 0001's one Job/primary container per executable attempt and `backoffLimit: 0`.
- Explicitly update P-06/P-07/P-11/P-14 and restricted-baseline language in O-07. C-12 remains true: scratch is disposable. C-03 tenant separation and C-21/C-22 backend/environment selection remain in force.
- Steps in a run share a filesystem trust domain. Credentials remain scoped/outside shared mounts, but a command explicitly given a secret can copy it into shared files. Publication/output filtering must account for relevant prior run disclosures, not just current-attempt values.
- Serial execution reduces intra-run parallelism. Services/storage hold resources during gates. One checkout and retained generated files reduce repeated Git/network work.
- Kernel/CRI/CSI/LSM/CNI combinations need qualification. This ADR does not assert the current stage cluster already satisfies the new profile.

## Alternatives considered

1. **Private clones with artifact transfers:** retain current behavior but do not provide the requested shared working directory or arbitrary uncommitted scratch.
2. **One Pod with all step images/services:** simplifies `emptyDir` sharing but couples retries, admission, secret/resource scopes, and gates to a long-lived Pod, changing native attempt Jobs.
3. **Unrestricted concurrent writable steps:** race in source, `.git`, generated files and publication. Serialized handoff gives a testable first contract.
4. **Broad privileged execution/shared Docker or BuildKit daemon:** widens host/cross-run authority, conflicting with isolation and scoped credentials.
5. **Keep `restricted` and reject incompatible builders:** fails the requested product capability.

## Evidence references

Local owners at the pinned commit: `docs/specs/pipeline-execution.md`, `docs/specs/workspaces.md`, `docs/specs/identity-tenancy-and-rbac.md`, `docs/specs/variables-and-secrets.md`, `docs/specs/delivery-and-quality-gates.md`, `docs/contracts/pipeline-format.md`, `docs/implementation/step-entrypoint.md`, and `contracts/runtime/README.md`.

Implementation reviewed: `internal/kube/template.go`, `internal/kube/policy.go`, `internal/kube/preflight.go`, `internal/runtime/podman/program.go`, `internal/execution/runtime.go`, `internal/execution/context.go`, `internal/entrypoint/source.go`, and Helm tenant namespace/network templates.

Upstream documentation inspected on 2026-10-26; these are design references, not qualification of a release/image:

- [BuildKit rootless deployment and no-process-sandbox limitations](https://github.com/moby/buildkit/blob/28a12c50ad96f155947c62831c144f83a30a0e48/docs/rootless.md).
- [Buildah mapping helpers, capabilities, and rootless storage](https://github.com/containers/buildah/blob/8d751b3ab02cd9763136406d572b6f318bfab9e2/docs/tutorials/05-openshift-rootless-build.md).
- [Kubernetes user namespaces: idmapped filesystems, ID ranges, and Pod Security](https://github.com/kubernetes/website/blob/a7bc475d34acc95bea1d71dd167f7d72d708ed14/content/en/docs/concepts/workloads/pods/user-namespaces.md).
- [ko builds without a Docker daemon](https://github.com/ko-build/ko/blob/932656364f069bfef8e8c0b2e31572b5d4cad383/docs/index.md).
