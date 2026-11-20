# Quickstart: a local Kodema with `kodema up`

`kodema up` starts a complete Kodema on one machine, and `kodema run` runs a
pipeline file on it. This is the shortest way to try Kodema.

It is **not a production installation**: one host, a local keyfile, and a
built-in sign-in that asks for no password. For production, see the
[production deployment guide](production-deployment.md).

## Prerequisites

- Linux with rootless Podman 5 or newer and its user API socket:
  `systemctl --user enable --now podman.socket`, and `podman info` succeeds.
  Docker is not supported, because Kodema starts workloads through the Podman
  API.
- Or macOS with a running Podman machine of Podman 5 or newer:
  `podman machine start`.
- `curl` and `tar`, for the installer.
- Port 8443 free.

## Start

Check the version printed by the installer. For **0.2.3**, use the
[0.2.3 instructions](https://github.com/optimiware/kodema/blob/v0.2.3/docs/runbooks/quickstart.md);
its client is `kodactl` and its workloads have no toolbox. To try the
single-binary version before it is released, [build from a checkout](#from-a-checkout).

```sh
curl -fsSL https://raw.githubusercontent.com/optimiware/kodema/master/install.sh | sh
kodema up
source ~/.kodema/instances/default/env.sh
kodema whoami
```

[`install.sh`](../../install.sh) installs `kodema` into `~/.local/bin`, which
must be on your `PATH`. It is one binary: the client, the local installation
and the server. The script downloads the archive of the latest release for
your operating system and architecture, and installs nothing unless it matches
the release's `SHA256SUMS`. `KODEMA_VERSION` names
another release (0.2.1 or newer) and `KODEMA_INSTALL_DIR` another directory.
Run it again to upgrade.

`kodema up` pulls the container image of its own version.

### From a checkout

With Go (the version in [`.go-version`](../../.go-version)), `git` and `make`:

```sh
git clone https://github.com/optimiware/kodema && cd kodema
make build image
bin/kodema up
source ~/.kodema/instances/default/env.sh
```

`make build` builds `bin/kodema`. `make image` builds the container image of
the same version, which is the image `kodema up` starts. The command below is
then `bin/kodema`.

## What `kodema up` does

The command is safe to run again. It:

1. Starts PostgreSQL, NATS, the Kodema server and the Git service as
   containers, and a gateway on `https://127.0.0.1:8443` that serves the API,
   MCP (`/mcp`), sign-in (`/auth/`) and Git (`/git/`).
2. Creates a local CA and certificates, the tenant `tenant-local` and the
   project `project-local`.
3. Gives the four built-in personas their project roles: `admin` (every role),
   `engineer`, `reviewer` and `operator`.
4. Writes `env.sh`, which points `kodema` at this installation as `admin`.

Everything is kept in `~/.kodema/instances/default`. `whoami` should answer
with the roles `admin,engineer,reviewer,operator`.

## Run a first pipeline

The runtime never pulls an image. Pull the one the pipeline names first:

```sh
podman pull docker.io/library/alpine:3.24

cat > hello.yaml <<'EOF'
actions:
  - name: hello
    image: docker.io/library/alpine:3.24
    timeout: 2m
    commands:
      - echo "hello from a Kodema workload"
EOF

kodema run hello.yaml
```

```text
execution execution-… started
hello from a Kodema workload
execution execution-… succeeded
```

`kodema run` starts the file, waits for the execution to end and prints the
output of its actions. It exits 0 when the execution succeeded and 1 when it
failed, with the reason the server reports. The output appears when the
execution ends, not while it runs.

## Ask for an approval

An action can ask a role for a decision instead of running a container:

```sh
cat > review.yaml <<'EOF'
actions:
  - name: prepare
    image: docker.io/library/alpine:3.24
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

kodema run review.yaml
```

`run` stops when the execution waits for a person and names the execution. It
exits 1 then, as the execution has not succeeded. Answer it from the inbox:

```sh
kodema inbox
kodema approve <execution-id>      # or: kodema reject <execution-id> --reason "…"
kodema watch <execution-id>
kodema logs <execution-id> --action prepare
```

## Store a pipeline and start it from a task

`run` stores nothing. To keep a pipeline in the project and start it from a
task:

```sh
kodema pipeline apply hello.yaml          # prints the pipeline id
kodema task run --title "First task" --pipeline <pipeline-id>
kodema task list
```

## Sign in as a person

`env.sh` uses an administrator token that `kodema up` wrote. To act as one of
the personas, sign in:

```sh
unset KODEMA_CREDENTIAL_FILE
kodema login            # or: kodema login --device, without a browser
kodema whoami
```

The sign-in page offers the four personas and asks for no password. It answers
this machine only.

Project and membership administration needs the tenant-wide administrator
token instead:

```sh
kodema --credential-file "$KODEMA_TENANT_ADMIN_CREDENTIAL_FILE" member list
```

## Git

The tenant's admin repository is served under `/git/`. Configure Git once for
this installation's origin, with `kodema` as its only credential helper and
the installation's CA:

```sh
origin=https://127.0.0.1:8443
git config --global --replace-all "credential.$origin.helper" ''
git config --global --add "credential.$origin.helper" \
  "!kodema --trusted-origin $origin --credential-file ~/.kodema/instances/default/admin-token git credential"
git config --global "http.$origin.sslCAInfo" ~/.kodema/instances/default/tls/ca.crt

git clone https://127.0.0.1:8443/git/admin
```

The first line empties the helper list of that origin. Without it a system
helper, such as the macOS keychain, keeps the token Git used and offers it
again after it has expired: the next Git command is then refused once. To act
as a persona instead of the administrator, sign in with `kodema login` and
leave `--credential-file` out. `kodema down --purge` does not remove these
three settings; remove them with `git config --global --unset-all`.

## Add a repository

A repository is created in the Git service and registered with Kodema as a
resource of the project. The default rules let everyone in the project read
it, let engineers and executions write below `refs/heads/work/`, and let only
an execution update `main`, after a reviewer approved that exact update:

```sh
podman exec kodema-default-git kodema git provision-repository \
  --tenant tenant-local --storage-root ~/.kodema/instances/default/git/repos \
  --repository demo --resource git-demo

kodema call resource.binding.register --idempotency-key=bind-git-demo --input='{
  "project_id": "project-local", "binding_id": "git-demo", "resource_id": "git-demo",
  "enforcing_service": "git",
  "trusted_origins": ["https://127.0.0.1:8443", "https://host.containers.internal:8443"],
  "operations": ["git.read", "git.ref.update"], "reportable_events": ["git.ref.updated"]}'

git clone https://127.0.0.1:8443/git/demo
git -C demo commit --allow-empty -m "first commit"
git -C demo push origin HEAD:refs/heads/work/first
```

The first command says to restart `kodema git serve`; a local Kodema needs no
restart, because its Git service finds a registered repository by itself.

## Use Git and outputs in a pipeline

Every container action has the `kodema` binary at `/kodema/bin`, on its
`PATH`, and Git already configured for this installation: a clone or a push to
`$KODEMA_GIT_URL` authenticates as the execution, with no credential in the
pipeline. An action that declares outputs writes each one to a file named for
its key in `/kodema/outputs/<its name>`; later actions read the same path.

```sh
cat > outputs.yaml <<'EOF'
actions:
  - name: source
    image: docker.io/library/alpine:3.24
    timeout: 5m
    outputs: [commit]
    commands:
      - apk add --no-cache git >/dev/null
      - git clone --quiet --branch work/first "$KODEMA_GIT_URL/demo" demo
      - git -C demo rev-parse HEAD > /kodema/outputs/source/commit
  - name: report
    image: docker.io/library/alpine:3.24
    timeout: 2m
    commands:
      - echo "work/first is at $(cat /kodema/outputs/source/commit)"
EOF

kodema run outputs.yaml
```

After the last command of `source` has succeeded, Kodema runs
`kodema output push`, which submits the files of its output directory with one
trailing newline removed from each value. A refused value fails the action;
its message is in `kodema logs <execution-id> --action source --stream stderr`.
Commands start in `/workspace`, which the actions of one execution share.

## Connect an MCP client

```sh
kodema mcp install claude --print     # or cursor, codex; without --print it writes the client's config
```

## Operate

```sh
kodema status
kodema up              # again: renews the credentials below
kodema down            # stops it and keeps the state
kodema down --purge    # removes the containers, volumes and state
```

- The administrator token in `env.sh` lasts 24 hours and the Git service
  credential 8 hours. Run `kodema up` again to renew both.
- `kodema up --name <name>` runs a second installation; give it another
  `--listen` port.
- `kodema up` finds the Podman socket itself: your user's on Linux, the
  running Podman machine's on macOS. `--socket <path>` or `KODEMA_SOCKET`
  names another; `status` and `down` then use it without either.
- Workloads default to 2 GiB memory and 2 CPUs (max 4 GiB / 4 CPUs). Override
  with `--workload-memory`, `--workload-cpus`, `--workload-max-memory`, and
  `--workload-max-cpus`. See
  [unprivileged OCI builds and workload limits](unprivileged-oci-builds.md) for
  per-action `resources`, `OOMKilled` diagnostics, and building container images
  without a host Docker/Podman socket.

## Limits

- The containers of the installation that mount its state directory or the
  Podman socket (PostgreSQL, the server, Git) run with SELinux labeling
  disabled (`label=disable`): a confined container may read neither. Workloads
  keep their label.
- One host, one tenant, a local keyfile. Anyone who can use this machine can
  sign in as any persona.
- On Linux the gateway listens on every address, because containers reach the
  host on its outward address. It refuses every peer that is not this machine,
  so the installation is not reachable from another host.
- Podman only. `kodema run` prints a workload's output when the execution
  ends; live output is not shown.
- The [Podman bundle](podman-quickstart.md) (`deploy/podman/install.sh`) is a
  second single-host installation, with a separate identity provider. The
  release smoke test runs on it.
