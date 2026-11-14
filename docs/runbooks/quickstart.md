# Quickstart: a local Kodama with `kodama up`

`kodama up` starts a complete Kodama on one machine, and `kodactl run` runs a
pipeline file on it. This is the shortest way to try Kodama.

It is **not a production installation**: one host, a local keyfile, and a
built-in sign-in that asks for no password. For production, see the
[production deployment guide](production-deployment.md).

## Prerequisites

- Linux with rootless Podman 5 or newer and its user API socket:
  `systemctl --user enable --now podman.socket`, and `podman info` succeeds.
  Docker is not supported, because Kodama starts workloads through the Podman
  API. macOS with a Podman machine is expected to work and has not been run.
- Go (the version in [`.go-version`](../../.go-version)), `git` and `make`, to
  build from a checkout.
- Port 8443 free.

## Start

```sh
git clone https://github.com/optimiware/kodama && cd kodama
make build image
bin/kodama up
source ~/.kodama/instances/default/env.sh
bin/kodactl whoami
```

`make build` builds `bin/kodama` and `bin/kodactl`. `make image` builds the
container image of the same version, which is the image `kodama up` starts.

`kodama up` does this, and is safe to run again:

1. Starts PostgreSQL, NATS, the Kodama server and the Git service as
   containers, and a gateway on `https://127.0.0.1:8443` that serves the API,
   MCP (`/mcp`), sign-in (`/auth/`) and Git (`/git/`).
2. Creates a local CA and certificates, the tenant `tenant-local` and the
   project `project-local`.
3. Gives the four built-in personas their project roles: `admin` (every role),
   `engineer`, `reviewer` and `operator`.
4. Writes `env.sh`, which points `kodactl` at this installation as `admin`.

Everything is kept in `~/.kodama/instances/default`. `whoami` should answer
with the roles `admin,engineer,reviewer,operator`.

## Run a first pipeline

The runtime never pulls an image. Pull the one the pipeline names, and name it
by digest:

```sh
podman pull docker.io/library/alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b

cat > hello.yaml <<'EOF'
actions:
  - name: hello
    image: docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b
    timeout: 2m
    commands:
      - echo "hello from a Kodama workload"
EOF

bin/kodactl run hello.yaml
```

```text
execution execution-… started
hello from a Kodama workload
execution execution-… succeeded
```

`kodactl run` starts the file, waits for the execution to end and prints the
output of its actions. It exits 0 when the execution succeeded and 1 when it
failed, with the reason the server reports. The output appears when the
execution ends, not while it runs.

## Ask for an approval

An action can ask a role for a decision instead of running a container:

```sh
cat > review.yaml <<'EOF'
actions:
  - name: prepare
    image: docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b
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

bin/kodactl run review.yaml
```

`run` stops when the execution waits for a person and names the execution.
Answer it from the inbox:

```sh
bin/kodactl inbox
bin/kodactl approve <execution-id>      # or: kodactl reject <execution-id> --reason "…"
bin/kodactl watch <execution-id>
bin/kodactl logs <execution-id> --action prepare
```

## Store a pipeline and start it from a task

`run` stores nothing. To keep a pipeline in the project and start it from a
task:

```sh
bin/kodactl pipeline apply hello.yaml          # prints the pipeline id
bin/kodactl task run --title "First task" --pipeline <pipeline-id>
bin/kodactl task list
```

## Sign in as a person

`env.sh` uses an administrator token that `kodama up` wrote. To act as one of
the personas, sign in:

```sh
unset KODAMA_CREDENTIAL_FILE
bin/kodactl login            # or: kodactl login --device, without a browser
bin/kodactl whoami
```

The sign-in page offers the four personas and asks for no password. It answers
this machine only.

Project and membership administration needs the tenant-wide administrator
token instead:

```sh
bin/kodactl --credential-file "$KODAMA_TENANT_ADMIN_CREDENTIAL_FILE" member list
```

## Git

The tenant's admin repository is served under `/git/`. Authenticate with a
credential-store file, never an argument:

```sh
creds=~/.kodama/instances/default/git-credentials
(
  umask 077
  printf 'protocol=https\nhost=127.0.0.1:8443\nusername=kodama\npassword=%s\n\n' \
    "$(cat ~/.kodama/instances/default/admin-token)" |
    git credential-store --file "$creds" store
)
git -c http.sslCAInfo="$KODAMA_CA_FILE" -c credential.helper="store --file=$creds" \
  clone https://127.0.0.1:8443/git/admin
```

## Connect an MCP client

```sh
bin/kodactl mcp install claude --print     # or cursor, codex; without --print it writes the client's config
```

## Operate

```sh
bin/kodama status
bin/kodama up              # again: renews the credentials below
bin/kodama down            # stops it and keeps the state
bin/kodama down --purge    # removes the containers, volumes and state
```

- The administrator token in `env.sh` lasts 24 hours and the Git service
  credential 8 hours. Run `kodama up` again to renew both.
- `kodama up --name <name>` runs a second installation; give it another
  `--listen` port.

## Limits

- One host, one tenant, a local keyfile. Anyone who can use this machine can
  sign in as any persona.
- On Linux the gateway listens on every address, because containers reach the
  host on its outward address. It refuses every peer that is not this machine,
  so the installation is not reachable from another host.
- Podman only. `kodactl run` prints a workload's output when the execution
  ends; live output is not shown.
- The [Podman bundle](podman-quickstart.md) (`deploy/podman/install.sh`) is a
  second single-host installation, with a separate identity provider. The
  release smoke test runs on it.
