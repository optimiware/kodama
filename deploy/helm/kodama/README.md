# Kodama Helm chart

**Task:** T-018. **Status:** chart renders and passes schema and policy checks (`helm lint`, `helm template`, Go structural assertions). The chart is joined to the composed server (T-027): it selects the core assembly and the production environment, renders the server's settings document and execution policy, and projects the secret files the core assembly reads; a no-cluster render/configuration test in `internal/app` (`TestChart*`) feeds the rendered objects to the server's own configuration loader and namespace preflight. It has **not** been installed on a cluster: the installed round trip and restricted baseline on kind are T-089 (T-056 for the untrusted profile). Statements below about runtime behavior describe the design the chart is built for; the chart tests verify rendered manifests only.

## Trust profile

This chart packages the **trusted-engineer** execution baseline only: one control-plane server, restricted per-tenant Job namespaces, and the policy that surrounds them (installation design: [installation and configuration](../../../docs/operations/installation-and-configuration.md)).

- It is **not** an isolation boundary for mutually untrusted tenant code. Pod Security `restricted`, default-deny NetworkPolicy, quotas and per-tenant service accounts limit trusted-engineer workloads; stronger sandboxing is the separate, release-gated untrusted-hosted profile (T-055/T-056).
- There is **no operator, no CRD, and no bundled production database**. PostgreSQL, NATS JetStream, S3-compatible storage, Git, an OCI registry, OIDC and a cloud KMS are external dependencies you operate.
- **Jobs are not created by Helm.** The server creates attempt Jobs inside the tenant namespaces at runtime (T-021). Helm renders the namespaces, policy, RBAC and execution settings those Jobs run under, plus one *fixture* Job in a ConfigMap (see [Execution settings](#tenant-execution-namespaces-and-jobs)).

## What is installed

| Resource | Notes |
| --- | --- |
| Deployment `<release>` | One replica, `Recreate` strategy, `kodama server --assembly core` with `KODAMA_ENV=production`; probes `/healthz` (liveness) and `/readyz` (readiness) |
| Service, optional Ingress | ClusterIP service on `service.port`; Ingress and TLS Secret reference are opt-in |
| ServiceAccount `<release>` | The control plane's identity; the only subject of any RoleBinding |
| ConfigMaps `<release>-config`, `<release>-execution` | `<release>-config`: the server's settings document (`config.yaml`, read strictly by the server). `<release>-execution`: a readable rendering of the execution settings and the Job fixture (not read by the server) |
| Migration Job + ServiceAccount (Helm hook) | `pre-install,pre-upgrade`; migration role only |
| Per tenant namespace: Namespace, ServiceAccount, ResourceQuota, LimitRange, NetworkPolicies, Role + RoleBinding | See [below](#tenant-execution-namespaces-and-jobs) |
| ClusterRole + ClusterRoleBinding | `get` on the named tenant namespaces only (to check Pod Security labels) |

## Prerequisites

- Kubernetes >= 1.28 on AWS, GCP or Scaleway with a NetworkPolicy-enforcing CNI.
- Helm (developed and tested with Helm 4.3.1; the chart uses `apiVersion: v2` and a draft-07 values schema).
- PostgreSQL with TLS and **separate** migration (owner) and runtime (non-owner, non-superuser, no `BYPASSRLS`) roles.
- NATS JetStream, S3-compatible storage, Git HTTPS, an OCI registry and OIDC as described in [integrations](../../../docs/operations/installation-and-configuration.md#integrations).
- The cloud's KMS (AWS KMS, GCP Cloud KMS or Scaleway Key Manager).
- The Secrets listed next, created **before** installing. The chart never creates credentials.

## Secret references

The chart accepts only names of existing Secrets and workload-identity annotations. No value in `values*.yaml` is a credential; `values.schema.json` rejects unknown fields (for example a `password`) and endpoint URLs with embedded userinfo.

Secret file names follow the [scaffold secret-file contract](../../../dev/configuration.md#secret-file-contract). Files are mounted read-only at `/secrets` in the server Pod and `/secrets` in the migration Pod.

| Secret (value) | Required keys | Mounted into |
| --- | --- | --- |
| `secrets.runtime.existingSecret` (default `kodama-runtime`) | `database-runtime-url`, `nats-token`, `git-read-token`, `git-write-token`, `git-credentials.json` (installation Git credential references: `ref`, `username`, `passwordFile` naming a token file, `urlPrefix`, `tenants`), `cursor-key` and `idempotency-hash-key` (raw bytes, at least 32 each), `runtime-signing-key` (PEM P-256 at generation `execution.runtime.signingGeneration`), `runtime-ca.crt` (PEM certificates the workloads trust for `execution.runtime.url`); `s3-access-key` and `s3-secret-key` when `config.s3.credentials=static`; `ca.crt` when `config.tls.customCA=true`; configured keyfiles (32 bytes each, default `master-key`) only for `secrets.adapter=keyfile` | server only |
| `secrets.migration.existingSecret` (default `kodama-migration`) | `database-migration-url`; `ca.crt` when `config.tls.customCA=true` | migration Job only |
| `secrets.kms.scw.credentialsSecret` | Scaleway API token in `secret-key` | server only, `/etc/kodama/kms-credentials` |
| `imagePullSecrets[]` | `kubernetes.io/dockerconfigjson` | server, migration |
| `ingress.tls.secretName` | `tls.crt`, `tls.key` (or issued by your controller) | Ingress |

Rules the chart enforces: the server never mounts the migration role and the migration Job never mounts the runtime role; tenant Jobs mount no Kubernetes Secrets (secret files reach them through the memory-backed `/kodama/secrets` volume under the runtime protocol, not through Helm); AWS and GCP use workload identity (annotate `serviceAccounts.server.annotations`), not static keys. `tls.crt`/`tls.key` of the development Git fixture are never mounted into the server.

## Install and render

Run from the repository root.

```sh
# Render without a cluster (schema validation happens here)
helm template kodama deploy/helm/kodama --namespace kodama-system
helm template kodama deploy/helm/kodama --namespace kodama-system -f deploy/helm/kodama/values-dev.yaml
helm template kodama deploy/helm/kodama --namespace kodama-system -f deploy/helm/kodama/values-external.yaml

# Lint every profile
helm lint --strict deploy/helm/kodama
helm lint --strict deploy/helm/kodama -f deploy/helm/kodama/values-dev.yaml
helm lint --strict deploy/helm/kodama -f deploy/helm/kodama/values-external.yaml

# Install (after creating the referenced Secrets; real install qualified by T-026)
helm upgrade --install kodama deploy/helm/kodama --namespace kodama-system --create-namespace \
  -f deploy/helm/kodama/values-external.yaml \
  --set image.repository=<registry>/kodama --set image.tag=<tag> --set image.digest=sha256:<64 hex>
```

`--create-namespace` creates only the release namespace; tenant namespaces come from `execution.tenants`.

### Profiles

| File | Purpose |
| --- | --- |
| `values.yaml` | Defaults. Renders a development-grade profile with `example.invalid` placeholders |
| `values-dev.yaml` | kind/dev fixture references (`kodama-deps` Services from `dev/configuration.md`), **local keyfile KEK (development only)**, plaintext fixture endpoints. Adjust the egress CIDR to your kind network |
| `values-external.yaml` | External PostgreSQL/NATS/S3/Git/registry/OIDC with TLS, AWS KMS selected explicitly, Ingress with TLS, two tenant namespaces. Switch clouds through `secrets.kms.provider`, resources and cloud settings |

**Placeholders.** The [image workflow](../../../.github/workflows/image.yml) publishes `ghcr.io/<owner>/kodama:<semver>` from semver git tags, starting at the committed `0.0.1`. Chart examples do not pin that image: every digest below is a placeholder of 64 identical hex digits (`0000…` in `values.yaml`, `1111…` in `values-dev.yaml`/`values-external.yaml`) and the repositories use `example.invalid` or a local registry. They exist so the schema's required `sha256:<64 hex>` pattern is exercised; override them with the published repository, tag, and digest. NOTES.txt warns when the all-zero default digest is still in use. Likewise every host name, CIDR (documentation ranges), ARN, bucket and Ingress host in the examples is illustrative.

## Values reference

The schema (`values.schema.json`) is strict: unknown fields are rejected at every level, so a mistyped key fails rendering instead of being ignored.

| Key | Meaning | Constraint |
| --- | --- | --- |
| `profile` | `development` or `external` | `external` requires `https://`/`tls://` endpoints and `secrets.adapter=kms` |
| `authority.mode` | Authority profile | `standalone` supported; `hosted` fails rendering ([below](#standalone-vs-hosted-authority)) |
| `image.repository/tag/digest/pullPolicy` | Release image (server, migration, entrypoint injector) | `digest` required, `sha256:` + 64 hex; tag must not be `latest` |
| `imagePullSecrets` | Names of existing pull Secrets | References only |
| `securityContext` | `runAsUser`/`runAsGroup` adjustable; `runAsNonRoot`, `privileged`, `allowPrivilegeEscalation`, `readOnlyRootFilesystem` are fixed by the schema | Containers also drop `ALL` and use `RuntimeDefault` seccomp |
| `serviceAccounts.{server,migration}.annotations` | Workload-identity annotations | |
| `server.replicaCount` | Server replicas | Fixed to `1` for the initial profile |
| `server.port`, `logLevel`, `shutdownTimeout` | Rendered as `KODAMA_HTTP_ADDR`, `KODAMA_LOG_LEVEL`, `KODAMA_SHUTDOWN_TIMEOUT` | Frozen process configuration; the process validates ranges (shutdown timeout in `(0, 1m]`) |
| `server.terminationGracePeriodSeconds` | Should exceed `shutdownTimeout` (not enforced by the chart) | Default 30 with a 10 s drain |
| `server.resources`, `server.probes`, `server.tmpSizeLimit` | Requests/limits, probe timing, `/tmp` emptyDir (root FS is read-only) | |
| `service`, `ingress` | Service type/port; opt-in Ingress (`host` required when enabled; TLS requires `secretName`) | |
| `secrets.runtime.existingSecret`, `secrets.migration.existingSecret` | Secret names | See [Secret references](#secret-references) |
| `installation.id` | Stable installation identifier (binds envelope-encryption contexts) | Required; never reuse for another installation |
| `config.*` | Non-secret endpoints/identifiers for NATS, S3, Git, registry, OIDC, TLS | Rendered into the settings document; `config.s3.credentials` must be `static` for the core assembly |
| `secrets.adapter` + selected settings | `keyfile` or `kms`; clouds `aws`, `gcp`, `scw` | See [KMS selection](#kms-selection) |
| `migration.*` | Hook toggle, command/args (`migrate`), `runtimeRole` (the runtime role's name, never its credential), backoff, deadline, resources | |
| `execution.tenants[]` | `namespace` (must match `kodama-*`, unique, different from the release namespace) and `tenantId` (the Kodama tenant whose attempts run there; one tenant per namespace) | Both required; the namespace is annotated with its tenant, which the server's preflight requires |
| `execution.runtime` | `url` (https origin workloads call back), `signingGeneration`, `egress[]` (the callback destination) | The server serves plain HTTP: `url` must terminate TLS in front of it, outside this chart; the trust roots are the `runtime-ca.crt` Secret file |
| `execution.serviceAccountName` | Per-namespace execution service account | Token never mounted |
| `execution.entrypoint.sourcePath/copyCommand` | Entrypoint binary injection (the server's injector command) | |
| `execution.volumes.*SizeLimit` | Bounded scratch, outputs, memory-backed secrets/identity, tools | |
| `execution.job.*` | Deadline, UID/GID, resources for tenant Jobs and the injector | |
| `execution.exampleJob.image` | Placeholder workload image for the fixture | Digest required |
| `execution.quota`, `execution.limitRange` | ResourceQuota and LimitRange applied to every tenant namespace | |
| `execution.network.egress[]` | Explicit CIDR egress rules (Git, S3, registry) | Every rule needs ports; ports 5432, 6432, 4222, 6222, 8222 are rejected |
| `execution.rbac.manageSecrets` | Grant `create/get/list/delete` on Secrets in tenant namespaces (the reconciler lists managed Secrets) | Default `false`; enable only if T-021 needs it |
| `execution.trustProfile` | Declared execution trust | Fixed to `trusted-engineer` |

### Server process configuration

The server is started as `kodama server --assembly core` (the composed product, never the health-only scaffold) with this environment, and no other:

| Variable | Value | Why |
| --- | --- | --- |
| `KODAMA_ENV` | `production` | The chart's workload runtime is Kubernetes, which is the production environment. Its construction guards refuse the development keyfile, plaintext fixtures and a non-https callback, so an unsupported selection stops the server at startup instead of starting weaker. |
| `KODAMA_CONFIG_FILE` | `/etc/kodama/config/config.yaml` | The settings document below. |
| `KODAMA_HTTP_ADDR`, `KODAMA_LOG_LEVEL`, `KODAMA_SHUTDOWN_TIMEOUT` | from `server.*` | The frozen process configuration ([`dev/configuration.md`](../../../dev/configuration.md)). |

The runtime Secret is mounted at `/secrets`, the server's default secrets directory (`KODAMA_SECRETS_DIR`), so no variable names it. The in-cluster Kubernetes API origin comes from the kubelet's `KUBERNETES_SERVICE_HOST/PORT`; its token and CA are the service-account defaults.

The startup probe allows three minutes for initialization, covering the maximum two-minute KMS deadline plus other dependency setup. Kubernetes enables liveness/readiness probing after the startup probe succeeds; the HTTP listener opens only after assembly completes.

Everything else reaches the process through:

1. **the settings document** `<release>-config` / `config.yaml` (`version: 2`): installation id, `secrets` adapter configuration, NATS/S3/OIDC endpoints, the runtime origin and signing generation, and the Kubernetes **execution policy** (tenants and namespaces, digest-pinned injector image and command, resource bounds, volume sizes, identity, termination grace, and every destination a workload may reach). It carries no credential. The server reads it strictly (an unknown member stops startup) and a variable that is set always wins over it; it never selects the environment or the assembly. The Go type is `ChartConfig` in `internal/app/chartconfig.go`.
2. **secret files** at `/secrets`, projected from the runtime Secret: `database-runtime-url`, `nats-token`, `git-read-token`, `git-write-token`, `git-credentials.json`, `cursor-key`, `idempotency-hash-key`, `runtime-signing-key`, `runtime-ca.crt`, `s3-access-key` and `s3-secret-key` (static object store credentials), `ca.crt` (custom CA) and `master-key` (development keyfile only). The migration Job mounts `database-migration-url` only.

#### What fails closed (and why a profile may not start)

The server's production guards are not weakened for any chart profile. Under this chart these selections render but the server refuses them at startup, by name; the no-cluster test asserts each refusal:

- **Cloud KMS providers (`aws`, `gcp`, `scw`)**: missing configuration, unavailable credentials or a failed startup wrap/unwrap probe stop startup.
- **`secrets.adapter: keyfile`** (and so the `development` and default profiles): the development key-encryption key is refused under production.
- **Plaintext endpoints** (the `development` profile's `http://` object store and issuer): the object store and OIDC fixtures are refused under production.
- **`config.s3.credentials: workloadIdentity`**: the core assembly reads static `s3-access-key`/`s3-secret-key` files and serves no other object store credentials.
- **A non-https `execution.runtime.url`** is refused by the schema and by the server.

Use actual cloud keys/workload credentials, static S3 credentials and HTTPS dependencies for deployment. T-104 qualifies installed readiness and live KMS access; T-029 qualifies full recovery and T-056 the supported release.

### KMS selection

`secrets.adapter` selects `keyfile` or `kms`. For KMS, set `secrets.kms.provider` to `aws`, `gcp` or `scw`, plus `kekId`, `current`, `resources` (immutable label to cloud key mapping), and optional `startupTimeout` (default `10s`, maximum `2m`, covering discovery and probe). Only the selected adapter/cloud settings may be populated. `profile=external` requires KMS. Old top-level `kms` values and version-1 server documents must be migrated.

| Provider | Block | Credentials |
| --- | --- | --- |
| `aws` | `aws.region`; resources are key IDs/ARNs, never aliases | SDK workload credentials, e.g. server service-account `eks.amazonaws.com/role-arn` annotation |
| `gcp` | Resources are `projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>` | Application-default credentials, e.g. server service-account `iam.gke.io/gcp-service-account` annotation |
| `scw` | `scw.credentialsSecret`; resources are key UUIDs or `fr-par/<uuid>` | Existing Secret key `secret-key`, mounted at `/etc/kodama/kms-credentials/secret-key`; only `fr-par` is supported |
| `keyfile` adapter | `secrets.keyfile.current` and `files`; empty chart block defaults to `v1: /secrets/master-key` | Test only; files contain exactly 32 raw bytes, outside PostgreSQL |

Example Helm values fragment (placeholder resource):

```yaml
secrets:
  adapter: kms
  keyfile: null
  kms:
    provider: scw
    kekId: kodama-stage
    current: v1
    resources:
      v1: fr-par/11111111-1111-4111-8111-111111111111
    startupTimeout: 10s
    scw:
      credentialsSecret: kodama-kms-scw
```

Helm renders `scw.credentialsFile` into the server document; credential bytes stay in the mounted Secret. The app initializes the selected client and probes before workers or HTTP serving begin. Changing configuration requires a rollout. Keep `installationId`, `kekId` and retained label mappings stable. Add a new label and change `current` to rotate; old labels/keys remain needed by stored envelopes and backups. The configuration name `scw` preserves the existing stored `scaleway` identity.

### Standalone vs hosted authority

`authority.mode` selects, explicitly and independently of execution trust, who owns tenant identity, membership and credential custody.

- `standalone`: local OIDC memberships and credential custody. This is the only supported value and the bootstrap sequence in the [installation guide](../../../docs/operations/installation-and-configuration.md#installation-acceptance) describes it.
- `hosted` (Optimi-hosted): **unsupported until T-061** (which depends on O-18/O-19). Selecting it **fails rendering** with an explanatory error. The chart never falls back to standalone, and a standalone install does not create a platform tenant. See [platform integration](../../../docs/specs/platform-integration.md).

## Migration ordering

The migration is a Helm `pre-install,pre-upgrade` hook, so it completes before the server Deployment is created or rolled:

1. Hook weight `-10`: the migration ServiceAccount (token not mounted).
2. Hook weight `0`: migration Job, restart `Never`, `backoffLimit` default 0, `activeDeadlineSeconds` default 600. It mounts only `database-migration-url` (and `ca.crt` if configured), runs `args` (default `migrate`) with the runtime role's name under the image ENTRYPOINT, and is deleted on success (`hook-succeeded`); a failed Job is kept for inspection until the next hook run (`before-hook-creation`).
3. Rendered resources are applied; the server rolls with `Recreate`, so old and new control planes never overlap.

The Secrets must already exist when the hook runs. The migration role owns the schema; the runtime role is a non-owner without `BYPASSRLS`, so the server cannot change the schema. Migrations run once under a database lock using the ordered module manifest (T-002/T-027). Set `migration.enabled=false` only when migrations are run by another controlled procedure.

The command is `kodama migrate`. It applies the declared module order as the owner and grants the runtime role by name (`KODAMA_DATABASE_RUNTIME_ROLE`, from `migration.runtimeRole`): the migration Job never holds the runtime credential.

## Tenant execution namespaces and Jobs

For every `execution.tenants[].namespace` the chart renders:

- **Namespace** with `pod-security.kubernetes.io/{enforce,audit,warn}: restricted` (versions `latest`).
- **Namespace annotation** `kodama.optimi.com/tenant: <tenantId>`; the server's namespace preflight refuses a namespace not bound to its tenant.
- **ServiceAccount** `kodama-execution` (configurable) with `automountServiceAccountToken: false`, distinct from the server and migration accounts.
- **ResourceQuota** and **LimitRange** (`execution.quota`, `execution.limitRange`).
- **NetworkPolicies:** `default-deny` (ingress and egress) and `allow-external-egress` with explicit `ipBlock` rules only: the runtime callback destination (`execution.runtime.egress`) and the Git, S3 and registry rules (`execution.network.egress`). These are exactly the destinations of the server's execution policy, and the server's namespace preflight refuses any other egress shape, so there is no DNS rule and no Pod or namespace selector peer: workloads reach destinations by address. The policy is always rendered; the callback rule is always present. There is **no rule for PostgreSQL or NATS**, the schema rejects their ports, and tenant Pods carry no control-plane database or broker credentials.
- **Role + RoleBinding** granting only the control plane: Jobs `create/get/list/watch/delete`, Pods `get/list/watch/delete`, `pods/log get`, events `get/list/watch`, and read-only `get/list` on ServiceAccounts, ResourceQuotas, LimitRanges and NetworkPolicies (policy checks). No wildcards, no `escalate/bind/impersonate`, no Secrets unless `execution.rbac.manageSecrets=true`. A single ClusterRole can only `get` the named tenant namespaces.

The server namespace itself is not made default-deny by this chart. The runtime callback goes to `execution.runtime.url`, an https origin that terminates TLS in front of the server; the server's own listener is plain HTTP.

### Execution settings and the Job fixture

`<release>-execution` carries, for operators and tests (the server does **not** read it; its execution policy is the `execution` section of `config.yaml`):

- `execution.yaml`: namespaces, service account, `callbackUrl` (`execution.runtime.url`), Job policy (`backoffLimit: 0`, `restartPolicy: Never`, `activeDeadlineSeconds`, no token), and the **entrypoint injection** settings: an init container from the *same release image* copies the static `kodama` binary (`execution.entrypoint.sourcePath`, default `/usr/local/bin/kodama`) into the shared tools volume `/kodama/bin`; the workload starts `/kodama/bin/kodama step`. Volumes: private `/workspace` and `/kodama/outputs` emptyDirs, memory-backed `/kodama/secrets` and `/kodama/identity` (`medium: Memory`), and `/kodama/bin` read-only to the command, all with size limits.
- `job-example.yaml`: a restricted example Job (one primary container, one injector init container, `backoffLimit: 0`, `activeDeadlineSeconds`, `restartPolicy: Never`, non-root, drop `ALL`, no privilege escalation, `RuntimeDefault` seccomp, no service-account token, no Secret volumes). It is a **fixture to read, not a workload**: Helm never creates it, and the server builds the real Job with admission, labels and fingerprints at runtime.

The default `copyCommand` is `cp <sourcePath> /kodama/bin/kodama`, which requires a copy tool in the release image. If the release image is static/distroless, T-016/T-021 provide the self-copy command via `execution.entrypoint.copyCommand`.

## Upgrade notes

- Pin the release's tested image digest and verify a usable backup and KMS/Git/artifact recovery path first ([upgrades](../../../docs/operations/upgrades-backup-and-recovery.md#upgrade)).
- Pause dispatch when mixed-version execution is unsupported, run `helm upgrade`, then run identity/source/execution/result smoke tests and resume dispatch. Binary rollback requires a compatible schema; otherwise follow the documented restore procedure rather than `helm rollback` alone.
- The hook migrates before the server rolls. A failed migration fails the upgrade and leaves the previous Deployment running.
- Changing the settings document rolls the server (checksum annotation). Changing `execution.tenants` adds namespaces; **removing one never deletes it** (see below).
- **Kept guardrails persist.** Tenant NetworkPolicies, quotas, limit ranges and namespaces carry `helm.sh/resource-policy: keep`, so Helm never deletes them. Change their rules by editing values and upgrading (they are updated in place); removing a rule block never revokes it. `allow-external-egress` is therefore always rendered. Delete a kept resource with `kubectl` only when you intend to.
- The one-replica profile has no high availability; the `Recreate` strategy implies a short outage per rollout.

## Retention-aware uninstall

`helm uninstall` removes the chart's **control plane**, and keeps the tenant evidence boundary.

| Removed by `helm uninstall` | Kept |
| --- | --- |
| Server Deployment, Service, Ingress | Tenant **Namespaces** (`helm.sh/resource-policy: keep`) |
| Server ServiceAccount, config and execution ConfigMaps | Per-tenant ServiceAccount, ResourceQuota, LimitRange and NetworkPolicies (kept, so retained workloads stay restricted) |
| Control-plane Roles, RoleBindings, ClusterRole/Binding | Anything the server created at runtime in tenant namespaces: Jobs, Pods, logs, events (not Helm-owned) |
| Hook resources are not tracked by the release: the migration Job is deleted on success, the migration ServiceAccount hook object remains | The referenced Secrets (you own them), including `master-key` if used |

Removing the chart does **not** delete, and the chart has no option to delete:

- the **PostgreSQL** databases and roles (product state, audit, envelopes);
- **Git** repositories and refs, including retained run refs (source recovery depends on them and on your backup/mirror policy);
- **KMS** keys and key versions (encrypted secret envelopes need them; never destroy a shared wrapping key to remove one tenant);
- **S3** artifacts, logs and evidence, and the NATS streams;
- **registry** images;
- **external deployments** and previews created through delivery targets.

Removing a tenant from `execution.tenants` during an upgrade keeps its namespace and guardrails for the same reason. To decommission a tenant: stop admission, revoke access, settle active work, export or apply retention to evidence ([retention and offboarding](../../../docs/operations/upgrades-backup-and-recovery.md#retention-and-offboarding)), and only then delete the namespace yourself (`kubectl delete namespace kodama-tenant-<name>`). Reinstalling with the same release name and namespace re-adopts the kept resources because they still carry Helm ownership metadata (to be verified by T-026); a different release name needs explicit adoption or deletion first.

## Tests

The Go tests in this directory implement `helm_render_profiles` and `helm_policy_assertions` and need no cluster:

```sh
KODAMA_REQUIRE_HELM=1 go test -count=1 ./deploy/helm/kodama/...
```

Without `KODAMA_REQUIRE_HELM=1`, the tests skip when `helm` is not on `PATH`; with it, a missing binary fails. Each test uses an isolated Helm cache/config. They are structural (no golden files).

| Test | Checks |
| --- | --- |
| `Test_helm_render_profiles` | Strict schema; `helm lint --strict` and `helm template` for default, dev, external (AWS, GCP, Scaleway); expected resource inventory; package excludes test files |
| `Test_helm_policy_assertions` | One server replica; no privileged/privilege escalation/host namespaces/hostPath; non-root, drop `ALL`, seccomp on every container including init containers; frozen environment only; no plaintext credentials in rendered output or values files; distinct server, migration and tenant service accounts; runtime and migration database roles never share a Pod; RBAC without wildcards or cluster-admin; tenant Pod Security labels, quota, limits, default-deny NetworkPolicy and no DB/NATS egress; Job fixtures with `backoffLimit: 0` and one primary container; all images pinned by digest; negative `helm template` cases that must fail (replicaCount, plaintext password/token, privileged, zero/multiple KMS providers, dev keyfile in external, unpinned images, DB/NATS egress, hosted authority, and more) |

## Handoff

**T-027 (composition, done here)**
- The chart selects the core assembly and `KODAMA_ENV=production`, renders the settings document and execution policy the server reads, projects the secret files it needs and names the migration role without its credential. `TestChart*` in `internal/app` verifies this with no cluster.

**T-021 (Kubernetes executor)**
- The executor's policy is the settings document; `<release>-execution` is a readable rendering and fixture only.
- Create Jobs at runtime in the configured tenant namespaces under `kodama-execution`; Helm does not.
- If admission material must be delivered as a Secret, set `execution.rbac.manageSecrets=true` or request a narrower Role; the chart grants no Secret access by default.
- Confirm the RBAC set is sufficient (Jobs, Pods, `pods/log`, events, read-only policy objects) and the injector `copyCommand` against the release image (also T-016).

**T-089 (installed chart on kind; T-026 for the Podman checkpoint)**
- Perform the real install on kind (a profile the server can start once a supported key provider exists, T-029; adjust the egress CIDRs and replace placeholder digests, secret names and the runtime origin), verify migration-before-roll, probes, NetworkPolicy enforcement (callback and listed destinations allowed; Postgres/NATS denied), restricted Job admission under Pod Security `restricted`, quota behavior, and uninstall retention (kept namespaces, re-adoption).

**Other**
- **T-104/T-029:** live Kubernetes/cloud acceptance and full recovery; T-105 supplies startup wiring and the credential-file contract.
- **T-016:** release-image self-copy command for entrypoint injection.
- **T-061:** hosted authority; until then `authority.mode=hosted` fails rendering.
- **T-055/T-056:** untrusted-hosted isolation is a different profile, not this chart's claim.
