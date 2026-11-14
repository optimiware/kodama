# Podman quickstart: the single-host `test` environment

This runbook installs KODAMA.md's `test` environment on one machine with
rootless Podman: PostgreSQL, NATS JetStream, OAuthSonas, `kodama server
--profile test` and `kodama git serve`. It is the functional release on a
single host and the base for the release smoke test.

It is **not a production installation**: one host, trusted local operators, a
local keyfile and the OAuthSonas development identity provider. The complete
list of limitations is at the end of this page; read it before relying on the
installation.

- Bundle: [`deploy/podman/`](../../deploy/podman/)
- Product behavior and environments: [KODAMA.md](../../KODAMA.md)
- Release smoke: [release-smoke.md](release-smoke.md)

## Prerequisites

- A Linux host with rootless Podman 5 or newer and the user API socket active:
  `systemctl --user enable --now podman.socket` and `podman info` succeeds
  (`systemctl --user restart podman.socket` if the socket file is missing).
  The control plane, the Git companion and every workload share this one
  image store. The runtime does not pull workload images.
- `bash`, `openssl`, `curl`, `jq`, `git` 2.44 or newer (required by
  `git serve`) and `socat` (the host-side TLS front ends in front of OAuthSonas
  and of the loopback Git service).
- Roughly 2 GB of free memory and 4 GB of disk for the images, PostgreSQL,
  the NATS store and workspaces.
- Free local ports; the defaults are 18443 (Kodama HTTPS), 18444 (OAuthSonas
  HTTPS), 18081 (`git serve`, loopback only), 18445 (Git HTTPS front end),
  15432 (PostgreSQL), 14222 (NATS), 18222 (NATS monitoring) and 18181
  (OAuthSonas upstream). Every port must be distinct; the
  install refuses a collision and a port that is already taken when a
  container or TLS front end starts.
- Rootless Podman with pasta maps `host.containers.internal` to the host's
  default-route address. Workloads use that name to reach the server and the
  Git HTTPS front end, so both listen on a host interface and their
  certificates cover `localhost`, `host.containers.internal` and the host's
  default-route address.

The install pre-pulls these images by **tag and manifest digest** and refuses to continue if a pulled digest differs from the pin. An environment override of any of them must also be `tag@sha256:<64 hex>`; a bare tag is refused:

| Component | Default image |
|---|---|
| PostgreSQL | `docker.io/library/postgres:18-alpine@sha256:300ebd02c6d4de9ee27ad7f6855d5d21b4d3a4a5117767c1197d284c635d9736` |
| NATS JetStream | `docker.io/library/nats:2.15-alpine@sha256:89c5c61b5969261e05106a98e465ccb85c7f4bf4e94c1e70786c189759d79e7e` |
| OAuthSonas | `ghcr.io/optimiware/oauthsonas:1.5.0@sha256:39f371048640a95aef951d4ffbc5efd58168f58910e9bb4b972694fa7e82d178` |
| Workload fixture (tools) | `docker.io/library/buildpack-deps:trixie-scm@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c` |
| External harness | `docker.io/library/alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b` |

## Configuration

Every name, port and directory is configurable from the environment. The safe
defaults are in [`deploy/podman/lib.sh`](../../deploy/podman/lib.sh).

| Setting | Default | Meaning |
|---|---|---|
| `KODAMA_LOCAL_PREFIX` | `kodama-local` | Container name and label prefix; `down` removes exactly this set. |
| `KODAMA_LOCAL_STATE_DIR` | `~/.local/state/kodama/podman/<prefix>` | Private state: CA, certificates, keyfile, database and broker data, secrets, logs. The default includes the prefix, so two installations never share one directory. |
| `KODAMA_LOCAL_HTTPS_PORT` | `18443` | Kodama HTTPS port. |
| `KODAMA_LOCAL_OIDC_PORT` | `18444` | OAuthSonas HTTPS port (TLS terminator). |
| `KODAMA_LOCAL_GIT_PORT` | `18081` | `git serve` smart-HTTP port (loopback only). |
| `KODAMA_LOCAL_GIT_HTTPS_PORT` | `18445` | Bundle-owned Git HTTPS front end on the host interface. |
| `KODAMA_LOCAL_POSTGRES_PORT` | `15432` | PostgreSQL on loopback. |
| `KODAMA_LOCAL_NATS_PORT` | `14222` | NATS client on loopback (no authentication). |
| `KODAMA_LOCAL_NATS_MONITOR_PORT` | `18222` | NATS monitoring and health port on loopback. |
| `KODAMA_LOCAL_OAUTH_UPSTREAM_PORT` | `18181` | OAuthSonas plain-HTTP upstream behind the loopback TLS terminator. |
| `KODAMA_LOCAL_TENANT` | `tenant-local` | Tenant id created by bootstrap. |
| `KODAMA_LOCAL_PROJECT` | the tenant | Project id. OAuthSonas carries one string claim, so humans are project-scoped; the default project is named like the tenant for convenience. |
| `KODAMA_LOCAL_WORKLOAD_IMAGE` | `docker.io/library/buildpack-deps:trixie-scm@sha256:712f969c…` | Digest-pinned tools image pre-pulled for pipelines. |
| `KODAMA_LOCAL_EXTERNAL_HARNESS_IMAGE` | `docker.io/library/alpine:3.24@sha256:0f49f331…` | Digest-pinned small external-harness image pre-pulled for pipelines. |
| `KODAMA_LOCAL_POSTGRES_IMAGE`, `KODAMA_LOCAL_NATS_IMAGE`, `KODAMA_LOCAL_OAUTHSONAS_IMAGE` | see above | Digest-pinned images; an override must keep the digest. |
| `KODAMA_LOCAL_GIT_RESOURCES` | `admin=git-admin` | Comma-separated `repository=resource-id` mappings passed to `git serve --resource`. |
| `KODAMA_LOCAL_KODAMA_IMAGE` | empty (build) | Use an existing image instead of building this checkout. |
| `KODAMA_LOCAL_PODMAN_SOCKET` | `$XDG_RUNTIME_DIR/podman/podman.sock` | Podman API socket the server uses. |

Choose a different `KODAMA_LOCAL_PREFIX` and port set for a second
installation on the same host. The default state directory already contains
the prefix; a custom `KODAMA_LOCAL_STATE_DIR` must be absolute and must not be
`/`, your home directory or its parent. The install refuses a non-empty
directory whose marker names another prefix, and `down.sh --purge` refuses to
touch such a directory. Do not point two installations at one state directory.

## Install

From the repository root:

```sh
./deploy/podman/install.sh
```

To install the released image instead of building the checkout, pull it and
name it. Keep the variable set on every later run of this installation:

```sh
podman pull ghcr.io/optimiware/kodama/kodama:0.1.0
export KODAMA_LOCAL_KODAMA_IMAGE=ghcr.io/optimiware/kodama/kodama:0.1.0
./deploy/podman/install.sh
```

The install is re-runnable. A re-run reuses every container whose configuration
did not change, keeps a still-valid service credential and only rotates an
expired one. It performs, in order:

1. Validates the state directory and its marker (refusing a foreign or
   degenerate directory), then creates or reuses the private CA, the Kodama
   server certificate (SANs: `localhost`, `host.containers.internal`,
   `127.0.0.1` and the host's default-route address), the OAuthSonas
   certificate and the Git HTTPS certificate with the same host SANs.
2. Generates the PostgreSQL superuser password, the non-superuser
   `kodama_admin` password, the four role passwords and the OAuthSonas picker
   access code into 0600 files under `$STATE/secrets/`. They reach containers
   only through `--env-file`; they are never arguments and never printed.
3. Pre-pulls both digest-pinned workload images above and the infrastructure
   images, and builds the Kodama image from this checkout with
   `deploy/images/kodama/Containerfile` (which installs Alpine's `git-daemon`
   package for the `git-http-backend` program `git serve` uses), or uses
   `KODAMA_LOCAL_KODAMA_IMAGE`. A prebuilt image must carry its
   `org.opencontainers.image.revision` label and `git-http-backend`; the
   install refuses one without the backend.
4. Starts or reuses PostgreSQL, creates the `kodama_admin` login role, starts
   JetStream NATS and OAuthSonas, and starts or reuses both host-side `socat`
   TLS front ends: OAuthSonas on loopback and the Git HTTPS edge on the host
   interface.
5. Runs `kodama migrate` with `KODAMA_ADMIN_DATABASE_URL` and the four role
   URLs. Migration creates the database and the four runtime roles.
6. Obtains an OAuthSonas access token with `scripts/oauthsonas-token`, runs
   `kodama bootstrap-admin --profile test --init-keyfile` on the first run and
   creates the tenant plus its first administrator. The keyfile is 32 random
   bytes in a 0600 file and is never regenerated.
7. Starts or reuses `kodama server --profile test` over HTTPS and waits for
   `/readyz` to answer 200.
8. Creates or reuses the project, explicitly grants the first administrator the
   engineer, reviewer and operator project roles, and grants the `engineer`,
   `reviewer` and `operator` personas their own single project memberships
   through `membership.grant` (verified with `membership.list`). The persona
   grant runs when `personas.done` is absent or its fingerprint (issuer, project
   and the three persona principals) changed, so an upgrade re-grants the
   memberships in place. A re-run whose marker still matches does not repair a
   membership an operator revoked by hand.
9. Creates the `git` and `engineer` service accounts, keeps or rotates their
   credentials (0600 files, 8 h lifetime), registers the `git-admin` resource
   binding with the Git HTTPS host and workload origins, provisions the tenant
   admin repository with `kodama git provision-tenant`, starts or reuses
   `kodama git serve` on loopback, and starts the Git HTTPS front end.
10. Writes `run/installation.json`: the private (0600) schema-version-1
    installation descriptor that names the installation identity, observed
    revision and server image digest, API/Git/OAuthSonas endpoints, the four
    persona principal ids and the digest-pinned workload images. It is
    rewritten atomically on every successful install or re-run.

The command prints only step names and endpoints. On success:

```text
kodama-local: installed and ready
kodama-local:   Kodama:    https://localhost:18443 (CA /home/you/.local/state/kodama/podman/kodama-local/tls/ca.crt)
kodama-local:   git serve: http://127.0.0.1:18081 (loopback only)
kodama-local:   git https: https://localhost:18445 (workloads: https://host.containers.internal:18445)
kodama-local:   installation: /home/you/.local/state/kodama/podman/kodama-local/run/installation.json
```

Useful generated files (all under the state directory):

| Path | Contents |
|---|---|
| `tls/ca.crt` | The installation CA. Trust it with `curl --cacert` and `SSL_CERT_FILE`. |
| `run/installation.json` | The 0600 installation descriptor. `status.sh` validates it; the common smoke runner reads it. It contains paths and public values, never a token, CA private key, database URL or socket path. |
| `run/socat.pid`, `run/git-https.pid` | The two owned TLS front-end processes (`run/*.conf` holds their exact configuration). |
| `secrets/operator.env` | Non-secret operator settings (`KODAMA_PUBLIC_URL`, `KODAMA_GIT_URL`, `KODAMA_CA_FILE`, token paths). Safe to source. |
| `secrets/oauthsonas.env` | `OAUTHSONAS_URL`, `OAUTHSONAS_CA` and `OAUTHSONAS_ACCESS_CODE_FILE` for `scripts/oauthsonas-token`. No secret is in the file. |
| `secrets/admin-token`, `secrets/engineer-token` | 0600 access/service tokens (the engineer token is a service credential with an 8 h lifetime). |
| `git-credential/token` | 0600 Git service credential; mounted read-only into `git serve`. |
| `secrets/keys/kek-v1.key` | The installation keyfile. Back it up with the database; never commit or copy it into an image. |
| `service-versions/` | The last committed service version per service, used to rotate credentials without guessing. |
| `data/`, `git/` | Runtime material and Git repositories. PostgreSQL and JetStream data live in the `kodama-local-pgdata` and `kodama-local-natsdata` Podman volumes, not in the state directory. |

The server reads the Podman API socket and the state directory at their
host-absolute paths. A containerized server must mount them at the same paths;
this bundle does.

## Status

```sh
./deploy/podman/status.sh
```

It prints the endpoints, the containers carrying `kodama.local.prefix=<prefix>`,
both TLS front ends, the installation descriptor path and its validation,
`/readyz`, and both Git listeners. It exits 0 only when the descriptor belongs
to this exact installation and is mode 0600, readiness is 200, both forwarders
and all owned containers run, and both Git listeners answer. Logs:

```sh
podman logs kodama-local-server
podman logs kodama-local-git
podman logs kodama-local-postgres
```

## Sign in with OAuthSonas

OAuthSonas is a development identity provider with a persona picker. Signing
in does not use a browser; `scripts/oauthsonas-token` performs the
authorization-code + PKCE exchange and prints one access token.

```sh
export STATE="${KODAMA_LOCAL_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/kodama/podman/${KODAMA_LOCAL_PREFIX:-kodama-local}}"
export TENANT="${KODAMA_LOCAL_TENANT:-tenant-local}"
export PROJECT="${KODAMA_LOCAL_PROJECT:-$TENANT}"
export KODAMA_PUBLIC_URL="https://localhost:${KODAMA_LOCAL_HTTPS_PORT:-18443}"
export GIT="https://localhost:${KODAMA_LOCAL_GIT_HTTPS_PORT:-18445}"
export GIT_WORKLOAD="https://host.containers.internal:${KODAMA_LOCAL_GIT_HTTPS_PORT:-18445}"
export WORK="$HOME/kodama-quickstart"   # your own files; $STATE belongs to the installation
mkdir -p "$WORK"
set -a
. "$STATE/secrets/oauthsonas.env"
set +a
scripts/oauthsonas-token admin > "$STATE/secrets/admin-token"
chmod 600 "$STATE/secrets/admin-token"
```

The personas defined by the install are `admin`, `engineer`, `reviewer` and
`operator` (all in the installation tenant). `admin` is a tenant-wide
administrator and, because this is a trusted single-operator installation, the
install explicitly granted it the engineer, reviewer and operator roles as
well. The other three personas get exactly one project membership each:
`engineer` has `engineer`, `reviewer` has `reviewer` and `operator` has
`operator`. A role claim alone grants no Kodama authority; these memberships are
written through the public `membership.grant` operation (a first grant omits the
expected version, an existing one uses the version observed by
`membership.list`, and the committed set is verified with `membership.list`).
The persona grants run when `personas.done` is absent or its fingerprint
(issuer, project and the three persona principals) changed — not on every
install — so an upgrade re-grants the memberships in place while a re-run whose
marker still matches does not repair a membership an operator revoked by hand.
That is also the upgrade path for an installation created before the operator
persona existed.

Never pass a token on a command line. For `curl`, write a private header file
and use `--header @file`:

```sh
header="$STATE/secrets/admin-header"
(
  umask 077
  printf 'Authorization: Bearer %s\n' "$(cat "$STATE/secrets/admin-token")"
  printf 'Content-Type: application/json\n'
) > "$header"
chmod 600 "$header"
curl --request GET --silent --show-error --cacert "$STATE/tls/ca.crt" --header @"$header" \
  --data-binary '{"input":{}}' "$KODAMA_PUBLIC_URL/api/v1/projects/$PROJECT/executions"
```

The admin OAuthSonas token is short-lived. The `git` and `engineer` service
credentials live 8 h; renew them before they expire with
`./deploy/podman/install.sh --refresh-credentials` (it rotates both and does
not restart a container). If an admin call answers 401, repeat the sign-in
block above: that block rewrites `$STATE/secrets/admin-header` with the new
token, and the old header keeps failing until you do.

The install keeps its own administrator header in a private header
directory it creates for each invocation and removes again on every exit path,
so `install.sh` never reads or removes your `$STATE/secrets/admin-header` and a
successful or failing install leaves it untouched. Regenerate your header
whenever your admin token changes: after the sign-in block, when an admin token
expires, and after `./deploy/podman/install.sh --refresh-credentials` if your
admin token expired in the meantime.

The server is at `https://localhost:18443` and workloads reach it at
`https://host.containers.internal:18443`; both names are in the certificate.

## Provision a project repository

The tenant and one project already exist after the install. Provision a new
repository with the installed product CLI, `kodama git provision-repository`
(W32). It creates the bare repository, installs the managed hooks and appends
the default `repo <name>` block to the admin repository's committed
`conf/git.conf`. Do not substitute a hand-made `git init --bare`: the default
policy and hooks are part of the served repository.

The examples below use `TENANT=tenant-local`, `PROJECT=tenant-local`,
`GIT=https://localhost:18445`, `GIT_WORKLOAD=https://host.containers.internal:18445`,
the administrator token and (in step 5) the engineer persona token. Adjust the
ids to your `KODAMA_LOCAL_TENANT` and `KODAMA_LOCAL_PROJECT`.

1. Run the installed CLI inside the running server container, which already
   mounts the state directory at its host-absolute path and has the Git binary
   and hooks environment. The command prints the exact `--resource` mapping the
   next `git serve` start needs:

   ```sh
   podman exec kodama-local-server kodama git provision-repository \
     --tenant "$TENANT" --storage-root "$STATE/git/repos" \
     --repository demo --resource git-demo
   ```

2. Register the repository as a resource binding (the
   `resource.binding.register` operation). The enforcing service is the
   `git` service account the install created. Trusted origins are the real Git
   HTTPS endpoints (host and workload), not the API origin:

   ```sh
   jq -n --arg project "$PROJECT" --arg host "$GIT" --arg workload "$GIT_WORKLOAD" '{
     input: {
       project_id: $project,
       binding_id: "git-demo",
       resource_id: "git-demo",
       enforcing_service: "git",
       trusted_origins: [$host, $workload],
       operations: ["git.read", "git.ref.update"],
       reportable_events: ["git.ref.updated"]
     },
     idempotency_key: "local-binding-git-demo"
   }' > "$WORK/demo-binding.json"
   curl --silent --show-error --cacert "$STATE/tls/ca.crt" --header @"$STATE/secrets/admin-header" \
     --data-binary @"$WORK/demo-binding.json" \
     "$KODAMA_PUBLIC_URL/api/v1/projects/$PROJECT/bindings"
   ```

   `git serve` stays on plain loopback HTTP (`127.0.0.1:18081`). The bundle's
   Git HTTPS front end terminates TLS on `18445` and forwards to it; only that
   authenticated HTTPS edge is reachable from the host interface and from
   workloads.

3. Inspect the committed default block. `provision-repository` has already
   appended the repository's rules to the admin repository's `conf/git.conf`
   and moved its trusted `refs/heads/main` with a compare-and-swap; it did not
   need a remote push. The admin repository is a real Git repository served by
   `git serve`. Authenticate Git through a 0600 credential-store file, never an
   argument, and verify the installation CA:

   ```sh
   creds="$STATE/secrets/git-credentials"
   (
     umask 077
     printf 'protocol=https\nhost=localhost:18445\nusername=kodama\npassword=%s\n\n' \
       "$(cat "$STATE/secrets/admin-token")" |
       git credential-store --file "$creds" store
   )
   chmod 600 "$creds"

   git -c http.sslCAInfo="$STATE/tls/ca.crt" \
     -c credential.helper="store --file=$creds" clone "$GIT/admin" "$WORK/admin-work"
   cat "$WORK/admin-work/conf/git.conf"
   ```

   `git credential-store` reads `key=value` lines, not a URL: a piped
   `https://…@host` line is refused with exit 128. The token reaches the helper
   on stdin (through the shell builtin `printf`), never as an argument.

   The demo block is exactly `R = @all`; `RW+ refs/heads/work/ = @engineer
   @execution`; `RW refs/heads/main = @execution`; and `option
   approve.refs/heads/main = reviewer`. It names no `@admin` rule. The policy is
   evaluated first-match-wins.

   Never add a second `repo demo` block. `git.ParseConfig` refuses a repository
   declared twice, so a candidate commit that contains one is rejected before
   it is accepted. To grant anything extra, edit the existing `repo demo` block
   in `$WORK/admin-work/conf/git.conf` in place, commit it with
   `git -C "$WORK/admin-work"`, and push the admin repository's `main` through
   the policy path. That push is governed by the admin repository's own policy,
   which `provision-tenant` wrote as `repo admin` with `RW = @admin`: the admin role
   (the administrator persona's `admin` claim) may fast-forward the admin
   repository's `refs/heads/main`. The platform refuses a candidate policy that
   removes that rule or approval-guards the trusted ref. A rejected push leaves
   the old policy in force.

4. Map the repository and restart only `git serve`. The mapping is read at
   startup:

   ```sh
   export KODAMA_LOCAL_GIT_RESOURCES="admin=git-admin,demo=git-demo"
   ./deploy/podman/install.sh
   ```

   The re-run keeps the CA, keyfile, database, tenant and every container whose
   configuration did not change; it keeps a still-valid credential and only
   rotates an expired one. The changed resource mapping makes it replace the
   `git serve` container while the Git HTTPS front end keeps forwarding. A new
   repository always needs this restart with its `--resource` mapping (see the
   M1 limitations). The re-run also rewrites the installation descriptor with
   the refreshed repository list.

5. Clone and push with the engineer persona. The default rules grant
   `@engineer` and `@execution` forced updates below `refs/heads/work/`, and
   `@execution` fast-forward updates of `refs/heads/main` only after the
   reviewer approval `option approve.refs/heads/main = reviewer`. The
   `engineer-token` service credential is a service identity and does not match
   a human-role rule; use a persona access token:

   ```sh
   engineer_token="$STATE/secrets/engineer-human-token"
   scripts/oauthsonas-token engineer > "$engineer_token"
   chmod 600 "$engineer_token"

   creds_engineer="$STATE/secrets/engineer-git-credentials"
   (
     umask 077
     printf 'protocol=https\nhost=localhost:18445\nusername=kodama\npassword=%s\n\n' \
       "$(cat "$engineer_token")" |
       git credential-store --file "$creds_engineer" store
   )
   chmod 600 "$creds_engineer"

   git -c http.sslCAInfo="$STATE/tls/ca.crt" \
     -c credential.helper="store --file=$creds_engineer" clone "$GIT/demo" "$WORK/demo-work"
   git -C "$WORK/demo-work" -c user.name="Local Engineer" \
     -c user.email="engineer@localhost.test" commit --allow-empty -m "first work commit"
   git -C "$WORK/demo-work" -c http.sslCAInfo="$STATE/tls/ca.crt" \
     -c credential.helper="store --file=$creds_engineer" \
     push origin HEAD:refs/heads/work/first
   git -C "$WORK/demo-work" -c http.sslCAInfo="$STATE/tls/ca.crt" \
     -c credential.helper="store --file=$creds_engineer" \
     push origin HEAD:refs/heads/main   # refused: only an execution may update main, after reviewer approval
   ```

   The work-branch push succeeds; the `main` push is refused for the engineer
   role. Main is written only by an execution, after a reviewer approval. The
   complete request, approval and execution push is the M1 smoke Git scenario
   in [release-smoke.md](release-smoke.md); the protected-task walk-through
   below shows the request and approval half.

## Run a first pipeline

The install writes an engineer service credential to
`$STATE/secrets/engineer-token`. Service credentials act in project scope. Use
the digest of the pre-pulled image so the workload cannot drift (the
descriptor records it as `repository:tag@sha256:...`; a pipeline takes
`repository@sha256:...`, without the tag). This start uses the
`pipeline.start` operation on the inline executions route:

```sh
IMAGE="$(jq -er '.images.tools' "$STATE/run/installation.json" | sed 's/:[^@/]*@/@/')"
cat > "$WORK/first-pipeline.yaml" <<EOF
actions:
  - name: hello
    image: "$IMAGE"
    timeout: 2m
    commands:
      - echo "hello from a Kodama workload"
      - 'echo "action: \$KODAMA_ACTION"'
EOF
jq -n --arg project "$PROJECT" --arg yaml "$(base64 -w0 "$WORK/first-pipeline.yaml")" \
  '{input: {project_id: $project, yaml: $yaml}, idempotency_key: "local-pipeline-first"}' \
  > "$WORK/first-start.json"
(
  umask 077
  printf 'Authorization: Bearer %s\nContent-Type: application/json\n' \
    "$(cat "$STATE/secrets/engineer-token")" > "$STATE/secrets/engineer-header"
)
chmod 600 "$STATE/secrets/engineer-header"
curl --silent --show-error --cacert "$STATE/tls/ca.crt" \
  --header @"$STATE/secrets/engineer-header" --data-binary @"$WORK/first-start.json" \
  "$KODAMA_PUBLIC_URL/api/v1/projects/$PROJECT/executions"
```

Quote a command that contains `: `, as above, or YAML reads it as a mapping.

Read the execution id from the response (`body.execution_id`), then inspect the
execution until `body.state` is `succeeded` and `body.cleanup.state` is
`complete`. Reads are `GET` requests with a JSON body, so `curl` needs
`--request GET`:

```sh
execution=execution-...   # from the start response
jq -n --arg execution "$execution" '{input: {execution_id: $execution}}' > "$WORK/inspect.json"
curl --request GET --silent --show-error --cacert "$STATE/tls/ca.crt" \
  --header @"$STATE/secrets/engineer-header" --data-binary @"$WORK/inspect.json" \
  "$KODAMA_PUBLIC_URL/api/v1/executions/$execution"
```

For the log, take `body.actions[0].action_id` and call
`GET /api/v1/executions/<id>/logs` with
`{"input":{"execution_id":"...","action_id":"...","stream":"stdout"}}`. The
retained chunks are in `body.chunks[].data`, base64-encoded.

Workload containers run from the same Podman daemon, so the image must already
be in that store. The runtime never pulls one for an execution; an image that
is missing leaves the execution `running` until the action timeout. Name an
image as `host/repository@sha256:...` (or `host/repository:tag`, which the
server resolves through the registry).

## Run a first protected task

A protected task runs a stored pipeline whose action is a request assigned to
a human role. This installation's administrator holds the reviewer role
explicitly, so one operator can walk the whole path on this trusted local host.
The request subject in this walk-through is literal (`operation` and
`resource` only) because no earlier action declares and submits an output here:
a `{from: …}` lookup resolves only from an earlier succeeded action's declared
outputs, and the product refuses an unavailable subject value before the request
becomes answerable. A workload can submit declared outputs over HTTPS — the
release smoke and `TestE2E_AC12Outputs` do — when it declares them and has the
installation's public CA certificate in its workspace (for example at
`/workspace/w49-ca.crt`, as the smoke does) or in its image; the install does
not project the CA into workloads (P15).

1. Apply a stored pipeline with a container prepare action and a literal
   review request. The prepare action declares no output and submits nothing:

   ```sh
   cat > "$WORK/protected-pipeline.yaml" <<EOF
   actions:
     - name: prepare
       image: "$IMAGE"
       timeout: 2m
       commands:
         - echo prepared
     - name: review
       request: role/reviewer
       subject:
         operation: local.approve
         resource: local-demo
       timeout: 10m
   EOF
   jq -n --arg project "$PROJECT" --arg pipeline "protected-demo" \
     --arg yaml "$(base64 -w0 "$WORK/protected-pipeline.yaml")" \
     '{input: {project_id: $project, name: $pipeline, yaml: $yaml}, idempotency_key: "local-pipeline-protected-demo"}' \
     > "$WORK/protected-apply.json"
   curl --request PUT --silent --show-error --cacert "$STATE/tls/ca.crt" \
     --header @"$STATE/secrets/admin-header" --data-binary @"$WORK/protected-apply.json" \
     "$KODAMA_PUBLIC_URL/api/v1/projects/$PROJECT/pipelines/protected-demo" \
     > "$WORK/protected-apply-response.json"
   pipeline="$(jq -er '.body.pipeline_id' "$WORK/protected-apply-response.json")"
   ```

2. Create and start the task, then read the execution id from the response:

   ```sh
   jq -n --arg project "$PROJECT" --arg pipeline "$pipeline" \
     '{input: {project_id: $project, title: "First protected task", pipeline_id: $pipeline}, idempotency_key: "local-task-protected-1"}' \
     > "$WORK/protected-task.json"
   curl --silent --show-error --cacert "$STATE/tls/ca.crt" \
     --header @"$STATE/secrets/admin-header" --data-binary @"$WORK/protected-task.json" \
     "$KODAMA_PUBLIC_URL/api/v1/projects/$PROJECT/tasks/create-and-start" \
     > "$WORK/protected-task-response.json"
   execution="$(jq -er '.body.execution_id' "$WORK/protected-task-response.json")"
   ```

3. Read the waiting action's identity and version from the execution, then
   answer it with the reviewer role the administrator holds:

   ```sh
   for _ in $(seq 1 60); do
     jq -n --arg execution "$execution" '{input: {execution_id: $execution}}' \
       > "$WORK/protected-inspect.json"
     curl --request GET --silent --show-error --cacert "$STATE/tls/ca.crt" \
       --header @"$STATE/secrets/admin-header" \
       --data-binary @"$WORK/protected-inspect.json" \
       "$KODAMA_PUBLIC_URL/api/v1/executions/$execution" \
       > "$WORK/protected-inspect-response.json"
     action="$(jq -r '.body.actions[] | select(.name == "review" and .state == "running") | .action_id' "$WORK/protected-inspect-response.json" | head -n 1)"
     version="$(jq -r --arg action "$action" '.body.actions[] | select(.action_id == $action) | .version' "$WORK/protected-inspect-response.json")"
     [ -n "$action" ] && [ -n "$version" ] && break
     sleep 2
   done
   [ -n "$action" ] && [ -n "$version" ] || { echo "review action never became answerable" >&2; exit 1; }

   jq -n --arg execution "$execution" --arg action "$action" \
     --argjson version "$version" \
     '{input: {execution_id: $execution, action_id: $action, expected_action_version: $version, decision: "pass"}, idempotency_key: "local-resolve-1"}' \
     > "$WORK/protected-resolve.json"
   curl --silent --show-error --cacert "$STATE/tls/ca.crt" \
     --header @"$STATE/secrets/admin-header" --data-binary @"$WORK/protected-resolve.json" \
     "$KODAMA_PUBLIC_URL/api/v1/executions/$execution/actions/$action/resolve"
   ```

4. Poll the execution until it is terminal (`succeeded`, with
   `cleanup.state: "complete"`). A failed or timed-out request settles the
   execution; there is no automatic approval reuse.

No script runs this walk-through. The full protected Git integration path
(request, approval and a protected `git.ref.update` through a repository) is
the release smoke scenario `TestReleaseSmoke/ProtectedTask`
([release-smoke.md](release-smoke.md)).

## Re-run, upgrade and operate

- Re-running `./deploy/podman/install.sh` is safe: it keeps the CA, keyfile,
  PostgreSQL and NATS state, reuses every container whose configuration did
  not change, keeps a still-valid service credential, rotates an expired one
  and applies new migrations. It does not tear down a working `git serve`
  before the new credential exists.
- Service credentials (the `git` and `engineer` tokens) live **8 h**. Renew
  them at any time with:

  ```sh
  ./deploy/podman/install.sh --refresh-credentials
  ```

  The refresh rotates both credentials without restarting a container; `git
  serve` reads its credential file on every verification attempt. Run it again
  to rotate a second time. `status.sh` prints the recorded service versions
  and this hint.
- To run a different revision, `git checkout` it and re-run the install; the
  image is tagged `local` and labelled with the checkout revision, and the
  build is reused when the recorded revision matches.
  Restore the database and the keyfile together; a missing keyfile is a
  refusal, not a fresh installation.
- Never expose the PostgreSQL, NATS or `git serve` ports beyond loopback. NATS
  on loopback has no authentication. The bundle publishes these ports on
  `127.0.0.1` only and serves Git over plain HTTP on loopback; the bundle-owned
  Git HTTPS front end (`18445`) is the only workload-reachable Git edge and
  forwards to it.
- Both `socat` forwarders (OAuthSonas and Git HTTPS) are normal background
  processes of the install shell, not systemd units: they are not detached from
  this host's session and do not survive a reboot. Re-run the install after a
  reboot; a re-run restarts only a forwarder whose configuration changed and
  never signals a process merely because a stale pid file exists.
- Logs are container logs. `$STATE/logs/socat.log` and
  `$STATE/logs/git-https.log` hold the forwarders' output.
- The installation descriptor at `$STATE/run/installation.json` is rewritten
  atomically on every successful install or re-run; `status.sh` prints its path
  and refuses one whose identity, endpoints, images or persona principals do
  not match the selected installation.

## Smoke test

`deploy/podman/smoke.sh` runs one disposable installation for the M1 smoke. It
requires an infrastructure slot and an explicit complete port set, and it never
inherits its identity from the environment. The prefix defaults to
`smoke-<run>` and the state directory to `/tmp/oc/smoke/state-<run>`; override
them only with `KODAMA_SMOKE_PREFIX` and `KODAMA_SMOKE_STATE_DIR`, and pick a
fresh state directory. An inherited `KODAMA_LOCAL_PREFIX`,
`KODAMA_LOCAL_STATE_DIR` or `KODAMA_LOCAL_GIT_RESOURCES` (for example from a
sourced `secrets/operator.env`) is ignored, and so is every other
`KODAMA_LOCAL_*` identity or endpoint setting (tenant, project, service ids,
URL and OIDC overrides), because a stopped real installation
keeps its CA, keyfile and PostgreSQL/NATS volumes in its own state directory and
a disposable `--purge` would delete them. The run refuses a state directory that
is not absent or empty, and it refuses a prefix whose containers or
`<prefix>-pgdata`/`<prefix>-natsdata` volumes already exist, both before
installing anything. It records the state directory it owns and purges only that
installation:

```sh
scripts/infra-slot.sh --exclusive smoke-local stack -- \
  env KODAMA_SMOKE_RUN=local1 \
      KODAMA_LOCAL_HTTPS_PORT=28443 KODAMA_LOCAL_OIDC_PORT=28444 \
      KODAMA_LOCAL_GIT_PORT=28081 KODAMA_LOCAL_GIT_HTTPS_PORT=28445 \
      KODAMA_LOCAL_POSTGRES_PORT=25433 KODAMA_LOCAL_NATS_PORT=24223 \
      KODAMA_LOCAL_NATS_MONITOR_PORT=28223 KODAMA_LOCAL_OAUTH_UPSTREAM_PORT=28182 \
      deploy/podman/smoke.sh --preflight
```

The example uses a port set that does not collide with the install defaults
above, so it can run beside a real local installation. Omit `KODAMA_SMOKE_RUN`
to derive a run id from the clock and PID; a reused run id with the default state
directory is refused, because that directory is no longer empty.
`--preflight` stops
after the installation access proof and the runner's
`TestE2E_InstallationAccess` selection. The default mode runs the full selected
release tests. Both modes go through
`test/e2e/internal/harness/verify-go-test.sh` with an explicit expected-name
list, so a zero-test selection, a renamed test, a skipped subtest or a failure
fails the run; `KODAMA_SMOKE_TEST_RUN`/`KODAMA_SMOKE_EXPECT` narrow a
diagnostic run and are marked `NARROWED SELECTION - NOT RELEASE EVIDENCE` at the
start and end and in `summary.txt`. `--print-selection` prints the exact
selection (one argument per line) without installing anything. The common runner
is required; without it the wrapper reports the missing path and exits non-zero
rather than skipping the proof. The wrapper keeps the test's exit status when
cleanup also fails and retains the evidence directory for recovery.

## Uninstall

```sh
./deploy/podman/down.sh
```

`down` stops each TLS front end only after confirming its pid still runs this
installation's listen port, and removes the containers carrying
`kodama.local.prefix=<prefix>` plus this installation's exact container names.
It keeps the state directory and the PostgreSQL/NATS volumes so a later install
resumes the same installation. The workload containers and workspace volumes
that `kodama server` created for executions belong to the server's own cleanup
path; `down` does not remove them (a host-wide sweep could hit another run).

```sh
./deploy/podman/down.sh --purge
```

`--purge` additionally deletes this installation's own state children (CA,
keyfile, tokens, runtime data and Git repositories), removes the exact
PostgreSQL/NATS volumes after checking their installation label, and then
removes the state directory with `rmdir`. It refuses a directory whose marker
does not name this exact prefix, and it refuses to delete an unknown file
rather than `rm -rf` the directory. No command prunes the host or another run's
containers, volumes or images.

## Troubleshooting

- `/readyz` answers 503: check `podman logs kodama-local-server`. Readiness
  covers the four PostgreSQL pools, NATS, the keyfile and the Podman ping.
- `podman logs kodama-local-git` shows a policy refusal: the committed
  `conf/git.conf` must still declare `repo admin` with `RW = @admin`, and every
  repository served needs an explicit rule.
- A workload cannot reach the server: confirm the server listens on
  `0.0.0.0:18443`, that the certificate has `host.containers.internal` as a
  subject alternative name, and that the workload trusts `$STATE/tls/ca.crt`.
  The image must also already be in the same Podman store.
- A workload cannot reach Git: use `https://host.containers.internal:18445`
  (never the loopback `127.0.0.1:18081` listener), trust the same installation
  CA, and check `$STATE/logs/git-https.log`. A different CA must be refused.
- `bootstrap-admin` refuses with `version_conflict`: the tenant already exists
  but the install marker is missing (for example after restoring only part of
  the state). Use the existing keyfile and database; do not regenerate either.
  A genuine fresh start is `./deploy/podman/down.sh --purge` and a new install.
- Port already allocated: set the corresponding `KODAMA_LOCAL_*_PORT` or stop
  the conflicting process. Do not reuse another installation's prefix.
- The install refuses a state directory with a foreign marker or a non-empty
  directory without one. That protects another installation's keyfile and CA;
  choose the matching prefix/state pair or a new directory, never delete the
  marker to force it.
- `service.credential` operations answer `version_conflict` or a service call
  answers 401 after 8 h: run `./deploy/podman/install.sh --refresh-credentials`.
- An image override is refused unless it is `tag@sha256:<64 hex>`; the install
  also refuses a pulled digest that differs from the pin, and a prebuilt
  `KODAMA_LOCAL_KODAMA_IMAGE` that lacks `git-http-backend` (its image must
  install Alpine's `git-daemon` package like
  `deploy/images/kodama/Containerfile`).
- OAuthSonas: `podman logs kodama-local-oauthsonas` and
  `$STATE/logs/socat.log`. The picker access code is only in
  `$STATE/secrets/oauth-access-code` (0600).
- Sign-in token exchange fails: `scripts/oauthsonas-token` needs `jq`,
  `openssl` and `curl`, and the persona id must match `[A-Za-z0-9_-]`.

## M1 release limitations

The limitations of this single-host installation. They started as the M1
release list and are kept current here; they are part of the release, not
installation bugs:

- Single host, trusted operators, not production: keys in a local keyfile;
  production refuses these adapters.
- The API (`18443`) and the Git HTTPS front end (`18445`) listen on
  `0.0.0.0`, so every host interface can reach them, not only `localhost`.
  Both are TLS-only and authenticated (every API route except the
  unauthenticated `/healthz`, `/readyz` and `/metrics` routes needs an
  installation identity's bearer token; the Git edge needs a service or persona
  credential), but on a shared network put a host firewall in front of both
  ports. PostgreSQL, NATS (client and monitor), the loopback `git serve`
  listener and the OAuthSonas upstream/TLS terminator stay on `127.0.0.1`.
- The workload secret-name allow-list (`Config.SecretRules`) is
  deny-by-default and not configurable through the installation or the `test`
  profile. A pipeline action that declares `secrets:` is refused at activation
  with `forbidden: secret name is not authorized`; workloads read only the
  mounted execution credential at `/kodama/secrets/KODAMA_TOKEN`. Automatic CA
  projection is P15 work, so a workload that needs verified HTTPS gets the
  installation's **public** CA certificate from its image or from bytes the
  pipeline writes into its workspace (`/workspace`, as the smoke and
  `TestE2E_AC12Outputs` do). The certificate is public data, never a key.
- A workload can submit declared outputs over HTTPS with its mounted execution
  token and the CA above; outputs submitted by an action that then fails are
  discarded, and a promoted output is visible only after success.
- Retained logs are collected when the action's workload is removed at
  completion, and before a stop. A still-running action has no retained chunks
  yet; read the log after the action has finished (and `cleanup.state` is
  `complete` for the execution). While the action runs, log chunks are
  published best-effort on the broker, which this bundle exposes only on
  loopback.
- No live NATS subscriptions for external clients in this bundle: the broker
  is anonymous and loopback-only, with no per-credential authorization, so it
  enforces no expiry or revocation and must never be exposed. A process on the
  host can subscribe over loopback, as the portable e2e suite's live
  subscription tests do. The per-credential authorization callout is
  configured only by the Kubernetes chart, and `event.subscribe` refuses a
  human token (DEF-05). Read events and logs over HTTP or MCP (`event.read`,
  `execution.logs.read`).
- The `webhook` and `email` service kinds deliver nothing; email-to-task
  intake works (DEF-08).
- Git ref deletes are refused (DEF-01).
- The bundle starts no services container, so there is no merge coordinator,
  and it sets none of `KODAMA_MODELS_FILE`, `KODAMA_TOOL_PROXY_FILE` and
  `KODAMA_GIT_STORAGE_ROOT`: the model gateway, the MCP tool proxy and the
  retained-commit keep refs stay off. The binary supports them
  (`kodama services --kind merge_coordinator`, `kodama agent`, and the three
  settings on `kodama server`).
- The server runs the retention sweeper and serves operations metrics at
  `/metrics`; neither has been exercised on an installed bundle.
- A new repository needs a `git serve` restart with its `--resource` mapping.
- AWS and GCP: the production entry accepts `aws-kms` and `gcp-kms`, but
  neither cloud is live-validated yet (DEF-11).
