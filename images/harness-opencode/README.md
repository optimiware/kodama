# images/harness-opencode — headless OpenCode wrapper image (task T-031)

The first real agent image for the [agent runtime](../../docs/specs/agent-runtime.md): a pinned
OpenCode build driven by a small Go wrapper under the common step entrypoint. It consumes the
[wrapper protocol checkpoint](../../docs/implementation/opencode-wrapper-protocol.md) (T-074) and
qualifies it against the merged entrypoint, agent profiles, model gateway, MCP proxy, artifacts and
workspace publication. Requirements: AR-02, AR-05, AR-06, AR-09, AR-11, AR-14, GM-12 (supporting
AR-01, AR-10, AR-13, GM-07). Acceptance evidence lives in
[`test/integration/harness-opencode`](../../test/integration/harness-opencode/README.md).

**What this is not.** It is not a production pin of a provider or model: every model response in the
acceptance run comes from a deterministic OpenAI-compatible fixture reached through the real gateway.
It does not edit production wiring (T-027/T-039) or populate `agents.Catalogue`.

## Layout

| Path | Purpose |
| --- | --- |
| `Containerfile`, `build.sh` | Reproducible image build (podman), checksum-verified OpenCode binary, optional registry push, `evidence/build.json` |
| (build context) `opencode-system.json` | rendered by `build.sh` from `spec.ManagedConfig()` (`wrapper/cmd/render-config`); baked as `/etc/opencode/opencode.json` |
| `wrapper/` | `kodama-opencode-wrapper`: payload decoding, config, loopback credential proxy, process containment, structured completion, conformance report |
| `wrapper/spec/` | constants shared with the image build and the acceptance composition (versions, paths, reason codes, managed configuration) |
| `evidence/build.json` | what the last documented build produced (digests, checksums, package versions) |

## Build

```bash
# podman, a checksum-verified OpenCode binary, optional registry push
OPENCODE_BIN=$HOME/.opencode/bin/opencode \
  images/harness-opencode/build.sh --push localhost:33067/t-batch/t031/harness-opencode
```

Pinned inputs: base `debian@sha256:5ae3c39e…` (bookworm-slim); apt packages from the
`snapshot.debian.org` timestamp `20260824T000000Z`; OpenCode `2.1.2`, sha256
`22636c800ba6c8dc5353e264183bb3927725d824cba412f007dea9c694740f52` (the binary is copied from the
build context and verified inside the build; it is never committed); the wrapper built from this
tree with `-trimpath -buildid=`. The inputs are pinned and the wrapper binary is bit-for-bit reproducible (two builds, one sha256, recorded in
`evidence/build.json`); the image digest is **not** (layer timestamps), so the registry digest of each build is
recorded there and printed by the acceptance run. The
acceptance run builds and pushes the image itself and runs the Jobs by that digest.

## Invocation, measured (OpenCode 2.1.2)

`opencode run --standalone --format json --model kodama/<model> --title <t> <prompt>` with stdin closed,
the workspace as working directory and an explicit environment. `--dir` and `--pure` are not used
(retired on 2.1.2). `--standalone` is mandatory: without it `run` talks to the shared per-user service.
Measured, not assumed (all re-runnable with the local binary):

| Observation | Consequence |
| --- | --- |
| `run --standalone` starts `opencode serve --stdio --port 0` as a child **in its own session**; the shell tool runs commands in their own process groups; `setsid` escapees re-parent to PID 1 | a process-group signal does not stop the tree: the wrapper is a child **subreaper** and kills by ancestry (U-04) |
| a permission left at `ask` under `run` with no human is **auto-rejected** (stderr `permission requested: shell (…); auto-rejecting`), never waited on | no interactive wait (U-03); the wrapper still writes explicit allow/deny rules, never `ask`, and never passes `--auto` |
| a workspace `opencode.json` **overrides** the user-level provider `baseURL` (a redirect succeeded) | untrusted workspace configuration (U-05): the highest-precedence managed file `/etc/opencode/opencode.json` fixes the provider endpoint, verified with `opencode debug config` and a redirected project file; the wrapper also sets `OPENCODE_DISABLE_PROJECT_CONFIG=1` (measured: ignores project configuration; present in the binary, absent from the V2 documentation) |
| a model `limit.output` is honoured as `max_completion_tokens`; without it OpenCode asks for 32000 | the wrapper sets the output cap (`KODAMA_OPENCODE_OUTPUT_TOKENS`, default 4096) so requests fit the gateway's run bound; the session-title request carries no cap and uses the run's limit |
| request members: `model`, `messages`, `stream`, `stream_options{include_usage}`, `tools`, `max_completion_tokens`; every tool declared with `"strict": false` | the gateway rejects `strict`; see mismatch M-2 |
| `tools` offered with `* deny` plus allows: `edit glob grep read shell write` and the allowed MCP tools; `question skill subagent webfetch websearch execute` are not offered | explicit deny-by-default permissions work as the hide mechanism (GM-07) |
| MCP servers connect asynchronously: the first model turn can precede their tools | scripted turns wait for the tool to appear; a real profile must not assume MCP tools on turn one |
| MCP `protocol: legacy` (default) interoperates with the proxy pinned to `2025-11-25` through the loopback proxy (initialize, list, call) | U-08 for `legacy` resolved by observation; `auto` and `2026-07-28` remain unused |
| a failing upstream (HTTP 500 through the gateway) made OpenCode send 11 requests for one prompt before exiting 1 with an `error` event | the harness retries on its own; the gateway admitted exactly one (`unknown/provider_error`, upstream called once) and refused the other ten at admission while it was unresolved (GM-05); the wrapper reports `harness_error` |
| stdout events: `step_start`, `text`, `tool_use`, `step_finish`, `error`; a final result envelope does not exist | completion is the agent's result file, not the stream (U-02) |

## The wrapper

Command: `/opt/kodama/bin/kodama-opencode-wrapper <payload args>` (the profile's pinned wrapper command; the
arguments are the `kodama.agent-config/v1` payload decoded with `agents.DecodeArgs`).

1. **Fail-closed manifest.** A provisional `{"outcome":"failed","reason":"wrapper_incomplete"}` is written to
   `KODAMA_RESULTS_FILE` first and replaced only by a reached verdict, so an exit status of zero cannot complete
   a step (U-10). Verdicts and reason codes (`wrapper/spec`): `payload_invalid`, `config_invalid`,
   `harness_version_mismatch`, `harness_start_failed`, `harness_exit_nonzero`, `harness_error`, `harness_idle`,
   `result_missing`, `result_invalid`, `context_mismatch`, `output_undeclared`, `commit_failed`,
   `agent_reported_failure`.
2. **Pins are checked, not trusted.** Wrapper and harness versions must equal the pins in the image, the payload's
   baseline digest and profile pin must equal the runtime context's (`GET /context`), and the step must declare the
   `conformance` artifact output (and `transcript` when the profile records).
3. **Structured completion.** The agent must write `.kodama-result.json` (strict JSON, no duplicate or unknown
   members, regular file only). `completed` needs a summary, `needs_input` a question (→ AR-13), `failed` an optional
   reason code. A prose reply, a clean event stream or exit 0 without it is `result_missing`. The file is removed
   before anything is committed.
4. **Commit and conformance.** On `completed` the wrapper commits the workspace (identity `Kodama
   <step@kodama.invalid>`, hooks off, no push, no Git credential) and writes the declared conformance report bound
   to the pinned baseline digest and that candidate commit. Required checks come from the baseline; an unreported
   check is `not_run`; deviations are kept, sorted and always block. The entrypoint then proposes the commit through
   the workspace protocol (AR-11).
5. **Credentials.** OpenCode never holds a credential. It is configured with plain-HTTP loopback endpoints
   (`127.0.0.1:18941`); the wrapper's proxy forwards `POST /model/v1/chat/completions` to
   `…/gateway/v1/attempts/{attempt}/chat/completions` and `/mcp/{registration}` (only registrations the profile
   names) to `…/internal/mcp/v1/attempts/{attempt}/servers/{registration}`, replacing any inbound credential with the
   **current** token read from the token file on every request (renewal needs no restart), over TLS verified against
   the platform CA. No redirects, no environment proxy.
6. **Transcript.** The harness's stdout is never copied to the retained command log, which holds only content-free
   counts. With `transcript.record` the redacted NDJSON stream becomes the declared `transcript` output; redaction
   removes every token value read from the token file and bearer/JWT shapes (encodings of a value are not
   recognized, as in the entrypoint's own log redaction). Harness error texts are logged only when recording is on.
7. **Cancellation.** SIGTERM stops the harness and every descendant: the wrapper becomes a child subreaper, signals
   by ancestry (TERM, then KILL after 3 s) until none is left, and exits 143 without a verdict. After a normal end it
   sweeps background processes the agent left behind.
8. **No interactive wait.** Stdin is `/dev/null`; permissions are explicit; `question`, web, skill and subagent tools
   are denied by policy; an idle watchdog (`KODAMA_OPENCODE_IDLE_SECONDS`, default 900) kills a silent harness.

Inputs the wrapper reads: `KODAMA_PLATFORM_URL` (else the parent `kodama step`'s `--server-url`), `KODAMA_TOKEN_FILE`
(default `/kodama/identity/token`), `KODAMA_CA_FILE` (default `/kodama/bootstrap/ca.crt`), and the entrypoint's
`KODAMA_ATTEMPT_ID`, `KODAMA_WORKSPACE`, `KODAMA_OUTPUTS_DIR`, `KODAMA_RESULTS_FILE`.

## Mismatches found, and who owns the next step

These are real protocol or wiring mismatches observed while qualifying. None was fixed outside this task's paths.

| # | Mismatch | Interim handling here | Owner / next step |
| --- | --- | --- | --- |
| M-1 | The entrypoint does not export the platform origin (or token and CA paths) to its command, and an agent step cannot carry `env` (`unknown_field`) | wrapper defaults for the token and CA paths (the Job recipe's paths); the origin falls back to the parent `kodama step --server-url` (accepted only when the parent really is `kodama step`) | T-016/T-021: export `KODAMA_SERVER_URL` (or the origin) to the command; then drop the fallback |
| M-2 | OpenCode 2.1.2 sends `tools[].function.strict:false`; gateway `chat-completions-stream/1` rejects the member (`400 invalid_request`) | the wrapper proxy drops exactly `strict:false` (the default) and forwards everything else, including `strict:true`, unchanged | T-032: accept `strict` in a new protocol revision with fixtures; then drop the shim |
| M-3 | **CLOSED by T-079:** the Job delivers a read-only checkout credential and Git CA in its immutable bootstrap Secret | the entrypoint receives `--checkout-credential-file` and `--git-ca-file` from the Job; the image no longer defaults `KODAMA_GIT_CA_FILE`, and the test composition injects no Git authorization | T-016/T-021: delivered in T-079 |
| M-4 | In a Job the workspace `emptyDir` is root-owned while the entrypoint's Git runs unprivileged: Git refuses the first command (`dubious ownership`); the entrypoint runs Git with no system or global config | `/usr/local/bin/git` shim adds exactly `-c safe.directory=/workspace` | T-016/T-021: own the fix (ownership of the mount or the Git invocation); then drop the shim |
| M-5 | The entrypoint names a file artifact by the **basename** of its declared path; the artifact module accepts only the **declared output name** (`output_invalid` 422) | outputs are declared with basename == name (`/kodama/outputs/conformance`, `/kodama/outputs/transcript`) | T-016/T-015: agree one rule (names by output name is the module's contract) |
| M-6 | The contract requires evidence artifact IDs for a `passed` conformance check; IDs are assigned by the artifact store only when the entrypoint uploads, after the wrapper exits | an agent claim of `passed` is recorded `not_run`, so every wrapper report is `blocked` and none can reach `conforms` | T-034/T-016: a post-upload evidence binding (or a wrapper-side evidence upload with a runtime client). Until then no wrapper report can reach `conforms` |
| M-7 | `kube.DefaultActions` omits `model.invoke` and `tool.invoke`, so a default Job cannot reach the gateway or proxy | the composition sets `ReaderActions`/`WriterActions` explicitly | T-021/T-039: configure the ceilings for agent profiles |
| M-8 | OpenCode connects MCP servers asynchronously (first turn can lack MCP tools) and retries failed model requests on its own | scripted turns wait for tools; failures surface as `harness_error` | profile authors: do not rely on tools in turn one; request budgets must count harness retries |

## Unavailable or unproven controls (honest list)

* **Renewal over a long run.** The proxy re-reads the token file on every request, but no run here outlived one
  token lifetime (5 minutes), so mid-run renewal is not observed end to end.
* **`tool_calls` limit** (`maxToolCalls`): the MCP proxy's own tests cover it; this task does not exercise a
  harness hitting it, so the catalogue entry below does not claim it.
* **Model allow-list and request/token allowance against the real harness**: enforced by the gateway (T-032 tests);
  here the harness only ran inside them (usage rows are recorded per request). No second model was tried.
* **A real provider or model**: none. The upstream is a deterministic fixture; GM-12 is satisfied for the pinned
  protocol/harness/gateway combination, not for any provider.
* **Hosted identity, delegation and credentials (O-18/O-19)** and **`conforms` evidence** (M-6).
* **Byte-for-byte image reproducibility** (the wrapper binary is reproducible; the image is not).

## Compatibility catalogue handoff (T-039)

`agents.Catalogue` is empty by design. The entry this evidence supports (to be created by the integration owner
from the recorded digest; it must not claim more):

```text
Tuple      image   <registry>@sha256:<pushedDigest of evidence/build.json>
           wrapper kodama-opencode 0.1.0, command ["/opt/kodama/bin/kodama-opencode-wrapper"]
           harness opencode 2.1.2
Controls   tool_allowlist, transcript            (proved by tool_scope_denied and opencode_real_smoke)
           model                                 (the gateway allow-list; only the one route is exercised)
Not claimed requests, tokens, tool_calls
Evidence   the T-031 acceptance run recorded in the task handoff
```

Wall time stays an execution deadline, not a control.
