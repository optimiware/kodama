# Installation and configuration

**Status:** installation design; verified chart commands are implementation deliverables.

## Supported progression

| Profile | Purpose | Qualification |
| --- | --- | --- |
| `development` environment | In memory, Process runtime; not installable | T-080–T-082 unit evidence, T-027 composition smoke |
| `test` environment | Podman dependency stack and Podman runtime, local keyfile KEK, no cluster | T-026 foundation e2e |
| Kubernetes component conformance | kind runtime, restricted baseline and chart install/configuration refusal; no ready-server claim | T-102/T-089 under O-24/O-25 |
| Full Kubernetes readiness | Ready chart/server, live KMS and assembled M1 roundtrips with supported TLS | T-104 after T-105 and T-103; explicitly scheduled |
| Trusted-engineer staging (current target) | Regular Kubernetes containers with the existing restricted execution baseline in Platform stage | T-090 after T-104 and the actual installation's prerequisites |
| Trusted-engineer production | Same regular-container trust model with qualified operations and recovery | T-028/T-029 and T-056; Optimi-hosted authority integration also requires T-061 |
| Shared run workspace, services and rootless builders (composed, off by default) | `run_shared` model of [ADR 0010](../adr/0010-run-workspace-services-builders.md): explicit enablement ([runbook](run-shared.md)), a qualified node/storage/network profile and the cutover below. No installation supports it until integrated acceptance passes | T-109 contract → T-111 profile → T-112–T-116 → T-117; not a prerequisite of staging or the current trusted-engineer release |
| Stronger/untrusted execution (deferred) | Later evaluation/support for gVisor, Substrate or other isolation/security options | T-055 after the baseline T-056 release, separately scheduled and qualified |

A supported installation is Kubernetes on AWS, GCP, or Scaleway. It requires PostgreSQL and that cloud's KMS (AWS KMS, GCP Cloud KMS, or Scaleway Key Manager). The chart and product contracts are the same on each cloud. The installation selects one KMS provider; a missing provider prevents startup, and one cloud's KMS is not a fallback for another. The development keyfile is not a production provider. Platform data residency does not choose this cluster's cloud.

`KODAMA_ENV` (`development`, `test` or `production`) is required, selects the adapters and has no default; the chart sets `production`, which refuses every development and test adapter ([ADR 0009](../adr/0009-environments.md)). The environment is deployment configuration, not a runtime setting.

One server Deployment runs API/reconciliation/events/gateways. The initial chart uses one replica. Jobs inject the same release's static `kodama step` binary. No source PVC, remote editor service, operator, or pipeline CRD is required.

The first release excludes full air-gap support (O-08); document required network access to the selected dependencies rather than advertising disconnected operation. Helm/ordinary Jobs remain the operating baseline and operator assessment/implementation are explicitly deferred (O-16). Kodama uses Go 1.27 (current pin 1.27.2); Platform's toolchain update does not require a downgrade here.

Select standalone or Optimi-hosted authority explicitly, independently of execution trust. Standalone uses local OIDC memberships and credential custody. Hosted O-18 selects verified Optimi OAuth UUID tenant/role claims; O-19 selects versioned KMS-encrypted PostgreSQL secrets and proxy-first runtime delivery. The qualified [platform adapters](../specs/platform-integration.md) name the supported OAuth/core revision, actual claims/current-access and secret-version contracts; they never fall back to local authority on failure. The current install sequence below describes standalone bootstrap. T-061 supplies hosted onboarding/disable/recovery qualification; local bootstrap or tenant creation does not create a Platform tenant.

### First Platform stage installation

The owner-selected first staging destination is Platform's stage cluster. Install the existing chart with a separate Kodama control-plane namespace/release, scoped execution namespaces and distinct database roles, storage prefixes, service accounts and KMS permissions. A namespace alone does not establish the execution/tenant isolation profile. Keep Platform's database and domain authority behind its service contracts.

O-07 selects trusted engineers using regular Kubernetes containers for this stage. Keep the current restricted Pod, namespace/network/quota and scoped-authority baseline; no custom sandbox runtime or security-provider abstraction is required to start. gVisor, Substrate and other options are deferred evaluation/integration candidates, not existing support. The stage handoff states this trust model explicitly.

Record the cluster/context, namespace set, image/chart revisions, selected cloud KMS and authority profile in the stage handoff. An explicitly standalone trusted-engineer smoke deployment can precede hosted qualification once its actual prerequisites are wired and tested; do not relabel it as a hosted integration. T-028 supplies the KMS adapters and T-105 supplies startup wiring. T-104 verifies live KMS, full installed-server readiness and the assembled Kubernetes roundtrips before T-090; T-089's O-24 component checkpoint alone is insufficient. Co-location does not bypass that work or authorize using the development keyfile as a cloud KMS. T-061 qualifies the real Platform caller and records the supported installation/API pairing. Stage deployment commands and evidence remain deployment deliverables.

### Run-shared profile (designed, not installed)

T-116 composes this model in the server and renders it in the chart, **off by default and not qualified**; the enablement procedure, the stage policy values, the cutover/drain/rollback order and uninstall are in [Enabling `run_shared`](run-shared.md). The node profiles are [their own runbook](run-builder-profile.md). The contract: [execution policy](../contracts/run-resources.md#execution-policy), [pipeline `v1alpha2`](../contracts/pipeline-format-v1alpha2.md). The current `attempt_private` model with the existing restricted baseline stays the installed behavior until the steps below pass.

- **Prerequisites (T-111 freezes values, T-116 renders and preflights them).** Qualified kernel, Kubernetes, CRI/OCI runtime and CNI; Pod user namespaces and a CSI storage class supporting `ReadWriteOncePod`, delete reclaim and idmapped mounts; installed seccomp/LSM profiles; node subordinate ID allocation sufficient for the qualified builder images; Pod Security `baseline` at a tested version on the execution namespaces; replacement of broad tenant egress with per-run policies; resolver and egress-profile destinations; a pinned builder-service mode or none (a host-privileged mode additionally needs an exclusive disposable worker pool). Preflight checks capabilities by behavior, not declarations; a missing prerequisite is an actionable installation error, never a silent fallback to wider privilege.
- **Capacity.** Services and scratch are reserved for the whole run, gate waits included. Set the run-deadline ceiling, quotas and PVC capacity accordingly; PVC storage is charged separately from container ephemeral storage.
- **Cutover.** (1) Install the policy with `runShared: closed`; (2) enable `v1alpha2` registration, then admission, on a trusted-engineer stage; (3) set `legacyFormat: drain` and let old runs finish under their original semantics; (4) `closed` when none remain. Historical definitions/results stay readable throughout. Rollback: stop admission, cancel/drain `run_shared` runs, reconcile their resources, then revert the controller/profile; never resume a `run_shared` run on an older controller.
- **Uninstall and offboarding.** Run scratch, services, policies and PVC/PV allocations are disposable per-run resources: tenant offboarding and uninstall stop admission, cancel runs, wait for verified stop and remove them; stuck cleanup is reported, not hidden. Scratch contents are deleted with the volume and are never restored. Git refs, artifacts and registry content follow their own retention.

## Integrations
O-26 updates the original T-029 startup handoff: T-105 supplies `secrets.adapter: keyfile | kms` and cloud `aws | gcp | scw` startup wiring. T-104 performs live-provider validation in Kubernetes before T-090 staging. T-029 retains full disaster recovery. See [configuration and credentials](../../deploy/charts/kodama/README.md#kms-selection) for version-2 settings, stable key mappings and rollout behavior.


| Dependency | Configuration and preflight |
| --- | --- |
| PostgreSQL | TLS, separate migration/runtime identities, RLS/pool round trip |
| NATS JetStream | TLS/scoped credentials, stream capacity, durable publish/consume/reconnect |
| Git HTTPS | Repository/read credentials, publisher-write secret, ordinary branch/CAS and retained-ref support |
| S3-compatible storage | Bucket/prefix, scoped identity, upload/digest/download/delete checks |
| OCI registry | Digest pull and rootless-builder push scopes |
| OIDC | Issuer/JWKS, factory access-token audience, PKCE redirect/client |
| KMS | Required. AWS KMS, GCP Cloud KMS, or Scaleway Key Manager, one per installation. Wrap/unwrap scope keys and restore a derived value. Development keyfile is not valid here |
| Kubernetes | Restricted Pods, NetworkPolicy, quotas, runtime baseline and reachable callback URL. For `run_shared`, additionally the preflighted execution-policy prerequisites ([below](#run-shared-profile-designed-not-installed)) |
| Optional AI/MCP | Approved endpoint, typed credential, pinned protocol/wrapper compatibility |
| Optional delivery | Namespace-scoped kubeconfig, explicit runtime config, preview TTL |

Live logs use Kubernetes; finished logs use artifact storage. Loki is optional. Production must retain source commits through Git refs and a declared repository backup/mirror policy.

Apply the [approved recovery/retention objectives](upgrades-backup-and-recovery.md#approved-retention-and-recovery-baseline), and publish measured results rather than assuming the chart establishes them. O-10's initial qualification workload is 20 tenants, 100 projects and 20 concurrent attempts, with declared per-tenant limits and concurrent API/SSE/model/webhook traffic. Declare the host resources and dependency limits used for the measurement.

Platform owns subscription billing, commercial usage/access and entitlements; deployment configuration selects that integration without creating Kodama-local pricing or billing state. Optional UI is Platform-owned and deferred; API/CLI/MCP/webhook operation does not require a separate dashboard deployment.

## Chart groups

Define values for server image/resources, database, messaging, identity, secret encryption, Git publisher, artifacts, execution limits, ingress/TLS, and observability. Values contain secret references or workload-identity configuration, not plaintext tenant credentials. Fixed configuration names/types are frozen with the Helm/scaffold tasks before composition.

Deployment configuration establishes a process: database and KEK access, listen address, OIDC trust, and execution budgets. It stays in the environment and Helm and changes with a rollout. Runtime behavior administrators change while the server runs belongs in Postgres variables/settings and the secret store, read per operation. A missing runtime value must not prevent startup or handler registration. Do not add an environment variable for that behavior. This is the same split as platform settings, applied to Kodama's project variables rather than a second deployment channel.

Migrations run once under a database lock using the ordered module manifest. Readiness distinguishes process health, database availability, dispatch admission, and integration degradation. A NATS outage can leave local transactions available while the outbox grows; configured backlog limits apply backpressure. Missing required artifacts or failed Git publication prevents successful finalization.

## Installation acceptance

1. Pin tested dependency/image versions and declare the execution trust profile.
2. Configure TLS/identities and pass integration preflight.
3. Migrate and start the server; perform one-time verified admin bootstrap.
4. Create tenant/project, repository credentials, and a work item.
5. Run a container, retrieve evidence/logs, and verify cross-tenant denial.
6. Restart/recover the control plane and confirm the same committed state/source.
7. For production, complete KMS and disaster-recovery qualification before accepting the supported profile.

The release supplies exact commands/values examples, expected results, support versions, and retention behavior on uninstall. Removing the chart does not implicitly delete databases, Git refs, KMS keys, artifacts, or external deployments.
