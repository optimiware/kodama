# Changelog

## 0.2.2

Changed:

- **The project is renamed from Kodama to Kodema.** The repository is
  `optimiware/kodema`. The binary is `kodema`; the Go module is
  `github.com/optimiware/kodema`; the charts, the image and the specification
  (`KODEMA.md`) carry the new name. `kodactl`, `kodex` and `kodsvc` keep their
  names. Releases up to 0.2.1 were published as `kodama` and stay as they were.
- **Breaking for anything deployed.** Environment variables are `KODEMA_*`,
  workload mounts are `/kodema/...`, the label domain is `kodema.dev`, the
  PostgreSQL roles and schema are `kodema_*`, and the Kubernetes namespaces
  and resources are `kodema-*`. Rename them with the upgrade; there is no
  fallback to the old names. The token MAC domain labels changed
  (`kodema-execution-v1`, `kodema-service-v1`), so credentials issued by 0.2.1
  or earlier do not verify. The migration setup lock key changed too, so old
  and new versions do not exclude each other during a rolling upgrade.
- `install.sh` installs the `kodema` and `kodactl` binaries of this release:
  `curl -fsSL https://raw.githubusercontent.com/optimiware/kodema/master/install.sh | sh`.
  The old `optimiware/kodama` address redirects to the new one.

Fixed:

- The example pipeline in `KODEMA.md` submitted its output in a form the server
  rejects (`400 invalid_input`). It now sends the operation envelope with
  `expected_version` and an `idempotency_key`, and addresses the action by its
  ID. The example's image needs `jq` beside Git and curl.

Documentation:

- The README opens with who Kodema is for, one contract for every harness and
  what a company needs at enterprise scale, and its Status section says where
  the design is ahead of the evidence.

Known limitations:

- `install.sh` installs 0.2.2 or newer. Releases up to 0.2.1 carry `kodama`
  archives and no `kodema` archive.
- The macOS binaries are built and have not been run. `kodema up` has run on
  Linux only.

## 0.2.1

Added:

- `install.sh`: one command installs `kodama` and `kodactl` into
  `~/.local/bin` on Linux and macOS, amd64 and arm64, with no checkout and no
  Go: `curl -fsSL https://raw.githubusercontent.com/optimiware/kodama/master/install.sh | sh`.
  It installs nothing unless both downloads match the release's `SHA256SUMS`.
- Releases attach a `kodama` archive per platform, beside the `kodactl`
  archives. `kodama up` pulls the released image of its version, so the
  quick start is the installer and `kodama up`.

Known limitations:

- `install.sh` installs 0.2.1 or newer; earlier releases have no `kodama`
  archive.
- The macOS binaries are built and have not been run. `kodama up` has run on
  Linux only.

## 0.2.0

Added:

- `kodama up`: a complete local Kodama on rootless Podman in one command
  (PostgreSQL, NATS, the server, Git, a built-in sign-in), with its project,
  roles and credentials ready. `kodama down` and `kodama status` go with it.
  See the [quickstart](docs/runbooks/quickstart.md).
- `kodactl run <file>`: runs a pipeline file, follows it to its end and prints
  its output and outcome.
- `kodactl`: help, `whoami`, `inbox`, `approve`, `reject`, `logs`, `task`,
  `member`, `--ca-file`, and an MCP stdio bridge with `mcp install`.
- MCP over Streamable HTTP that standard clients accept: protocol version
  negotiation, `notifications/initialized`, tool descriptions, `content` and
  `structuredContent` results, and a 401 with resource metadata on an
  unauthenticated tool call.
- Discovery operations: `principal.whoami` and list operations for projects,
  pipelines, tasks, bindings and services. A read accepts an empty body.
- A built-in OIDC provider and social sign-in broker (`kodama auth serve`),
  and `kodama auth principal-id`.
- Git: a new repository is served without restarting `git serve`; a push
  trigger may name a ref prefix ending in `/`.
- A streaming model proxy for workloads, and the installation CA mounted into
  Podman workloads.
- Releases: `make release-prepare VERSION=<version>` sets the version
  everywhere, a change of `VERSION` on `master` publishes it and creates the
  tag, a released version is never replaced, and the image must install and
  run a pipeline before anything is pushed. The image is built for
  `linux/amd64` and `linux/arm64`, and `latest` follows a release that is not
  a pre-release. `master` carries a development version between releases.

Changed:

- `kodactl pipeline apply` takes one file as well as a directory, and prints
  the id of each pipeline it applied.
- The MCP endpoint refuses a request whose `Origin` is not the installation's
  own. A request without an `Origin`, as command-line clients send, is
  unaffected.
- An image reference the server cannot read is refused as `invalid_input`; it
  was retried as a registry outage. An image that is missing from the local
  Podman store fails the execution at start; it ran until the action timeout.
- Podman bundle: a new installation's CA carries a critical key usage, which
  strict verifiers such as Python 3.13 require.

Fixed:

- Podman bundle: a refused `down.sh --purge` left an installation that could
  be neither reinstalled nor purged.
- Podman bundle: a first install could fail in rare cases, because it took
  PostgreSQL for ready while the image was still initialising a new volume.
- The first-pipeline steps of the Podman bundle runbook failed as written.
  They now use `kodactl run`.
- An action with `timeout: 1m` could fail to get its execution credential,
  because the shortest credential lifetime equalled the shortest timeout. The
  shortest lifetime is now 15 seconds.
- An event cursor beyond the signed 64-bit range failed the read with an
  internal error; it is `invalid_input`.
- AWS KMS: a decrypt cancelled as the reply arrived could report a failed
  credential exchange instead of the cancellation.

Known limitations:

- `kodama up` needs Podman; it refuses a Docker engine, because workloads run
  through the Podman API. It has run on Linux only.
- `kodama` is published as an image, not as a binary: `kodama up` is built
  from a checkout with `make build`.
- `kodama up` writes two administrator credentials that last 24 hours, and a
  Git service credential that lasts 8 hours. Running it again renews them.
  The default credential is scoped to the project; membership administration
  needs the tenant-scoped one in `KODAMA_TENANT_ADMIN_CREDENTIAL_FILE`.
- The built-in sign-in of `kodama up` asks for no password and answers its
  own machine only. On Linux the gateway listens on every address and refuses
  peers that are not this machine.
- `kodactl run` prints a workload's output when the execution ends, not while
  it runs.
- An unauthenticated MCP `tools/list` answers 200; only tool calls answer 401.
- The workload model proxy forwards any path of a configured provider; it has
  no model or endpoint allowlist.
- `kodama auth serve` does not limit the rate of sign-in attempts or client
  registrations.
- This is the first release the new release workflow publishes. Its
  `linux/arm64` image is built and has not been run.

## 0.1.0

The first public release.

Deployment targets:

- Single host on rootless Podman, and Kubernetes through the Helm charts.
- Scaleway is the only cloud target that has run live. AWS and GCP are
  implemented and not validated against a real cloud.

Known limitations:

- Git ref deletes are refused, also where `RW+` allows them.
- There is no public query for referenced commits; retained commits are
  protected by the control plane's keep-refs.
- Human and browser NATS subscriptions are refused; humans read events and
  logs over HTTP or MCP.
- The `webhook` and `email` service kinds deliver nothing outbound;
  email-to-task intake works.
- A workload start the runtime refuses leaves the execution `running` until
  its action deadline, and the cause is not shown in the execution view.
- The reference Scaleway configuration backs up every 6 hours, which misses
  the recommended RPO of 15 minutes.
- No service has been resumed from a restored database on a cloud deployment,
  so there is no measured RTO.
- The image is linux/amd64 only. Artifacts are not signed.
