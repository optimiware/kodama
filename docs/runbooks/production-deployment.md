# Production deployment guide

Status: **operator guidance for the reference deployments in this repository.**
This guide describes how to take those deployments to production. It is not
evidence that any installation is qualified. Read
[KODAMA.md](../../KODAMA.md#5-implementation-operation-and-acceptance) and
[S10 — Environments, production, and operations](../specs/10-production.md) as
the authoritative behaviour; this runbook adds deployment procedure and does not
replace them. The qualification state lives in the task notes and the evidence
files, not in this guide.

The repository ships **reference deployments, not production-grade
infrastructure**. Treat every default in this guide as a starting point that an
operator must review, size and test.

## 1. What the reference deployment is, and what it is not

### 1.1 What ships

| Piece | Repository path | What it gives you |
|---|---|---|
| Scaleway OpenTofu root | [`deploy/scw`](../../deploy/scw) | Kapsule cluster, Managed PostgreSQL (RDB), Key Manager key, private network, remote state |
| Google Cloud OpenTofu root | [`deploy/gcp`](../../deploy/gcp) | GKE cluster, Cloud SQL for PostgreSQL, Cloud KMS key, private network and private services access, GCS remote state |
| AWS OpenTofu root | [`deploy/aws`](../../deploy/aws) | EKS cluster, RDS for PostgreSQL, AWS KMS key, VPC, S3 remote state |
| Shared context module | [`deploy/modules/context/main.tf`](../../deploy/modules/context/main.tf) | Cluster name prefix, installation id, labels, state key, HA switch (`cost_profile == "ha"`) |
| Application chart | [`deploy/charts/kodama`](../../deploy/charts/kodama) | One control-plane Deployment, HTTPS Service, ServiceAccount/RBAC, retained-log PVC, tenant namespaces, LimitRanges and network policies |
| Stack chart | [`deploy/charts/kodama-stack`](../../deploy/charts/kodama-stack) | The application chart plus one TLS NATS JetStream StatefulSet; optional stage-only OIDC provider |
| Image | [`deploy/images/kodama/Containerfile`](../../deploy/images/kodama/Containerfile) | Static `kodama` binary on Alpine with `git` and CA roots |
| Orchestration | [`scripts/bootstrap-state`](../../scripts/bootstrap-state), [`scripts/deploy`](../../scripts/deploy), [`scripts/bootstrap-secrets`](../../scripts/bootstrap-secrets), [`scripts/image`](../../scripts/image) | Remote state, OpenTofu + Helm lifecycle, cluster Secrets, image build |
| Deployment descriptor | emitted by the OpenTofu roots, consumed by the scripts | Cloud, cluster, namespace, tenants, PostgreSQL endpoint, KMS key reference |

The stack chart is fail-closed on broker authentication: `helm template` and
`helm upgrade` refuse to render unless `nats.auth.mode` is `callout` (the
default: client-certificate mTLS plus the broker's authorization callout, with
the `kodama-nats-auth` Secret), `mtls` or `credentials`, each with its Secret
configured. There is no anonymous broker mode (see
[deploy/README.md](../../deploy/README.md)).

### 1.2 What it is not

The reference deployment deliberately omits operational infrastructure that a
production service normally has. Do not assume any of the following exists
because `helm upgrade` succeeded:

- **No control-plane HA.** `replicaCount: 1` and `strategy.type: Recreate` in
  [`templates/deployment.yaml`](../../deploy/charts/kodama/templates/deployment.yaml).
  The old and new controller never overlap; an upgrade is a short outage. There
  is no PodDisruptionBudget and no anti-affinity, and a multi-replica control
  plane is not a supported mode.
- **No NATS cluster.** One StatefulSet replica, one `ReadWriteOnce` JetStream
  PVC, `maxMemory: 256M` and `maxFile: 8G` in
  [`values.yaml`](../../deploy/charts/kodama-stack/values.yaml). A broker pod
  restart is an outage; events persist in PostgreSQL, not in the broker.
- **No ingress or load balancer.** The chart renders a `ClusterIP` Service on
  port 8443 and nothing else. Public exposure, DNS and an external certificate
  are operator work.
- **No certificate manager.** `scripts/bootstrap-secrets` issues an internal CA
  and fixed-SAN leaves. There is no cert-manager, ACME or external issuer
  integration.
- **No monitoring stack.** The server serves `/healthz`, `/readyz` and an
  unauthenticated Prometheus-text `/metrics` route on the same HTTPS listener.
  The chart ships no scrape configuration, dashboard, alert rules or log
  shipper. `/metrics` is covered by the composition integration test only; no
  scrape of a live cluster has been recorded.
- **No application backup automation.** OpenTofu configures the managed
  database's own backups; the repository has no job that snapshots the retained
  log PVC, the JetStream PVC or the key Secrets.
- **No multi-region or cross-region failover.** The roots create one cluster and
  one database in one region.
- **No resource quota.** Tenant namespaces get a `LimitRange` but no
  `ResourceQuota`; cluster capacity is the only ceiling.
- **No hostile-workload isolation claim.** The threat model is trusted project
  engineers in ordinary containers ([KODAMA.md](../../KODAMA.md#execution-environments)).

## 2. Supported clouds and qualification status

All three clouds remain supported installation targets (user decision U15);
none is being withdrawn. **Supported is not the same as qualified.** Read this
table as the evidence status on the revision you deploy, not as a product
roadmap:

| | Scaleway | Google Cloud | AWS |
|---|---|---|---|
| Cluster | Kapsule, Cilium CNI (`deploy/scw/kubernetes.tf`) | GKE with Dataplane V2 (`deploy/gcp/kubernetes.tf`) | EKS with VPC CNI network policy enabled (`deploy/aws/kubernetes.tf`) |
| Managed PostgreSQL | Scaleway Managed Database (RDB), `PostgreSQL-17` default | Cloud SQL, `POSTGRES_16` default | RDS for PostgreSQL, major version 16 default |
| Key service | Scaleway Key Manager, adapter `scw-kms` | Cloud KMS, adapter `gcp-kms` | AWS KMS, adapter `aws-kms` |
| Remote state | Scaleway Object Storage, `use_lockfile = true` | GCS bucket | S3, `use_lockfile = true` |
| Container registry | Operator-supplied | Operator-supplied | Operator-supplied |
| OpenTofu root validates | Yes | Yes | Yes |
| Live deployment qualification | Yes, for 0.1.0: the e2e tests, the production probes and an inline Vite build pipeline passed on a Scaleway cluster after a full reset; the content of a restore was verified by hand and the restore procedure of §9.4 rehearsed; the automated restore drill does not complete on Scaleway | No | No |
| Q02 probes | 18 of 18 stage probes passed, none skipped | Not run (DEF-11) | Not run (DEF-11) |
| Production entry accepts the adapter today | Yes: `scw-kms`, static `SCW_SECRET_KEY` | Yes: `gcp-kms`, GKE Workload Identity; not live-validated | Yes: `aws-kms`, EKS IRSA; not live-validated |

**Scaleway is the only live-qualified cloud target today.** AWS and GCP have
OpenTofu roots that validate and no live qualification yet (`DEF-11`). Do not
describe AWS or GCP as live-qualified in a release note until Q02 records their
provider-specific evidence.

**Be precise about the last row.** [`cmd/kodama/production.go`](../../cmd/kodama/production.go)
selects the KMS client from `KODAMA_KEY_ADAPTER` (`scw-kms`, `aws-kms` or
`gcp-kms`) and refuses any other value. The AWS and GCP clients use the
standard library only and are proven by unit tests against loopback endpoints
and, for AWS request signing, the published Signature Version 4 vectors. No
AWS or GCP KMS call, token exchange or deployment has run against the real
cloud: an AWS or GCP installation is implemented, not live-validated, until
its G04 qualification.

The KMS credential mode is per cloud, and the deployment assets follow it:

| | Scaleway (`scw-kms`) | AWS (`aws-kms`) | Google Cloud (`gcp-kms`) |
|---|---|---|---|
| Credential | Static IAM API key | EKS IRSA role, no static credential | GKE Workload Identity, no static credential |
| `kodama-kms-credentials` Secret | Required; `scripts/bootstrap-secrets` creates it and the chart reads it through a required `envFrom` | Not created, not referenced by the chart | Not created, not referenced by the chart |
| `kodama` ServiceAccount annotation | None | `eks.amazonaws.com/role-arn` | `iam.gke.io/gcp-service-account` |
| Source of the annotation | n/a | `kubernetes.workload_identity_ref` in the descriptor (`aws_iam_role.kodama_controller`), rendered by `scripts/deploy` | `kubernetes.workload_identity_ref` in the descriptor (`google_service_account.kodama_controller`), rendered by `scripts/deploy` |
| Chart refusal in the production profile | Render fails without the Secret name | Render fails when the chart-created ServiceAccount lacks the annotation | Render fails when the chart-created ServiceAccount lacks the annotation |

The chart no longer requires the Scaleway-only Secret on AWS or GCP: for those
adapters it renders no reference to `kodama-kms-credentials`, so a missing
Secret cannot block the pod. The IRSA trust policy
([`deploy/aws/kms.tf`](../../deploy/aws/kms.tf)) and the Workload Identity
member ([`deploy/gcp/kms.tf`](../../deploy/gcp/kms.tf)) name exactly the
ServiceAccount `kodama` in the release namespace and grant decrypt on the one
installation key. Any process that loads the keys, including an operator-run
`kodama bootstrap-admin`, must therefore run as that ServiceAccount in that
namespace (for example inside the control-plane pod); the chart renders no Job
and no second ServiceAccount for it.

**The AWS and GCP credential modes are implemented but not yet validated
against a live EKS or GKE cluster.** That qualification is planned at gate G04
(`DEF-11`); until it is recorded, only the Scaleway mode has live evidence.

The AWS root exports `database_ca_certificate` (sensitive) from the
operator-supplied public RDS CA PEM bundle `TF_VAR_database_ca_bundle`, with
`postgres_ca_cert_identifier` wired to the instance
([`deploy/aws/outputs.tf`](../../deploy/aws/outputs.tf)); matching that bundle to
the real RDS endpoint is still live (R5) evidence. Do not treat an AWS or GCP
run as production-ready until it is qualified.

[deploy/README.md](../../deploy/README.md) and
[KODAMA.md](../../KODAMA.md#execution-environments) describe all three clouds
as supported targets and do not record the qualification status. Use the
table above for the evidence status.

One KMS provider is selected per installation. A successful check on one cloud
never qualifies another; each advertised target needs its own runtime, KMS and
deployment evidence ([S10 §2](../specs/10-production.md#2-deployment-contract)).

## 3. Sizing and availability

### 3.1 Control plane

| Setting | Default | Source | Production advice |
|---|---|---|---|
| Replicas | 1 | `replicaCount` in the chart values | Keep one. The controller owns the commit lane and the Deployment is `Recreate`; a second replica is not a supported HA mode. |
| CPU request / limit | 250m / 1 | `resources` | Raise the limit only after measuring; the server also runs the controller loops. |
| Memory request / limit | 256Mi / 1Gi | `resources` | At least 1Gi limit; the retained-log index and reconciler share the process. |
| Retained-log volume | 10Gi `ReadWriteOnce` | `persistence.logs` | Size the volume for your log volume plus 30 days of retention; 10Gi is not a guarantee that 30 days fits. |
| Strategy | `Recreate` | `templates/deployment.yaml` | An upgrade stops the old pod before the new one starts. |
| Probes | HTTPS `/healthz`, `/readyz`, timeout 10s | `probes` | Do not change to `tcpSocket`; the schema and template refuse it. |

A cluster that still has the Deployment as `RollingUpdate` cannot be switched to
`Recreate` by Helm server-side apply. The documented one-time remedy is
`kubectl -n kodama-system delete deployment kodama` before the upgrade; Helm
recreates it from the release ([deploy/README.md](../../deploy/README.md)).

### 3.2 Database

| | Scaleway | Google Cloud | AWS |
|---|---|---|---|
| Default tier | `db-play2-pico`, 20GiB SBS | `db-custom-1-3840`, 20GiB SSD | `db.t4g.small`, 20GiB gp3 |
| HA switch | `cost_profile = "ha"` → `is_ha_cluster` | `ha` → `availability_type = "REGIONAL"` | `ha` → `multi_az = true` |
| Encryption at rest | `encryption_at_rest = true` | Provider default with Cloud SQL | `storage_encrypted = true` |
| Private access | Private network with IPAM | Private IP only; public IPv4 disabled | Private subnets; `publicly_accessible = false` |
| Backups | Every 6h, 30-day retention, same region | Daily 02:00, PITR on, 7-day transaction logs, 30 retained backups | Automated backups, 30-day retention, window 02:00–03:00 |

The `cost-conscious` profile used by the shipped `stage` and `preview` tfvars is
**not HA**. For a production installation select `cost_profile = "ha"`, or
document that you accept a zonal database. A database outage makes `/readyz`
fail and stops new work; persisted execution progress remains in PostgreSQL.

### 3.3 NATS JetStream

- One replica, TLS on port 4222, client verification always on
  (`verify: true`, no opt-out).
- JetStream storage: `maxMemory: 256M`, `maxFile: 8G`, PVC 10Gi, class from
  `nats.jetstream.storageClass` (the generated values use the cluster's
  `workspace_storage_class`).
- The durable source of truth for events is PostgreSQL; JetStream carries
  at-least-once delivery and replay. A full JetStream store degrades publication,
  not execution progression ([S10 §3](../specs/10-production.md#3-health-and-observability)).
- The broker image is pinned by tag and digest and the render aborts if either is
  missing. Do not replace the digest with a floating tag.
- Single replica means no broker HA. Plan maintenance as an outage and verify
  that the publication backlog drains afterwards.

### 3.4 Tenants, workloads and node capacity

- `tenants` is the complete served set. Each entry renders
  `kodama-tenant-<tenant>`, a Role/RoleBinding, a default-deny network policy and
  a Kubernetes runtime adapter. Tenant ids are lowercase DNS labels of at most
  49 characters; duplicates are refused. With more than one tenant the
  Deployment sets `KODAMA_TENANTS`; one tenant keeps `KODAMA_TENANT`.
- Every tenant Namespace carries `pod-security.kubernetes.io/enforce: baseline`
  and `warn`/`audit: restricted`. The chart does not create a `ResourceQuota`.
- The `LimitRange` in each tenant namespace defaults every container to the
  control plane's resource values. The runtime overrides those with
  `KODAMA_WORKLOAD_{CPU,MEMORY}_{REQUEST,LIMIT}`, defaulting to 250m/256Mi
  request and 1 CPU/1Gi limit. Jobs have a 600-second TTL; cleanup uses 3
  attempts with a 1-second backoff (`cmd/kodama/production.go`).
- Workloads and the control plane share the one worker node pool. There are no
  taints, tolerations, node selectors or dedicated workload pools in the chart.
  Size the pool for control plane + NATS + concurrent Jobs. As a rough capacity
  check, a 1 CPU/1Gi Job and the control plane compete on the same nodes; a
  4-node maximum with `t3.medium`/`e2-standard-2`/`PLAY2-NANO` nodes is not a
  concurrency guarantee. Load-test your own mix.
- Cloud vCPU, disk, database-connection and address quotas are provider-side and
  are not managed here. **Verify with your provider** before a large tenant
  rollout.

### 3.5 Optional Git service and service adapters

Two chart blocks are off by default and render nothing until enabled:
`gitService.enabled` deploys one per-tenant `kodama git serve` StatefulSet
(`gitService.tenants[]`) with a repositories/journal PVC
(`/var/lib/kodama/git/repos` and `/var/lib/kodama/git/journal`, siblings on the
same volume), a ClusterIP Service, a pinned Caddy TLS sidecar, and a
NetworkPolicy whose ingress peers are exactly the declared
`gitService.tenants[].clients[]` and whose egress is only cluster DNS and the
Kodama API. `services.enabled` deploys one `kodama services --kind <kind>`
Deployment per `services.entries[]` entry; the accepted kinds are `webhook`,
`email`, `timer` and `responder` (the chart still refuses `merge_coordinator`:
the `kodama services --kind merge_coordinator` entry exists, but the chart does
not render its `--state-dir` and `--git-url` settings), a timer
needs `schedule`, and every adapter has only DNS and Kodama API egress and
denies all ingress (it opens no listener; both probes are exec-based).

Prerequisites: an existing service-credential Secret for every Git tenant and
service entry (`credential.existingSecret` plus `credential.key`), an existing
`kubernetes.io/tls` Secret for every Git tenant (`tls.existingSecret`), and a
storage class that can provision each tenant's
`gitService.tenants[].storage` claim. An email entry may also name a
`mailboxCredential.existingSecret` and key; the mailbox credential is mounted
read-only and exposed only through `KODAMA_MAILBOX_CREDENTIAL_FILE`, never as a
value; no `kodama services` code reads that variable today (DEF-08). The values
schema refuses an enabled block with a missing entry, tenant,
credential, TLS Secret, storage or client source, and caps a Git tenant id at 41
characters so `kodama-git-<tenant>` plus the StatefulSet revision label stays
within 63. Both workloads mount the API CA from
`secrets.tls.existingSecret` (`ca.crt`) and set `SSL_CERT_FILE` to it; only the
Kodama API is called, so the system trust roots are replaced by the installation
CA, not appended to them.

The Git workloads run in the release namespace and the listener is
TLS-terminated by the sidecar. Each Git Service name needs its own certificate
SAN: `kodama-tls` carries only the control-plane SANs, so issue a certificate
whose SAN covers `kodama-git-<tenant>.<release-namespace>.svc` (or the name
clients use) before enabling the entry. `scripts/bootstrap-secrets` issues it
as `kodama-git-<tenant>-tls` for every descriptor tenant when
`KODAMA_WITH_GIT_TLS` is set. Provisioning is an explicit operator
step: run `kodama git provision-tenant` with the same `--storage-root`
(`/var/lib/kodama/git/repos`) and `--journal-dir` (`/var/lib/kodama/git/journal`)
on the PVC, for example through `kubectl exec` into the Git Pod. A `baseURL`
pointing outside the cluster is refused by the rendered egress policy, which
allows only cluster DNS and the Kodama API. The tenant default-deny policy adds
the configured `gitService.port` to its Kodama egress when it differs from 8443,
so a non-default port stays reachable from tenant workloads. `gitService.port`
must be unprivileged (1024-65535) and must not be 8081: that port is the
plaintext smart-HTTP listener the git container exposes on the pod network, and
because both containers share the Pod network namespace the sidecar would
crash-loop on the taken socket. The values schema and the template guard both
refuse it, and the guard still refuses it when a render skips schema
validation.

The Git PVCs carry `helm.sh/resource-policy: keep`. Uninstalling the release or
removing a tenant entry leaves the repositories and receive journal in place;
re-enabling the same tenant name adopts the existing claim, and deleting the
data is a deliberate `kubectl delete pvc` after verifying it is no longer
needed. The repository PVC is mounted by the Git workload and, when
`gitService.enabled` is true, by the control plane; it is never mounted by an
execution workload. The control plane mounts only `repos/<tenant>`, never the
receive journal, and sets `KODAMA_GIT_STORAGE_ROOT`.

Retained-commit keep refs. The control plane's retention loop writes one ref
per commit named by a retained execution under `refs/kodama/keep/<sha>`, so a
rewinding `RW+` push cannot let a later `git gc` prune a commit that execution
evidence still names. The keeper enumerates each tenant's bare repositories
from the mounted storage root on every sweep: a new repository needs no
control-plane restart and no static list. `uploadpack.hideRefs` and
`transfer.hideRefs` pin `refs/kodama` out of every smart-HTTP advertisement, so
clients never see or fetch the bookkeeping. A sweep runs once at startup and
then once per day (`DefaultRetentionInterval`), so the window between a
rewinding push and the keep ref appearing is up to one interval. Do not run
`git gc` on a repository PVC inside that window after a rewinding push; keep a
repository-wide `git gc` maintenance job disabled or scheduled well after the
sweep. A repository with no retained evidence is a clean no-op, and one damaged
repository never stops the other tenants' sweeps.

On the single-worker stage the control plane and the tenant's Git StatefulSet
share the `ReadWriteOnce` claim directly. A multi-node cluster needs an `RWX`
storage class or both pods pinned to one node; an `RWO` claim that cannot attach
on both nodes leaves the control plane Pod unscheduled. The keeper refuses to
start if `KODAMA_GIT_STORAGE_ROOT` is missing or not a mounted directory, so an
unmounted claim is a loud configuration error, never an empty sweep.

The model gateway, the MCP tool proxy and private registries are not part of
these blocks. The server reads `KODAMA_MODELS_FILE`, `KODAMA_TOOL_PROXY_FILE`,
`KODAMA_REGISTRY_CONFIG_FILE` and `KODAMA_WORKLOAD_IMAGE_PULL_SECRETS`, but the
chart has no values for them and `config.extraEnv` refuses them; inject them
with an operator overlay (see [deploy/README.md](../../deploy/README.md)).

## 4. Network policy and exposure

### 4.1 Namespaces

| Namespace | Rendered by | Purpose |
|---|---|---|
| `kodama-system` (default `release_namespace`) | `scripts/deploy ... helm --create-namespace` | Control plane, broker, retained logs |
| `kodama-tenant-<tenant>` | [`templates/namespaces.yaml`](../../deploy/charts/kodama/templates/namespaces.yaml) | One per tenant; workloads, PVCs, workload CA |
| `kube-system` | cluster | DNS |
| `git` | operator | Expected by `permitGit`; the chart does not create it |

The control-plane namespace is deliberately **not** fenced by a
NetworkPolicy: the controller needs egress to PostgreSQL, NATS, KMS and DNS, and
a blanket deny would cut it off. Every tenant namespace starts from default-deny
ingress and egress.

### 4.2 What the tenant policies allow

[`templates/networkpolicy.yaml`](../../deploy/charts/kodama/templates/networkpolicy.yaml)
renders one policy per tenant:

- **Ingress:** only from the release namespace's `app.kubernetes.io/name=kodama`
  pods, to TCP 8443 and 8080, and only when `networkPolicy.permitKodama` is true.
- **Egress:** DNS (TCP/UDP 53) to `kube-system/kube-dns`; the control plane on
  TCP 8443/8080; and, when `networkPolicy.permitGit` is true, the `git`
  namespace's `app.kubernetes.io/name=git` pods on TCP 443.
- Nothing else. There is no CIDR, service, entity, port-only or allow-all rule.

[`templates/ciliumpolicy.yaml`](../../deploy/charts/kodama/templates/ciliumpolicy.yaml)
is the only way to widen tenant egress. It renders exactly two rules per tenant
when `networkPolicy.egressFQDNs` is non-empty **and** the
`cilium.io/v2/CiliumNetworkPolicy` CRD exists: DNS to `kube-system/kube-dns` on
port 53 restricted to the listed names, and `toFQDNs` on TCP 443. On a cluster
without Cilium FQDN support nothing renders; the chart never approximates the
allowlist with a CIDR. Cilium's L7 DNS proxy must be enabled for the DNS rule to
work. Scaleway Kapsule uses Cilium; GKE Dataplane V2 is Cilium-based; EKS uses
the VPC CNI policy engine, so verify FQDN behaviour before relying on
`egressFQDNs` there.

Workload egress to a package registry, an external API or an on-premises service
must be added deliberately. The safe pattern is an FQDN (the Cilium policy) or a
separate, reviewed policy in the tenant namespace. Do not add `0.0.0.0/0`.

### 4.3 Exposure

- The chart renders `Service/kodama` type `ClusterIP` on port 8443 only. There is
  no Ingress, Gateway API object, LoadBalancer or NodePort.
- `config.publicURL` defaults to
  `https://kodama.<namespace>.svc.cluster.local:8443`. That is the URL workloads
  use and the URL the server certificate is issued for.
- For operator access the stage practice is `kubectl port-forward`; the cluster
  API itself is restricted by the Scaleway ACL (`k8s_api_allowed_cidrs`) and the
  stage ACL admits only a named host. Port-forward is not a production ingress
  design.
- If you expose Kodama through your own ingress or gateway, terminate TLS with a
  certificate that covers the external DNS name. **The certificate generated by
  `scripts/bootstrap-secrets` has fixed SANs** — `kodama`,
  `kodama.<namespace>`, `kodama.<namespace>.svc`,
  `kodama.<namespace>.svc.cluster.local` and `127.0.0.1` — so a public name such
  as `kodama.example.com` is not in it. Re-issue through your PKI and replace the
  `kodama-tls` Secret, or terminate at the ingress and forward to the Service.
  The chart does not manage ingress certificates.
- The Kubernetes API endpoint is public on AWS (`endpoint_public_access = true`)
  and open by default on GCP; restrict it with the provider's authorized-network
  control. **Verify with your provider.**

### 4.4 Private database access

- Scaleway: the RDB instance is attached to the private network with IPAM
  (`deploy/scw/postgres.tf`); the descriptor exports the private endpoint and
  port.
- GCP: `ipv4_enabled = false` and private services access through the VPC
  peering range (`deploy/gcp/network.tf`).
- AWS: private subnets, a dedicated security group allowing 5432 only from the
  private subnet CIDRs, and `publicly_accessible = false`
  (`deploy/aws/network.tf`).
- The control plane reaches the database directly. Tenant namespaces do not need
  database egress and must not have it.

## 5. TLS and KMS

### 5.1 Certificate sources

| Trust | Source | Rotation |
|---|---|---|
| Control-plane HTTPS | `kodama-tls` Secret, issued by the installation CA in `scripts/bootstrap-secrets` | `scripts/bootstrap-secrets <cloud> --rotate-pki` |
| NATS server and client mTLS | `kodama-broker-tls` (server cert/key + client CA) and `kodama-broker` (client cert/key + CA + `nats_url`) | `--rotate-pki` |
| Tenant workload CA | `kodama-workload-ca` in each tenant namespace, mounted read-only at `/etc/kodama/ca/ca.crt` | `--rotate-pki` |
| PostgreSQL server | Provider CA, exported by OpenTofu as `database_ca_certificate` and stored as `ca.crt` in `kodama-database`; on AWS it is the operator-supplied RDS CA bundle (`TF_VAR_database_ca_bundle`) | Provider-managed; on AWS rotate the bundle together with `postgres_ca_cert_identifier`, then rerun `bootstrap-secrets aws` |
| OIDC discovery/JWKS | System roots plus optional `KODAMA_OIDC_CA_FILE` | Your issuer |

`generate_cluster_pki` creates a 4096-bit RSA CA valid 3650 days and 2048-bit
RSA leaves valid 825 days. The CA private key stays in the operator work
directory; the cluster receives only Secrets. There is no automatic renewal:
calendar the 825-day leaf expiry and the 3650-day CA expiry, and test a rotation
before you need it.

**Back up the PKI work directory.** The CA private key and every leaf live only
under `<state>/<cloud>/<cluster>/pki` (`KODAMA_DEPLOY_STATE_DIR`, otherwise
`${XDG_STATE_HOME:-$HOME/.local/state}/kodama/deploy`). The cluster holds
Secrets, never the key. If that directory is lost, the next
`scripts/bootstrap-secrets` run generates a **new** CA while preserving the
existing cluster Secrets (they already exist), so the CA on disk no longer
matches the CA the cluster trusts. You can then never issue a leaf the running
installation will accept, the re-issued leaves do not match the deployed
Secrets, and the next rotation becomes a forced, unprepared CA replacement.
Back the directory up together with `kodama-keys`.

**`--rotate-pki` replaces the CA.** It re-issues `kodama-tls`, the broker
leaves and every tenant `kodama-workload-ca`, so anything still holding the old
workload CA — including a running tenant workload whose mounted Secret predates
the rotation — cannot verify certificates from the new CA and loses TLS to the
control plane until it restarts. **Quiesce executions before rotating** rather
than relying on workloads to pick the new CA up in place.

### 5.2 The verify-full DSN rule

Production refuses to start unless **all four** PostgreSQL DSNs — runtime,
controller, publisher and auth — are strict `postgres://` URLs with exactly one
`sslmode=verify-full` option. The validator allows only the query keys
`sslmode`, `sslrootcert`, `connect_timeout` and `application_name`, at most once
each; `sslmode` must be exactly `verify-full`. `postgresql://`, a comma host
list, a Unix-socket host, a fragment and percent-encoded credentials are
refused. Passwords must be URL-safe (`[A-Za-z0-9._~-]`);
`scripts/bootstrap-secrets` refuses anything else instead of percent-encoding
it ([S10 §1](../specs/10-production.md#1-explicit-compositions),
[S07 §4](../specs/07-runtime-secrets.md#4-secret-custody-and-key-providers)).

The `ca.crt` key in `kodama-database` is mounted at `/etc/kodama/pg-ca`
**without** `optional: true`. A missing CA stops the pod rather than starting it
without a certificate root.

### 5.3 Broker TLS

The production composition requires `KODAMA_NATS_URL` (a single `tls://` URL
from the `kodama-broker` Secret) plus `KODAMA_NATS_CA_FILE`,
`KODAMA_NATS_CERT_FILE` and `KODAMA_NATS_KEY_FILE`. The stack chart renders
`verify: true`, mounts the client CA read-only and refuses to render without a
client-auth mode. There is no `verify: false` and no `no_auth_user`. In the
default `callout` mode the chart also renders `KODAMA_NATS_USER`,
`KODAMA_NATS_PASSWORD` and the `KODAMA_NATS_CALLOUT_*` settings from
`kodama-nats-auth`; the entry accepts them all or none.

### 5.4 KMS key reference grammar

One provider family is selected per installation, and a reference from another
family is refused. The Scaleway rule is an exact case-sensitive prefix followed
by a non-empty, whitespace-free name; AWS and GCP references must also match
the C-ADAPTER r17 provider grammar.

| Provider | Reference the OpenTofu root emits | Grammar the code enforces today |
|---|---|---|
| Scaleway | `scw-kms:<region>/<key-id>` (`deploy/scw/outputs.tf` prepends `scw-kms:` to the Key Manager key id, which is region-scoped) | `parseScalewayKeyRef` splits on one `/`; both parts must match `[a-z0-9-]+`. Traversal, empty parts, uppercase, percent escapes, a query or fragment and a foreign prefix are refused before any request. |
| AWS | `aws-kms:<key ARN>` | `ParseAWSReference` (C-ADAPTER r17): a full key or alias ARN, or `alias/<name>`. A full ARN names the region the client calls; an alias reference needs `AWS_REGION`, and when both are present they must agree. |
| GCP | `gcp-kms:<projects/.../cryptoKeys/...>` | `ParseGCPReference` (C-ADAPTER r17): `projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>`, each part validated before any request. |

The **scw-kms `<region>/<key id>` grammar is reserved as contract revision r16
and the revision has not been written** (finish-plan D3: "the `scw-kms` grammar
(r16) is deferred"). The parser and its tests exist, so treat the grammar as
current implementation behaviour, not as a published frozen contract. The
deferral is about publishing the revision, not about a missing parser:
`parseScalewayKeyRef` is present and pins the shape
([`scaleway_test.go`](../../internal/shell/integration/keys/scaleway_test.go)). Do not
invent a second spelling such as `scw-kms:<key-id>` without a region; the
current parser refuses it.

**Warning for AWS and GCP:** the descriptors, the chart and the production
entry agree on `aws-kms` and `gcp-kms`, but none of it has run against the real
cloud yet (`DEF-11`). Expect the first live deployment to be the G04
qualification run, not a routine install.

### 5.5 Key rotation and key loss

- **Provider key rotation is not the installation KEK rotation.**
  Scaleway sets `rotation_policy { rotation_period = "720h" }` (30 days), GCP
  sets `rotation_period` (default `2592000s`), AWS sets
  `enable_key_rotation = true`. The provider rotates the backing key material;
  existing wrapped blobs stay decryptable as long as the old key version exists.
- **`kodama-keys` is created once and never regenerated.** It holds
  `kek-v1.blob`, the KMS-wrapped 32-byte installation KEK. Deleting or
  regenerating that Secret makes every encrypted secret row unreadable. It is a
  data-loss decision, not a rotation procedure
  ([deploy/README.md](../../deploy/README.md)).
- The key provider supports several configured versions (`Current`, `Versions`,
  `Retained` in [`internal/shell/integration/keys/cloud.go`](../../internal/shell/integration/keys/cloud.go)),
  but `loadProductionKeys` loads only the current version from
  `kek-v<version>.blob`. There is no shipped re-wrap or multi-version rotation
  command. A future rotation needs a new blob plus both versions configured; do
  not improvise it on a live installation.
- The Scaleway IAM API key that authenticates KMS decrypt is created with a
  one-year expiry. Rotate it by replacing `scaleway_iam_api_key.kodama_kms`,
  rerunning `scripts/bootstrap-secrets` (which reapplies
  `kodama-kms-credentials`) and then **restarting the control-plane
  Deployment**. The order matters: the chart consumes
  `kodama-kms-credentials` through `envFrom`
  ([`templates/deployment.yaml`](../../deploy/charts/kodama/templates/deployment.yaml)),
  so the process reads the credential only at pod start
  ([`deploy/scw/kms.tf`](../../deploy/scw/kms.tf) creates the key). The running pod keeps
  the replaced (now invalid) key, and `/readyz` starts failing within 30 seconds
  of the old key ceasing to work (a successful KMS decrypt is cached for that
  long) and keeps failing until the restarted pod starts.
  That readiness gap is expected; do not debug it as a chart defect.
- AWS and GCP have no static KMS credential to rotate. The control-plane pod
  authenticates as its ServiceAccount: on EKS through the IRSA role named by
  `eks.amazonaws.com/role-arn`, on GKE through the Google service account
  named by `iam.gke.io/gcp-service-account`. The cloud issues short-lived
  tokens, so there is no `kodama-kms-credentials` Secret and no yearly expiry;
  access is revoked or changed in the role policy or the key IAM binding. The
  EKS webhook injects its variables and token volume only when a pod is
  created, so a ServiceAccount annotation added or changed later needs a
  control-plane restart. Not yet validated on a live cluster (G04).
- The replacement is a `scripts/deploy` limitation. The script has no
  action or flag that forwards `-replace` (or any other extra argument) to
  `tofu`: `plan` writes `<state>/scw/<cluster>/plan.tfplan`, and `apply`
  consumes that saved plan when it exists and deletes it. The replacement
  therefore has to be a manual `tofu` run against the same per-cluster data
  directory (`TF_DATA_DIR=<state>/scw/<cluster>/tfdata`) and the same
  `-var-file`/`-var` inputs the script used; that is outside the script's
  supported actions. A saved plan created before the replacement is stale —
  run `scripts/deploy scw plan` again to overwrite it before the next `apply`.
- **Scaleway only:** `kms_unprotected = false` on a production tfvars
  ([`deploy/scw/variables.tf`](../../deploy/scw/variables.tf)) keeps the Key
  Manager key from being deleted on cluster teardown, so old backups remain
  decryptable. The shipped `preview` tfvars sets it to `true`; do not copy that
  to production.
- **AWS:** `kms_deletion_window_days` (default `7`,
  [`deploy/aws/variables.tf`](../../deploy/aws/variables.tf)) is the waiting
  period before teardown actually deletes the key. The key is scheduled rather
  than destroyed at once, but once that window elapses blobs encrypted under it
  are unreadable. Decide the window deliberately; it is not a backup.
- **GCP:** the root has no deletion-protection variable for the key
  ([`deploy/gcp/kms.tf`](../../deploy/gcp/kms.tf)); the key ring and crypto key
  are ordinary resources and go with the teardown.
- **Teardown destroys the database, not just the cluster.**
  `scripts/deploy <cloud> destroy` uninstalls the Helm release (unless
  `--skip-helm-uninstall` is given) and then runs `tofu destroy`; the remote
  state bucket is preserved, nothing else is. The managed database instance is
  removed, and where the provider ties automated backups to the instance they
  go with it: AWS defaults to `postgres_skip_final_snapshot = true` (with
  `deletion_protection = !var.postgres_skip_final_snapshot`), so no final
  snapshot is taken; GCP defaults to `postgres_deletion_protection = false`
  (and `gke_deletion_protection = false` for the cluster). Export any database
  backup and key Secret you intend to keep before teardown, and verify the
  provider's backup-retention behaviour for Scaleway RDB. The state bucket is
  not a backup.

## 6. Secret custody and rotation

### 6.1 The Secret-name policy

The chart references Secrets **by name only**; it never templates, prints or
creates a Secret value. `scripts/bootstrap-secrets` creates the live set:

| Secret | Namespace | Keys | Created by |
|---|---|---|---|
| `kodama-database` | release | `database_url`, `controller_url`, `publisher_url`, `auth_url`, `ca.crt` | `bootstrap-secrets` (once) |
| `kodama-database-admin` | release | `admin_url` | `bootstrap-secrets` only when `TF_VAR_postgres_admin_password` is set |
| `kodama-broker` | release | `nats_url`, `ca.crt`, `tls.crt`, `tls.key` | `bootstrap-secrets` |
| `kodama-broker-tls` | release | `tls.crt`, `tls.key`, `ca.crt` | `bootstrap-secrets` |
| `kodama-nats-auth` | release | `issuer.seed`, `issuer.pub`, `shell-user`, `shell-password`, `callout-user`, `callout-password`, `account` | `bootstrap-secrets` (reused on a rerun; replaced only by `--rotate-nats-auth`) |
| `kodama-keys` | release | `kek-v1.blob` | `bootstrap-secrets` (never regenerated) |
| `kodama-kms-credentials` | release | `SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID`, `SCW_DEFAULT_REGION` | `bootstrap-secrets` on Scaleway only (reapplied every run). AWS and GCP have no such Secret: the chart does not reference it for `aws-kms` or `gcp-kms` (workload identity, §2) |
| `kodama-tls` | release | `tls.crt`, `tls.key`, `ca.crt` | `bootstrap-secrets` |
| `kodama-git-<tenant>-tls` | release | `tls.crt`, `tls.key` | `bootstrap-secrets` with `KODAMA_WITH_GIT_TLS` set |
| `kodama-workload-ca` | each tenant | `ca.crt` | `bootstrap-secrets` |
| `oauthsonas-tls`, `oauthsonas-access` | release | stage only | `bootstrap-secrets` with `KODAMA_WITH_OAUTHSONAS=1` |

**The administrator credential is a setup credential, not a runtime credential.**
The chart never mounts `kodama-database-admin`; only the one-shot
`scripts/deploy <cloud> migrate` pod reads it as `KODAMA_ADMIN_DATABASE_URL`.
[`internal/shell/app/migrate/migrate.go`](../../internal/shell/app/migrate/migrate.go)
records why the setup administrator must be removed or disabled after the
installation completes: it keeps `ADMIN` and `SET` on `kodama_owner` and can
assume that role at will. Treat the Secret as the highest-value credential in
the release namespace. Three workable shapes:

- **Keep it.** Simplest: every upgrade runs `migrate` without extra setup.
  Restrict read access with RBAC, alert on reads, and rotate the password
  deliberately.
- **Delete the Secret after installation.** Re-create it before the next
  upgrade by rerunning `bootstrap-secrets` with `TF_VAR_postgres_admin_password`
  set, which reapplies it. This removes the in-cluster copy but **not** the
  database role: the role remains usable with the infrastructure password, so
  deleting the Secret alone is custody hygiene, not a disabled credential.
- **Disable the role at the database after installation.** Strongest posture,
  but every future `migrate` then needs administrator re-enablement and a
  password change — an infrastructure operation, not a `scripts/deploy`
  action.

The trade-off is upgrade convenience against blast radius, and it interacts
with password rotation: rotating the administrator password means rerunning
`bootstrap-secrets` with the new `TF_VAR_postgres_admin_password` before any
`migrate`; rotating only the four role passwords does not touch it. If you
delete or disable the administrator, record how the next upgrade will restore
it before you need to upgrade under pressure.

The chart's `config.extraEnv` is a narrow allowlist for non-secret server
variables. It refuses cloud credentials (`SCW_*`, `AWS_*`, `GOOGLE_*`),
credential-shaped names, `KODAMA_DEV_*`, object values and values with URL
`user:password@` userinfo. Never put a secret there.

`bootstrap-secrets` keeps credential material out of argv: generated passwords go
to a 0600 env-file, the Scaleway KEK plaintext goes to the CLI as
`plaintext=@<file>` from a 0700 temporary directory, and `EXIT`/`INT`/`TERM`
traps remove every temporary path. Do not rewrite it to pass a password as an
argument.

### 6.2 Product secret store

Kodama's own secrets live in the `secrets` table as envelope-encrypted rows
(`ciphertext`, `wrapped_key`, `key_ref`), bound to installation and scoped
identity ([KODAMA.md](../../KODAMA.md#secret-custody)). The wrapping key is the
KEK in `kodama-keys`, itself wrapped by cloud KMS. The database backup and the
key Secret therefore have to be restored **together**; either alone is useless.

### 6.3 Rotation procedures

**Database role passwords.** There is no dedicated rotation command. The
supported primitives are `kodama migrate`, which sets each role's password from
the four DSNs, and `kodama-database`, which supplies those DSNs. Because
`bootstrap-secrets` preserves an existing `kodama-database`, a rotation has to
replace the Secret explicitly. A safe order is:

1. Generate four new passwords in the URL-safe grammar and write the new
   `database_url`, `controller_url`, `publisher_url` and `auth_url` values into
   the Secret (keep `ca.crt`).
2. Run `scripts/deploy <cloud> migrate` with the new Secret. The one-shot pod
   sets the role passwords to the new values.
3. Restart the control-plane Deployment so its connections use the new DSNs.
   Run this step **back to back with step 2**: once `migrate` changes the role
   passwords, old sessions keep working only until they reconnect, and every
   new connection fails until the Deployment has restarted with the new
   Secret.

This procedure is **not exercised on the stage**; rehearse it on a scratch
cluster. Do not rotate by deleting the database or the KEK.

**NATS credentials.** The default `callout` mode has two independent
rotations. `scripts/bootstrap-secrets <cloud> --rotate-pki` replaces the
installation CA, the broker server leaf and the client leaf in one run.
`--rotate-nats-auth` replaces the callout issuer key pair and both bypass
passwords in `kodama-nats-auth`. Neither touches the other's material. After
either, restart the NATS StatefulSet and the controller together: a live
connection keeps its old certificate, a CA change makes old clients untrusted,
and both pods read `kodama-nats-auth` only at start. A failed `--rotate-pki`
in the stage's history left mixed CAs; rerunning the same command repaired it
(finish-plan "Known limits"). In `credentials` mode, update the
`user`/`password` keys in the configured Secret and restart the broker and
controller; no script rotates that Secret.

**OIDC configuration.** `KODAMA_OIDC_ISSUER`, `KODAMA_OIDC_AUDIENCE`,
`KODAMA_OIDC_JWKS_URL` and `KODAMA_OIDC_TENANT_CLAIM` are required for a
`production` render and refused by `cmd/kodama/production.go` when missing. Put
them in the operator extra values (`kodama.config.extraEnv.*` for the stack
chart) and run `scripts/deploy <cloud> helm`. `KODAMA_OIDC_SUBJECT_CLAIM`,
`KODAMA_OIDC_PROJECT_CLAIM` and `KODAMA_OIDC_CA_FILE` are optional. The issuer
must be `https://`.

**OAuthSonas is stage-only.** `oauthsonas.enabled` defaults to `false`; when
enabled, anyone with the access code can become any persona. It must never be
enabled where real identities matter. Production takes any OIDC issuer through
configuration; `scripts/oauthsonas-token` is a stage convenience and refuses the
access code in argv or the environment ([scripts/oauthsonas-token](../../scripts/oauthsonas-token)).

**Model provider credential.** The model gateway and the external MCP tool
proxy are optional and stay disabled unless `KODAMA_MODELS_FILE` and
`KODAMA_TOOL_PROXY_FILE` name a trusted configuration file; a named file that
is unreadable or malformed refuses startup. The chart neither renders these
variables nor accepts them in `config.extraEnv`, so enabling them needs an
operator overlay that mounts the file and sets the variable. When enabled, the
gateway's upstream
credential is a Shell-managed secret in the `secrets` table and is resolved only
when the Shell calls the provider; it is not a chart value and has no
`existingSecret` name. Rotate by replacing the stored secret value through the
authorized API; no pod restart is required. **This path is not yet qualified.**

**KMS credential.** On Scaleway it is reapplied by every `bootstrap-secrets`
run. Rotate the Scaleway IAM API key with the replacement and restart order in
§5.5, then run `bootstrap-secrets` again; the KEK blob is not touched. The
restart is part of the procedure, not an optional cleanup: the Deployment reads
the credential through `envFrom` only when the pod starts. On AWS and GCP there
is no stored KMS credential: the pod uses EKS IRSA or GKE Workload Identity
through its annotated ServiceAccount (§2, §5.5).

## 7. Upgrades and migrations

### 7.1 Build and pin the image

```sh
scripts/image build-push --repository <registry>/<path>/kodama --tag <tag>
# prints <registry>/<path>/kodama@sha256:<64 hex>
```

The script builds [`deploy/images/kodama/Containerfile`](../../deploy/images/kodama/Containerfile)
with Podman, labels it with the revision and reports the digest the registry
accepted, not the local image id. Registry login is your responsibility; the
script never reads or prints a credential. Export the printed values:

```sh
export KODAMA_IMAGE_REPOSITORY=<registry>/<path>/kodama
export KODAMA_IMAGE_DIGEST=sha256:<64 hex>
```

The chart renders `repository@digest` whenever `image.digest` is set. Pin the
digest in production: a tag can be repointed, and `scripts/deploy <cloud>
migrate` refuses to run without both variables and a `sha256:` digest. The
`helm` action refuses the placeholder `registry.example.com/kodama` unless you
pass `--allow-placeholder-image`.

A released version is also published as `ghcr.io/optimiware/kodama/kodama:<version>`
with its digest in the release notes; see [release.md](release.md).

### 7.2 The deployment sequence

```sh
# once per cloud account, then per cluster
scripts/bootstrap-state <scw|gcp|aws> --project <id>      # or --account for aws
scripts/deploy <cloud> init
scripts/deploy <cloud> plan
scripts/deploy <cloud> apply
scripts/deploy <cloud> kubeconfig
scripts/bootstrap-secrets <cloud>                         # TF_VAR_postgres_admin_password set (AWS: TF_VAR_database_ca_bundle is also required before plan/apply)
scripts/deploy <cloud> migrate                            # BEFORE the controller serves traffic
scripts/deploy <cloud> helm
```

Notes that matter in production:

- `apply` writes the descriptor under the deploy state root
  (`KODAMA_DEPLOY_STATE_DIR`, otherwise
  `${XDG_STATE_HOME:-$HOME/.local/state}/kodama/deploy`) as
  `<cloud>/<cluster>/descriptor.json`. Every later action verifies the
  descriptor's cloud and cluster identity.
- `bootstrap-secrets` must run after a successful `apply` (it reads the provider
  CA and KMS outputs) and before `migrate` (which needs
  `kodama-database-admin`). It is idempotent and preserves existing PKI and
  database Secrets.
- `migrate` creates the application database when it does not exist, applies the
  reviewed migrations, and sets the four role passwords from the DSNs. On
  Scaleway the database is not an OpenTofu resource; `kodama migrate` creates it
  as the administrator and hands it to the owner role.
- `migrate` is a one-shot pod, not a chart Job. It takes a session-level advisory
  lock (`SetupLockKey`), refuses an already-applied migration whose body changed,
  and records each applied migration and checksum in `kodama_meta.schema_migrations`
  ([`cmd/kodama/migrate.go`](../../cmd/kodama/migrate.go)).
  `scripts/deploy` waits up to 300 seconds and deletes the pod on every exit path.
  **A migration blocked on locks for more than 300 seconds is interrupted.**
- The `helm` action installs or upgrades release `kodama-<cluster>` in
  `release_namespace`, creates the namespace, waits up to 10 minutes, and accepts
  extra values only through `KODAMA_HELM_EXTRA_VALUES`.

The quick-start in [deploy/README.md](../../deploy/README.md) shows
`bootstrap-secrets` followed directly by `helm` and omits `migrate`; follow the
order above instead. That README omission is a documentation follow-up.

### 7.3 Migrations are forward-only

- There are no down migrations and no rollback command. The ledger refuses a
  rewritten applied migration; it never re-runs one.
- Each migration runs in its own transaction. A migration that alters an
  existing table can take `ACCESS EXCLUSIVE` locks in a different order from
  runtime transactions. Run `scripts/deploy <cloud> migrate` (`kodama migrate`)
  **before** the controller takes traffic on that revision, and ideally with
  the old controller stopped, so a migration does not deadlock against live
  transactions.
- The product guarantees the schema converges for a fresh install and an upgrade
  from the previous ledger. It does not guarantee that an older binary can run
  against a newer schema.

### 7.4 Rolling upgrade order

1. Read the migration set for the target revision and decide whether it takes
   `ACCESS EXCLUSIVE`, rewrites a large table or adds a constraint.
2. Take or verify a database backup and confirm the key Secret is backed up.
3. Build and push the image; export the digest.
4. Quiesce traffic if the migration needs exclusive locks.
5. `scripts/deploy <cloud> migrate`.
6. `scripts/deploy <cloud> helm`. The controller is `Recreate`; the NATS
   StatefulSet may also roll in the same release. Expect a short publication
   outage and a readiness gap, and let the release wait for readiness.
7. Verify `GET /readyz` is 200, then run a smoke execution and read its logs
   after `cleanup.state=complete`.
8. Confirm `kodama_meta.schema_migrations` contains the expected newest entry.

### 7.5 Rollback limits

- **Image rollback:** re-deploy the previous digest with
  `scripts/deploy <cloud> helm`. This is the supported rollback.
- **Schema rollback:** none. Migrations are forward-only. If the new migration
  is not backward-compatible with the previous binary, going back to the old
  image is unsafe.
- **Data rollback:** only a restore from a verified backup, following the
  restore order in §9. There is no automatic data rollback; promotion and
  rollback of build artifacts are a new pipeline execution with an older digest
  ([KODAMA.md](../../KODAMA.md#delivery-recipes-and-artifacts)).

### 7.6 The stuck-cleanup class

Terminal execution state and cleanup are independent. `execution.inspect`
(`GET /api/v1/executions/{execution}`) always returns a `cleanup` object with
`state` (`none`, `pending`, `complete`), `workload_id`, `workspace_id`,
`attempts` and `last_error` ([`api/execution.go`](../../api/execution.go)).
Read it when:

- an execution is terminal but its Job or PVC still exists;
- `cleanup.state` is `pending` with `attempts` at the runtime policy's limit
  (3) and a `last_error`;
- a workspace cannot be released before the next handoff.

A pending cleanup with exhausted attempts is visible, not "done". The runtime
rescans unfinished executions and retries within its backoff, but there is no
public "force cleanup complete" operation. **Do not delete the Job to make the
record look clean:** Kubernetes Job deletion alone does not prove the process
stopped, and uncertain stop blocks workspace handoff
([S07 §5](../specs/07-runtime-secrets.md#5-logs-and-cleanup)). Escalate with the
resource identities from `cleanup`. If you manually remove a resource, record
the identity and the evidence in the incident, and let the reconciler confirm
the new state.

## 8. Monitoring and alerting

### 8.1 What the server exposes

| Endpoint | Meaning | Dependencies |
|---|---|---|
| `GET /healthz` (HTTPS) | Process can answer. Calls no dependency. | None |
| `GET /readyz` (HTTPS) | 200 only when the server is started, not closing, all four role pools can begin/roll back a transaction, the authenticated TLS broker dials, and the configured KMS decrypts the current blob (a successful decrypt is reused for up to 30 s). Otherwise 503 with a fixed body. | PostgreSQL ×4, NATS, KMS |
| `GET /metrics` (HTTPS) | 200 with `kodama_publication_backlog`, `kodama_consumer_lag` and `kodama_pending_cleanup` (gauges sampled from durable state, capped at 500) and `kodama_refused_pushes_total`, `kodama_refused_reports_total` and `kodama_dependency_errors_total` (in-process counters, reset on restart); 503 with a fixed body if a sample fails | PostgreSQL |

The readiness budget is 4 seconds total; the chart's probe timeout is 10 seconds
(minimum 5) ([`internal/shell/app/readiness.go`](../../internal/shell/app/readiness.go)).
A PostgreSQL, NATS or KMS outage makes the pod not ready but does
not restart it. There is no built-in dashboard, scrape configuration or alert
rule; alert on the signals below using your platform's probe, metrics, log and
database tooling.

### 8.2 Alert set

| Signal | Source | Suggested condition | Why it matters |
|---|---|---|---|
| Readiness | `GET /readyz` | Non-200 for 5 minutes | Database, broker or KMS dependency is down; no new work can be admitted safely. |
| Liveness/restarts | Kubernetes pod state | Restart count rising or `CrashLoopBackOff` | Configuration refusal, certificate/key problem, or a crash loop. |
| Controller lag | Execution records and pending publication rows | Executions past an action deadline with unchanged state, or `events.publish_state = 'pending'` growing for 15 minutes | The controller or publisher is not draining durable work. `/metrics` exposes `kodama_publication_backlog` and `kodama_consumer_lag` (each capped at 500); no metric covers executions past an action deadline, so use the durable rows for that. |
| Cleanup backlog | `kodama_pending_cleanup`, `cleanup` rows / `execution.inspect` (the metric has no attempts dimension) | `state = 'pending'` with `attempts >= 3`, or `next_retry_at` in the past | A Job or PVC cannot be removed; workspace handoff and capacity are at risk. |
| Migration status | `kodama_meta.schema_migrations` | Expected migration missing after an upgrade, or `scripts/deploy <cloud> migrate` exiting non-zero (the script deletes its pod on every exit path) | The binary may be ahead of the schema. |
| Backup age | Provider backup listing | Older than your RPO (recommendation 15 minutes) | Data-loss window exceeds the objective. |
| NATS storage | `jetstream-data-nats-0` PVC / `maxFile` | PVC above 80% or `maxFile` exhausted | Publication and replay degrade; events still persist in PostgreSQL. |
| NATS availability | Broker readiness and `/readyz` | Probe failure | Publication and live logs stop; execution progression continues from persisted state. |
| KMS credential | KMS readiness, provider IAM | Decrypt denied/unauthenticated, or API key near the one-year expiry | Secrets cannot be unwrapped; readiness fails. |
| Retained-log volume | `kodama-retained-logs` PVC | Above 80% | Log retention is capacity-bound; monitor the volume. |
| Database capacity | Provider metrics | Connections/disk/IO near tier limits | Readiness and migration time degrade. **Verify with your provider.** |

The publisher backlog query must run in a database session with the same scope
the application uses; the `events` and `cleanup` tables have row-level security.
Prefer the authorized API for cleanup and use the database only for read-only
aggregate checks.

### 8.3 Log retention and capacity

Policy: logs 30 days, ordinary evidence 90 days, audit metadata one year,
operation receipts until project deletion, raw model/tool content off by
default with a seven-day opt-in ([S10 §5](../specs/10-production.md#5-retention-and-measured-targets)).
The chart gives the process a 10Gi retained-log PVC by default. That is a
capacity limit, not a guarantee that 30 days of logs fit: size the volume for
your log volume, monitor it, and export long-term audit data if you need it
beyond the PVC. Workload logs are authorized sensitive data; do not assume
arbitrary workload output can be sanitized
([S07 §5](../specs/07-runtime-secrets.md#5-logs-and-cleanup)).

## 9. Backups, restore drills and objectives

### 9.1 Scope

Back up PostgreSQL (including the event table and the `secrets` ciphertext), the
retained-log PVC, the JetStream PVC, and the key Secrets (`kodama-keys`, and the
Secret material needed to authenticate to KMS). Workspaces are disposable and
excluded. Git repositories, external registries and their journals are owned by
their operators and are not backed up by this repository
([S10 §4](../specs/10-production.md#4-backup-and-restore)).

A database backup without `kodama-keys` cannot decrypt the secret rows. A key
Secret without the database cannot reconstruct anything. Treat them as one
recovery set.

### 9.2 Scheduled and manual mechanisms by cloud

The repository configures the **scheduled** database backup; it ships **no**
manual snapshot command and no PVC or Secret backup job.

| | Scaleway RDB | Cloud SQL | RDS |
|---|---|---|---|
| Scheduled | Logical backup every 6 hours (`postgres_backup_frequency_hours = 6`), 30-day retention, same region | Daily backup at 02:00, point-in-time recovery on, 7-day transaction-log retention, 30 retained backups | Automated backups, 30-day retention, window 02:00–03:00 |
| Point-in-time recovery | Not configured by the root; **verify with your provider** | Enabled by the root; window is the 7-day transaction-log retention | Enabled within the backup retention window; **verify with your provider** |
| Cross-region | `backup_same_region = true`; cross-region not configured | Not configured; available as a provider option — **verify with your provider** | Not configured; cross-region snapshot copy/replica is a provider option — **verify with your provider** |
| Manual/on-demand | Provider console or CLI; **verify the exact command with your provider** | Provider console or CLI; **verify with your provider** | Provider console or CLI; **verify with your provider** |

Manual database backups are provider operations: this repository does not wrap
them. For the retained-log PVC, the JetStream PVC and the key Secrets there is no
manual repository command either; use your platform's volume-snapshot and
Secret-backup tooling. Test the combination you choose, because a database-only
restore cannot decrypt secret rows.

The shipped Scaleway stage takes backups every 6 hours, which **cannot meet an
RPO of 15 minutes**. That is a documented limitation of the reference
configuration, not a product defect. If you need a 15-minute
RPO, change the provider backup or PITR setting deliberately and re-measure.

### 9.3 Objectives are recommendations

Treat **RPO ≤ 15 minutes, RTO ≤ 4 hours and 30-day PostgreSQL recovery** as
recommendations for a production installation, not as properties the chart
enforces. KODAMA.md and S10 §5 call them recommended objectives that Kodama
does not enforce; they remain targets until a timestamped drill measures them.
This guide does not certify that any installation meets them.

S10 §5 also states the reporting rule: a **failed restore is a failure**; a
**missed recommended objective is a documented limitation** of that
deployment. Do not report an RPO miss as "acceptable" without recording it,
and do not report a restore that lost state or could not reconcile as "passed
with a caveat".

### 9.4 Restore drill procedure (Q03)

The drill harness is [`test/qualification/restore`](../../test/qualification/restore);
its current limitations follow. For 0.1.0 the local coordinated
drill passed. On the Scaleway stage the content of a restored database was
verified by hand and the export and `pg_restore` procedure below was
rehearsed. No service was resumed from a restored database, so no stage RTO
was measured.

Order of operations, from S10 and the product restore plan:

1. **Migrate** the restore target first: `kodama migrate` creates the
   application roles, and a missing role refuses before any restore effect.
2. **Fence** old controllers, enforcing services and consumers so no old actor
   can effect a change.
3. **Restore** database, key, log and event state.
4. **Reconcile** existing workloads and external effects from stable identities.
5. **Enable dispatch** only after fencing and reconciliation are complete. A
   missing fence or unresolved effect blocks dispatch; it never triggers replay.

Local drill (real Podman stack; no cloud):

```sh
export TMPDIR=/tmp/oc/w-q03
mkdir -p "$TMPDIR"
go test -count=1 ./test/qualification/restore/
scripts/infra-slot.sh stack -- env KODAMA_REQUIRE_INFRA=1 \
  go test -race -tags=qualification -count=1 -timeout=30m \
  ./test/qualification/restore/
```

Stage drill (never restore into the managed database):

- Restore an RDB backup into a **new** database named
  `kodama-restore-<run-id>` on the existing instance.
- The committed guard in
  [`test/qualification/restore/stage.go`](../../test/qualification/restore/stage.go)
  refuses an empty `database-name` (the CLI would default to the source), a name
  that is the source or managed database, a different instance, or an existing
  database.
- Drop only the exact database this run created. An ambiguous restore or drop is
  never retried; observe and reconcile first.
- `scripts/q03-stage-drill plan|restore|drop` runs the guard and the planners
  and executes the restore and the drop inside the exclusive `scw-stage-rdb`
  slot. `restore` and `drop` require `KODAMA_Q03_STAGE_APPLY=1`. The operator
  creates the backup, passes its ID, verifies the restored database and then
  runs `drop` with the same run ID.
- Record the revision, image digest and run IDs, never credentials.
- On Scaleway the drill's own SQL confirmation fails (DEF-12). The database
  that `scw rdb backup restore` creates is managed, owned by
  `_rdb_superadmin`, and refuses the bootstrap admin; the privilege API
  accepts `kodama_runtime`. Use it for a content check only: grant
  `kodama_runtime` `readonly` on the restored database with
  `scw rdb privilege set`, then read the migration version and the per-tenant
  counts from a pod in the cluster.

Restoring a database Kodama can use (rehearsed on Scaleway RDB, PostgreSQL
17):

A provider-restored database cannot serve Kodama, because its roles cannot
connect to it. Restore the exported archive into a database created through
SQL instead. Run the SQL as the bootstrap administrator from a pod that can
reach the database.

1. Export the logical backup and download the archive inside the cluster
   (`scw rdb backup export <backup-id> --wait` returns the URL; treat it as a
   credential).
2. Let the administrator inherit the owner role for the duration of the
   restore: `GRANT kodama_owner TO <admin> WITH INHERIT TRUE`. Without it the
   table grants of the archive are refused.
3. `CREATE DATABASE "<target>"` and
   `ALTER DATABASE "<target>" OWNER TO kodama_owner`, as `kodama migrate`
   does.
4. `pg_restore -d "<target>" <archive>` as the administrator. Do **not** pass
   `--no-owner` or `--role`: that loses every table grant and gives
   `kodama_meta` to the wrong owner. A correct restore prints no error.
5. Put the membership back: `GRANT kodama_owner TO <admin> WITH INHERIT
   FALSE`. A `REVOKE ... GRANTED BY <admin>` does not undo step 2 on Scaleway,
   where the grant is recorded under `_rdb_superadmin`.
6. Check the migration version, the per-tenant counts as `kodama_owner`, and
   that `kodama_runtime` reads its scope and nothing unscoped.
7. Only then follow the order of operations above: fence, reconcile, and
   point `Secret kodama-database` at the restored database before dispatch is
   enabled. Steps 1–6 were rehearsed; step 7 was not rehearsed on a live
   installation.

### 9.5 Reading the timeline report

A drill records UTC milestones: `backup.started`, `backup.completed`,
`failure.injected`, `restore.started`, `restore.completed`, `service.resumed`.
It records a committed and a restored watermark. Then
([`timeline.go`](../../test/qualification/restore/timeline.go)):

| Field | Meaning |
|---|---|
| `rpo_ms` | Committed watermark time minus restored watermark time, in milliseconds — the data-loss window |
| `rto_ms` | `failure.injected` to `service.resumed`, in milliseconds — the outage window |
| `lost_ids` | Committed IDs absent after the restore, sorted |
| `meets_rpo`, `meets_rto` | Inclusive comparison against the objectives; exactly at the limit still meets it |
| `rpo_basis` | How the RPO was measured; the local same-commit drill writes `local-same-commit` and omits `meets_rpo` rather than presenting a zero as an objective verdict |

An incomplete timeline (missing a milestone or watermark) is refused rather
than reported as a partial objective met.

For the stage drill the RPO basis is the age of the most recent recoverable
backup, not a same-commit artefact. Read a missed RPO as a backup-cadence
limitation; read a restore that lost committed state, failed to reconcile, or
could not be fenced as a failed drill.

### 9.6 Manual recovery of a real installation

There is no shipped promotion or failover script. The supported shape is:
restore the provider backup to a new instance or database, restore the key
Secret and the two PVC snapshots with your platform tooling, fence consumers,
start the controller against the restored database, reconcile, then enable
dispatch. The controller must not be pointed at the old database while the
restore is in progress. Rehearse this on a scratch installation; the repository
has not qualified it on AWS or GCP. On Scaleway, `scripts/q03-stage-drill`
ships only the scratch-database restore and drop. The database that
`scw rdb backup restore` creates refused Kodama's bootstrap admin role on the
stage; §9.4 has the export and `pg_restore` procedure that was rehearsed
instead.

## 10. Pre-production checklist

### Access and inputs

- [ ] The cloud account/project, region and cluster name are decided; the
      cluster name is 2–18 lowercase alphanumeric characters or hyphens,
      starts with a letter, ends with an alphanumeric character, and matches
      the tfvars.
- [ ] `tofu >= 1.11`, `helm >= 3.14`, `kubectl`, `jq`, `openssl`, `base64` and
      `podman` are available on the operator host, plus Go (the `go.mod`
      version) or a prebuilt `tools/nats-auth-key` named by
      `KODAMA_NATS_AUTH_KEY_TOOL`, and on Scaleway the `scw` CLI.
- [ ] The state bucket exists (`scripts/bootstrap-state`) and the state key is
      unique per `(cloud, account/project, region, cluster)`.
- [ ] A container registry and a repository path are chosen; the image is built
      and pushed with `scripts/image build-push` and pinned by digest.
- [ ] An OIDC issuer, audience, JWKS URL and tenant claim exist. The issuer is
      `https://`. OAuthSonas is **not** enabled.
- [ ] The external DNS name, ingress/gateway and certificate plan are decided;
      do not assume `kodama-tls` covers a public name.

### Infrastructure

- [ ] `cost_profile = "ha"` is selected, or a zonal database is an accepted,
      recorded risk.
- [ ] AWS: `postgres_skip_final_snapshot = false` (which also turns on
      `deletion_protection`). GCP: `postgres_deletion_protection = true`, and
      `gke_deletion_protection` is decided for the cluster.
- [ ] `TF_VAR_postgres_admin_password` is set in the operator environment and is
      URL-safe.
- [ ] AWS: `TF_VAR_database_ca_bundle` (the public RDS CA PEM bundle, never a
      private key) is set before `plan` and `apply`, and
      `postgres_ca_cert_identifier` matches it; a change to the identifier is an
      in-place instance modification that RDS applies in the maintenance window
      and normally with a reboot (verify with your provider).
- [ ] `k8s_api_allowed_cidrs` is set (Scaleway); the API is not open to
      `0.0.0.0/0`. The AWS/GCP API exposure is restricted with provider controls.
- [ ] Node pool minimum/maximum sizes accommodate control plane + NATS + peak
      concurrent Jobs.
- [ ] The Kubernetes version is a version the provider still supports.
- [ ] Scaleway: `kms_unprotected = false` on production. AWS: the
      `kms_deletion_window_days` value (default `7`) is the one you accept.

### Deployment

- [ ] `scripts/deploy <cloud> init`, `plan`, `apply` and `kubeconfig` succeeded
      on the exact revision.
- [ ] `scripts/bootstrap-secrets <cloud>` ran after `apply`, with the admin
      password set; `kodama-database`, `kodama-database-admin`, `kodama-broker`,
      `kodama-broker-tls`, `kodama-nats-auth`, `kodama-keys`, `kodama-tls` and
      every tenant `kodama-workload-ca` exist, plus `kodama-kms-credentials` on
      Scaleway. On AWS and GCP there is no KMS credentials Secret; instead the
      `kodama` ServiceAccount in the release namespace carries
      `eks.amazonaws.com/role-arn` or `iam.gke.io/gcp-service-account`.
- [ ] `scripts/deploy <cloud> migrate` succeeded **before** the controller
      served traffic, and `kodama_meta.schema_migrations` contains the expected
      newest migration.
- [ ] `scripts/deploy <cloud> helm` succeeded with the digest-pinned image and
      the production profile; the release is `kodama-<cluster>`.
- [ ] `GET /readyz` returns 200 and `GET /healthz` returns 200 through the
      Service.

### Security and network

- [ ] The tenant list is complete, ids are lowercase DNS labels, and each
      `kodama-tenant-<tenant>` namespace exists with its LimitRange and
      default-deny policy.
- [ ] Tenant egress is limited to Kodama, DNS, Git and the explicit FQDN
      allowlist; there is no `0.0.0.0/0` rule.
- [ ] The database is reachable only from the private network; it is not
      publicly accessible.
- [ ] `sslmode=verify-full` is present exactly once in all four DSNs, and the
      provider CA is mounted.
- [ ] The broker is `tls://`, mTLS is on with the authorization callout (the
      default mode), and no anonymous or `verify: false` path exists.
- [ ] The KMS key reference is the family the server supports; on Scaleway it is
      `scw-kms:<region>/<key-id>`. AWS and GCP are implemented but not
      live-validated (`DEF-11`).

### Operations

- [ ] A backup schedule meets the chosen RPO, or the gap is recorded as a
      limitation. The Scaleway 6-hour default does not meet 15 minutes.
- [ ] `kodama-keys` and the other Secrets are in the backup set, and a restore
      of the database alone is known to be insufficient.
- [ ] The PKI work directory under `<state>/<cloud>/<cluster>/pki` is backed up
      with `kodama-keys`; losing it means the next `bootstrap-secrets` run
      generates a new CA against the preserved cluster Secrets.
- [ ] A teardown (`scripts/deploy <cloud> destroy`) is known to remove the
      managed database and, where the provider ties them to it, its backups; any
      backup or key material you intend to keep is exported first.
- [ ] A restore drill was run within the period your policy requires; its
      timeline, lost IDs and RPO/RTO were recorded.
- [ ] Alerts exist for readiness, restarts, controller lag, cleanup pending
      with attempts, migration status, backup age and NATS/retained-log storage.
- [ ] Log and audit retention match policy, and the retained-log PVC is sized
      for it.
- [ ] The 825-day leaf and 3650-day CA expiry dates are calendared, and a
      `--rotate-pki` rehearsal is planned.
- [ ] The one-time `Recreate` remedy is understood if the Deployment was ever
      `RollingUpdate`.
- [ ] The release revision, image digest, descriptor and execution IDs are
      recorded. No credential, token or `.env*` content is in the record.

## What this guide cannot verify

These items are marked **verify with your provider** above and are listed here
so an operator does not mistake guidance for evidence:

- Provider point-in-time-recovery windows, cross-region backup/snapshot options
  and exact on-demand snapshot commands.
- Cloud quota limits, database connection limits and node capacity for a
  specific workload mix.
- GKE/EKS API endpoint exposure defaults and the provider controls that restrict
  them.
- Cilium FQDN policy behaviour on EKS's VPC CNI (the chart renders it only when
  the Cilium CRD exists).
- The yet-to-be-written r16 scw-kms grammar record; the runtime parser is the
  current behaviour.
- The AWS and GCP production key adapters, which are not wired into
  `cmd/kodama/production.go` at this revision.

## Related documents

- [KODAMA.md](../../KODAMA.md) — product behaviour, secret custody, operations
  and acceptance criteria.
- [S10 — Environments, production, and operations](../specs/10-production.md).
- [S07 — Runtime adapters, materialization, secrets, and logs](../specs/07-runtime-secrets.md).
- [deploy/README.md](../../deploy/README.md) — chart contents, broker
  authentication and secret handling.
