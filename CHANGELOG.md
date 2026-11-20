# Changelog

## 0.6.0

Added:

- **Embedded documentation across MCP and CLI, plus Git company context CLI.**
  Kodema embeds its documentation topics inside the binary and exposes them via
  MCP resources (`kodema://docs/<topic>`) and `kodema docs [<topic>|install]`.
  Added `kodema context pull` to clone/sync shared company and project Git
  context repositories and skills into agent workspaces.
- **First-class project, repository, and pipeline management across REST, MCP,
  and CLI** (61 registered operations total):
  - `project.create` (`kodema project create <id> [--name ...] [--copy-members-from <project>]`)
    and `project.list` (`kodema project list`, defaulting `tenant_id` to the
    caller's authenticated tenant).
  - `repository.create` (`kodema repo create <name>`) and `repository.list`
    (`kodema repo list`) for one-step HTTP bare-repository provisioning,
    `admin/conf/git.conf` stanza registration, and project resource binding.
  - `kodema pipeline apply <file-or-dir>` (deriving names from filenames),
    `kodema pipeline show <name|id> [--yaml]`, and `kodema pipeline delete`.
- **Native credential-injecting LLM proxy and token-usage attribution.**
  Workloads authenticate to `${KODEMA_URL}/llm/<provider>/*` with
  `/kodema/secrets/KODEMA_TOKEN` while provider API keys remain strictly inside
  the control plane (never mountable via `secrets: [...]`). Enabled out of the
  box in `kodema up` for OpenAI, Anthropic, and OpenCode Zen plus custom
  `--llm-provider` entries, with per-execution `model_usage` attribution
  surfaced in `execution.inspect` and `kodema inspect`.
- **Live project SSE event stream and ergonomic monitoring CLI:**
  - `GET /api/v1/projects/{project}/events/stream` (`text/event-stream`) with
    `since`/`after`, repeatable `type`, and `Last-Event-ID` cursor resume (zero
    gaps, zero duplicates) sharing REST auth, visibility, and expiry rules.
  - `kodema events [--follow]`, `kodema watch --project <id> [--until request]`,
    `kodema inbox --wait`, and multi-action `kodema logs --follow <execution-id>`.
- **Multi-stage task transitions and clean `task.restart`:**
  - `task.start` (`kodema task start <task-id> --pipeline <name|id> --input ... --input-from-output <k>=<action>.<out>`)
    atomically switches pipelines and inputs across stages on one task,
    including transitioning from `done`.
  - `task.restart` (`kodema task restart <task-id> [--pipeline ...] [--input ...] [--input-from-output ...] [--reason "..."]`)
    atomically cancels a running or waiting execution with Kernel
    `CancelSuperseded` (`superseded`) and starts a fresh attempt once the
    predecessor stop settles.
  - Two-stage agent pipeline recipes under `examples/pipelines/`.
- **Stale approval detection and approval guards.** Verified `git.ref.updated`
  events automatically mark pending `git.ref.update` requests stale in
  `task.inbox` (`STALE` badge + superseding commit/reason in `kodema inbox`).
  `kodema approve` refuses stale requests by default unless `--allow-stale` is
  supplied and supports `--candidate <sha>` confirmation.
- **Opt-in disjoint rebase approval in `kodema git serve`.** Repositories can
  opt in per ref via `option approve-rebase.<ref> = disjoint` alongside
  `option approve.<ref> = <role>` to accept clean, linear, merge-free rebases
  pushed with `-o kodema.approved_base=<sha> -o kodema.approved_candidate=<sha>`
  when intervening target-ref commits touch strictly disjoint paths and
  per-commit raw diffs/modes match byte-for-byte.
- **Read-only task specification materialization.** Every task-associated
  container action receives `/kodema/task/title`, `/kodema/task/description`,
  `/kodema/task/metadata.json`, and `/kodema/task/spec.json` (immutable at
  admission across process, Podman, and Kubernetes runtimes) alongside
  `/kodema/task.json`.

Fixed:

- Propagate verified human OIDC JWT expiry into `event.subscribe` and the NATS
  authorization callout across `test` and `production` profiles so human users
  can subscribe to live project events without switching to service accounts.
- Surface failed container action stderr tails and actionable `kodema logs`
  hints in `kodema inspect` and `kodema watch`.
- Fix `kodema up` identity-mismatch preflight guard and tenant-scoped local
  workload secrets across multi-project installations.
- Document `kodema task create/run --meta`/`--metadata`, align metadata JSON
  Schema (`additionalProperties: {"type": "string"}`) and docs, display the
  active server URL in `kodema whoami`, and attach actionable client-side hints
  on project-scope JWT mismatch without falling through an explicit
  `--credential-file`.
- Stabilize `TestTaskSpecMaterializationEndToEnd` version/cleanup synchronization
  and add live Podman and kind E2E coverage for v0.6 capabilities.

## 0.5.0

Changed:

- **Project is now the read/visibility boundary.** Every human project member
  and every enabled service in scope (including `roles: []`) can read
  non-secret project operational state (`execution.*`, `task.*`, `pipeline.*`,
  `resource.binding.*`, `secret.metadata.*`, `service.list`, `event.*`), while
  roles strictly gate mutations, approvals, and administration.
- Local Podman workload defaults are raised to 2 GiB memory / 2 CPUs (max
  4 GiB / 4 CPUs). Default and maximum workload limits are configurable with
  `--workload-memory`, `--workload-cpus`, `--workload-max-memory`, and
  `--workload-max-cpus` (`KODEMA_WORKLOAD_*`), and container actions accept
  bounded per-action `resources` (`memory`, `cpu`) in pipeline YAML.
- `/mcp` Streamable HTTP protocol semantics are aligned with standard MCP
  clients: `Edge.authenticate` enforces `principal.Enabled` and requires
  authentication across `initialize`, `tools/list`, and `tools/call`; server
  version reporting reflects the build version; float64 JWT `exp` claims decode
  cleanly; argument validation emits field-level diagnostics; and tool
  execution errors follow MCP tool-error conventions.

Added:

- First-class CLI commands: `kodema cancel`, `kodema pipeline list`,
  `kodema execution <list|inspect|watch|cancel>`, `kodema service`,
  `kodema secret`, and `kodema git configure`, plus `-d`/`--detach` on
  `kodema run`, pipeline-name resolution on task commands, and concise startup
  progress output on `kodema up` and `kodema run`.
- Optional bounded decision notes (`reason`/`comment`) persisted on
  `execution.request.resolve` (surfaced in `execution.inspect`, rejected
  request `failure.detail`, and `kodema approve`/`reject`/`inspect`),
  commit-boundary `on.push` service validation on `pipeline.apply` and
  `pipeline.update`, and execution provenance (`started_by`, admitted inputs,
  push trigger metadata, `created_at`) in `execution.inspect` and
  `execution.list`.
- Project-scoped workload secret materialization enabled by default in
  `kodema up` (`--allow-project-secrets`, `--allow-secret`), and
  `KODEMA_SECRET_RULES_FILE` / `--secret-rules-file` / `secret_rules_file`
  support across development, test, and production profiles.
- End-to-end `OOMKilled` diagnostics surfaced as `runtime/oom_killed` from
  Podman and Kubernetes into `execution.inspect` and `kodema run`, alongside an
  unprivileged Kaniko OCI build example (`examples/pipelines/oci-build.yaml`)
  and operator guide (`docs/runbooks/unprivileged-oci-builds.md`).

Fixed:

- Drain completed container logs into the retained sink as soon as a workload
  exits so logs are readable while downstream request actions wait for
  approval, and fix `kodema logs` action selection on failed or timed-out
  pipelines.
- Preserve `task.ask` questions and durable references across `task.reply` in
  `/kodema/task.json`, `task.read`, `task.history`, `kodema inbox`, and
  `kodema task show`, and derive version-prefixed `task_messages` row IDs so
  equal-timestamp messages keep causal order in `(at, id)` scans.
- Surface bounded container stderr and actionable `kodema logs` hints on
  `kodema run` failure, and detect service-assigned request waits.
- Fix silent auth and credential failures: tighten/validate `0700` session
  directory permissions, fall back to session API URL and default instance CA
  in `kodema git credential`, surface `--ca-file` hints and actionable x509
  errors, auto-discover running `~/.kodema/instances/default`, warn when
  environment credentials shadow a saved session, and clarify project vs tenant
  scope in `kodema whoami` and denials.
- Wire structured pre-receive rejection logging into `git serve`, validate
  push-option resource bindings and cross-repo `--resource` collisions, support
  unborn `expected_base` approvals, and improve `git provision-repository`
  output.
- Propagate `--session` / `KODEMA_SESSION` through `kodema mcp install` and
  refuse conflicting `--session` and `--credential-file` flags.
- Align CLI flag hoisting, JSON/table output, validation, help routing, MCP
  catalog schemas, and Podman/kind e2e and integration suites.

Documentation:

- Align `README.md`, `docs/runbooks/quickstart.md`,
  `docs/runbooks/podman-quickstart.md`, `docs/runbooks/release-smoke.md`, and
  `KODEMA.md` with the single-binary `kodema` CLI, project read boundary, and
  secret custody model.

## 0.4.0

Changed:

- Kodema is one binary. `kodactl` is gone: its commands are `kodema`'s, under
  the same names (`kodema run`, `kodema login`, `kodema inbox`, `kodema
  approve`, `kodema logs`, `kodema task`, `kodema pipeline`, `kodema call`,
  `kodema mcp`). Two are renamed: `kodactl status` is `kodema session`, because
  `kodema status` inspects the local installation, and `kodactl helper` is
  `kodema git credential`. A release has one archive per platform,
  `kodema_<version>_<os>_<arch>.tar.gz`. `install.sh` installs it, and still
  installs `kodactl` from a release that ships one (0.2.3 and older).
- `kodema serve` runs the control plane. `kodema server` remains accepted.
- `kodema git credential <get|store|erase>` speaks Git's credential-helper
  protocol: Git passes the request on standard input, so Git can call it
  directly. A URL argument is still accepted in place of that request.
- On Podman and Kubernetes a new container action starts in `/workspace`, not in the
  image's working directory.

Added:

- The toolbox. On Podman and Kubernetes every container action has the control
  plane's own `kodema` binary read-only at `/kodema/bin`, at the end of
  `PATH`. Inside an action it authenticates with the mounted execution
  credential and needs no flag: `kodema whoami`, `kodema call <operation>`.
- Outputs as files. An action that declares outputs has a writable
  `/kodema/outputs/<its name>`. A command writes a value to the file named for
  its key, `git rev-parse HEAD > /kodema/outputs/build/candidate`, and after
  the last command has succeeded Kodema runs `kodema output push`, which
  submits the files in one request with one trailing newline removed from each
  value. A refused submission fails the action. Submitting over HTTP from a
  command keeps working.
- Git with no credential code. With a workload Git URL (`--workload-git-url`,
  `KODEMA_WORKLOAD_GIT_URL`; `kodema up` and the Podman bundle set it) an
  action has `KODEMA_GIT_URL` and Git is configured, for that origin alone, to
  authenticate as the execution through `kodema git credential` and to trust
  the installation's CA: `git clone "$KODEMA_GIT_URL/<repository>"`. The
  helper list of that origin is reset first, so no helper of the image is
  asked for the execution credential or keeps it.
- `kodema serve --toolbox-file` (`KODEMA_TOOLBOX_FILE`) names the binary the
  test profile mounts. On Linux it defaults to the running executable; on
  another system it is required.

Documentation:

- The quick start uses `kodema` throughout and configures Git with
  `kodema git credential` as the only helper of the installation's origin. A
  system helper such as the macOS keychain otherwise keeps a Kodema token past
  its lifetime, and the first Git command after a renewal is refused once.

Known limitations:

- Production requires a digest-pinned control-plane image (`image.digest` in
  Helm). Each Job copies `kodema` from that image in an init container. The
  tenant's nodes need pull access to it as well as the action image.
- The public OAuth client ID is still `kodactl` by default. It is an
  identifier registered with identity providers, not the name of a binary.

## 0.2.3

Fixed:

- `kodema up` starts on macOS with a Podman machine. It finds the machine's
  socket (`<machine>-api.sock` in `$TMPDIR/podman`, the per-user temporary
  directory or `/tmp/podman`), and the server mounts the socket Podman reports
  for itself: a rootless machine, the default, has none at
  `/run/podman/podman.sock`.
- `kodema up` starts on a host with SELinux, such as Fedora, RHEL or a Podman
  machine. The containers that mount the state directory or the Podman socket
  (PostgreSQL, the server, Git) run with `label=disable`; PostgreSQL could not
  read its initialization script and the server could not reach Podman.
- `kodema status`, `down` and a repeated `up` use the socket the installation
  was started on, without `--socket`.
- A `kodema up` that fails stops the gateway it started, and an error about a
  server that is not ready ends with the last lines of the server's log.
- The `linux/arm64` image ran an amd64 Alpine: its `git` and shell worked only
  where the host emulates x86-64. Its base is now pinned by the digest of
  Alpine's multi-architecture index.
- The LLM proxy ended a streamed model response cleanly when the provider
  ended it after the action deadline or a cancellation, so a workload could
  take a cut-short response for a complete one. It now aborts the stream.
- `scripts/image`, and so `make image`, runs under bash 3.2, the bash of macOS.

Changed:

- `KODEMA_SOCKET` names the Podman socket of `kodema up`, `down` and `status`.
  `KODEMA_UP_SOCKET`, read by 0.2.2 and never documented, is no longer read.
- The image's build stage is `golang:1.27`, a tag. Only the static binary it
  builds reaches the image; the runtime stage stays pinned by digest.

Documentation:

- The quick start covers macOS with a Podman machine, pulls and runs
  `alpine:3.24`, and says that `kodactl run` exits 1 when it stops for an
  approval.

Known limitations:

- On macOS, `kodema up` has run on a rootless Podman machine of Podman 6.2 on
  Apple silicon. A rootful machine has not been run.
- On Linux with SELinux, no complete run has been made. Workloads keep their
  SELinux label, so files of the state directory mounted into them may be
  refused.
- `kodactl run` waits without end when the server cannot reach Podman.

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
