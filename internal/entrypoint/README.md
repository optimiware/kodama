# `internal/entrypoint` — `kodama step`

The in-Pod entrypoint (task T-016). It runs one ordinary command through the
attempt lifecycle defined in the
[step entrypoint contract](../../docs/implementation/step-entrypoint.md) over the
frozen [runtime contract v1](../../contracts/runtime/README.md): admit, check out
the pinned commit, deliver secrets, run the command in its own process group,
collect declared outputs, publish source (writers), and submit one idempotent
result.

Requirements served: **PX-08** (revoked attempts stop and finalize nothing),
**PX-10** (success validates outputs and source, not just exit; diagnostics are
retained), **PX-14** (the workspace is exactly the pinned commit), **WS-04**
(publication is the control plane's expected-tip compare-and-swap; the step never
writes the branch), **WS-06** (success only after publication is confirmed;
credentials stay out of source), **AR-06** (completion needs a valid structured
result, never just exit zero). Also VS-11 (secret and identity files are never
collected) and WS-A05 (credentials never enter the bundle).

## Bootstrap contract

The init container copies the binary to `/kodama/bin/kodama`; the workload runs
`/kodama/bin/kodama step`. Flags override the environment. Every setting is read
from `KODAMA_STEP_<NAME>` first and the shorter `KODAMA_<NAME>` alias second.

| Flag | Environment (either prefix) | Default | Meaning |
| --- | --- | --- | --- |
| `--server-url` | `KODAMA_STEP_SERVER_URL`, `KODAMA_SERVER_URL` | required | Origin of the runtime API. `https`; plain `http` only for loopback |
| `--attempt-id` | `…ATTEMPT_ID` | required | Attempt ID (routes are `/runtime/v1/attempts/{id}`) |
| `--identity-dir` | `…IDENTITY_DIR` | `/kodama/identity` | Bootstrap files and the token file |
| `--admission-credential-file` | `…ADMISSION_CREDENTIAL_FILE` | `<identity-dir>/admission` | One-time admission credential (trimmed, non-empty) |
| `--checkout-credential-file` | `…CHECKOUT_CREDENTIAL_FILE` | `<identity-dir>/checkout-credential` | Read-only Git credential, one line `username:password`. Absent file = anonymous clone |
| `--token-file` | `…TOKEN_FILE` | `<identity-dir>/token` | Receives the current runtime token (0600, atomically replaced on renewal) for the wrapper's own tools |
| `--job-name`, `--job-uid`, `--pod-name`, `--pod-uid` | `…JOB_NAME`, `…JOB_UID`, `…POD_NAME`, `…POD_UID` | files `<identity-dir>/job-name`, `job-uid`, `pod-name`, `pod-uid` | Job/Pod identity sent in the admission request (all four required) |
| `--workspace-dir` | `…WORKSPACE_DIR` | `/workspace` | Private checkout (must be empty) |
| `--outputs-dir` | `…OUTPUTS_DIR` | `/kodama/outputs` | Output root. Declared `/kodama/outputs/...` paths map onto it |
| `--secrets-dir` | `…SECRETS_DIR` | `/kodama/secrets` | Supplied secrets volume root. Never chmod-ed: declared secrets go into an exclusive process-owned child (0700; files 0600) exposed as `KODAMA_SECRETS_DIR`, removed on every exit |
| `--results-file` | `…RESULTS_FILE` | `/kodama/results.json` | Result manifest |
| `--scratch-dir` | `…SCRATCH_DIR` | `os.TempDir()` | Command log, notes, bundle (removed) |
| `--ca-file` | `…CA_FILE` | system roots | Extra PEM roots for the runtime API and uploads |
| `--git-ca-file` | `…GIT_CA_FILE` | system roots | `GIT_SSL_CAINFO` for the clone |
| `--git-binary` | `…GIT_BINARY` | `git` | Git executable |
| `--kill-grace`, `--finalize-timeout`, `--settle-timeout` | `…KILL_GRACE`, `…FINALIZE_TIMEOUT`, `…SETTLE_TIMEOUT` | 10s, 5m, 2m | SIGTERM→SIGKILL delay; bound for everything after the command; bound for a pending publication |
| `--log-level` | `…LOG_LEVEL` | `INFO` | slog level; logs are JSON on stderr |

The workspace, outputs, secrets and identity directories must not overlap, and
the results and token files must lie outside workspace, outputs and secrets
(validated at start; failure is exit 2). Flags only: positional arguments are
rejected.

**Command environment.** Explicit, never the process environment: an allowlist
(`PATH HOME TMPDIR LANG LC_ALL TZ USER`), then `KODAMA_ATTEMPT_ID`,
`KODAMA_RUN_ID`, `KODAMA_STEP_ID`, `KODAMA_WORKSPACE`, `KODAMA_OUTPUTS_DIR`,
`KODAMA_RESULTS_FILE`, `KODAMA_SECRETS_DIR`, `KODAMA_INPUT_<NAME>` (inputs; strings
verbatim, other values as JSON), the context's `command.env`, and finally secrets
delivered as `env`. The admission credential, runtime token and checkout
credential are never in it. The command runs as `command` + `args` (argv, no
shell) in the workspace, with stdin closed and stdout/stderr merged into one
bounded log.

### Results manifest (`/kodama/results.json`)

Optional unless the step declares non-file outputs. Strict JSON (no duplicate
keys, no floats, no unknown fields, at most 1 MiB, regular file). A stale file is
removed before launch.

```json
{
  "outcome": "completed",
  "outputs": {
    "count": 7,
    "label": "x",
    "flag": true,
    "image": {"digest": "sha256:<64 hex>", "registryRef": "registry.example/app"}
  }
}
```

* `outcome`: `completed` (default when the file is absent), `failed` (optional
  `reason` matching `^[a-z][a-z0-9_]{0,63}$`, no outputs; reported as
  `command_reported_failure` when omitted) or `needs_input` (non-blank
  `question` ≤ 4096 characters, no outputs, no reason).
* `outputs` carries `string`, `boolean`, `integer` and `oci_artifact` values.
  File outputs are **not** listed: they are collected from the declared path
  under the outputs root (regular file, no symlink anywhere on the path, single
  hard link, within `maxOutputBytes`/`maxOutputFiles`). Undeclared outputs and
  file outputs named in the manifest are refused.
* A file output is reserved and finalized as an artifact under its **declared
  output name**, not the file's base name, so `report` may be written to
  `/kodama/outputs/quality.json` or to any subdirectory, and two outputs whose
  files share a base name keep their own names. Two outputs can never reserve the
  same name: a repeated or non-`SafeName` output name fails the result with
  `output_invalid` before anything is uploaded. The diagnostic artifact names are
  unchanged.

Exit zero alone never completes: a declared output that is missing, mistyped or
escapes the root fails the result with `output_invalid` (the exit code stays 0 in
the result and the entrypoint's note names the cause).

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | The control plane durably acknowledged a result — **whatever its outcome**. A failed command is reported through the result, not through the process status |
| 1 | Unexpected internal error |
| 2 | Bad flags/environment/bootstrap files (also an unreadable or empty admission credential) |
| 10 | Admission failure (refused, wrong Job, `profile_unsupported`, consumed, unreachable, invalid context); the command was never launched |
| 11 | Authority lost: attempt revoked or cancelled, token refresh denied or past its maximum life, or the result refused for authority. The command is stopped (or never started) and nothing further is finalized |
| 12 | `result_conflict`: the server holds a different result; it is never rewritten or retried |
| 13 | Bounded finalization failed without durable acknowledgment (loss past `--finalize-timeout`, mismatched acknowledgment, repeated rejection, contract violation) |

### Reason codes of failed results

All are contract-valid `ReasonCode`s. Only `exit_nonzero` and `timeout` appear in
the contract fixtures; the rest are **entrypoint vocabulary the contract does not
define** and must be adopted (or mapped) by T-022:

`exit_nonzero`, `timeout`, `command_failed` (signal), `command_start_failed`,
`command_reported_failure`, `interrupted`, `checkout_failed`, `secret_unavailable`,
`context_mismatch`, `output_invalid` (also covers every missing/invalid/escaping
output and an invalid manifest), `output_upload_failed` (**flag for T-022**: a
transfer failure that is neither a contract refusal nor revocation),
`source_failed`, `secret_in_source`, `bundle_too_large`, `publication_conflict`,
`publication_failed`, `publication_unsettled`, `result_rejected`.

## Lifecycle (docs sequence steps 1–8)

1. `POST /admit` (credential, fresh nonce, `clientVersion`, Job identity), then
   `GET /context`; the two contexts must be identical and the context must
   validate, else nothing launches. A renewal loop (half the remaining life) runs
   until exit; a denied renewal or the maximum token lifetime cancels the run.
2. Checkout (skipped for `workspace.access = none`): `git init`, `fetch origin
   <exact commit>`, detached `checkout`, hardened Git (see below).
3. Secrets are resolved only for the declared bindings and delivered as files or
   env, outside source and outputs.
4. The command runs in its own process group with the context timeout; logs are
   bounded. On cancel/timeout the group gets SIGTERM, then SIGKILL; after the
   leader exits, stray group members are killed.
5. Output validation and upload (`reserve` → upload → `complete`) with local
   checks first, so a refused output uploads nothing.
6. Writers (completed only): commit uncommitted non-ignored work on top of HEAD
   as `Kodama <step@kodama.invalid>`, require that HEAD descends from the pinned
   commit, scan every object of `pinned..HEAD` (including intermediate commits and
   messages) for injected secrets, upload `git bundle` of `pinned..HEAD` as role
   `bundle`, and `POST /source`. `pending` and transport loss repeat the identical
   proposal until `accepted` (or the settle bound: `publication_unsettled`). A
   `409 publication_conflict` is final: failed result, no retry, no overwrite. If
   HEAD equals the expected tip, nothing is proposed and `sourceCommit` is that tip.
7. The result is normalized, checked with `runtimev1.CheckResult`, hashed once and
   the identical payload is resubmitted after transport loss. A 4xx rejection
   replaces it once by a failed `result_rejected`; `result_conflict` and authority
   refusals (`attempt_revoked`, `invalid_credential`, `forbidden`) end the step.
8. Exit only after acknowledgment or a bounded failure (codes above).

Invariants (each has a test): reader never publishes; writer success only after
`/source` accepted; `sourceCommit` only for completed writers; revoked or
refused authority means the command is not launched or is killed and nothing is
uploaded, proposed or submitted afterwards; `result_conflict` is never rewritten.

## Retained log upload (task T-063)

Requirements: **AC-06**, **PX-10**, **VS-11**. The command's merged stdout/stderr
is retained as three ordinary diagnostic artifacts, uploaded through the existing
`Runtime`/`Uploader` ports (no new port, route or wire field):

| Artifact | Content |
| --- | --- |
| `command.log` | Bounded, sanitized command output (`text/plain`) |
| `command.log.meta.json` | `internal/logs/contract` sidecar: source `wrapper`, completeness, reasons, bound to the acknowledged `command.log` ID, digest and size |
| `entrypoint.log` | Bounded, sanitized notes of the entrypoint's own decisions. It says nothing about the completeness of the command log |

Sequence: capture (private temporary file, bounded) → freeze once → sanitize in
fixed chunks → digest/size of the sanitized bytes → reserve, upload and verify
`command.log` (completion state, ID, digest and size must match) → encode,
reserve, upload and verify the sidecar → notes → result. The result cites
`command.log`, then the sidecar, then the notes, only after each was verified
usable. A log without a sidecar has **unknown** completeness; the entrypoint never
uploads a sidecar for a log whose completion is unknown.

* **Redaction.** Injected secret values, the checkout credential and every
  admission/runtime token this process held are removed (`logs.NewRedactor`,
  byte-exact, split/overlapping matches merged; no short-secret exemption).
  When the capture is not whole (truncated, stopped, failed) the redactor's held
  tail is discarded because it may start a value whose rest was never seen. An
  unusable redaction set (more than 256 values, a value over 8 KiB) fails
  closed: the log and notes are withheld, never uploaded unscrubbed. Encodings of
  a value (base64, URL escapes) are not recognized.
* **Completeness.** `complete` only when the whole output was read to its end of
  a command that ran to its own end, nothing was dropped, and every write/close
  succeeded. Truncation or a failed write → `partial/truncated`; a timeout,
  interrupt, pipe read error, failed close, or a pipe still held open by an
  escaped descendant after the drain grace period → `partial/interrupted`.
  Completeness is independent of the command's outcome. An empty, successfully
  observed output is `complete` when the sidecar fits the budget; otherwise
  nothing is uploaded.
* **Aggregate budget** (`limits.maxDiagnosticBytes` covers log, sidecar and notes).
  When the budget is at least 1280 bytes the log may use the budget minus
  640 bytes (sidecar reserve) minus `min(budget/16, 4096)` (notes allowance);
  a smaller budget goes entirely to the log and the sidecar/notes are attempted
  only with what remains. Bytes of an upload whose outcome is unknown are counted
  as spent. This allocation is an entrypoint-local choice; T-064 qualifies it
  against the real artifact limits.
* **Recovery.** The adapter-owned bounded retries (`Client`) replay the identical
  declaration and bytes after lost reservation/PUT/completion responses; there
  is no second retry loop. Diagnostics stay best-effort (a failed upload never
  fails the step); revoked or denied authority and an exhausted finalization
  budget abort without a result. An interrupt during an upload ends with exit 13
  and no result, never with a complete claim for an unfinished upload.
* **Not established here.** Real reservation/completion idempotency, aggregate
  limit enforcement, retained linkage, controller fallback and cross-tenant
  denial belong to T-064, which must observe a log only after the wrapper's
  diagnostic finalization has settled (completeness is pinned at first
  observation). `test/integration/entrypoint/duplicate_test.go` still counts one
  reservation (the command log); with the sidecar it is two.

## Container-log mirror (task T-078)

Requirements: **PX-10**, **AC-06**, **VS-11**. The bytes the entrypoint captures
for `command.log` are also written, after redaction and within the same bound, to
the container's log stream (the entrypoint's stderr, interleaved by whole lines
with its own JSON log lines). It exists so that the controller fallback (T-064)
can still retain diagnostics when the wrapper cannot upload its retained log. It
adds no route, wire field, artifact, port or flag: `Config.ContainerLog` is set by
`Run` to stderr (nil disables it for programmatic `NewStep`/`Execute` use).

* **Best-effort transport, not evidence.** The retained `command.log`, its
  sidecar, the result and the finalization order are unchanged; the mirror has no
  completeness claim. It can be partial for these reasons, each visible as a
  constant marker line in the stream: `[kodama: output truncated]` (bound reached,
  identical to the retained marker), `[kodama: container log mirror dropped output]`
  (queue overflow, written once before the next surviving output) and
  `[kodama: container log mirror disabled: redaction unavailable]` (see below). A
  failed or abandoned sink and a discarded held tail leave no marker, only a
  `container log mirror is partial` warning in the entrypoint log. T-064 must treat
  the mirror as unverified: a retained, sidecar-bound `command.log` always wins.
  The mirror adds one `\n` after an unterminated last line so the next log line
  starts cleanly; repeated identical lines are never suppressed, so a consumer that
  merges the stream with the retained log must do its own de-duplication.
* **Same redaction.** One `logs.Redactor` over the retained log's value set:
  injected file/env secrets, the admission credential, every issued or renewed
  token, and the checkout credential in every representation (password,
  `user:password`, its base64, the `Authorization: Basic` header). The retained log
  and `entrypoint.log` now also redact those representations (the Git source scan
  keeps its previous value set). An unusable set (over 256 values or a value over
  8 KiB) disables the mirror: only the disabled marker is written, never raw bytes.
* **Tokens issued during the command.** The token file is rewritten on renewal
  after the token is registered, so a command can print a renewed token. The
  mirror notices the registry change before the next byte, rebuilds the redactor
  and replays a bounded window (about 44 KiB) of the latest raw input into it, so
  a possible secret start that was being held stays held. The tradeoff is only
  over-redaction: a replayed window may start inside a value that was already
  redacted and merge a few more bytes into a marker. A new value that the command
  printed *before* it was registered is outside the guarantee (it was unknown).
* **Bounded.** At most the retained capture capacity of raw input and of redacted
  output (`dataCapacity(commandLogCapacity(maxDiagnosticBytes))`), then one
  truncation marker. Whole lines are queued, an unterminated line is held up to
  4 KiB. Redacted output waits in a queue of at most 64 chunks of 8 KiB; one
  goroutine writes to the sink. A full queue drops output; a failed sink stops
  writing.
* **Never blocks.** Capture and the process pipe never wait for the sink. After the
  command ends, `Close` flushes (releasing the redactor's held tail only for output
  seen to its end of a command that ran to its own end, discarding it after a
  timeout, interrupt or pipe failure), then waits at most `--kill-grace` for the
  queue before abandoning the sink; later writes are discarded.
* **A stalled stderr cannot block the step.** `Run` gives the entrypoint's logger
  and the mirror one `containerStream` over stderr: a bounded (1 MiB) queue and one
  writer goroutine; `Write` always accepts and never waits, whole writes are
  delivered in order, and when the queue is full or stderr failed the write is
  dropped and one constant `[kodama: container log dropped output]` line precedes
  the next delivered output. Logging, the mirror's `Close` warning and capture
  freeze/upload/result therefore never wait for the container log. At exit `Run`
  waits at most 2 s to flush, then abandons a stalled stderr.
* **Handoff to T-064.** The qualification must run a real Pod, read the Kubernetes
  log stream and prove: ordinary output present, no injected/issued/checkout value
  present, bounded size, and marker lines read as partial. This package proves only
  the entrypoint side against a test sink (`Test_container_log_mirror`,
  `Test_container_log_redaction`, `Test_container_log_bounds`). The kubelet's own
  container-log rotation and node loss are outside this contract.

## `run_shared` (v2), `kodama prepare` and `kodama service-run` (task T-113)

Not wired by `internal/app`: T-116 registers `PrepareCommand()` and
`ServiceRunCommand()` beside `StepCommand()` and has the runtimes pass the
environment below. The sequence is
[the v2 sequence](../../docs/implementation/step-entrypoint.md#run_shared-v2-sequence).

**`kodama step` with `KODAMA_RUNTIME_API=v2`** (unset and `v1` keep every v1 byte;
anything else is a usage error). `NewClientV2`/`NewStepV2`: admit and context on
`/runtime/v2/attempts/{id}`; a context of the other version is refused before any
command (a v1 body on the v2 wire fails validation; a `contextVersion` member on
the v1 wire is a protocol error). No clone, reset or clean and no checkout
credential: `verifyWorkspace` only looks. Source-free: the mount exists and a
probe file can be created and removed (the scratch is never cleared). Source-backed:
`sealedgit.go` reads `HEAD` and the one ref it names as data through checked,
non-following opens (symlinks, gitfile `.git`, foreign owners refused), points a
**private** repository at the workspace's objects, work tree and a private index,
checks the pinned commits exist and computes head, `trackedClean`,
`untrackedPresent`, `ignoredPresent`. `.git/config`, hooks, attribute filters,
credential helpers, `core.worktree`, `include.path` and info files of the workspace
are never read or executed. `POST /begin` is made after secrets are materialized and
immediately before the child starts; the command runs only after an acknowledgment
that is the derivation of the request (`403 attempt_revoked`, a transport failure or
an inconsistent acknowledgment start nothing). A writer's publication uses the same
sealed Git, bundles on top of the run's cumulative head (`headCommit`, the
reservation's input and the expected tip) and, once accepted, moves the scratch's own
HEAD and index to the published commit so the next holder begins clean.

**`kodama prepare`** (environment of seam S4: `KODAMA_SERVER_URL`, `KODAMA_CA_FILE`,
`KODAMA_RUN_ID`, `KODAMA_RESOURCE_KEY=preparation`, `KODAMA_ADMISSION_CREDENTIAL_FILE`,
`KODAMA_WORKLOAD_NAME`, `KODAMA_WORKLOAD_UID`, `KODAMA_POD_NAME`/`KODAMA_POD_UID` for a
Job-owned Pod, `KODAMA_WORKSPACE_DIR`, `KODAMA_CHECKOUT_CREDENTIAL_FILE`): admission on
`/runtime/v2/runs/{run}/resources/preparation` (retrying `resource_unbound` and unknown
outcomes with the same nonce), a refetched context that must equal the admitted one, then
up to `maxAttempts` checkout tries inside the one workload under one timeout (clamped to
the committed deadline): empty workspace only (plus a fresh volume's `lost+found`),
`git init`, `git fetch <https URL> <commit>` with the credential only as an HTTP header in
that process's environment, `--no-recurse-submodules`, hooks/attributes/excludes disabled,
`protocol.allow=never` plus https, `fsckObjects`, no redirects; `checkout --detach`, HEAD
must equal the commit, tree ID computed; no remote or credential configuration is written,
and the repository is scanned for any form of the credential before the receipt. A failed try
removes only its own partial tree. The receipt (`attempts` = tries used) is posted after;
it is idempotent and `receipt_conflict` is final. Exit codes add 14 (checkout exhausted).

**`kodama service-run -- argv…`**: admission as `service.NAME`; the argv must equal the
committed command; only the committed bindings are resolved (file secrets to
`KODAMA_SECRETS_DIR/<name>` 0400 created exclusively, env secrets into the child's
environment); the token is dropped before the service runs (no renewal, no other call);
the child gets the process environment minus every `KODAMA_*` variable plus the committed
env, runs in its own process group with stdout/stderr passed through, receives forwarded
signals, is killed after the grace once the supervisor is stopped, and its exit status is
mirrored (128+n for a signal). Exit 15: delivery or start failed.

Tests: `Test_unit_entrypoint_v2_*`, `Test_unit_prepare_*`, `Test_unit_service_run_*`,
`Test_unit_publication_git_hardening` and a round trip against the real runtime authority.

## Unit qualification (task T-082)

Requirements: **PX-05**, **PX-07**, **VS-03**, **VS-07** (with PX-08/PX-10/VS-11 above).
`unit_step_test.go` drives the unchanged ports with the scriptable fake runtime, a
real child process in a temporary Pod layout and a local HTTPS repository; no image
or container is provisioned (real image execution stays T-026, production
qualification stays T-064).

* `Test_unit_step_lifecycle` - exit, interrupt, revocation and timeout ordering:
  the log and its sidecar precede outputs, source and the single final result; a
  failed command collects and publishes nothing; a revoked attempt stops the
  command and uploads, proposes and submits nothing.
* `Test_unit_step_retry` - lost result/log/proposal responses replay the identical
  payload and declaration (one artifact per name, one bundle) without rerunning
  the command; a consumed admission, a mismatched acknowledgment, a conflicting
  result and a publication conflict are never retried or rewritten.
* `Test_unit_step_secret_scope` - the checkout credential reaches only the fetch
  (not the command environment, `.git`, logs, scratch, uploads or source bundle),
  anonymous checkout sends none, and secret resolution asks only for declared
  pinned references and refuses rotated or foreign answers before launch.

## Git and credential handling

* The clone credential travels as `http.<origin>.extraHeader` in the environment
  of the single `git fetch` (`GIT_CONFIG_COUNT/KEY/VALUE`). The remote URL is
  stored without it; `.git/config` has no header; errors are redacted.
* Git runs with no system/global config, `core.hooksPath=/dev/null`, no
  credential helpers or prompts, `GIT_ALLOW_PROTOCOL=https`, no redirects, no
  submodule recursion, `commit --no-verify --no-gpg-sign`.
* The bundle holds objects only. Before upload the objects are scanned for: the
  checkout password, every injected secret value, the admission credential and
  every runtime token this process held. Values shorter than 8 bytes are not
  scanned (a documented limit); secret files and identity live outside the
  workspace and are never committed.
* The runtime token lives in memory and in `--token-file` only.

## Design (hexagonal)

`step.go`, `source.go` and `result.go` are the application core and own every
decision. They depend on two consumer-owned ports in `port.go`:

* `Runtime` — the eight runtime operations (adapter: `Client`, bearer token,
  bounded retry of transient failures only, error taxonomy `APIError`,
  `TransportError`, `ProtocolError`).
* `Uploader` — transfer to short-lived upload instructions (adapter: `Client`;
  no bearer token is sent).

Process, Git, outputs, secrets and config are adapters over the OS. `cli.go`
(`Run`, `Execute`, `StepCommand`) is the composition. Unit tests drive the core
with a scriptable fake `Runtime` (`fakert_test.go`) and real local Git;
integration tests run it against an in-process contract server, real TLS
`git-http-backend` repositories and real processes.

## Running the checks

```bash
# Unit + integration, race detector (full)
timeout 300 go test -race -count=1 ./internal/entrypoint/... ./test/integration/entrypoint/...

# The five named checks
timeout 180 go test -count=1 -v -run 'Test_exit_zero_missing_output' ./test/integration/entrypoint/   # exit_zero_missing_output (PX-10, AR-06)
timeout 180 go test -count=1 -v -run 'Test_path_escape'              ./test/integration/entrypoint/   # path_escape (PX-10, VS-11)
timeout 180 go test -count=1 -v -run 'Test_cancel_child_tree'        ./test/integration/entrypoint/   # cancel_child_tree (PX-08)
timeout 180 go test -count=1 -v -run 'Test_duplicate_result'         ./test/integration/entrypoint/   # duplicate_result (PX-10)
timeout 180 go test -count=1 -v -run 'Test_source_conflict'          ./test/integration/entrypoint/   # source_conflict (WS-04, WS-06)

# Also in the integration package
timeout 180 go test -count=1 -v -run 'Test_cancelled_admission|Test_profile_unsupported|Test_token_refresh|Test_exact_commit_checkout|Test_cli_happy_paths|Test_scenarios_replay' ./test/integration/entrypoint/
```

Git-backed checks skip when `git` or `git-http-backend` is missing. The contract
server in `test/integration/entrypoint/server_test.go` is a test double of the
control plane, not production evidence.

## Handoff

* **T-019 (executor / Job creation)** — inject the files above (`admission`,
  optional `checkout-credential`, `job-*`/`pod-*` identity, or the `KODAMA_STEP_*`
  variables from the downward API), memory-backed secrets dir, emptyDir workspace
  and outputs; set `terminationGracePeriodSeconds` above `--kill-grace` plus the
  finalization budget. Treat exit 10–13 as "no acknowledged result"; exit 0 is not
  proof of success (read the attempt status).
* **T-021 (runtime API/auth)** — implement what `server_test.go` doubles: admission
  replay by nonce, `attempt_revoked` on every route after cancel, token renewal
  bounded by `maxExpiresAt`, `/source` idempotent for identical proposals (pending
  → accepted) with `publication_conflict.details.observedTip`, `/result` idempotent
  by hash and `result_conflict` otherwise. The step sends the bundle as
  `role: "bundle"`, name `source.bundle`, media type `application/x-git-bundle`,
  holding a ref `refs/kodama/candidate` for `pinned..candidate`.
* **T-022 (results/outputs)** — adopt or map the reason vocabulary above,
  especially `output_upload_failed`; the result's `diagnosticArtifactIds` holds the
  bounded `command.log` and the entrypoint's own `entrypoint.log` (a plain-text
  explanation; it can contain Git/transport error text, never credentials).
* **T-027 / composition root** — register without editing this package:
  `entrypoint.StepCommand()` returns `Command{Name: "step", Run}`; dispatch
  `kodama step …` to `cmd.Run(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)`
  and `os.Exit` with the returned code. `Run` installs SIGTERM/SIGINT handling
  itself. The release build may set `entrypoint.Version` with `-ldflags`.
