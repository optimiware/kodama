# Unified test suite

`scripts/test-suite` runs Kodama's real-infrastructure suites as waves of
concurrent lanes. One orchestrator (`test/suite`) plans every target:

| Target | What runs |
|---|---|
| `local-all` | Every `make suites-stack` and `make suites-kind` row and the local restore drills, all at once |
| `podman` | The portable e2e proofs on a Podman installation, and the local restore drills |
| `kind` | The kind lifecycle and component suites, or the portable e2e proofs on a described Kubernetes installation |
| `stage` | The stage qualification (Q01, Q02 and Q03) in one session |

The sequential `make suites-stack` and `make suites-kind` targets stay as they
are; `local-all` runs the same packages with the same tags
(`TestLocalAllCoversTheMakefileSuites` keeps the two in step).

## Commands

```sh
make suites-parallel                                   # local-all
make suite-kind                                        # kind suites
make suite-podman INSTALLATION=/abs/installation.json  # an existing Podman installation
make suite-stage                                       # stage, see below

make suites-parallel KEEP_GOING=1
make suite-stage SUITE_FLAGS='--skip-chaos --q03-mode=restore'
scripts/test-suite --target=local-all --dry-run        # print the plan, run nothing
```

Flags (`scripts/test-suite --help` lists them):

| Flag | Meaning |
|---|---|
| `--target` | `local-all` (default), `podman`, `kind` or `stage` |
| `--installation` | Descriptor of an existing installation (default `$Q01_INSTALLATION_FILE`) |
| `--evidence-dir` | Absolute evidence root (default `/tmp/oc/evidence/suite-<target>-<UTC time>`) |
| `--concurrency` | Scenario concurrency of every lane; `1` runs scenarios one at a time |
| `--keep-going` | Start the remaining waves after a failed one |
| `--skip-q03` | Leave out the restore drills |
| `--skip-chaos` | Stage: leave out the disruptive second wave |
| `--q03-mode` | Stage: `plan` (default), `restore` or `drop` for `scripts/q03-stage-drill` |
| `--race=false` | Run `go test` without the race detector |
| `--token-refresh` | Stage: refresh the persona token files this often; `scripts/test-suite` passes `5m` |
| `--dry-run` | Print waves, lanes, commands and settings; run nothing |

The evidence root receives one directory per lane, `logs/<lane>.log` with each
lane's complete output, and `summary.json` with the verdict and duration of
every lane. The terminal shows the same output line by line, prefixed with the
lane name.

## Waves and lanes

The lanes of a wave run concurrently. The next wave starts only after every
lane of the previous one finished, and only if they all passed (unless
`--keep-going`). Only the stage target has a second wave.

Inside a lane, scenarios run in parallel up to a per-package bound. Each bound
is an environment variable; the orchestrator sets it from `--concurrency`, and
a plain `go test` run reads it too:

| Variable | Default | Package |
|---|---|---|
| `KODAMA_E2E_CONCURRENCY` | 6 | `test/e2e` live scenarios |
| `KODAMA_STAGE_CONCURRENCY` | 1 (the suite sets 6) | `test/qualification/production` wave 1 probes |
| `KODAMA_Q03_CONCURRENCY` | 4 | `test/qualification/restore` local drills |
| `KODAMA_LIFECYCLE_KIND_CONCURRENCY` | 3 | `test/integration/lifecycle` kind clusters |

`0` or `1` runs the package sequentially. The orchestrator always sets the four
variables, and its Podman e2e lane uses 8 unless `--concurrency` says
otherwise. In a plain `go test` run the restore and lifecycle defaults drop to
`KODAMA_STACK_SLOTS` and `KODAMA_KIND_SLOTS` when a host sets those lower. A scenario takes its slot before it starts its clock, its deadline or
reads a credential, so time spent queued is never measured.

### Portable e2e proofs

`test/e2e` reads only an installation descriptor, so the same proofs run on
`kodama server --profile test` under Podman and on a Kubernetes installation.
The suite selects `^TestE2E_(AC|Parity$|InstallationAccess$)`: the 24 tests of
`suite.PortableE2ETests`. `TestReleaseSmoke` is left out because the AC04, AC05,
AC14, AC17 and AC18 tests run its five scenarios.

Rules a new e2e test must follow (`TestPortableE2ESelectionMatchesThePackage`
enforces them):

- Start with `e2eParallel(t)`. Assert only on objects the test created; other
  scenarios admit executions and publish events in the same project meanwhile.
  To prove a refused request admitted nothing, use
  `smokeRequireNoExecutionWithActions`.
- A test that must assert on project-wide state omits `e2eParallel` and is
  listed in `suite.SerialE2ETests`. Go runs such tests alone, before any
  parallel test starts. `TestE2E_Parity` is the only one: it compares the page
  after the project's event tail across the REST, MCP and CLI edges.
- A test that advances a protected ref gets its repository from
  `requireProtectedRepository`. Scenarios spread over the installation's
  ordinary repositories by slot and hold the repository's protected ref for
  the test. With one repository they take turns; with three (the disposable
  Podman installation of this suite) they run side by side.

### Podman

With `--installation` the proofs run against that installation; export the
four persona token files and `Q01_NATS_URL` yourself, because that lane runs
`go test` directly. Without it,
the lane installs a disposable one through `deploy/podman/smoke.sh`, runs the
proofs through the verifying wrapper (a removed or skipped proof fails the
run) and purges it. That mode needs the complete port set of the disposable
installation:

```sh
KODAMA_LOCAL_HTTPS_PORT=26201 KODAMA_LOCAL_OIDC_PORT=26202 \
KODAMA_LOCAL_GIT_PORT=26203 KODAMA_LOCAL_GIT_HTTPS_PORT=26204 \
KODAMA_LOCAL_POSTGRES_PORT=26205 KODAMA_LOCAL_NATS_PORT=26206 \
KODAMA_LOCAL_NATS_MONITOR_PORT=26207 KODAMA_LOCAL_OAUTH_UPSTREAM_PORT=26208 \
make suite-podman
```

`smoke.sh` reports this selection as narrowed: it is not the release smoke
selection of [release-smoke.md](release-smoke.md). The lane's evidence files
record no revision; note the checkout revision with the run.

### Local matrix

`local-all` splits `test/integration/lifecycle` by runtime: the Podman tests
run in a stack slot (`-skip ^TestKubernetes`) and the kind tests in a kind slot
(`-run ^TestKubernetes`). Every test that starts a kind cluster is therefore
named `TestKubernetes*` and starts with `acquireLifecycleKindSlot(t)`
(`TestLifecycleKindTestsFollowTheLaneSplit`).

Every lane takes its own slot through `scripts/infra-slot.sh`, so the host's
`KODAMA_STACK_SLOTS`, `KODAMA_KIND_SLOTS` and load bound still apply.

#### Kernel key quota

Every container takes one kernel session keyring, charged to the user that
runs it. The containers nested in a rootless kind node are charged to the same
host user, so one small kind cluster already holds about 30 keys. The kernel
default for a non-root user is 200 (`kernel.keys.maxkeys`), and the parallel
lanes of `local-all` reach it.

At the quota the OCI runtime refuses the start (`crun: create keyring ...: Disk
quota exceeded`). Podman keeps the container in `created`, the Podman adapter
reports the start outcome as unknown, and the execution stays `running` until
its action deadline. A test then fails on a two-minute wait whose runtime
evidence shows `launch=unknown` and a `created` container carrying that error.

Raise the quota once per host before running lanes in parallel:

```sh
sudo sysctl -w kernel.keys.maxkeys=2000 kernel.keys.maxbytes=200000
printf 'kernel.keys.maxkeys = 2000\nkernel.keys.maxbytes = 200000\n' | sudo tee /etc/sysctl.d/60-kodama-keys.conf
```

`scripts/infra-slot.sh` prints one warning per lane while the quota is below
`KODAMA_KEYS_MIN` (default 2000). `/proc/key-users` shows the current use.
Without root, lower the pressure instead: `KODAMA_KIND_SLOTS=1` and
`KODAMA_LIFECYCLE_KIND_CONCURRENCY=1`.

## Stage

`scripts/test-suite --target=stage` is one stage session:

1. It holds the host-wide stage lock (`KODAMA_STAGE_LOCK`, default `scw-stage`)
   for the whole run.
2. `test/e2e/internal/harness/run-stage.sh` opens the port-forwards, acquires
   the four persona token files, describes the installation, and removes all of
   it on exit. Its inputs apply unchanged; without `--installation` or
   `Q01_INSTALLATION_FILE` the descriptor is generated (`Q01_STAGE_DESCRIPTOR=1`).
3. The orchestrator runs the two waves and refreshes the token files every five
   minutes (`--token-refresh`). Every test reads its token file when it starts,
   so no test starts with a token that is about to expire.

| Wave | Lane | Content |
|---|---|---|
| 1 | `stage-e2e` | The portable e2e proofs |
| 1 | `stage-probes` | The 13 `suite.StageWave1Probes`: authority, isolation and the resilience probes that touch only their own workload |
| 1 | `stage-rdb-drill` | `scripts/q03-stage-drill <mode>` |
| 2 | `stage-chaos` | The 5 `suite.StageWave2ChaosProbes`, one at a time (`-parallel 1`) |

The chaos probes delete or scale the controller, the broker and the Git
service. They run alone, after wave 1 passed, so no other scenario observes the
outage and a stage that already failed is not disrupted further. Every
`TestStage*` probe is in exactly one list of `test/suite/selection.go`
(`TestHarnessStageWavesClassifyEveryLiveProbe`); a new probe needs that
decision before it can run.

The RDB drill restores into `kodama-restore-<run-id>` through the Scaleway
control plane and never touches the cluster, so it takes its own lock
(`KODAMA_Q03_STAGE_LOCK`, default `scw-stage-rdb`) instead of the stage lock.
A standalone `scripts/q03-stage-drill` therefore no longer waits for a cluster
run, and the stage session runs it beside the cluster lanes.

**`restore` mode fails on Scaleway (DEF-12).** The drill confirms the restored
database over SQL from the operator host
(`KODAMA_Q03_RESTORE_DATABASE_URL`). The stage RDB has a private endpoint
only, and a database created by `scw rdb backup restore` gives the bootstrap
admin no right to connect. The lane therefore ends with `restore accepted ...
and not confirmed` and the stage target reports a failed lane. Verify the
restored content from a pod, then run `scripts/q03-stage-drill drop` with the same run ID.

Inputs:

- `run-stage.sh`'s: `Q01_KUBE_CONTEXT`, `Q01_CA_PEM`, `Q01_PROJECT_ID`, and for
  a generated descriptor `Q01_DEPLOYED_REVISION`, `Q01_SERVER_IMAGE_DIGEST`,
  `Q01_OAUTHSONAS_AUDIENCE`. A qualification run needs `Q01_SMOKE_GIT=1` with
  `Q01_SMOKE_REPOSITORY` and `Q01_SMOKE_RESOURCE_ID`: without it the Git proofs
  are skipped, or the preflight fails when the descriptor is generated. The
  live subscription proofs need a port-forward to `svc/nats`, `Q01_NATS_URL`,
  `Q01_NATS_TLS_SERVER_NAME` and the client pair from `Secret kodama-broker`
  as `Q01_NATS_CLIENT_CERT_FILE` and `Q01_NATS_CLIENT_KEY_FILE`, as that
  script documents.
- The production probes read the same descriptor: API, CA, project, tenant,
  namespace (`kodama-tenant-<tenant>`), Git service (`kodama-git-<tenant>`),
  image and repository are derived from it, and the admin credential is the
  session's token file (`Q02_ADMIN_TOKEN_FILE`). Any `Q02_*` input you export
  wins, for example a `Q02_NETWORK_IMAGE` for the network isolation probe. The second tenant is never derived: export `Q02_SECOND_TENANT_ID`,
  `Q02_SECOND_PROJECT_ID`, `Q02_SECOND_TENANT_NAMESPACE`,
  `Q02_GIT_REPOSITORY_B`, `Q02_GIT_SERVICE_B` and
  `Q02_SECOND_ADMIN_PERSONA`; the session acquires that persona's token into a
  private file of its own.
- The probes run with `Q02_REQUIRE_STAGE=1` unless you export another value: a
  probe with a missing input fails instead of skipping. The workspace-loss
  probe deletes a tenant claim and therefore needs
  `Q02_WORKSPACE_LOSS_NAMESPACE` and `Q02_WORKSPACE_LOSS_CONFIRM=destroy-claim`.
- The drill's `KODAMA_Q03_STAGE_*` inputs (see `scripts/q03-stage-drill`).
  `restore` and `drop` also need `KODAMA_Q03_STAGE_APPLY=1`.

A missing descriptor, cluster context or drill input is an error. Leave a part
out with `--skip-q03` or `--skip-chaos`. The lanes are plain `go test -v` runs,
so a test that skips does not fail its lane: check the lane log for `--- SKIP`
before counting a run as complete.

Not covered by `scripts/test-suite --target=stage`: the pre-gate reset and
bootstrap, the backup
that the `restore` drill needs (`KODAMA_Q03_STAGE_BACKUP_ID`), the check of
the restored database, the `drop` that follows it, and the `TestReleaseSmoke`
run ([release-smoke.md](release-smoke.md)).

`scripts/test-suite --target=stage --dry-run` prints the plan for an existing
descriptor without taking the lock or opening a session.

The former entry points still work: `run-stage.sh` of `test/e2e` and of
`test/qualification/production`, each under
`scripts/infra-slot.sh --exclusive scw-stage`. A whole-package run of the
production probes stays sequential, in its previous order.

Related: [release-smoke.md](release-smoke.md),
[production-deployment.md](production-deployment.md).
