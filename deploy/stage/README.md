# Stage & cloud deployment (`deploy/stage/`)

OpenTofu roots for **AWS**, **GCP** and **Scaleway (`scw`)**, one shared Helm
stack (`deploy/stage/shared/stack`), and the kernel qualification scripts
(`deploy/stage/scripts/`). The executable chart is
[`deploy/charts/kodama-kernel`](../charts/kodama-kernel/README.md); the legacy
`kodama` chart is not used by this stage profile.

A stage cluster is **disposable and named**. Any number can run side by side
(`--name blue`, `--name green`), each with its own installation id, state,
kubeconfig and qualification sessions. The first live Scaleway run for the
kernel profile has not yet been recorded.

## What the kernel stage installs

One `kodama server --assembly kernel` instance over PostgreSQL, NATS, Kubernetes
Jobs and the native Git service. Workers execute as restricted Jobs in each
tenant namespace with one ReadWriteOnce workspace claim per run. The stage
profile embeds one in-process Git worker per tenant under the server's retained
data volume (`git.hosting=embedded`, `git.embeddedAck=stage-only`). That is a
test-stage deviation, not a supported production identity or isolation claim.

The shared stack also installs NATS JetStream, ingress-nginx, cert-manager, the
OIDC persona fixture and the optional Grafana/Loki/Prometheus operator view. It
does not install Gitea: repositories live in Kodama.

## Deployment runbook

### Prerequisites

| Need | For |
| --- | --- |
| `tofu` (OpenTofu >= 1.8; developed with 1.13), `helm`, `kubectl`, `python3`, `curl`, `go` | every deployment and qualification |
| `scw` CLI, authenticated for the target project | creating the state bucket and inspecting Kubernetes resources |
| `SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, `SCW_DEFAULT_PROJECT_ID` | the Scaleway provider, state backend and cluster resources |
| `KODAMA_IMAGES_FILE` (JSON with `kodama` repository/tag/digest) | the release; placeholder digests are refused |
| `KODAMA_ACME_EMAIL` | Let's Encrypt account contact |
| `KODAMA_STAGE_STORAGE_CLASS` | storage class for the server volume and run workspaces (default `scw-bssd` on Scaleway) |

Keep credentials, kubeconfig and tfvars outside the checkout. Check the scripts
without cloud access first:

```bash
deploy/stage/scripts/test_scripts.sh
deploy/stage/scripts/deploy.sh --cloud scw --validate-only
```

### 1. Create the state bucket once

```bash
export SCW_ACCESS_KEY=... SCW_SECRET_KEY=... SCW_DEFAULT_PROJECT_ID=...
eval "$(deploy/stage/scripts/state-bucket.sh)"
echo "$KODAMA_TFSTATE_BUCKET"
```

The bucket is private and versioned and is never managed by OpenTofu. Put
`KODAMA_TFSTATE_BUCKET` in the operator shell profile.

### 2. Configure a cluster

```bash
export KODAMA_STAGE_CLUSTER=blue
mkdir -p ~/kodama-stage/state/clusters/blue
cp deploy/stage/scw/terraform.tfvars.example ~/kodama-stage/state/clusters/blue/scw.tfvars
```

Set `project_id`, `operator_user_ids`, and `ingress_host`. A real DNS name that
resolves to the ingress load balancer avoids the nip.io rate limits and a second
deploy pass. The installation id is always `kodama-<name>`.

### Build once from committed source

The chart pins the release image by digest. Build from the committed candidate
and publish the same multi-arch image the workers will pull:

```sh
test -z "$(git status --porcelain)"
candidate="$(git rev-parse HEAD)"
podman build --platform linux/amd64,linux/arm64 -f Containerfile \
  --build-arg "REVISION=$candidate" \
  --label "org.opencontainers.image.revision=$candidate" \
  -t "$KODAMA_STAGE_IMAGE_TAG" .
podman push "$KODAMA_STAGE_IMAGE_TAG"
```

Record the registry manifest digest in `KODAMA_IMAGES_FILE` (tags alone are
rejected). The stage's checkout Job uses the same release image; no separate
workload image is needed for the kernel suite.

### 4. Deploy

```bash
export KODAMA_IMAGES_FILE=~/kodama-stage/images.json
export KODAMA_ACME_EMAIL=ops@example.com
deploy/stage/scripts/deploy.sh --cloud scw --name blue
```

The script initializes the cluster's state, applies the cloud root, writes the
cluster kubeconfig, renders and lints the kernel values, then applies the shared
stack. The stack creates the database roles, the runtime/migration/KMS Secrets,
the Git signing keys and the kernel release before the qualification session is
prepared. Add `--contract-out` and `--values-out` to keep inputs for the gate.

### 5. Check the result

```bash
export KUBECONFIG=~/kodama-stage/state/clusters/blue/kubeconfig
kubectl get nodes
kubectl get pods --all-namespaces
deploy/stage/scripts/cluster.sh list
```

Wait for the kernel server's `/readyz` and the public certificate. The first
boot requires a tenant seed before the API has a usable tenant:

```sh
kubectl -n kodama-system get secret kodama-kernel-migration
# run `kodama seed` in a short-lived Pod using that Secret, as qualify.sh's
# fixture does for qualification sessions
```

### 6. Prepare a fresh qualification session

Use the original base stage contract, not another session's derived contract:

```bash
deploy/stage/scripts/qualify.sh prepare --name blue --session kb \
  --contract ~/kodama-stage/state/clusters/blue/stage_contract.json
```

The session creates a database on the stage instance, its own control-plane and
tenant namespaces, NATS server, Git keys, KMS credential and database relays.
The tests install, upgrade and uninstall the kernel release themselves.

Run the readiness gate:

```bash
export KODAMA_QUAL_SESSION=kb KODAMA_STAGE_CLUSTER=blue
export KODAMA_STAGE_CONTRACT=~/kodama-stage/state/clusters/blue/qualification/kb/contract.json
export KODAMA_STAGE_VALUES=~/kodama-stage/state/clusters/blue/qualification/kb/values.yaml
deploy/stage/scripts/gate.sh --gate readiness --cloud scw \
  --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES" --preflight
KODAMA_GO_TEST_TIMEOUT=60m deploy/stage/scripts/gate.sh --gate readiness --cloud scw \
  --contract "$KODAMA_STAGE_CONTRACT" --values "$KODAMA_STAGE_VALUES"
```

The suite is `test/e2e/kube-kernel` and requires five named checks:
`kernel_clean_install`, `kernel_pipeline`, `kernel_git`,
`kernel_secrets_kms`, `kernel_cancel_restart`. It covers migration/readiness,
database outage recovery, shared workspace and retained logs, tenant isolation,
native Git seed push and exact-commit trigger, run-scoped Git publication,
KMS-backed secrets across a restart, cancellation and controller adoption.

### 7. Teardown

```bash
deploy/stage/scripts/cluster.sh destroy --cloud scw --name blue
```

This destroys the cluster inventory and leaves earlier state versions in the
bucket. It refuses a production-tier cluster. Destroy a qualification session
first if you want its database removed separately:

```bash
deploy/stage/scripts/qualify.sh destroy --name blue --session kb \
  --contract ~/kodama-stage/state/clusters/blue/stage_contract.json
```

## Layout

```text
deploy/stage/
  aws/ gcp/ scw/          # Dedicated Kubernetes, PostgreSQL, object storage, KMS
  shared/stack/           # Helm releases, Secrets, OIDC fixture and observability
    values/kodama.yaml.tftpl
  qualification/          # Isolated database, namespaces, NATS, relays and kubeconfig
  scripts/
    lib.sh                # Contract validator, strict values renderer, result verifier
    deploy.sh             # cloud apply -> kubeconfig -> chart lint -> shared stack
    qualify.sh            # prepare/destroy/show one qualification session
    gate.sh               # readiness and evidence runner
    checks/readiness.sh   # kernel acceptance names
    test_scripts.sh       # offline OpenTofu/Helm/ShellCheck/gate checks
```

## Named clusters and state

`--name <cluster>` (or `KODAMA_STAGE_CLUSTER`) selects the cluster in
`deploy.sh`, `qualify.sh`, `gate.sh` and `cluster.sh`. The installation id is
always `kodama-<name>`; cluster names are 1-16 lowercase letters, digits and
`-`, starting with a letter.

| Item | Location |
| --- | --- |
| Variables | `<state dir>/clusters/<name>/<cloud>.tfvars` |
| Operator kubeconfig | `<state dir>/clusters/<name>/kubeconfig` |
| State objects | `scw/<name>/infra.tfstate`, `<cloud>/<name>/stack.tfstate`, `scw/<name>/qualification/<session>.tfstate` |
| Qualification files | `<state dir>/clusters/<name>/qualification/<session>/` |
| OpenTofu data directories | `<state dir>/clusters/<name>/tfdata/<root>` |

Every cluster and session initializes its own `TF_DATA_DIR`, so several named
clusters can be deployed from one checkout at the same time. The shared stack is
cloud-neutral and always keeps its state in the bucket. The `aws` and `gcp`
infrastructure roots keep a local `<cloud>.tfstate`, which is not backed up.

## Credentials and state

Secrets and database credentials are generated by the shared stack and pushed
before any chart installs. Operator files stay outside the checkout, under
`$STAGE_STATE_DIR` (default `~/kodama-stage/state`), mode 0700/0600. OpenTofu
state lives in the versioned bucket. Both hold database, API-key and signing
material.

The qualification session inherits the stage PostgreSQL instance, KMS keys and
OIDC issuer, but has its own database, namespaces, NATS server, Git signing
keys, Secret values and ingress host. `kq-pgrelay` is the server's faultable
database path; `kq-pgprobe` is the test's inspection path.

## Recorded deviations

- **NATS:** plaintext `nats://` with token authentication on the cluster network
  (T-106 design work remains open). `KODAMA_ENV=production` still refuses all
  development and test adapters.
- **OIDC:** the `oauthsonas` persona fixture is a development/test issuer. It is
  gated by an access code and an operator CIDR allowlist; it is not an identity
  system. Hosted authority is T-061.
- **Git hosting:** the in-process per-tenant worker on one server volume is a
  stage-only profile. A supported installation needs per-tenant workers and
  separate volumes.
- **Backups:** Scaleway RDB provides scheduled backups rather than continuous
  WAL PITR; recovery and production-release gates remain deferred.

## Offline checks

```sh
deploy/stage/scripts/test_scripts.sh
helm lint --strict deploy/charts/kodama-kernel
go test ./deploy/charts/kodama-kernel/ ./test/support/kernelstage/
go test -race -count=1 ./test/support/stage/...
make lint test docs-check validate-tasks
```

The full suite needs a prepared session and a real cluster; a missing session or
namespace is an infrastructure pause, never a pass.
