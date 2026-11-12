# Release smoke: any documented Kodama installation

This runbook describes the installation-neutral release smoke harness in
`test/e2e/`. It targets the disposable Podman installation of
`deploy/podman/smoke.sh` and the Scaleway stage, and it can connect to any
installation whose operator supplies an installation descriptor.

It documents the **connector** and the five release scenarios
(`TestReleaseSmoke/...`): the request wait, the operator retry, the
conversation handoff, the protected task and the human push. The scenarios run
through the public API and the served Git service only. The local bundle
installs the four OAuthSonas personas and the workload-reachable Git HTTPS
front end they need.

- Product behavior: [KODAMA.md](../../KODAMA.md)
- Local Podman install: [podman-quickstart.md](podman-quickstart.md)
- All suites and the full 24-test portable selection:
  [unified-test-suite.md](unified-test-suite.md)

## What runs

| Test | Kind | Needs |
|---|---|---|
| `TestE2E_AC01InstalledPipeline`, `AC02`, `AC09`, `AC11`, `AC12`, `AC13`, `AC16` | the seven Git-free live acceptance tests of the smoke selection | API URL, CA, project, one basic-test credential |
| `TestE2E_InstallationAccess` | explicit preflight, selected by name | the full descriptor: four personas, Git endpoint and repository, both image pins |
| `TestReleaseSmoke/RequestWait` | S1: planned request, same execution/workspace | |
| `TestReleaseSmoke/OperatorRetry` | S4: operator-only retry of a failed task | |
| `TestReleaseSmoke/ConversationHandoff` | S3: ask/reply handoff into a fresh execution | |
| `TestReleaseSmoke/ProtectedTask` | S2: external harness, test, human review and protected integration in one execution | the served Git endpoints, the run repository, both image pins |
| `TestReleaseSmoke/HumanPush` | S5: a human push starts a subscribed pipeline | the served Git endpoints, the run repository, the Admin+engineer persona |

The default selection of `deploy/podman/smoke.sh` is these seven tests plus
the five scenarios: 12 selected tests. `TestE2E_InstallationAccess` is not part
of that selection; name it explicitly. The full portable selection is the 24
tests of `suite.PortableE2ETests`, which includes the preflight
([unified-test-suite.md](unified-test-suite.md)). The five smoke scenarios are
parallel subtests of `TestReleaseSmoke` (`KODAMA_E2E_CONCURRENCY=1` runs them
one at a time); each has its own bounded context and public cleanup. In
required installation mode (`Q01_REQUIRE_INSTALLATION=1`) every missing
preflight input is a failure, never a skip.

## Installation descriptor

`Q01_INSTALLATION_FILE` points at JSON schema version 1. The local bundle writes
it after a successful install; another operator writes the equivalent
document. The file is deployment configuration, not a public API, and must be a
private file (0600) that carries no secret value.

```json
{
  "schema_version": 1,
  "installation_id": "installation-w49-local",
  "profile": "test",
  "runtime": "podman",
  "revision": "FULL_SOURCE_SHA",
  "server_image_digest": "sha256:ACTUAL_IMAGE_DIGEST",
  "tenant_id": "tenant-local",
  "project_id": "tenant-local",
  "api": {
    "url": "https://localhost:18443",
    "ca_file": "/absolute/state/tls/ca.crt",
    "tls_server_name": "",
    "workload_url": "https://host.containers.internal:18443",
    "workload_ca_file": "/absolute/state/tls/ca.crt"
  },
  "images": {
    "tools": "docker.io/library/buildpack-deps@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c",
    "external_harness": "docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b"
  },
  "oauthsonas": {
    "issuer": "https://localhost:18444",
    "audience": "https://localhost:18443",
    "ca_file": "/absolute/state/tls/ca.crt",
    "client_id": "kodactl",
    "redirect_uri": "http://127.0.0.1:52790/callback",
    "resolve": "",
    "access_code_file": "/absolute/state/secrets/oauth-access-code",
    "personas": {
      "admin": {"id": "admin", "subject": "admin"},
      "engineer": {"id": "engineer", "subject": "engineer"},
      "reviewer": {"id": "reviewer", "subject": "reviewer"},
      "operator": {"id": "operator", "subject": "operator"}
    }
  },
  "credentials": {
    "admin_file": "/absolute/state/credentials/admin-token",
    "engineer_file": "",
    "reviewer_file": "",
    "operator_file": ""
  },
  "git": {
    "url": "https://localhost:18445",
    "workload_url": "https://host.containers.internal:18445",
    "ca_file": "/absolute/state/tls/ca.crt",
    "workload_ca_file": "/absolute/state/tls/ca.crt",
    "service_id": "git",
    "service_token_file": "/absolute/state/git-credential/token",
    "admin_binding_id": "git-admin",
    "admin_resource_id": "git-admin",
    "repositories": [
      {"name": "admin", "resource_id": "git-admin", "binding_id": "git-admin"}
    ]
  },
  "smoke_repository": {"name": "smoke/RUN", "resource_id": "w49-repo-RUN", "binding_id": "w49-bind-RUN"}
}
```

Notes:

- Unknown members are refused, so a typo cannot be silently ignored.
  `schema_version` must be exactly `1`.
- `runtime` and `profile` are evidence labels, never branches in an assertion.
- `api.url`, `git.url` and the OAuthSonas URLs are verified HTTPS origins.
  Without `api.ca_file` the system roots are used; a private installation names
  its CA file. `api.tls_server_name` is empty for a direct URL and is set only
  when a port-forward's loopback host differs from the served name.
- `images.tools` and `images.external_harness` must stay digest-pinned for a
  release run; a release invocation without a digest is a hard failure. The
  admission resolver refuses a reference carrying both a tag and a digest, so
  the harness accepts an installation's `repository:tag@sha256:<64 hex>` pin and
  normalizes it to the equivalent `repository@sha256:<64 hex>` for the action;
  the digest remains the immutable identity.
- `credentials.*_file` name private token files. When they are empty the common
  runner acquires the persona token from OAuthSonas into its own 0600 file.
- `oauthsonas.personas.*.principal_id` is optional in the file: the harness
  derives `oidc-` plus the first 32 lowercase hex characters of
  `SHA-256(issuer + NUL + subject)` and refuses a supplied value that does not
  match.
- `smoke_repository` is added by the setup wrapper after provisioning; without
  it the first `git.repositories` entry is used.
- Never put an executable command, a CA private key, a raw token, a database
  URL, a Podman socket path or a Kubernetes credential in the descriptor.

## Environment variables

Explicit environment values override the descriptor. `Q01_TOKEN` remains the
legacy single basic-test credential; it is never silently used for all four
smoke personas. A raw token value and a token file for the same persona are
ambiguous and refused.

| Variable | Meaning |
|---|---|
| `Q01_INSTALLATION_FILE` | schema version 1 descriptor path |
| `Q01_API_URL` | public API base URL (https) |
| `Q01_CA_PEM` | installation CA file; omitted means system roots for the seven tests. The installation preflight materializes the CA into its workload, so it requires a configured CA file. |
| `Q01_TLS_SERVER_NAME` | explicit verified TLS name; omitted means the URL host |
| `Q01_PROJECT_ID` | project the pipelines run in |
| `Q01_IMAGE` | tools image override; must stay digest-pinned for release evidence |
| `Q01_EXTERNAL_HARNESS_IMAGE` | small external-harness image override |
| `Q01_WORKLOAD_CA_FILE` | CA file the workload actions declare |
| `Q01_TOKEN` | legacy single basic-test token |
| `Q01_ADMIN_TOKEN_FILE`, `Q01_ENGINEER_TOKEN_FILE`, `Q01_REVIEWER_TOKEN_FILE`, `Q01_OPERATOR_TOKEN_FILE` | private persona credential files |
| `Q01_NATS_URL` | live subscription broker endpoint (`nats://` or `tls://`); absent means those probes skip outside a required run |
| `Q01_NATS_CA_FILE` | broker CA PEM bundle for a `tls://` URL or an explicit TLS selection |
| `Q01_NATS_CLIENT_CERT_FILE`, `Q01_NATS_CLIENT_KEY_FILE` | client certificate pair for a broker that verifies client certificates (the stage broker; Secret `kodama-broker`); supplied together or not at all |
| `Q01_NATS_TOKEN_FILE` | a private 0600 file with a store-backed service credential. The file is validated, but the live subscription tests always connect with the run-unique service credential they mint. A human OIDC token is refused by the callout because it has no store-backed expiry |
| `Q01_SMOKE_GIT` | stage adapter: `1` runs the Git tests and scenarios, `0` (the adapter's default) leaves them out |
| `Q01_NATS_TLS_SERVER_NAME` | explicit verified broker TLS name; omitted means the URL host |
| `Q01_EVIDENCE_DIR` | absolute evidence directory (0600 files, 0700 directory) |
| `Q01_RUN_SECONDS` | expected selected-run duration used as the token-lifetime floor (default 600) |
| `Q01_TOKEN_MIN_LIFETIME` | explicit acquired-token lifetime floor; overrides `Q01_RUN_SECONDS` |
| `Q01_HARNESS_REVISION`, `Q01_DEPLOYED_REVISION` | evidence revisions |
| `Q01_REQUIRE_STAGE`, `Q02_REQUIRE_STAGE` | legacy: an unconfigured live test fails |
| `Q01_REQUIRE_INSTALLATION` | `1`: a live invocation fails instead of skipping; the installation preflight also fails on any missing persona credential, image pin, Git endpoint or repository |
| `Q01_KUBE_CONTEXT` | stage adapter only; never a prerequisite of the generic runner |
| `KODAMA_SMOKE_TEST_RUN`, `KODAMA_SMOKE_EXPECT` | `deploy/podman/smoke.sh` diagnostics only: a narrowed `-run` selection and its matching space-separated expected-name list; both must be narrowed together |

`Q01_KUBE_CONTEXT` and the port-forward/Secret logic belong to
`test/e2e/internal/harness/run-stage.sh` (the Scaleway adapter). The generic
runner never calls kubectl or Podman.

## Common runner

```sh
test/e2e/internal/harness/run-installation.sh -- <command> [args...]
```

It connects to the supplied installation, acquires every requested persona
token under `umask 077` directly into a private file, exports only the file
paths, runs the command in the foreground, joins its child, and removes only the
files it created. It never installs, provisions through the filesystem, calls
Podman or kubectl, or destroys the installation.

Persona acquisition comes from `Q01_ACQUIRE_PERSONAS` when set; otherwise from
the descriptor's `oauthsonas.personas` keys. Tokens are acquired immediately
before the command, and an acquired token that is already expired or expires
inside `Q01_TOKEN_MIN_LIFETIME` is reported distinctly. The default is the
run's expected duration, `Q01_RUN_SECONDS` (600 seconds); override either
variable for a longer run. The measured local OAuthSonas administrator token
lifetime on 2026-11-08 was about **899 seconds (15 minutes)**. A run longer than
the token lifetime needs a refresh/re-acquisition; a ~20-minute smoke run
cannot rely on one admin token. `scripts/test-suite --target=stage` refreshes
the persona token files every five minutes for that reason; this runner does
not. The picker access code is read from the descriptor's private file; it is
not printed.

## Preflight

`TestE2E_InstallationAccess` proves, before a long scenario run:

1. the four personas carry four distinct derived principal ids in the
   descriptor and each has its real project membership: role-appropriate public
   operations succeed (admin, engineer and operator read `execution.list`, the
   reviewer reads its `task.inbox`). The installed server is project-scoped, so
   the preflight never calls the tenant-scoped `membership.list`; the committed
   grants are verified at install time on the tenant-wide setup server;
2. a role-absent operation is refused with the public `forbidden` code: an
   engineer cannot read `resource.binding.read`, a reviewer cannot read
   `execution.list` and an operator cannot read `pipeline.read`;
3. the host reads the served Git repository over authenticated HTTPS;
4. one tools-image workload materializes the installation's **public** CA
   certificate at `/workspace/w49-ca.crt` and proves verified HTTPS access to
   the API (`curl` with that CA and the mounted execution token) and to the
   workload Git URL (`git ls-remote` with `http.sslCAInfo` and a
   token-mounted `GIT_ASKPASS`). The execution token is re-read from
   `/kodama/secrets/KODAMA_TOKEN` for each call.

   The certificate is public data, not a credential. The `test` profile builds
   the workload secret-name policy from `SecretRules`, which is deny-by-default
   and has no configuration entry today; a declared secret name is refused at
   activation with `forbidden: secret name is not authorized`. Materializing the
   public bytes keeps TLS verification on and needs no product change. Automatic
   or mounted-secret CA projection remains a P15 follow-up; when an installation
   exposes a secret-name allow-list, the same helper can switch to a declared
   secret without weakening this proof.

The preflight fails, not skips, when the installation is required and an input
is missing. It needs the four persona memberships and a workload-reachable Git
HTTPS endpoint; `deploy/podman/install.sh` provides both.

The disposable local wrapper runs the same preflight against its own
installation (complete port set required, one slot). Its identity is
invocation-owned: it never inherits `KODAMA_LOCAL_PREFIX`,
`KODAMA_LOCAL_STATE_DIR` or `KODAMA_LOCAL_GIT_RESOURCES`, and only
`KODAMA_SMOKE_PREFIX`/`KODAMA_SMOKE_STATE_DIR` override its run-unique prefix
and state. The example below uses ports that do not collide with the install
defaults:

```sh
scripts/infra-slot.sh stack -- \
  env KODAMA_SMOKE_PREFIX=w49arpre \
      KODAMA_SMOKE_STATE_DIR=/tmp/oc/w49ar-pre-state \
      KODAMA_SMOKE_RUN=w49arpre1 \
      KODAMA_LOCAL_HTTPS_PORT=22443 KODAMA_LOCAL_OIDC_PORT=22444 \
      KODAMA_LOCAL_GIT_PORT=22081 KODAMA_LOCAL_GIT_HTTPS_PORT=22445 \
      KODAMA_LOCAL_POSTGRES_PORT=19432 KODAMA_LOCAL_NATS_PORT=20222 \
      KODAMA_LOCAL_NATS_MONITOR_PORT=22222 \
      KODAMA_LOCAL_OAUTH_UPSTREAM_PORT=22181 \
      Q01_EVIDENCE_DIR=/tmp/oc/evidence/w49ar-pre \
  deploy/podman/smoke.sh --preflight
```

`smoke.sh` supplies the descriptor and acquisition settings to the runner in
the `-- env VAR=value ...` form; the runner applies those assignments to itself
and then runs the command, so descriptor validation and persona acquisition
always happen. The documented form (`env ... run-installation.sh -- ...`) is
equivalent.

## Commands

Hermetic helpers (no installation, no network):

```sh
make test-e2e-hermetic
bash -n test/e2e/internal/harness/*.sh
```

The seven Git-free live tests alone (use `verify-go-test.sh` so a zero-test
selection cannot pass):

```sh
scripts/infra-slot.sh stack -- \
  env KODAMA_REQUIRE_INFRA=1 Q01_REQUIRE_INSTALLATION=1 \
      Q01_INSTALLATION_FILE=/absolute/prepared-installation.json \
      Q01_EVIDENCE_DIR=/tmp/oc/evidence/RELEASE-RUN \
  test/e2e/internal/harness/run-installation.sh -- \
  test/e2e/internal/harness/verify-go-test.sh \
    --expect TestE2E_AC01InstalledPipeline \
    --expect TestE2E_AC02ImmutableAdmission \
    --expect TestE2E_AC09CleanupAttributable \
    --expect TestE2E_AC11Materialization \
    --expect TestE2E_AC12Outputs \
    --expect TestE2E_AC13OrderedActions \
    --expect TestE2E_AC16PlannedWait \
    -- go test -race -tags=e2e -count=1 -timeout=30m -json \
      -run '^TestE2E_AC(01|02|09|11|12|13|16)' ./test/e2e/
```

The complete release selection (seven live tests and all five smoke scenarios)
against a prepared descriptor. This is the exact default selection
`deploy/podman/smoke.sh` runs; the verifying wrapper fails on zero executed
tests, on a missing expected name (top-level test or subtest), on a skipped
selected test and on a failed selected test:

```sh
scripts/infra-slot.sh stack -- \
  env KODAMA_REQUIRE_INFRA=1 Q01_REQUIRE_INSTALLATION=1 \
      Q01_INSTALLATION_FILE=/absolute/prepared-installation.json \
      Q01_EVIDENCE_DIR=/tmp/oc/evidence/RELEASE-RUN \
  test/e2e/internal/harness/run-installation.sh -- \
  test/e2e/internal/harness/verify-go-test.sh \
    --expect TestE2E_AC01InstalledPipeline \
    --expect TestE2E_AC02ImmutableAdmission \
    --expect TestE2E_AC09CleanupAttributable \
    --expect TestE2E_AC11Materialization \
    --expect TestE2E_AC12Outputs \
    --expect TestE2E_AC13OrderedActions \
    --expect TestE2E_AC16PlannedWait \
    --expect TestReleaseSmoke/RequestWait \
    --expect TestReleaseSmoke/OperatorRetry \
    --expect TestReleaseSmoke/ConversationHandoff \
    --expect TestReleaseSmoke/ProtectedTask \
    --expect TestReleaseSmoke/HumanPush \
    -- go test -race -tags=e2e -count=1 -timeout=30m -json \
      -run '^(TestE2E_AC(01|02|09|11|12|13|16)|TestReleaseSmoke)' ./test/e2e/
```

Expected duration: on a local Podman installation the 12 selected tests finish
well inside the ~899 s token lifetime, so the persona tokens are acquired
once. Allow 10–20 minutes for install,
selected tests and teardown; the wrapper's outer bound is 30 minutes and the
Go run's is 30 minutes. On the Scaleway stage the five scenarios took 91 s.

A focused diagnostic run narrows both the `-run` selection and the expected
list, so a partial run never claims the full selection. A narrowed run prints
`kodama-smoke: NARROWED SELECTION - NOT RELEASE EVIDENCE` to stderr at the
start and end and writes `selection=narrowed` to `summary.txt`; a full run
writes `selection=full` and prints `full selection verified: N tests`:

```sh
scripts/infra-slot.sh stack -- \
  env KODAMA_SMOKE_RUN=diag1 KODAMA_SMOKE_TEST_RUN='^TestReleaseSmoke$/^ProtectedTask$' \
    KODAMA_SMOKE_EXPECT='TestReleaseSmoke/ProtectedTask' \
    KODAMA_LOCAL_HTTPS_PORT=22443 KODAMA_LOCAL_OIDC_PORT=22444 \
    KODAMA_LOCAL_GIT_PORT=22081 KODAMA_LOCAL_GIT_HTTPS_PORT=22445 \
    KODAMA_LOCAL_POSTGRES_PORT=19432 KODAMA_LOCAL_NATS_PORT=20222 \
    KODAMA_LOCAL_NATS_MONITOR_PORT=22222 \
    KODAMA_LOCAL_OAUTH_UPSTREAM_PORT=22181 \
  deploy/podman/smoke.sh
```

`deploy/podman/smoke.sh` runs the verifier by default; a bare `go test` is not
the documented gate. `KODAMA_SMOKE_EXPECT` is a space-separated expected-name
list.

The preflight:

```sh
scripts/infra-slot.sh stack -- \
  env KODAMA_REQUIRE_INFRA=1 Q01_REQUIRE_INSTALLATION=1 \
      Q01_INSTALLATION_FILE=/absolute/prepared-installation.json \
      Q01_EVIDENCE_DIR=/tmp/oc/evidence/RELEASE-RUN \
  test/e2e/internal/harness/run-installation.sh -- \
  test/e2e/internal/harness/verify-go-test.sh \
    --expect TestE2E_InstallationAccess \
    -- go test -race -tags=e2e -count=1 -timeout=30m -json \
      -run '^TestE2E_InstallationAccess$' ./test/e2e/
```

Do not set `TMPDIR`; the slot launcher supplies its own. The `-run` pattern is
unanchored at the end on purpose: the test names carry suffixes
(`TestE2E_AC01InstalledPipeline`, …), so an anchored `…16)$` would select
nothing and `go test` would still exit 0 with `PASS`. `verify-go-test.sh` reads
the `-json` stream and fails on zero executed tests, on a missing expected test,
on any skipped selected test and on any failed selected test. `go test` exit
zero alone is never release evidence.

On the Scaleway stage, run the scenarios through the stage adapter inside the
stage lock. The adapter keeps its namespace, context and exclusivity guards
and forces required mode. This is the command of the G03 stage run:

```sh
Q01_STAGE_DESCRIPTOR=1 Q01_SMOKE_GIT=1 \
  scripts/infra-slot.sh --exclusive scw-stage stack -- \
  test/e2e/internal/harness/run-stage.sh \
  test/e2e/internal/harness/verify-go-test.sh \
    --expect TestReleaseSmoke \
    -- go test -v -race -tags=e2e -count=1 -timeout=20m -json \
      -run '^TestReleaseSmoke$' ./test/e2e/
```

It needs the adapter's inputs (`Q01_KUBE_CONTEXT`, `Q01_CA_PEM`,
`Q01_PROJECT_ID`, and for the generated descriptor `Q01_DEPLOYED_REVISION`,
`Q01_SERVER_IMAGE_DIGEST`, `Q01_OAUTHSONAS_AUDIENCE`, `Q01_SMOKE_REPOSITORY`
and `Q01_SMOKE_RESOURCE_ID`). The whole stage qualification, including the 24
portable tests and the live subscription inputs, is
`scripts/test-suite --target=stage`
([unified-test-suite.md](unified-test-suite.md)).

The adapter and the harness default to the digest-pinned
`docker.io/library/buildpack-deps@sha256:712f969c…` tools image. A `Q01_IMAGE`
override must be digest-pinned and contain `python3`, `git` and `curl`: the
output-submission helper is a Python standard-library script.

## The Git scenarios (S2 and S5)

Both scenarios act only through the public API and the served Git service. They
read the committed policy, seed commits, push, read events and inspect
executions; they never query the database or journal, never write bare
repository state, never use a factory harness and never disable TLS.

**S2 `TestReleaseSmoke/ProtectedTask`.** The subtest first prepares the
run-unique repository (`Installation.SmokeRepository`): the host administrator
clones the served `admin` repository with their own token and asserts the
committed `conf/git.conf` carries W32's default block (`R = @all`, `RW+` work
refs for `@engineer` and `@execution`, `RW` main for `@execution`,
`option approve.refs/heads/main = reviewer`). The host engineer then creates
commit B in a fresh private HOME and pushes B to a run-unique
`refs/heads/work/...` through the served Git URL. Main is created **only**
through a task-free request→integration pipeline: candidate B with the empty
expected base of an absent ref, a `role/reviewer` pass and a workload push with
`--force-with-lease=refs/heads/main:`. The `pre-receive` hook normalises the zero
object id of a created ref to the empty expected base; the approval subject
uses that representation.

The protected task then runs one task, one execution and one workspace through
five actions:

1. `prepare` (tools image) clones B through the workload Git HTTPS URL with the
   mounted token and the CA materialised in `/workspace`, checks the served main
   is B, and writes the execution/run markers.
2. `implement` (the pinned Alpine external-harness image) consumes the markers
   and `/kodama/task.json` and writes a deterministic program and its test; it
   uses no Git and no network. This is a functional fixture, not a model or
   native-agent claim.
3. `test` (tools image) commits C, pushes C to the granted work ref (the
   authentication/reachability positive control), runs the committed test, then
   attempts C→main **before approval** and requires the hook's public
   `kodama git: update rejected` refusal with main still B. It then submits
   `candidate=C` and `test_result=pass:C` through the Python output helper
   (with the test-computed candidate substituted into the generated helper).
4. `review` requests `role/reviewer` with the immutable subject
   `{operation: git.ref.update, resource, ref: main, expected_base: B,
   candidate: C}`. The reviewer discovers it in its inbox and resolves
   action-only; the engineer's direct main push and the engineer's resolve
   attempt are both refused with main and the request unchanged.
5. `integrate` (tools image) requires the same markers, the retained checkout
   at C, B as C's parent, the settled `pass:C`, remote main still B, and pushes
   `C:refs/heads/main` with `--force-with-lease=refs/heads/main:B` using the
   current mounted execution token.

The test then requires five succeeded actions, complete cleanup, exactly one
task execution-history entry, one occurrence of each action's marker, a host
fetch of main at C with parent B, and the genuine
`resource.verified`/`git.ref.updated` report found by paging `event.read`: the
Git service as source, the execution as correlation, the exact
resource/ref/base/candidate payload and a non-empty verification id as
causation. No report is synthesised and the database is never read.

**Repository/resource input distinction.** `inputs.repository` is the
registered Shell **resource ID** used in approval subjects and verified reports;
`inputs.repository_path` is the served Git **repository path** used to build Git
URLs. Keep the two inputs distinct: a Git URL built from the resource ID is
answered 401 ("repository ... has no registered resource identity") before
actor verification, and changing `repository` to the path would break approval
matching.

**S5 `TestReleaseSmoke/HumanPush`.** The administrator creates a run-unique
engineer-role starter service and applies a pipeline with `repository`, that
`service` and `on.push` over two run-unique work refs (pipeline management plus
Admin authority; the installed admin persona holds both). A separate inline
execution pushes a commit to one subscribed ref with its mounted token; its
accepted report must carry that execution as correlation and start nothing
(the exclusion control: the test polls the subscribed pipeline's execution
list for a fixed multi-read window before the human push and requires it to
stay empty). The host engineer then pushes H to the other
subscribed ref; its accepted report must carry a verification causation and no
correlation, and exactly one execution of the pipeline must appear, succeed
with complete cleanup and prove the human event's `repository`, `ref`, `commit`
and `previous_commit` inputs in its retained log. `pipeline.start` is never
called for the subscribed pipeline, and the execution list is re-read after
completion to require still exactly one execution. The run-owned starter
service is disabled during cleanup.

## Evidence and redaction

Evidence files are 0600 JSON under a 0700 directory, only allowlisted fields:
installation id/profile/runtime, revisions, server/tools/external-harness image
digests, execution/task/workspace/action/request/verification/report ids,
scenario, candidate/base/target ref, decision, actor, states and durations. Raw
API bodies, environments, JWTs and credential-bearing Git output are never
stored.

For S2 the record carries the task/execution/workspace/action ids, the reviewed
base B, candidate C, target ref main, resource, reviewer decision and actor, the
accepted report event id and its verification id. For S5 it carries the human
event id and verification id, the human commit, the target ref, the triggered
execution/workspace/action ids and the repository; a separate control record
names the execution-originated push and its report.

Captured logs and evidence are scanned for issued-token prefixes, every
configured token value, and Basic/base64-encoded forms before a test renders a
diagnostic. A release run that cannot name a digest-pinned tools image fails
rather than recording `unpinned`. No token is placed in argv, a remote URL,
repository configuration or evidence: the host uses a private HOME, no
inherited Git configuration, a token-reading askpass and the descriptor CA; the
workloads read the mounted token for each Git call via `--config-env`.

## Token lifetime and run duration

The measured local OAuthSonas persona token lifetime is about **899 seconds**.
The runner acquires the tokens immediately before the Go command and refuses a
token that expires inside `Q01_TOKEN_MIN_LIFETIME` (default `Q01_RUN_SECONDS`,
600 s). The full selection (seven live tests plus five scenarios) is expected to
finish well inside the lifetime on the local host. A 401 is a loud
credential-lifetime failure naming the ~899 s lifetime, never a scenario
assertion. `run-installation.sh` does not re-acquire tokens during a run; the
stage session of `scripts/test-suite` refreshes the token files every five
minutes. Never extend the token TTL or retry an authentication failure as
though it passed.

## Limitations this harness does not change

- The local bundle is single host, trusted operators, not production; keys
  live in a local keyfile.
- The local bundle's broker is anonymous and loopback-only. The live
  subscription tests of the portable suite subscribe over loopback there. On
  the stage they use verified TLS, a client certificate and a run-unique
  service credential that the authorization callout accepts.
- Git refs are not deleted.
- A new repository needs a `git serve` restart with its `--resource` mapping.
- The harness exercises no model gateway, MCP proxy, merge coordinator,
  retention sweep or `/metrics` scrape.
- The `implement` action is a deterministic external-harness fixture (a tiny
  shell program and test), not a model, OpenCode/Claude integration or
  native-agent claim.
- The `test` profile's workload secret-name allow-list is empty and not
  configurable, so the smoke materializes public CA bytes in the workspace
  instead of a declared secret. Mounted CA projection is P15/I01e work. The
  certificate is public data, never a private key.
- The default selection of `deploy/podman/smoke.sh` is 12 selected tests: the
  seven live tests plus the five scenarios.
