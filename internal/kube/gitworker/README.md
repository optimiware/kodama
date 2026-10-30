# `internal/kube/gitworker` — tenant Git hosting on Kubernetes

Task T-129. One Git worker and one durable volume per tenant, in a Kodama-managed service
namespace that is separate from every namespace that runs tenant commands ([ADR 0011](../../../docs/adr/0011-internal-tenant-git-service.md)
section 3; requirements GR-03, GR-04, IT-17; acceptance GR-A03, IT-A13). **Package implementation
only**: nothing wires it. `kodama git serve` is T-128/T-132, the chart and values are T-156, and
`internal/app` composes none of it, so no installation provisions a worker because this package
exists. The `repository.Hosts` HTTP adapter that calls a discovered worker is not part of this task.

## What it does

`Reconciler.Reconcile(ctx, tenant, WorkerSpec)` renders and converges, for one tenant:

| Object | Name | Shape |
| --- | --- | --- |
| Namespace | `kodama-git-<tenant key>` (`TenantKey`: 24 hex of a framed digest) | Pod Security `restricted` (enforce, audit, warn) at a pinned version |
| ServiceAccount | `git-worker` | `automountServiceAccountToken: false`; the Pod does not mount a token either |
| PersistentVolumeClaim | `git-data` | `ReadWriteOncePod`, the named class, never owned by the Deployment |
| Secret | `git-worker-identity` | `token`, `public-keys`, `ca.crt` only (see below) |
| Service | `git-worker` | ClusterIP on the worker port |
| NetworkPolicy | `git-default-deny`, `git-allow-control-plane-ingress`, `git-allow-control-plane-egress` | deny all; ingress from the control plane's Pods (namespace and Pod selector in one peer) on the worker port; egress to the control plane's Pods and kube-dns only |
| Deployment | `git-worker` | 1 replica, `Recreate`, restricted Pod (non-root, read-only root, drop ALL, RuntimeDefault seccomp, no escalation), readiness and liveness on `/healthz` |

Order: preflight (storage class, identity) → namespace → **isolation check** (no foreign NetworkPolicy
or Pod) → service account → claim → Service → policies → identity Secret → Deployment. A refused class
or identity creates nothing; a namespace holding an allow-all policy or a stray Pod from a partial
provisioning receives neither the identity nor a worker (`ErrIsolation`) until an operator removes
the object; and the worker never starts without its network policies.

### Worker environment (the contract for `kodama git serve`)

`KODAMA_GIT_TENANT_ID`, `KODAMA_GIT_DATA_ROOT=/data`, `KODAMA_GIT_LISTEN_ADDR=:<port>`,
`KODAMA_GIT_CONTROL_URL`, `KODAMA_GIT_IDENTITY_TOKEN_FILE`, `KODAMA_GIT_PUBLIC_KEYS_FILE`,
`KODAMA_GIT_CA_FILE` (files under `/var/run/kodama/identity`), `KODAMA_GIT_WORKER_INSTANCE`
(the Pod UID) and `HOME`/`TMPDIR=/tmp`. No variable references a Secret. The token file is
refreshed in place by the kubelet when the Secret changes; the Deployment is not restarted for a
token rotation. The default command is `/usr/local/bin/kodama git serve`.

### Identity material

`IdentityProvider.WorkerIdentity` (T-136 implements it) returns an issued, tenant-bound worker
token, public verification keys and the control-plane CA. The reconciler refuses material bound
to another tenant and an expired or nearly expired token. The public keys must be PEM `PUBLIC KEY`
(PKIX) blocks and the CA PEM `CERTIFICATE` blocks, parsed strictly with nothing else in the document;
any other format (a JWK set, public or private, a private key of any kind, text around the blocks) is
refused. Workers hold no signing key, no database or broker credential and no Kubernetes API token. Material is written only when it differs from the Secret.

## Ownership and adoption

Every object carries the owner label, the tenant key, its role, the tenant ID, a **fingerprint**
of the object as rendered and, below the namespace, the **namespace UID**. An existing object is
adopted only when all of that agrees, it has no owner and no foreign finalizer; a Pod is accepted
only through ReplicaSet → Deployment controller UIDs. Anything else that holds one of the names is
`ErrForeign` and is never changed or deleted.

* Owned but stale or tampered Deployments, NetworkPolicies, the identity Secret and the
  namespace's labels are corrected in place (`PUT` with UID and `resourceVersion` preconditions).
* Owned objects that this package cannot correct in place (claim, Service, ServiceAccount, an
  immutable Secret) are `ErrDrift`: reported, not changed.
* A NetworkPolicy or Pod in the namespace that this package did not create is `ErrIsolation`:
  policies are additive and a second Pod contends for the volume, so the tenant is reported not
  ready (and `Endpoint` refuses it) until an operator removes it.
* A live Deployment, Service and NetworkPolicy must equal their rendering **as documents**: every member
  present in the live object that was not rendered (a `subPath`, `mountPropagation`, `envFrom`, a
  sidecar, a lifecycle hook, an unrendered Pod field) is drift, except the defaults the API server adds
  (enumerated in `verify.go`); quantities compare by value. `Status.Drift` lists the paths, never values.
* A cached ready status survives a failed pass only when the failure established nothing (a failed read
  before drift was found or any mutation began). Once drift is found or a create or replace is issued,
  any failure of the pass withdraws readiness, so `Endpoint` stops answering.
* A lost create (`ErrUnknownOutcome`) or a lost race (`ErrAlreadyExists`) is resolved by reading
  the object back and verifying it like any other.

## Volume lifecycle

`Reconcile` never deletes anything. Replacing the worker (new image, command or resources) replaces
the Deployment in place; the claim, volume and namespace are untouched. The storage class must exist,
have `reclaimPolicy: Retain` and a provisioner the installation declared `ReadWriteOncePod`-capable
(`Options.RWOPProvisioners`: the API cannot tell, so an undeclared provisioner is refused); a bound
volume that is not the claim's, or whose own reclaim policy is not `Retain`, is `ErrStorage`.

`OffboardTenant` is the only operation that removes a claim. It requires `Retention.MayDeleteVolume`
to answer yes (a refusal or failure is `ErrRetention`), verifies the namespace is the tenant's, and
deletes it under its UID. With a `Retain` class the volume stays on the storage backend until the
storage administrator releases it: removing the data is a second, deliberate step outside Kodama.

## Discovery

`Reconciler.Endpoint(ctx, tenant)` returns `Endpoint{BaseURL}` (`<scheme>://git-worker.<namespace>.svc:<port>`)
only while the last reconciliation found that tenant's worker ready and not older than
`Options.StatusTTL` (default 2 minutes); otherwise `ErrWorkerUnavailable` (retryable; map it to the
`Unavailable` outcome of `repository.Hosts`). A transient cluster outage does not flip a fresh
ready observation; a conflict, drift, isolation or storage refusal does. Run `Reconcile` for every
tenant more often than the TTL.

## Tests

```sh
make check-go PKGS='./internal/kube/gitworker/... ./internal/kube/kubernetes/...'
```

The acceptance tests are `Test_unit_gitworker_*` (render per tenant, single Pod volume, ingress from
the control plane only, no platform credentials, adopt verified only, volume retained on replace,
service discovery) over an in-memory cluster, plus golden renders in `testdata` (regenerate with
`KODAMA_UPDATE_GOLDEN=1` and review the diff). `gitworker_client_test.go` in `../kubernetes` covers
the new client operations over an HTTP fixture.

### Kind conformance

`conformance_test.go` holds two tests that need a prepared cluster scope and skip in the unit layer and
without a cluster; a skip is not evidence:

```sh
make check-go-integration DEV_SCOPE=t129 DEV_STACK=cluster PKGS='./internal/kube/gitworker/...'
```

* `Test_conformance_gitworker_two_tenants` proves two tenants get isolated namespaces, claims and
  volumes; the control plane reaches a worker while an execution namespace, a control-plane-labelled
  Pod of another namespace and another Pod of the control plane's namespace do not; a verified tenant
  reconciles with no write against the real API server; a replaced worker continues the same volume;
  and offboarding one tenant leaves the other and keeps the retained volume. It runs on kind's
  `rancher.io/local-path` class, which is **not a CSI driver and cannot provide ReadWriteOncePod**: the
  test lists that provisioner in `Options.RWOPProvisioners` for itself only, and it **does not prove
  single-Pod exclusivity**.
* `Test_conformance_gitworker_rwop_exclusive` proves GR-04: a second writer Pod naming the claim is
  refused by the scheduler (`ReadWriteOncePod`) and the reconciler reports the Pod. Its prerequisite is
  `KODAMA_TEST_GIT_STORAGE_CLASS`, naming a qualified CSI class with `reclaimPolicy: Retain` that
  supports `ReadWriteOncePod`; without it the test skips with that message, and it refuses local-path.
  T-133 runs it on stage with the Scaleway CSI class.

The worker is the scope's `kodama` image running a busybox `nc` responder on the worker port; the real
worker is T-128/T-132's. See the header of the test for the environment variables
(`KODAMA_TEST_GIT_IMAGE`, `KODAMA_TEST_GIT_STORAGE_CLASS`, `KODAMA_TEST_GIT_PROVISIONER`).

## Limits

* Not wired into any assembly; no chart values; no `repository.Hosts` adapter.
* A claim's size, class, a Service's port and the ServiceAccount are not edited in place: a change of
  those in the desired spec is `ErrDrift` until an operator acts.
* Enforcement of NetworkPolicy by the cluster's CNI is proved only by the kind test, not from the API.
* A claim and a ServiceAccount are compared by the fields this package models, not as documents.
* The list of server defaults accepted for a Deployment was calibrated on kind; another Kubernetes
  version that adds a defaulted member shows up as drift (visible in `Status.Drift`) rather than being
  accepted silently.
* Per-tenant locking is in process; run one reconciler per installation (or accept that two instances
  converge by reading before every write).
