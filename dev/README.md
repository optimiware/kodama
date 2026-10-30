# Foundation development environment

This is the T-001 scaffold (a Go process, real dependency fixtures, and named acceptance checks) with the T-091 stack split: `DEV_STACK=services` builds the dependencies without a cluster. The task is merged; Docker/Linux CI qualification is pending its first run.

## Prerequisites

- Go **1.27.2 or later**, GNU Make **3.81 or later**, and Git. The development tooling is Go; no Bun, OpenForge checkout or sibling directory is needed.
- macOS ARM64: a running Podman machine. The initial acceptance environment uses Podman **6.1.4**, an AppleHV VM with 8 CPUs / 8 GiB, and its existing rootful connection.
- Linux AMD64: Podman locally by default; explicitly select `DEV_PROVIDER=docker` for Docker/CI. Local Podman results do not qualify Docker.
- Compose: `podman compose` (Podman's compose provider, such as podman-compose 1.6) locally, or the Docker Compose v2 plugin (`docker compose`) in CI. Compose runs the dependency containers; kind provides the cluster whose Pods reach them.
- Internet access for pinned tools, Go modules, images, and the Git HTTP-backend package. No LocalStack account/token or external OIDC account is required by these fixtures.

`make tools` installs kind and kubectl into this worktree's `dev/.tools/`, verifying the checked-in SHA-256 checksums. It does not install a container engine or start/change a Podman VM. Select an existing Podman connection before setup if necessary.

Dependency images are pinned to multi-architecture index digests in [compose.yaml](compose.yaml); the Git fixture base is pinned in the [Containerfile](images/Containerfile) and the kind node image in [versions.lock.json](versions.lock.json), together with Go, kind, kubectl, and the Git HTTP-backend package. Go clients and actionlint are pinned through [go.mod](../go.mod) and [go.sum](../go.sum). The Git HTTP backend is installed from a versioned, checksum-verified APK without network access during image assembly. LocalStack 4.14.0 and oauthsonas 1.4.0 are development fixture pins, not production support declarations. The development tooling is Go and lives in `dev/cmd/` and `dev/internal/`; see the [verification record](review.md#dependency-refresh) for the one dependency held below its latest release.

## Stacks

`DEV_STACK` selects what `make dev-up` builds ([ADR 0009](../docs/adr/0009-environments.md)). It is read from the command line or environment (`make dev-up DEV_STACK=services`); the Makefile does not mention it.

| `DEV_STACK` | Builds | For |
| --- | --- | --- |
| `cluster` (default) | The `services` stack plus a scoped kind cluster, its kubeconfig and the server Deployment | Kubernetes as the subject: delivery apply, the Kubernetes runtime, the chart |
| `services` | PostgreSQL, NATS, LocalStack S3, Git, oauthsonas and the registry on one scoped Podman network. No kind, no kubectl, no kubeconfig | The `test` environment: M1 and the M2/M3 persistence checkpoints, with the Podman runtime |

An unknown or empty value is rejected before anything is created. A scope keeps the stack it was created with: `dev-up`, `dev-preflight` and `test-integration` refuse a different `DEV_STACK` for an existing scope, while `dev-down` and diagnostics follow the recorded stack. Select it once per scope; scopes created before the split are `cluster` scopes.

```sh
make dev-up DEV_STACK=services DEV_SCOPE=t026
make dev-preflight DEV_STACK=services DEV_SCOPE=t026
make dev-down DEV_STACK=services DEV_SCOPE=t026
```

`services` starts in seconds (about 25 s on a warm image cache, including building the Git/probe image) where `cluster` takes about 90 s on the same machine.

## Tooling

The environment is Go, not Python. `make dev-up`, `dev-down`, `dev-preflight`, `dev-diagnostics`, `dev-integration`, `dev-runtime` and `dev-smoke` run `go run ./dev/cmd/devctl`; `make fmt-check`, `docs-check`, `config-check`, `validate-tasks`, `check-task` and `check-go` run `go run ./dev/cmd/repoctl`. `devctl` uses the standard library only; `repoctl` additionally uses the `go.yaml.in/yaml/v3` module that the product already requires, to read task frontmatter strictly. Neither adds a dependency, and both follow `DEV_STACK`. `make test-podman` runs the real runtime contract against the engine without a scope; `make test-integration` runs the assembled product suites under `DEV_STACK=services`. `make check-devtools` formats, vets and tests the tooling itself.

### Endpoint files by stack

Two files describe each scope in `dev/.state/environments/<scope>/`:

| File | Names | `cluster` | `services` |
| --- | --- | --- | --- |
| `host-env.json` (private, 0600) | Host URLs for tests running on the host | PostgreSQL admin, NATS, S3, registry, secrets directory, `KODAMA_TEST_KUBECONFIG`, `KUBECTL` | The same URLs, no kubeconfig or kubectl; adds `KODAMA_TEST_PODMAN_NETWORK` and `KODAMA_TEST_SERVER_CALLBACK_HOST` |
| `endpoints.json` | Workload URLs for code running inside the stack | `*.kodama-deps.svc.cluster.local` Pod URLs including `KODAMA_SERVER_URL` | Container names on the scoped network (`<project>-postgres`, ...), `KODAMA_POSTGRES_URL` (without credentials), `KODAMA_PODMAN_NETWORK`, `KODAMA_SERVER_CALLBACK_HOST`; no `KODAMA_SERVER_URL`, because the server under test runs on the host |

Both record `DEV_STACK` in `host-env.json`. Nothing in `services` creates a kubeconfig, and neither file of `services` names `svc.cluster.local`.

To start a workload that reaches the services, give the Podman runtime `Network` = `KODAMA_TEST_PODMAN_NETWORK` ([`podman.Config.Network`](../internal/runtime/podman/podman.go)). The workload resolves services by the hostnames in `endpoints.json` and reaches the server callback the host listens on through `KODAMA_TEST_SERVER_CALLBACK_HOST` (`host.containers.internal` for Podman, `host.docker.internal` for Docker, which also needs `--add-host host.docker.internal:host-gateway`). Database credentials stay in the scope's `fixtures/` directory (`database-runtime-url` is the full workload URL).

`make dev-integration` (and `make dev-smoke` on a fresh scope) run the dependency probe: as a restricted Job in `cluster`, and in `services` as a labelled Podman container on the scoped network, with the same PostgreSQL, JetStream, S3, Git, OIDC and registry checks but no server or kubelet-pull checks (`KODAMA_PROBE_CHECKS`). `make test-integration` instead runs the assembled product suites against the services scope.

### Template databases

`test/support/postgres` provisions a database per test. `NewFromTemplate(t, digest, migrate)` migrates one template database per manifest digest (`ManifestDigest`) under a cluster-wide advisory lock and creates each test's database `FROM` that template; `DropTemplate` removes a template and its roles, and `dev-down` destroys them with the PostgreSQL volume.

Measured on one shared 8-core Xeon E5-1410 v2 (load average about 10, so absolute times are noisy), with `go test ./test/support/postgres -run '^$' -bench Provision -benchtime 60x -cpu 1`. Each cell is ms per provisioned database (create plus migrate, or create plus clone; teardown untimed) for the whole core manifest (`internal/app.Manifest()`) and for the three-migration base schema:

| PostgreSQL 17.6 | Core: per-test migration → clone | Base: per-test migration → clone |
| --- | --- | --- |
| Scope container (`services` stack, default settings) | 585 → 145 ms (4.0×) | 244 → 122 ms (2.0×) |
| Native binary, default (durable) settings | 622 → 199 ms (3.1×) | 186 → 102 ms (1.8×) |
| Native binary, `fsync=off synchronous_commit=off full_page_writes=off` | 609 → 86 ms (7.1×) | 120 → 46 ms (2.6×) |

The clone clears the three-times bar for the core manifest on the scope's PostgreSQL (4.0× here, 3.8× in an earlier run) and not for the base schema, so the clone is for suites that migrate large manifests and `New` stays right for a base-only schema.

The embedded comparison is a native binary, not a container with changed settings: a one-off script (removed after the measurement; see Git history) downloaded the pinned (SHA-256 checked) zonky.io `embedded-postgres-binaries` 17.6.0 archive that embedded-PostgreSQL libraries use, ran `initdb` into a temporary directory, started `postgres` on a free loopback port, ran the same benchmark against it, stopped it with `pg_ctl -m immediate` and deleted the directory. It added no Go dependency. Besides the rows above, each embedded instance costs 1.4–1.6 s of `initdb` and about 0.11 s to start, which the scope container already paid at `dev-up`. An embedded binary does not remove the cost the clone removes: per-test migration of the core manifest still takes about 610 ms natively, 3–4× the container's clone time. Clones on an embedded instance are fast only with durability off, which the scope's PostgreSQL does not need to match for tests that examine persistence. No embedded binary is adopted; adopting one needs a recorded decision.

### Podman runtime acceptance

`make dev-up DEV_STACK=services` is accepted against the real runtime adapter, separately from the dependency probe:

```sh
make dev-runtime DEV_SCOPE=t026 DEV_PROVIDER=podman DEV_STACK=services
```

`make dev-runtime` runs `dev/runtimeacceptance`, which loads `host-env.json` and `endpoints.json`, builds `internal/runtime/podman` with `Network` taken from `KODAMA_PODMAN_NETWORK`, and starts workloads through `execution.Runtime`: one runs the probe's PostgreSQL check using the workload URL in the endpoint file (`KODAMA_POSTGRES_URL` host, which the scope's database URLs also use), and one calls an HTTP listener on the host through `KODAMA_SERVER_CALLBACK_HOST`. Each test asserts the workload joined only the scoped network and that `Remove` leaves no container behind. The harness pulls the pinned scope image from the plain-HTTP scope registry once, because the runtime's own `--pull=missing` uses HTTPS. The tests skip without `KODAMA_DEV_SCOPE_DIR`.

## Quickstart

Run from the assigned task worktree:

```sh
make tools
make lint test build
make dev-up DEV_PROVIDER=podman DEV_SCOPE=t-001-local DEV_STACK=services
make test-podman
make test-integration
```

Docker remains selectable for the dependency stack (`DEV_PROVIDER=docker`), but the Podman runtime suites need a Podman engine. `DEV_SCOPE` accepts 2–31 lowercase letters, digits, and hyphens, beginning with a letter and ending with a letter or digit. Assign a different scope to each concurrent test session. The random owner suffix also separates identical scope names in different worktrees.

`make build` writes the host binary to `dev/.state/bin/kodama`. It supports:

```sh
dev/.state/bin/kodama --help
dev/.state/bin/kodama server --help
dev/.state/bin/kodama server --http-addr=127.0.0.1:8080
```

The in-cluster server runs one replica in kind. Its [OpenAPI probe contract](contracts/health.openapi.json) describes `/healthz` and `/readyz`; readiness currently reflects only process startup/draining. Use `make dev-integration` for the dependency probe and `make test-integration` for the assembled product suites.

## Kernel Git hosting

The kernel (`kodama server --assembly kernel`) hosts repositories itself: a tenant's repositories live on that tenant's own Git worker, and every clone and push enters through the server's authenticated `/git/` route (O-36, the [plan](../docs/implementation/internal-git-service.md)). How the workers are hosted follows `KODAMA_ENV`:

| Environment | Workers | Needs |
| --- | --- | --- |
| `development` | One in-process worker per tenant, started on that tenant's first Git request, with its data root at `$KODAMA_STATE_DIR/git/<tenantId>` (default `dev/.state/kernel`). The state directory is ignored by Git and is the only thing to delete to reset the repositories (`dev-down` does not touch it); set `KODAMA_STATE_DIR` to a temporary directory for a throwaway data root | The `git` binary and PostgreSQL. No Git credential, worker address, control token or signing key file: the signing keys and the control token are generated for the process |
| `test` | One `kodama git serve` container per tenant on the scoped Podman network, selected explicitly with `DEV_GIT=native` ([below](#test-environment-one-worker-container-per-tenant)). The external `git-http-backend` fixture remains the default, as the compatibility profile | A services scope with `DEV_GIT=native`, and a server the workers can reach |
| `production` | One Deployment and volume per tenant on Kubernetes, with `KODAMA_GIT_WORKER_URL`, `KODAMA_GIT_CONTROL_TOKEN` and the two signing key files. Not installable yet (T-156/T-157) | The in-process worker is refused |

`make test-m1` and the unit loop never start a container: the development hosting is exercised in process (`Test_unit_kernel_git_development_hosting`), and the bootstrap below is automated in memory, with the real Git client, by `Test_unit_kernel_git_bootstrap_roundtrip`.

### Bootstrap example: project, repository, seed, push, pipeline, run

This is the single-tenant developer flow under `KODAMA_ENV=development`. It needs PostgreSQL with two distinct roles, an owner (`kodama_migration`) and a non-owner runtime role (`kodama_runtime`); `make dev-up DEV_STACK=services` provides them (passwords in the scope's `fixtures/`, host port in `host-env.json`), or use any PostgreSQL you control. Use `jq` for the identifiers, or read them from the JSON. Values in angle brackets are placeholders.

```sh
make build
export KODAMA=dev/.state/bin/kodama
export KODAMA_ENV=development
export KODAMA_DATABASE_MIGRATION_URL='postgres://kodama_migration:<password>@127.0.0.1:<port>/<database>'
export KODAMA_DATABASE_URL='postgres://kodama_runtime:<password>@127.0.0.1:<port>/<database>'
export KODAMA_DEV_TOKEN=dev-token KODAMA_DEV_USER_ID=u_dev   # an explicit development credential
export KODAMA_EXECUTOR=process                               # run steps on this host; podman runs containers

$KODAMA migrate
$KODAMA seed --tenant acme --user-id u_dev --role admin --role engineer
$KODAMA server --assembly kernel --http-addr 127.0.0.1:8080 &
```

`admin` gives no push authority, so the seeded person is also an `engineer`: only an engineer may request a seed credential or push. The server renders clone URLs from `KODAMA_PUBLIC_URL` (default `http://127.0.0.1:8080`).

```sh
API=http://127.0.0.1:8080/api/v1
AUTH="Authorization: Bearer dev-token"

# 1. A project and 2. a repository in it. The repository is ready once the tenant's worker initialized it.
PROJECT=$(curl -fsS -X POST $API/tenants/acme/projects -H "$AUTH" -d '{"key":"demo","displayName":"Demo"}' | jq -r .id)
REPO=$(curl -fsS -X POST $API/tenants/acme/projects/demo/repositories -H "$AUTH" -d '{"name":"hello"}')
REPO_ID=$(echo "$REPO" | jq -r .id); echo "$REPO" | jq '{path, status, empty, cloneUrl}'

# 3. A seed credential: single use, creates the default branch of the empty repository.
SEED=$(curl -fsS -X POST $API/tenants/acme/git-credentials -H "$AUTH" \
  -d "{\"projectId\":\"$PROJECT\",\"repositoryId\":\"$REPO_ID\",\"seed\":true}" | jq -r .password)

# 4. A pipeline that starts on pushes to main, pushed with the seed credential.
mkdir hello && cd hello && git init -q -b main && mkdir .kodama
cat > .kodama/build.yaml <<'EOF'
name: build
triggers:
  git:
    branches: [main]
steps:
  - name: hello
    image: alpine:3.20
    commands: ["echo hello from the pushed commit"]
EOF
git add . && git commit -q -m "first commit"
git push "http://kodama:$SEED@127.0.0.1:8080/git/acme/demo/hello.git" main:refs/heads/main

# 5. The push is recorded, the pipeline is indexed from the pushed commit (a second or two) and its run is admitted.
sleep 3
curl -fsS $API/tenants/acme/projects/$PROJECT/pipelines -H "$AUTH" | jq '.items[] | {name, source, sourceCommit}'
curl -fsS $API/tenants/acme/projects/$PROJECT/runs -H "$AUTH" | jq '.items[] | {id, status}'
```

The run is pinned to the pushed commit. After the seed the default branch advances only by integration, or by an engineer's push where the repository opted in with `"policy":{"directPushToDefault":true}` at creation; request an ordinary credential with `"seed":false` (bound to this person's current role, at most an hour) for other pushes and clones. An ordinary credential cannot create the first branch, a second use of the seed credential is refused, and a refused push changes and records nothing. A real-PostgreSQL run of this example is the acceptance recorded on `main`; under `development` it needs no container.

### Test environment: one worker container per tenant

`DEV_GIT=native` is selected explicitly, once per scope, on the services stack. The scope then does not start the `git-http-backend` fixture, which is the compatibility profile (`DEV_GIT=fixture`) and the default for every scope. Compose profiles carry the choice: `git` is `git-fixture`, `git-worker` is `git-native`.

```sh
make dev-up DEV_STACK=services DEV_SCOPE=t170 DEV_GIT=native
# One container per tenant, each with its own data volume and its own Compose project.
# <tenant-id> is the tenant's identifier as the kernel knows it (lowercase, for example t_0f3a...).
make dev-git-worker DEV_SCOPE=t170 GIT_TENANT=<tenant-id> \
  [GIT_CONTROL_URL=http://host.containers.internal:8080]   # or: go run ./dev/cmd/devctl git-worker --scope t170 --tenant <tenant-id>
make dev-down DEV_STACK=services DEV_SCOPE=t170   # removes every worker, its volume and the scope
```

Starting a worker again renews its identity (valid for under an hour; the worker re-reads it at every call) and leaves a running container and its data alone. The worker holds no database or broker credential: only its volume, the forwarding **public** key, its tenant-bound identity token and the control token, from `git-workers/<tenant-id>/` in the scope directory. The scope's signing keys and control token (`git-forwarding-key.pem`, `git-identity-key.pem`, `git-control-token`) are for the server under test and never reach a worker. They live in `git-server/` in the scope directory (mode 0700, files 0600), not in `fixtures/`, which PostgreSQL, NATS and oauthsonas mount.

The server under test takes `KODAMA_GIT_FORWARDING_KEY_FILE`, `KODAMA_GIT_IDENTITY_KEY_FILE` and `KODAMA_GIT_CONTROL_TOKEN` from `host-env.json`, and its worker address from `endpoints.json`: `KODAMA_GIT_WORKER_URL`, the template `http://<project>-git-{tenantId}:8443` that the server resolves per tenant. Inside the scope network the worker speaks plain HTTP; forwarding assertions and identity tokens are real and tenant-bound, but TLS on the worker (`--tls-cert`, `--tls-key`) is not enabled by this scope. `devctl` records each worker in `git-workers.json` (container, workload URL, loopback host URL). A worker calls the control plane at `--control-url` (default: the provider's name for the host, port 8080), so the server must listen on a non-loopback address (`--http-addr 0.0.0.0:8080`); Docker on Linux also needs `host.docker.internal` mapped to the host.

Two limits, stated plainly. The `{tenantId}` template resolves container names on the scope network, so it serves a server that runs there (for example the product image started on `KODAMA_TEST_PODMAN_NETWORK`); a server on the host cannot resolve those names, and the loopback ports in `git-workers.json` cannot be written as one template, so a host-run server uses the `development` in-process workers. And the dependency probe's HTTPS Git check belongs to the fixture, so a native scope runs the probe without `https_git`. Real Podman and Docker acceptance of this profile, like the other container suites, runs on `main`; `make check-devtools` covers it against a fake engine.

## Environment layout

Each scope owns (`cluster`; `services` has the same dependency containers and Podman network instead of the cluster and namespaces):

- A kind cluster named `kodama-<scope>-<owner-prefix>` and a private kubeconfig. The same name is the Compose project.
- Six Compose containers named `<cluster>-<service>` (registry, PostgreSQL, NATS, LocalStack, Git, oauthsonas), carrying the ownership label on kind's network. Registry, PostgreSQL, NATS and S3 publish dynamically allocated loopback ports for host tests.
- `kodama-deps` and `kodama-test` namespaces inside the cluster; `kodama-deps` holds the server Deployment and one selector-less Service plus EndpointSlice per Compose container.
- A database named `kodama_<scope_with_underscores>`, migration/runtime identities, and independent credentials.
- An S3 bucket `kodama-<scope>` and `<scope>/` object prefix. Each probe adds a unique suffix.
- Per-probe JetStream streams/subjects and Git `smoke/<nonce>` branches.
- A development CA, Git TLS certificate, tokens, and a 32-byte development master-key fixture.

The generated kubeconfig, credentials, endpoint inventory, image build context, and ownership journal live in `dev/.state/environments/<scope>/`. The scope directory is mode 0700; its `fixtures/` subdirectory (mode 0755, files 0644) is mounted read-only into containers that run as other UIDs, so treat the fixtures as development-only. Keep the journal until teardown finishes: it binds cleanup to the exact kind node IDs, and Compose containers are selected by the random ownership label. Commands serialize on a per-worktree scope lock.

### Dependencies

| Service | Configuration and behavior |
| --- | --- |
| PostgreSQL 17.11 | Separate `kodama_migration` and non-owner, non-superuser, non-BYPASSRLS `kodama_runtime` roles; product tables/RLS belong to T-002 |
| NATS 2.15.1 | Token authentication and file-backed JetStream; product outbox/inbox belongs to T-005 |
| LocalStack 4.14.0 | S3 only, SigV4 client requests, path-style URLs; no Docker socket mount |
| HTTPS Git | Real `git http-backend`, private bare repository, generated CA, independent read-only/write tokens |
| oauthsonas 1.4.0 | One container, generated configuration, public `kodama-cli` client and `engineer` persona |
| Registry 3.1.2 | Scoped development HTTP registry; node-specific containerd mirror mapping and Pod-reachable Service/EndpointSlice |

The containers start with `restart: "no"` and are not health-gated by Compose; the probe retries each check for up to two minutes while dependencies start, and asserts specific failures so retries cannot mask defects.

PostgreSQL, JetStream, and Git use Compose named volumes. Their data survives container recreation (`make dev-up` again) within the same scope. LocalStack data and oauthsonas protocol state are disposable; restarting oauthsonas invalidates its signing key and tokens. `dev-down` destroys the containers, volumes, and cluster (`services`: the scoped network `<project>-net`, created with the ownership label and removed only when it still carries it) of the selected scope. Recreating a scope is a fresh environment, not a restore operation.

### Network paths

`endpoints.json` contains the canonical **Pod** URLs. These use `*.kodama-deps.svc.cluster.local`; they are not host URLs. Each dependency Service has no selector; its EndpointSlice points at the container's address on kind's network, so Pods reach Compose containers through ordinary cluster Services. Server callbacks use its ClusterIP Service. Git validates its service hostname against the generated CA. The OIDC issuer is exactly `http://oauthsonas.kodama-deps.svc.cluster.local:8181`; tests execute the authorization flow within the cluster and never substitute `localhost` for that issuer.

`host-env.json` is private (0600), with host URLs, PostgreSQL admin credentials, secrets-directory, kubeconfig and kubectl paths for Go integration. `make dev-preflight DEV_SCOPE=<scope>` checks only what the scope's stack provides, without provisioning or printing credentials: endpoint reachability and the cluster readiness in `cluster`, endpoint reachability and the owned Podman network in `services` (no kubectl, no kubeconfig). OpenForge integration tasks load this file after preflight; units need none of it. The authoritative scope/provider rules are in [local testing](../docs/implementation/development-and-testing.md#first-usable-environment). Standalone environment maintenance remains usable while the queue is halted.

The registry has three addresses:

1. Host push: `localhost:<allocated-port>/<scope>/scaffold:dev`.
2. Pod push: `http://registry.kodama-deps.svc.cluster.local:5000`.
3. Kubelet image reference: `localhost:<allocated-port>/<scope>/<repository>@sha256:...`, resolved by the node's containerd mirror to the registry container on the kind network.

For inspecting process health from the host, use the private kubeconfig:

```sh
kubectl --kubeconfig dev/.state/environments/t-001-local/kubeconfig \
  -n kodama-deps port-forward --address 127.0.0.1 service/kodama 8080:8080
```

This scaffold has no host-browser OIDC login command. T-017 must configure a consistent browser/server issuer when implementing that workflow. The fixture's callback registration is `http://127.0.0.1:18999/callback`; the probe inspects the redirect and exchanges the authorization code without following it.

## Acceptance

### dev_smoke

Use a fresh scope:

```sh
make dev-smoke DEV_PROVIDER=podman DEV_SCOPE=t-001-acceptance DEV_STACK=cluster
```

This builds the host and Linux binaries, creates the cluster, starts the Compose dependencies, deploys the server, and runs a restricted probe Job with one primary container, `backoffLimit: 0`, a deadline, and no service-account token. It checks:

- Server health/readiness responses.
- Real SQL insert/read, a nonprivileged runtime role distinct from the migration role, and denial of table creation by the runtime role.
- JetStream persisted publish, durable consume, and acknowledgement.
- LocalStack S3 upload/download with SHA-256 comparison and object cleanup.
- HTTPS/token Git push/clone including a 2 MiB (chunked) push, HTTP 403 for a read-only push, and an unauthenticated rejection for an invalid token.
- oauthsonas discovery, S256 authorization-code exchange, JWKS verification of JWT access-token issuer/audience/expiry/subject, ID-token verification for the client audience with the request nonce, ID-token rejection for the API audience, code replay rejection (HTTP 400), and wrong-verifier rejection (HTTP 400).
- Host image push, Pod-side blob/manifest upload (single-platform or index), and a second Job pulling the Pod-published image by digest through kubelet; the server Deployment itself is pulled the same way.

These checks support PX-04 and AC-12; they do not implement factory attempt dispatch or product API routes. Probe helpers and dependency clients live under `dev/` and are not linked into the server.

### dev_teardown

```sh
make dev-teardown-check DEV_PROVIDER=podman DEV_SCOPE=t-001-acceptance DEV_STACK=cluster
```

In the `services` stack the sentinels are an independent Podman network and container and the guard is the network ownership label; the checks are otherwise the same. The test creates an independent sentinel cluster, verifies a mismatched node ID is rejected, removes one owned container to simulate partial cleanup, tears down the selected scope twice, and proves the sentinel cluster/ConfigMap and the user's kubeconfig remain unchanged. The harness then removes its sentinel cluster.

For ordinary cleanup:

```sh
make dev-down DEV_PROVIDER=podman DEV_SCOPE=t-001-local
```

`dev-smoke` keeps its environment for inspection or teardown acceptance. Setup failures also retain the journal. `dev-up` can resume after a recoverable failure; `dev-down` handles recorded partial resources. A missing runtime, a failed inventory command, or changed resource identity stops cleanup rather than guessing ownership. Scope teardown preserves shared runtime networks, cached images/tools, and reports.

## Diagnostics and review

```sh
make dev-diagnostics DEV_PROVIDER=podman DEV_SCOPE=t-001-local
```

Reports under `dev/.state/reports/<scope>/` include Pod summaries, events, Git/OIDC/NATS/LocalStack container logs, server Pod logs, and probe logs. The collector excludes Secret objects, local credential files, and PostgreSQL startup SQL logs. CI uploads only this report directory and always attempts scoped cleanup.

The [CI workflow](../.github/workflows/ci.yml) runs formatting, vet, unit/race tests, module verification and the in-memory compositions on every change. Two manually dispatched jobs qualify containers: Docker runs the cluster stack probe and teardown (Docker is what the runner provides), and Podman runs the runtime contract plus the services-stack probe. Neither is a production qualification. The assembled product suites stay `make test-integration` on a prepared scope. The [configuration contract](configuration.md) and [review handoff](review.md) record the interface and verification boundary. Keep the branch/worktree pending review; task status changes only after an authorized merge.
