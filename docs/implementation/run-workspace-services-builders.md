# Implementation plan: run workspace, support services, and builders

**Date:** 2026-10-26  
**Status:** implementation scheduled by the owner on 2026-10-26; contract and qualification checkpoints pending.
**Design:** [ADR 0010](../adr/0010-run-workspace-services-builders.md).  
**Register:** [O-28](../decisions.md#run-workspace-and-builders--2026-10-26-o-28).  
**Baseline:** `main` at `7a66d3493f2bc69d197406f757841e603efbb0c1`.

**Reviewed contract checkpoint:** T-109 merged at `9db322ffd37a4279f8a7ac1a404a06e46533527d`. The [pipeline v1alpha2](../contracts/pipeline-format-v1alpha2.md), [runtime v2](../contracts/runtime-v2.md) and [run-resource](../contracts/run-resources.md) contracts there supersede the proposal's detail where explicitly amended. Original executable behavior remains `attempt_private`; no new runtime/build capability is delivered by this documentation checkpoint.

## Outcome

A run prepares its pinned Git commit once and carries one disposable `/workspace` through all executable steps, each with its own pinned image. Declared database/browser/builder services start with the run, are reachable only through run-scoped policy, and disappear on completion/cancellation. Rootless ko and Buildah work in the tenant baseline; BuildKit works there or through a selected run-exclusive builder service. Preserve expected-tip Git publication, tenant ownership, scoped credentials, external default deny, and uncertain-effect reconciliation.

## Design baseline and change disposition

This is an intentional change to today's private-attempt checkout and fixed `restricted` baseline. Carry the pinned repository commit into task design baselines; refresh after the contract checkpoint merges. Preserve historical ADR rationale and reserve old requirement IDs when replacing meaning.

| Owner | Affected requirements/contracts |
| --- | --- |
| Pipeline/execution | PX-01–PX-04, PX-06–PX-16; pipeline format; `contracts/pipeline/v1`; `contracts/runtime/v1` |
| Workspace/publication | WS-02–WS-06, WS-08, WS-10–WS-12; ADR 0005 |
| Runtime/isolation | IT-06–IT-08, IT-12; ADR 0008; P-06/P-07/P-11/P-14; O-07 |
| Credentials | VS-02–VS-05, VS-08, VS-11, VS-13; PI-05 and PI-10 |
| Delivery/gates | DQ-01–DQ-05, DQ-07, DQ-14; candidate/evidence provenance |
| Design/assembly | SD-04, SD-06–SD-15; repository-layout seam contracts |

These documents record the design baseline, not delivered runtime behavior. The owner subsequently requested implementation; the coordinator allocated the tasks below. T-109 has updated the owning specifications before dependent implementation. Each task starts in its own worktree from `main` and requires exact-candidate review and scoped checks before merge.

### Scheduled implementation

| Phase | Task | Checkpoint |
| --- | --- | --- |
| P0 | [T-109](../tasks/T-109-run-resource-contracts.md), [T-118](../tasks/T-118-pipeline-version-admission.md), [T-110](../tasks/T-110-run-resource-wire-contracts.md) | Owning specifications and old-executor version fence independently, then executable versioned wire/parser fixtures |
| P1 | [T-111](../tasks/T-111-rootless-profile-qualification.md) | Real deployable builder/storage/network profile |
| P2 | [T-112](../tasks/T-112-run-resource-lifecycle.md) | Execution-owned resources, persistence and frozen companion seam |
| P3 | [T-113](../tasks/T-113-run-workspace-authority.md) | Preparation, authority, cumulative publication and provenance |
| P4 | [T-114](../tasks/T-114-kubernetes-run-resources.md) | Kubernetes resources and verified policy |
| P5 | [T-115](../tasks/T-115-local-run-resources.md) | Podman/Process lifecycle and real Podman evidence |
| P6 | [T-116](../tasks/T-116-run-resource-assembly.md) | Sole assembly/client/chart/migration owner |
| P7 | [T-117](../tasks/T-117-run-resource-acceptance.md) | Integrated qualification and staged rollout |

The source design is commit `0b12febef5b9e092982af74e2c951afe2dd80c19`; executable baseline remains the commit above. OpenForge bootstrap is currently unavailable because the separate checkout is absent. Manual isolated workers and independent candidate review may prepare changes; automated queue/index validation and batch receipts must not be claimed until the harness is available.

**Progress:** T-118's old-executor version fence is reviewed, unit-tested and merged at `16a5d0df`; T-109's normative specifications are reviewed, docs-checked (121 files) and merged at the checkpoint above. T-110 is ready; P0 is not complete until its executable fixtures pass. The integrated infrastructure-free lint/unit batch passed at `73404c51` before the documentation-only checkpoint; no real SQL/CSI/CNI/builder qualification is implied. All later phases remain pending.

## Contract decisions to freeze

1. **Versioning:** `kodama/v1alpha2` plus a new runtime context version for incompatible source/layout semantics. Preserve old canonical bytes/result hashes. Serve historical records, drain old runs under their original semantics, and reject new old-format runs after documented cutover.
2. **Ordering:** one executable workspace holder, deterministic topological order and declaration-order tie-breaks. Gates occupy their order position. `needs` still controls logical dependencies/expression validity.
3. **Storage:** run-scoped CSI `ReadWriteOncePod` PVC, bounded size, delete reclaim and qualified idmapped filesystem; Podman run volume. One preparation; no reclone recovery after commands begin.
4. **Services:** startup with run, readiness before steps, service loss fails run. No transparent replacement in the first contract. Separate storage and no implicit `/workspace` mounts.
5. **Lifetime:** bounded total run deadline including gates. Terminal completion includes resource stop/removal; stuck cleanup stays visible and retried. Gates create no Job.
6. **Builders:** nonprivileged rootless profile with mapping helpers, namespaces, mounts, storage and explicit egress. BuildKit no-process-sandbox uses a run-exclusive service; host privilege requires a disposable exclusive isolated worker.
7. **Retry:** initially only provably pre-command launches with intact scratch. Partial filesystem/service mutation and unknown effects are not replay-safe by default.

### Illustrative new-format document

Proposed fixture, not syntax accepted by the current implementation. Domains/digests are placeholders. Service aliases resolve to run-unique endpoints. Installation policy supplies security/resource ceilings; the document contains no raw Pod controls.

```yaml
apiVersion: kodama/v1alpha2
kind: Pipeline
metadata:
  name: integration-build
spec:
  timeoutSeconds: 3600
  services:
    db:
      image: registry.example.invalid/postgres-rootless@sha256:1111111111111111111111111111111111111111111111111111111111111111
      env:
        POSTGRES_USER: kodama
        POSTGRES_DB: app
        POSTGRES_PASSWORD: {secretRef: "secret://project/test-db-password"}
      ports:
        sql: {port: 5432, protocol: TCP}
      readiness:
        exec: ["pg_isready", "-U", "kodama", "-d", "app"]
        timeoutSeconds: 60
    browser:
      image: registry.example.invalid/headless-browser@sha256:2222222222222222222222222222222222222222222222222222222222222222
      ports:
        devtools: {port: 3000, protocol: TCP}
      readiness:
        tcp: devtools
        timeoutSeconds: 60
  steps:
    - id: generate
      kind: container
      image: registry.example.invalid/go-tools@sha256:3333333333333333333333333333333333333333333333333333333333333333
      workspace: {access: write}
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go generate ./..."]
    - id: integration
      kind: container
      needs: [generate]
      image: registry.example.invalid/test-tools@sha256:4444444444444444444444444444444444444444444444444444444444444444
      workspace: {access: read}
      env:
        PGHOST: "${{ services.db.host }}"
        PGPORT: "${{ services.db.ports.sql }}"
        PGUSER: kodama
        PGDATABASE: app
        PGPASSWORD: {secretRef: "secret://project/test-db-password"}
        BROWSER_HOST: "${{ services.browser.host }}"
        BROWSER_PORT: "${{ services.browser.ports.devtools }}"
      command: ["/bin/sh", "-ec"]
      args: ["cd /workspace && go test ./..."]
```

Images must contain declared probe/command dependencies and implement the endpoints. Add executable ko, Buildah and selected BuildKit examples with real pinned images at qualification. Services may use image-default entrypoints; injected supervision must preserve configured argv/signal handling. Include a declared service-to-service network-edge fixture and external preview/registry policy examples in the contract checkpoint.

## Work packages and dependencies

```text
P0 contracts and ADR
  -> P1 platform/build qualification and profile freeze
  -> P2 execution lifecycle and persistence
P0 + P1 + P2 -> P3 entrypoint, authority, publication integration
P0 + P1 + P2 -> P4 Kubernetes resources and execution policy
P0 + P1 + P2 -> P5 Podman and Process resource adapters
P3 + P4 + P5 -> P6 assembly, API/CLI, chart, migration
P6 -> P7 real acceptance and staged rollout
```

P1/P2 can proceed independently after P0. P3–P5 have independent paths once interfaces freeze. Shared schemas, migration ordering, task metadata and `internal/app` have ordered owners. The graph does not authorize overlapping ownership or unrelated deferred work.

### P0 — Contract checkpoint

**Own:** ADR/index/register, affected specification/contract docs, glossary; `contracts/pipeline/`; `contracts/runtime/`; `internal/pipeline/`; the narrow execution admission version-refusal guard. T-110 supplies the API contribution handoff; P6 owns public source-fragment integration and aggregate/client regeneration together, because existing fragments are bundled or checked against frozen v1 operations.

- Install ADR/plan at intended paths and rebase links. Amend ADR 0005/0008 and P-06/P-07/P-11/P-14/O-07 explicitly. Define run scratch/support services without renaming the durable logical workspace.
- Update workspace/execution/entrypoint semantics, gate-wait resource statements, recovery/retention, security, delivery provenance, lifecycle/architecture explanations, examples and traceability together.
- Version shapes/canonicalization. Reject mutable step/service images, unknown fields, bad ports/probes/references, service references to step outputs, arbitrary Pod security/mount/network controls and new-format `workspace.from`.
- Freeze endpoint expressions, secret bindings, deadlines, order, source-free runs and migration errors. Version runtime context/provenance while retaining the expected-tip publisher protocol and old result hashes.
- Keep new-format registration off by default and explicitly refuse new-format definitions at every old-executor admission path, including storage/resolver and cleanup-run paths. Readability of historical definitions must not authorize execution with old semantics.
- Draft execution-owned resource DTOs and companion-port identity/fingerprint, ready/unknown/stopped/removed evidence semantics for P2 to freeze. Deliberately extend capabilities: existing `Guarantees` is `uint8` with six used bits; do not overflow it. Keep optional launch-history proof separate.
- Allocate proposed requirement/acceptance IDs below after checking reservations. Describe implementation as proposed until delivered/qualified.

**Exit:** old/new shape, semantic and canonical fixtures; reviewed contract changes; `make docs-check` and scoped contract/pipeline checks in the task worktree.

### P1 — Qualify and freeze the deployable profile

**Own:** bounded profile qualification fixtures under `test/integration/kube/` and `test/integration/runtime/`, builder image fixtures under `images/`, acceptance/profile report. Coordinate `dev/`/node installation changes with their owner. P7 takes integrated-fixture ownership after this phase merges.

- Inventory actual kernel, Kubernetes, CRI/OCI runtime, CNI, CSI/storage class, filesystem, cgroups, LSM/seccomp and UID-range configuration. Record exact versions/image digests.
- Demonstrate sequential RWOP sharing across different images/UIDs with Pod user namespaces and correct group/idmapped file ownership. Verify delete reclaim and mount release; RWO or Pod deletion alone is not a fence.
- Qualify subordinate ranges, mapping helpers/capabilities, seccomp/LSM rules, writable mounts and Buildah storage/isolation. Support VFS; advertise overlay/FUSE only with evidence. Exercise nested networking under outer CNI policy.
- Build/push a Go image with ko. Build/push a Buildah Containerfile with `RUN`, multiple ownership IDs and authorized dependency access.
- Try normal rootless BuildKit. If no-process-sandbox is necessary, qualify a separate run service with whole-process-tree teardown. If host privilege is selected, include exclusive disposable worker provisioning/deletion; do not advertise it before isolation exists.
- Exercise browser sandbox and bounded writable `/dev/shm`; prove PostgreSQL readiness and data roundtrip with non-root-compatible images.
- Freeze a support matrix of image digest/mode, node/runtime/storage prerequisites, permissions, network destinations, credential flows, commands and negative controls. Missing prerequisites become explicit P4/P6 installation changes.

**Exit:** real ko/Buildah builds and at least one supported BuildKit mode work on the intended platform; exact settings frozen for dependent work. A failing rootless build cannot silently enable unrestricted privilege.

### P2 — Run resource lifecycle and persistence

**Own:** `internal/execution/` including memory/PostgreSQL, migrations, contracts, admission/dispatch/cancellation/reconciliation; execution HTTP fragment contributions as needed. Freeze execution seam definitions before dependents start.

- Persist intent before effects: scope/run/backend/policy, kind/key/name, fingerprint, bound UID, preparation/readiness evidence, deadlines, stop/removal evidence and reason. Persist no scratch contents or secret plaintext.
- Enforce workspace-holder admission and ordered dispatch transactionally. Keep logical-workspace writer admission separate; preserve authorization, quota locking and explicit output dependencies.
- Reserve startup/steady-state service capacity plus permitted active attempt and PVC storage. Prevent untracked partial provisioning/quota deadlock; release after verified settlement/removal.
- Reconcile preparation/services before dispatch; adopt verified resources after restart; reject tenant/name/spec collisions and tolerate duplicate/reordered wakeups.
- Extend cancellation/fail-fast/timeouts to observed and pending resources. Use cleanup reasons/evidence rather than new business run status enums. Stop services promptly while uncertain external/source effects settle independently.
- Implement conservative retry, resource-loss outcomes and total deadlines including gates. Never restore scratch or empty databases invisibly under the same run ID.
- Define scoped resource lifecycle observations/events containing safe references. No fake attempts or checkpoint events for scratch mutations.

**Exit:** shared memory/PostgreSQL contract tests; actual RLS, ownership, rollback, duplicate claims, quota, cancellation/create, crash/unknown-launch and orphan cleanup evidence. Existing writer/late-result barriers pass.

### P3 — Entrypoint, scoped authority and publication

**Own:** `internal/entrypoint/`, `internal/runtime/bootstrap/`, `internal/runtimeauth/`, scoped `internal/secrets/` adapters, `internal/workspace/`, `internal/results/`, `internal/logs/`, necessary `internal/agents/` context consumers. Reuse the existing publisher.

- Implement controller-selected preparation with Git/read-only credentials, pre-command idempotency and trusted receipt. Remove per-step clone/reset/clean for new contexts.
- Teach `kodama step` to verify the bound workspace and run commands there; keep attempt output/result/secret/identity roots separate. Source-free runs get empty shared scratch and no checkout credential.
- Add narrow preparation/service subjects. Exchange credentials against committed identity/UID; renew only while authorized. Services cannot invoke `source`/`result` or general attempt actions; supervisors receive no broader authority in their process namespace.
- Deliver declared pinned secrets just in time outside manifests/shared files. Use private build auth files/scoped BuildKit sessions; destroy on stop. Preserve proxy-first agent credentials and revocation.
- Preserve expected-tip CAS, ancestry, result acknowledgment/replay and unknown-publication reconciliation. Later writers consume cumulative files with explicit lineage; original run input stays immutable.
- Extend source/output secret detection/redaction to relevant earlier run disclosures. Mount exclusion and known-value scanning do not promise malicious explicitly authorized commands cannot copy secrets.
- Harden Git against retained user config/hooks, credential helpers, filters, symlinks and ownership changes; authoritative publication validation stays in the control plane.
- Record dirty/generated input provenance, retain gate/candidate binding and bounded service/preparation logs. Changed source cannot be labeled as an unchanged approved candidate.

**Exit:** checkout once across images/restart; generated/untracked/ignored files persist; non-Git reader image works; cumulative writers, human push conflict, response loss, cross-resource grant denial, prior-step secret-copy rejection, finalization ordering and diagnostic regressions pass.

### P4 — Kubernetes resources and policy

**Own:** `internal/kube/` including hand-written Kubernetes client/types, preflight, templates, verification and conformance. P6 owns chart integration.

- Add typed PVC, Service, service Pod, NetworkPolicy, preparation Job and selected builder-worker operations. Keep provider types inside adapters.
- Allocate run PVC and mount the same claim in all step Jobs. Keep tools/output/secret/result volumes attempt-private; retain one primary workload container and `backoffLimit: 0`.
- Apply qualified user mappings/capabilities/seccomp/LSM/storage/network profile. Verify policy/volume UID/service bindings and unexpected security fields, not just labels/fingerprints.
- Render run-only ingress/client egress, scoped DNS and authorized external access by workload role. Deny foreign run selectors, broad peers, host networking, public Service types and platform endpoints. Test CNI DNAT/Service behavior with traffic.
- Scope bootstrap/service secrets and RBAC; workloads cannot create/change Pods, volumes, endpoints or policy. Only preparation receives checkout authority.
- Handle create/observe/stop/remove races and actual process/mount release. Record cleanup of Pods/Jobs, Services, policies, bootstrap Secrets, PVC/PV allocations and optional isolated builder workers. Never reuse scratch while termination is uncertain.

**Exit:** real Kubernetes CSI/CNI contract, template/adoption negative controls, restart adoption, cancellation races, unknown termination, UID collisions, default deny and complete resource inventory teardown.

### P5 — Podman parity and Process fixtures

**Own:** `internal/runtime/podman/`, `internal/runtime/process/`, `internal/runtime/runtimetest/`. P3 owns shared bootstrap changes.

- Replace attempt `/workspace` tmpfs with a run-owned volume; keep credential/output tmpfs private. Add run network/services/readiness/endpoints, durable records and idempotent cleanup.
- Apply qualified rootless mapping/storage settings. No host engine socket or implicit broad container network access. A private bridge alone does not establish external default deny.
- Extend one exported runtime/resource contract across Process, real Podman and Kubernetes. Process proves lifecycle/order through explicit host programs; it advertises no image/network/builder guarantee.
- Advertise Podman network guarantees only when enforced/tested, e.g. an owned filtered egress path. Otherwise keep them absent and refuse policies requiring them. Local lifecycle tests cannot certify Kubernetes isolation.

**Exit:** real multi-image volume/service roundtrip, cancel/restart, mappings, declared-image execution, capability refusal and no leaked volumes/networks/containers. `make test-podman` remains a real-backend check.

### P6 — Assembly, chart and migration

**Own:** `internal/app/`, relevant `internal/cli/`, aggregate `api/openapi.yaml`/`api/client/`, `deploy/helm/kodama/`, migration ordering, shared dependencies and operations documentation. Sole shared-file integration owner.

- Wire resource ports/stores/auth/entrypoints/logs/workers/migrations explicitly per `KODAMA_ENV`. Preserve production refusal of Process/Podman/memory/keyfile fallbacks.
- Render versioned execution policy, PSA baseline/version, LSM/admission prerequisites, storage class, quotas, DNS/egress, RBAC, deadlines and BuildKit mode. Preflight capabilities, not mere declarations.
- Expose safe preparation/readiness/cleanup reasons and service diagnostics through run observation/CLI, with current authorization. Compose/regenerate schemas/client through this owner.
- Replace broad tenant egress so additive policy cannot defeat isolation; maintain control-plane namespace restrictions independently.
- Migrate resource metadata/version discriminators additively. Keep old hashes immutable. Drain old runs before removing old policy support; require new format for subsequent admissions with actionable migration errors.
- Document shared workspace, service/builder lifecycle, gate costs, recovery/offboarding/uninstall and scratch deletion. Retain unqualified status until P7 passes.

**Exit:** chart render/config/preflight, baseline-to-new migrations, generated API/client compatibility, composed pipeline/service flow and old-version drain/refusal. Older controllers must refuse active new-format runs, not adopt with old semantics.

### P7 — Integrated acceptance and rollout

**Own:** integrated suites in `test/integration/assembly-execution/`, `test/integration/kube/`, `test/integration/runtime/`, builder/service fixtures, acceptance report, status/traceability. Reuse P4/P5 component evidence rather than repeat without cause.

- Run the matrix below against the composed server and actual Kubernetes target. Retain commit/image/tool/profile/kernel/CSI/CNI versions, commands and cleanup inventories.
- Deploy to trusted-engineer stage after old runs drain. Exercise multiple tenants and concurrent runs within one tenant.
- Prove ko, Buildah and selected BuildKit build/push/register correct OCI provenance with explicit registry credentials/egress.
- Inject failures during preparation, readiness, commands, gates, publication, service loss, restart, cancellation/create and storage deletion. Cleanup/retry evidence must survive controller restart.
- Report only qualified combinations. Rollback stops admission, cancels/drains runs, reconciles resources, then reverts controller/profile. Never resume new-format runs on an old controller.

## Acceptance matrix and stable-ID proposal

These are proposed allocations at the pinned baseline; P0 checks integration-branch collisions. Reserve rather than repurpose replaced IDs. Add rows to owning specs and map tests by semantic name.

| New requirement / acceptance | Observable scenario | Evidence |
| --- | --- | --- |
| WS-13 / WS-A09 | One successful pinned checkout; different images see generated/untracked/ignored files; branch movement does not refresh scratch | Entrypoint/composed runtime |
| WS-14 / WS-A10 | Eligible steps cannot overlap workspace ownership; retry/redrive/partition cannot give a live prior process's volume to another attempt | SQL races/CSI fencing |
| WS-15 / WS-A11 | Lost scratch fails; rerun gets fresh resources; retained Git/artifacts survive; scratch never creates a checkpoint | Recovery/cleanup |
| WS-16 / WS-A12 | Cumulative writers use expected-tip CAS; human push conflicts; prior-step edits/secret disclosures handled | Publisher integration |
| PX-17 / PX-A13 | Two runs with identical service aliases reach only their own endpoints; readiness precedes commands | DB/browser roundtrip |
| PX-18 / PX-A14 | Startup timeout/service loss fails without empty-state replacement; restart adopts intact resources | Runtime faults |
| PX-19 / PX-A15 | Success/failure/timeout/cancel remove resources; lost responses/late creates converge without deleting foreign objects | Inventory/crash injection |
| PX-20 / PX-A16 | Atomic bounded service/storage reservations; gates retain resources only until run deadline | SQL/deadline teardown |
| PX-21 / PX-A17 | Context/profile/service pins survive restart; unsupported runtime refused; old hashes/history intact | Contract/capability tests |
| PX-22 / PX-A18 | Pre-command retry safely reuses scratch; executed/unknown attempts cannot replay partial filesystem/service effects automatically | Retry regressions |
| IT-14 / IT-A10 | Nested build/service traffic obeys same-run access and outside-run default deny, including other same-tenant runs/platform endpoints | Real CNI traffic |
| IT-15 / IT-A11 | ko/Buildah actually build/push; arbitrary privilege/host paths/namespaces/engine sockets/overrides refused | Builds/admission denial |
| IT-16 / IT-A12 | Selected BuildKit mode runs real `RUN` builds, authenticates run clients, removes daemon/process/cache; privileged mode proves worker disposal if selected | BuildKit profile evidence |
| VS-14 / VS-A11 | Service/preparation resolve only their pinned secrets; foreign/copied/revoked grants fail other resource/attempt/publication actions; manifests omit plaintext | Auth/secret integration |
| DQ-16 / DQ-A12 | Scratch changes cannot masquerade as approved candidate; source/tool/build-input/image provenance and gate invalidation remain correct | Gate/artifact composition |

Retain PX-A05/A06/A12, WS-A03/A05/A07, IT-A05/A06, VS-A10 and relevant DQ acceptance as regressions, updating deliberately superseded assumptions only. Migrations/events satisfy existing RLS/audit/outbox and shared-contract evidence.

## Scoped verification commands

Future implementation checks, run in each task worktree with packages narrowed to owned changes. This planning task has performed no runtime/build qualification.

```sh
make docs-check
make check-go PKGS='./contracts/... ./internal/pipeline/...'
make check-go PKGS='./internal/execution/...'
make check-go PKGS='./internal/entrypoint/... ./internal/runtimeauth/... ./internal/workspace/... ./internal/results/... ./internal/secrets/... ./internal/logs/...'
make check-go PKGS='./internal/kube/... ./internal/runtime/...'
make check-go-integration DEV_SCOPE=run-resources-sql DEV_STACK=services PKGS='./internal/execution/postgres/...'
make test-podman
make test-chart
make api-check
make test-m1 test-m2 test-m3
make check-go-integration DEV_SCOPE=run-resources-kube DEV_STACK=cluster PKGS='./test/integration/kube/... ./test/integration/assembly-execution/...'
```

P7 extends those suites with the acceptance cases; do not claim unimplemented tests/targets already exist. Use isolated scopes and record missing prerequisites. Follow integrated-main batch cadence rather than a full suite per merge. Run `make check-devtools` when `dev/` changes.

## Completion criteria

Owning contracts/examples agree; the new version works through the assembled Kubernetes product; every matrix row has retained evidence; rootless ko/Buildah and one explicit BuildKit mode build successfully; same-run access/cross-run denial work; cleanup survives restart; Git publication/gates retain their authority; status names the exact supported profile. Scratch survival is never a checkpoint or recovery guarantee.
